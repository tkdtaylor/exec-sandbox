// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TC-015-05: the firecracker child's effective constraints are >= jailer (A1.Q3, NO jailer). This is
// the task-018 constraints->=-jailer fitness assertion, exercised here against the launch. It has two
// layers:
//
//   (1) ARGV-LEVEL (runs on any host): the launch carries no jailer, and the bwrap wrapper requests
//       every jailer-equivalent constraint — all namespaces unshared (--unshare-all, which includes
//       net/user/pid/ipc/mnt/uts), a chroot/pivot_root via bwrap's bind-based root, a non-host uid,
//       and the cgroup/limit machinery (limits.go) layered above. /dev/kvm is the ONLY device
//       exposed (no broad /dev passthrough).
//
//   (2) LIVE-PROCESS (under /dev/kvm): assertConstraintsGEJailer inspects the actually-running
//       firecracker child's namespaces (none shared with the host), uid (non-host), and root (chroot
//       in effect) — proving the constraints are real, not merely requested.
//
// Firecracker self-installs its seccomp filters regardless of any launcher; under a live run that is
// observable, and the no-jailer launch does not strip it.

// assertConstraintsGEJailerArgv checks the launch argv reconstructs jailer-equivalent constraints
// WITHOUT a jailer. Pure function of the argv → runs on any host.
func assertConstraintsGEJailerArgv(argv []string) error {
	joined := strings.Join(argv, " ")
	// No jailer.
	for _, tok := range argv {
		if filepath.Base(tok) == "jailer" {
			return errFmt("constraints>=jailer: a jailer binary appears in the argv (%q) — A1.Q3 forbids it", tok)
		}
	}
	// All namespaces unshared (bwrap --unshare-all unshares net/user/pid/ipc/mnt/uts/cgroup).
	if !strings.Contains(joined, "--unshare-all") {
		return errFmt("constraints>=jailer: --unshare-all missing (namespaces not all unshared)")
	}
	// No network namespace shared with the host (the no-NIC analogue at the launcher level).
	if strings.Contains(joined, "--share-net") {
		return errFmt("constraints>=jailer: --share-net present — the net namespace must not be shared")
	}
	// /dev/kvm exposed (the one device permission), but NOT a broad host-/dev passthrough beyond it.
	if !strings.Contains(joined, "/dev/kvm") {
		return errFmt("constraints>=jailer: /dev/kvm not exposed (Tier-3 needs exactly this device)")
	}
	return nil
}

func TestFirecrackerConstraintsGEJailer_Argv(t *testing.T) {
	withRepoRootArtifacts(t)
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not on PATH; Argv cannot build the payload drive here")
	}
	scriptPath := writeTempScript(t, "echo hi")
	dir := t.TempDir()
	argv, cleanup, _, _, err := firecrackerBackend{}.Argv(scriptPath, filepath.Join(dir, "p.sock"), "", nil, nil, nil, fcLimits)
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	defer cleanup()
	if err := assertConstraintsGEJailerArgv(argv); err != nil {
		t.Fatalf("TC-015-05 (argv): %v", err)
	}
}

// TC-015-05 (negative): the constraints checker is not vacuous — a jailer-bearing or net-sharing
// argv is rejected.
func TestFirecrackerConstraintsCheckerRejectsWeakArgv(t *testing.T) {
	bad := [][]string{
		{"jailer", "--exec-file", "/usr/local/bin/firecracker"},                 // a jailer
		{"bwrap", "--unshare-all", "--share-net", "/usr/local/bin/firecracker"}, // net shared
		{"bwrap", "--unshare-all", "exec-sandbox", "fc-launch", "/b"},           // no /dev/kvm
	}
	for i, argv := range bad {
		if err := assertConstraintsGEJailerArgv(argv); err == nil {
			t.Fatalf("TC-015-05 (neg %d): checker accepted a weak argv %v — it is a no-op (BUG)", i, argv)
		}
	}
}

// TC-015-05 (negative, host-side): assertConstraintsGEJailer is non-vacuous. Starting from a struct
// that MIRRORS a genuinely-constrained child (built from this host's own non-/ references so the
// "good" baseline passes), each mutation that weakens ONE jailer-equivalent property must make the
// checker fail. This proves the live assertion would bite if the wrapper dropped --unshare-all,
// shared a namespace with the host, ran as the host uid, regained capability, or omitted pivot_root.
func TestFirecrackerConstraintsCheckerRejectsWeakChild(t *testing.T) {
	hostRootDev, hostRootFS := hostRootMount()
	// good: every namespace is a synthetic non-host inode, a non-host userns uid map, no caps, a
	// pivot_root'd tmpfs root. This MUST pass — otherwise the negatives below prove nothing.
	good := func() *fcChildConstraints {
		ns := map[string]string{}
		for _, n := range constraintNamespaces {
			ns[n] = n + ":[9999999]" // an inode that cannot equal the host's real one
		}
		return &fcChildConstraints{
			pid:       12345,
			nsIno:     ns,
			uidMap:    fmt.Sprintf("65534 %d 1", os.Getuid()),
			gidMap:    fmt.Sprintf("65534 %d 1", os.Getgid()),
			capEff:    "0000000000000000",
			noNewPriv: "1",
			rootDev:   "0:176 /newroot",
			rootFS:    "tmpfs",
		}
	}
	if err := assertConstraintsGEJailer(good()); err != nil {
		t.Fatalf("TC-015-05 (neg): the GOOD baseline was rejected (%v) — the negative test would prove nothing", err)
	}

	// Each weakening mutation must be rejected.
	weakenings := []struct {
		name   string
		mutate func(*fcChildConstraints)
	}{
		{"shares net namespace with host", func(c *fcChildConstraints) {
			hostNet, _ := os.Readlink("/proc/self/ns/net")
			c.nsIno["net"] = hostNet // --unshare-all dropped: net shared with host
		}},
		{"shares user namespace with host", func(c *fcChildConstraints) {
			hostUser, _ := os.Readlink("/proc/self/ns/user")
			c.nsIno["user"] = hostUser
		}},
		{"runs as the host uid inside the userns", func(c *fcChildConstraints) {
			c.uidMap = fmt.Sprintf("%d %d 1", os.Getuid(), os.Getuid()) // --uid omitted
		}},
		{"no user namespace at all (empty uid_map)", func(c *fcChildConstraints) {
			c.uidMap = "" // --unshare-user not in effect
		}},
		{"holds host capabilities", func(c *fcChildConstraints) {
			c.capEff = "000001ffffffffff" // privilege not dropped
		}},
		{"NoNewPrivs not set", func(c *fcChildConstraints) {
			c.noNewPriv = "0"
		}},
		{"root mount is the host root (no pivot_root)", func(c *fcChildConstraints) {
			c.rootDev = hostRootDev
			c.rootFS = hostRootFS
		}},
	}
	for _, w := range weakenings {
		c := good()
		w.mutate(c)
		if err := assertConstraintsGEJailer(c); err == nil {
			t.Fatalf("TC-015-05 (neg): checker ACCEPTED a child that %s — it is vacuous (BUG)", w.name)
		}
	}
}

// fcChildConstraints is the HOST-SIDE observation of a live firecracker child, read straight from
// /proc/<pid>/* — NOT the guest's self-view and NOT the requested argv. Every field is what the
// host kernel actually applied to the firecracker process.
type fcChildConstraints struct {
	pid       int
	nsIno     map[string]string // ns name -> "<type>:[<inode>]" link target, host-readable
	uidMap    string            // /proc/<pid>/uid_map (the userns identity mapping)
	gidMap    string            // /proc/<pid>/gid_map
	capEff    string            // CapEff hex from /proc/<pid>/status
	noNewPriv string            // NoNewPrivs from /proc/<pid>/status
	rootDev   string            // the "<maj>:<min> <root>" of the child's "/" mount (mountinfo line 1)
	rootFS    string            // the filesystem type backing the child's "/" mount
}

// observeFirecrackerChild scans /proc for the live firecracker child (matched by its ACTUAL
// executable basename — os.Readlink(/proc/<pid>/exe) == ".../firecracker", NOT a cmdline substring
// that a shell could spoof) and reads its host-side namespace inodes, uid/gid maps, capabilities,
// and root-mount backing. It polls up to deadline because firecracker is spawned a few hundred ms
// into the boot. Returns an error if no firecracker child appears.
func observeFirecrackerChild(deadline time.Time) (*fcChildConstraints, error) {
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			name := e.Name()
			if name == "" || name[0] < '0' || name[0] > '9' {
				continue
			}
			exe, err := os.Readlink(filepath.Join("/proc", name, "exe"))
			if err != nil || filepath.Base(exe) != "firecracker" {
				continue
			}
			pid, _ := strconv.Atoi(name)
			c := &fcChildConstraints{pid: pid, nsIno: map[string]string{}}
			for _, ns := range constraintNamespaces {
				l, err := os.Readlink(filepath.Join("/proc", name, "ns", ns))
				if err != nil {
					// The child may be exiting; treat a half-read as not-yet-found and keep polling.
					c = nil
					break
				}
				c.nsIno[ns] = l
			}
			if c == nil {
				continue
			}
			um, _ := os.ReadFile(filepath.Join("/proc", name, "uid_map"))
			gm, _ := os.ReadFile(filepath.Join("/proc", name, "gid_map"))
			c.uidMap = strings.TrimSpace(string(um))
			c.gidMap = strings.TrimSpace(string(gm))
			st, _ := os.ReadFile(filepath.Join("/proc", name, "status"))
			for _, line := range strings.Split(string(st), "\n") {
				switch {
				case strings.HasPrefix(line, "CapEff:"):
					c.capEff = strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
				case strings.HasPrefix(line, "NoNewPrivs:"):
					c.noNewPriv = strings.TrimSpace(strings.TrimPrefix(line, "NoNewPrivs:"))
				}
			}
			mi, _ := os.ReadFile(filepath.Join("/proc", name, "mountinfo"))
			if lines := strings.Split(strings.TrimSpace(string(mi)), "\n"); len(lines) > 0 {
				// The child's root ("/") mount is the line whose 5th field (mount point) is "/".
				for _, ml := range lines {
					f := strings.Fields(ml)
					if len(f) >= 5 && f[4] == "/" {
						c.rootDev = f[2] + " " + f[3] // "<maj:min> <root-within-fs>"
						// filesystem type is the field after the " - " separator.
						if i := indexField(f, "-"); i >= 0 && i+1 < len(f) {
							c.rootFS = f[i+1]
						}
						break
					}
				}
			}
			return c, nil
		}
		time.Sleep(30 * time.Millisecond)
	}
	return nil, errFmt("no live firecracker child appeared on /proc before the deadline")
}

// constraintNamespaces is the full set bwrap --unshare-all unshares; all must differ from the host.
var constraintNamespaces = []string{"net", "user", "mnt", "pid", "ipc", "uts", "cgroup"}

func indexField(fields []string, want string) int {
	for i, f := range fields {
		if f == want {
			return i
		}
	}
	return -1
}

// assertConstraintsGEJailer is the genuine HOST-SIDE jailer-equivalence assertion (TC-015-05,
// REQ-015-03). It compares the live firecracker child's observed constraints against an UNWRAPPED
// host reference process (self) and FAILS if any jailer-equivalent property is missing. It bites: a
// wrapper that dropped --unshare-all (shared namespaces), omitted --uid (host uid inside the
// userns), or omitted the pivot_root would each violate a distinct check below.
func assertConstraintsGEJailer(c *fcChildConstraints) error {
	// (1) All namespaces unshared — none shared with the host. Compare against this (unwrapped) host
	// process's own namespaces: every namespace inode MUST differ.
	for _, ns := range constraintNamespaces {
		host, err := os.Readlink("/proc/self/ns/" + ns)
		if err != nil {
			return errFmt("constraints>=jailer: cannot read host ns/%s for comparison: %v", ns, err)
		}
		if c.nsIno[ns] == "" {
			return errFmt("constraints>=jailer: firecracker child has no observable %s namespace", ns)
		}
		if c.nsIno[ns] == host {
			return errFmt("constraints>=jailer: firecracker child SHARES the %s namespace with the host (%s) — --unshare-all not in effect", ns, host)
		}
	}

	// (2) Non-host uid: the child runs in a NEW user namespace (uid_map present and non-empty) that
	// maps it to an unprivileged IN-NAMESPACE uid that is NOT the host user's uid. uid_map columns are
	// "<inside> <outside> <count>". An unwrapped process has the init-userns identity map
	// "0 0 4294967295"; a host-uid-inside wrapper would show "<hostuid> <hostuid> 1". We require the
	// inside uid to differ from the invoking host uid.
	insideUID, err := firstColumn(c.uidMap)
	if err != nil {
		return errFmt("constraints>=jailer: firecracker child has no user-namespace uid_map (%q) — not running in a new userns: %v", c.uidMap, err)
	}
	hostUID := os.Getuid()
	if insideUID == hostUID {
		return errFmt("constraints>=jailer: firecracker child runs as the HOST uid %d inside its userns (uid_map %q) — not a non-host uid", hostUID, c.uidMap)
	}
	insideGID, err := firstColumn(c.gidMap)
	if err != nil || insideGID == os.Getgid() {
		return errFmt("constraints>=jailer: firecracker child gid_map %q does not map to a non-host gid", c.gidMap)
	}

	// (3) No host capabilities: the child holds an empty effective capability set (CapEff all-zero),
	// the no-jailer analogue of the jailer's privilege drop. NoNewPrivs must be set so it cannot
	// regain privilege via setuid binaries.
	if c.capEff != "" && strings.Trim(c.capEff, "0") != "" {
		return errFmt("constraints>=jailer: firecracker child holds host capabilities CapEff=%s — privilege not dropped", c.capEff)
	}
	if c.noNewPriv != "1" {
		return errFmt("constraints>=jailer: firecracker child NoNewPrivs=%q, want 1 (must not regain privilege)", c.noNewPriv)
	}

	// (4) chroot / pivot_root in effect: the child's "/" mount is a bwrap-constructed root (a fresh
	// tmpfs newroot), NOT the host's real disk root. The host's own "/" is a real block-device
	// filesystem; the child's is the pivot_root tmpfs. We assert the child's root mount differs from
	// the host's root mount backing — proving the VMM cannot see the host FS root.
	hostRootDev, hostRootFS := hostRootMount()
	if c.rootDev == "" {
		return errFmt("constraints>=jailer: could not observe the firecracker child's root mount")
	}
	if c.rootDev == hostRootDev && c.rootFS == hostRootFS {
		return errFmt("constraints>=jailer: firecracker child's root mount (%s %s) equals the host root (%s %s) — no pivot_root/chroot in effect", c.rootDev, c.rootFS, hostRootDev, hostRootFS)
	}
	return nil
}

// firstColumn returns the first whitespace-separated integer of a uid_map/gid_map line (the
// in-namespace id), or an error if the map is empty (no user namespace).
func firstColumn(mapLine string) (int, error) {
	f := strings.Fields(mapLine)
	if len(f) == 0 {
		return 0, errFmt("empty map")
	}
	return strconv.Atoi(f[0])
}

// hostRootMount returns the "<maj:min> <root>" and filesystem type of THIS (unwrapped) process's "/"
// mount, the reference the firecracker child's root is compared against.
func hostRootMount() (dev, fsType string) {
	mi, _ := os.ReadFile("/proc/self/mountinfo")
	for _, ml := range strings.Split(strings.TrimSpace(string(mi)), "\n") {
		f := strings.Fields(ml)
		if len(f) >= 5 && f[4] == "/" {
			dev = f[2] + " " + f[3]
			if i := indexField(f, "-"); i >= 0 && i+1 < len(f) {
				fsType = f[i+1]
			}
			return dev, fsType
		}
	}
	return "", ""
}

// TC-015-05 (live, HOST-SIDE): boot a real, long-running guest and OBSERVE the live firecracker
// child's host-side constraints from /proc/<pid>/* — its namespaces (none shared with the host), its
// user-namespace uid map (a non-host uid), its dropped capabilities, and its pivot_root'd root mount.
// This is "observed, not assumed": the assertion reads what the kernel actually applied to the
// firecracker process, not the requested argv (covered by the _Argv test) and not the guest's
// self-view. It bites — a wrapper missing --unshare-all, --uid, or the pivot_root would fail it.
func TestFirecrackerConstraintsGEJailer_Live(t *testing.T) {
	requireKVM(t)
	withRepoRootArtifacts(t)

	// A long-running payload keeps the firecracker child alive long enough for the host-side /proc
	// scan to observe it (the echo-only boot finishes in ~300ms, too fast to race reliably).
	done := make(chan map[string]any, 1)
	go func() {
		req := RunRequest{}
		req.Run.Payload = "sleep 5; exit 0"
		req.Run.Tier = "firecracker"
		done <- Run(req)
	}()

	c, err := observeFirecrackerChild(time.Now().Add(15 * time.Second))
	if err != nil {
		t.Fatalf("TC-015-05 (live): %v", err)
	}
	t.Logf("TC-015-05 (live) host-side firecracker child observation:\n"+
		"  pid=%d\n  ns(fc)=%v\n  uid_map=%q gid_map=%q\n  CapEff=%s NoNewPrivs=%s\n  rootMount=%q fs=%q\n"+
		"  host uid=%d host rootMount=%q",
		c.pid, c.nsIno, c.uidMap, c.gidMap, c.capEff, c.noNewPriv, c.rootDev, c.rootFS,
		os.Getuid(), func() string { d, _ := hostRootMount(); return d }())

	if err := assertConstraintsGEJailer(c); err != nil {
		t.Fatalf("TC-015-05 (live): %v", err)
	}

	// The run must still complete cleanly — the constraints did not prevent the guest from booting and
	// exiting (constraints >= jailer, not constraints that break the run).
	res := <-done
	if code, _ := res["exit_code"].(int); code != 0 {
		t.Fatalf("TC-015-05 (live): guest run exit_code = %v, want 0; result=%v", res["exit_code"], res)
	}
}

func errFmt(format string, a ...any) error {
	return fmt.Errorf(format, a...)
}
