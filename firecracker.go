// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// firecrackerBackend is the Tier-3 isolation substrate: it runs the payload inside a Firecracker
// microVM. The load-bearing security property is no-NIC by construction (ADR 010 D2): the
// generated microVM config carries no network-interface key — the microVM analogue of
// bwrap --unshare-all and the gVisor empty netns. The only egress affordance is the vsock device
// wired through the host-side vsock bridge to the live EgressProxy socket (task 014).
//
// Argv (task 015) verifies the pinned kernel + rootfs (sha256, fail-fast on mismatch — A1.Q1),
// builds the per-run bundle (config + the writable payload drive), starts the host-side vsock
// bridge, and returns the spawn argv that launches `firecracker` DIRECTLY under exec-sandbox's
// existing unprivileged `bwrap --unshare-all` + `limits.go` wrapper (NO jailer — A1.Q3). The
// firecracker child is itself contained (non-host uid, all namespaces unshared, cgroup limits,
// chroot) and self-installs its seccomp filters; the cleanup func stops the bridge and removes the
// bundle so no guest or socket outlives the run.
//
// The absence of the firecracker binary / /dev/kvm, or a kernel/rootfs that fails verification,
// surfaces as a spawn error (exit 127), never a silent fall-back to another backend (ADR 010 D1;
// mirroring gvisor.go:18-20).
type firecrackerBackend struct{}

// guestNprocBootArg is the kernel-cmdline key the host emits (when pids > 0) and the guest init
// parses to apply an in-guest RLIMIT_NPROC (`ulimit -u N`, with the payload dropped to an
// unprivileged uid via setpriv so the cap bites) before running the payload. It is the pids delivery
// mechanism (ADR 010 D4): the guest, not the host, owns its pid space, so the cap must be set inside
// the guest — the cmdline is the simplest trusted host→guest channel (it is part of the verified
// boot, not the untrusted payload drive). Must match the key the init scans for.
const guestNprocBootArg = "exec_sandbox.nproc"

// guestFileReadBootArg is the kernel-cmdline key the host emits to tell the guest init which block
// device backs each read-only FileRead surface and where to mount it. The value is a
// comma-separated list of `<dev>:<hostpath>` pairs, e.g.
// `exec_sandbox.fileread=/dev/vdd:/host/tool,/dev/vde:/host/lib`. The init mounts each device
// READ-ONLY (`mount -o ro`) at its host path inside the guest, so a FileRead path is visible at the
// SAME absolute path the payload would use on the host (mirroring bwrap's --ro-bind <p> <p>). It
// rides the cmdline (the trusted, verified boot channel — like guestNprocBootArg) rather than the
// untrusted payload drive: the host, not the payload, decides the mount topology. The drives
// themselves carry is_read_only:true so the read-only-ness is enforced structurally by the virtio
// block layer even if a guest tried to remount; the cmdline only carries the mountpoint mapping.
const guestFileReadBootArg = "exec_sandbox.fileread"

// workDriveID / workGuestDev: /work is presented as the writable block device vdc (the third drive,
// after vda=rootfs and vdb=payload). The guest init mounts it READ-WRITE at /work and cd's there
// before running the payload. It is the SINGLE writable surface (F-006): the rootfs and payload
// drives are is_read_only:true, and every FileRead drive is is_read_only:true; only this drive is
// is_read_only:false (ADR 004). Writes land in the ext4 image and are copied back to the host
// workdir at TEARDOWN via debugfs (copy-out), so ADR 004's "writes persist to the host work dir" is
// satisfied with persist-at-teardown timing, not live (ADR 010 Q2: block device, unprivileged
// copy-in via mkfs.ext4 -d + copy-out via debugfs rdump — no root loop-mount).
const (
	workDriveID  = "work"
	workGuestDev = "/dev/vdc"
)

// Argv verifies the pinned guest artifacts, builds the per-run bundle, starts the host-side vsock
// bridge, and returns the bwrap-wrapped `exec-sandbox fc-launch <bundle>` argv plus a cleanup that
// stops the bridge and removes the bundle. A missing/unverifiable artifact, a missing /dev/kvm, or
// an absent firecracker binary is surfaced as a spawn error (exit 127) — never a fall-back.
func (firecrackerBackend) Argv(scriptPath, proxySock, workdir string, fileReads []string, env map[string]string, envCreds [][2]string, lim Limits) ([]string, func(), []degrade, []*os.File, error) {
	bundle, err := os.MkdirTemp("", "exec-sandbox-fc-")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// cleanup is the Tier-3 TEARDOWN (ADR 010 D5), run on the existing `defer cleanup()` path in Run()
	// so it fires on EVERY exit path — clean, non-zero, timeout, AND launch error. It (1) copies the
	// guest's /work writes back to the host (debugfs rdump, unprivileged — ADR 010 Q2), (2) stops the
	// host-side vsock bridge so no socket outlives the run, (3) DEFENSIVELY reaps any surviving
	// firecracker/fc-launch process for THIS run, and (4) removes the per-run bundle dir LAST so no
	// guest, socket, cgroup artifact, or bundle outlives the run.
	//
	// The reap is belt-and-suspenders: on the clean/non-zero/timeout paths the firecracker child is
	// already dead — fc-launch is the spawned child, it runs firecracker under bwrap --die-with-parent,
	// and Run() SIGKILLs the whole process group on the wall-clock deadline — but an orphan (e.g. a
	// firecracker that outlived a crashed fc-launch) is killed here, scoped to this run's bundle so a
	// concurrent run's child is never touched. The microVM owns no host cgroup (limits.go applies the
	// rlimits/affinity to the bwrap child, and the vcpu/mem caps are firecracker machine-config), so
	// there is no per-run jailer cgroup to reclaim under the no-jailer model (A1.Q3) — reaping the
	// process group + removing the bundle is the complete reclaim.
	//
	// workImage / workHostDir are set once the writable /work drive is built (below); the copy-out runs
	// AFTER the guest has exited so the guest's writes land back in the host workdir.
	var bridge *vsockBridge
	var workImage, workHostDir string
	cleanup := func() {
		if workImage != "" && workHostDir != "" {
			copyOutWorkdir(workImage, workHostDir)
		}
		if bridge != nil {
			bridge.Stop()
		}
		reapFirecrackerOrphans(bundle)
		os.RemoveAll(bundle)
	}

	// Verify the pinned kernel + rootfs BEFORE any boot (A1.Q1): a sha256 mismatch / missing pin is
	// a hard error here — the backend never boots an unverified artifact and never falls back.
	art, err := loadGuestArtifacts()
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	// The vsock host uds_path lives inside the bundle (per-run socket). firecracker forwards a
	// guest connection on hostVsockPort to <vsockUDS>_<hostVsockPort>, where the bridge listens.
	vsockUDS := filepath.Join(bundle, "proxy.vsock")
	bridgeListen := vsockUDS + "_" + strconv.Itoa(hostVsockPort)
	bridge, err = startVsockBridge(bridgeListen, proxySock)
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	// disk_mb sizes the writable payload drive (the guest's writable layer). It is a SECONDARY
	// control: when the host can't size the writable layer (diskQuotaSupported()==false) it degrades
	// (warn + continue) exactly like applyLimitsToOCISpec under gVisor — never a silent drop. The
	// degrade is decided once here so both the drive-build and the audit record see the same effective
	// disk cap.
	var degrades []degrade
	diskMB := lim.DiskMB
	if lim.DiskMB > 0 && !diskQuotaSupported() {
		degrades = append(degrades, degrade{"disk_mb",
			"disk_mb limit not enforced: writable-layer size quota unsupported on this host; running without disk quota"})
		diskMB = 0 // unsized — the floor default applies, never silently capped
	}

	// The untrusted payload is NEVER baked into the read-only base (A1.Q1). It is presented on a
	// separate writable drive (vdb) the backend builds per run from scriptPath; the guest init
	// mounts /dev/vdb read-only and runs /usr/bin/sh on payload.sh from it.
	payloadDrive := filepath.Join(bundle, "payload.ext4")
	if err := buildPayloadDrive(scriptPath, payloadDrive, diskMB); err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	cfg := firecrackerConfig(art.kernelPath, art.rootfsPath, scriptPath, vsockUDS, lim)
	addPayloadDrive(cfg, payloadDrive)

	// Present the validated run.workdir as the writable /work block device (vdc, is_read_only:false)
	// — the SINGLE writable surface (ADR 004 / F-006). The image is seeded from the host workdir
	// (copy-in via mkfs.ext4 -d, unprivileged) so a host-seeded /work file is readable in-guest; the
	// guest's writes are copied back to the host workdir at teardown (debugfs, in cleanup). When
	// workdir is "" (no mount requested) no /work drive is added — byte-for-byte the task-015 shape.
	if workdir != "" {
		workImage = filepath.Join(bundle, "work.ext4")
		workHostDir = workdir
		if err := buildWorkdirDrive(workdir, workImage, diskMB); err != nil {
			cleanup()
			return nil, nil, nil, nil, err
		}
		addWorkdirDrive(cfg, workImage)
	}

	// Present each validated FileRead path as its own READ-ONLY block device (vdd, vde, …,
	// is_read_only:true). The image is seeded from the host path (copy-in); the guest init mounts it
	// read-only at the SAME absolute path (mirroring bwrap's --ro-bind <p> <p>). A guest write fails
	// (the drive is read-only at the virtio layer) and the HOST file is never modified or created —
	// there is NO copy-out for FileRead drives (ADR 005 / F-007). The device→mountpoint mapping is
	// emitted on the trusted kernel cmdline (guestFileReadBootArg).
	if err := addFileReadDrives(cfg, fileReads); err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	// Config-level read-only guard (TC-017-06/07): exactly one writable drive (the /work surface), and
	// every FileRead drive carries is_read_only:true. A FileRead surface accidentally made writable is
	// a hard error here — the negative case proving the single-writable-surface invariant is enforced
	// structurally, not just by convention.
	if err := validateDriveReadOnly(cfg, fileReads); err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}
	cfgPath := filepath.Join(bundle, "vm-config.json")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	argv, err := firecrackerArgv(bundle, art, lim)
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	// cpu_count → machine-config.vcpu_count (a REAL vCPU cap — the guest literally has that many
	// vCPUs), NOT a host-side taskset affinity prefix. For the firecracker tier there is therefore NO
	// taskset prefix on the argv: the vcpu_count cap is STRONGER than the namespace tiers' affinity
	// hint (ADR 010 D4), not a degrade. The mapping is applied in firecrackerConfig above; nothing is
	// added to the argv here.

	return argv, cleanup, degrades, nil, nil
}

// firecrackerArgv builds the spawn argv that launches firecracker DIRECTLY under the unprivileged
// `bwrap --unshare-all` wrapper (NO jailer — A1.Q3). bwrap supplies the chroot + mnt/pid/ipc/net/
// user namespaces + a non-host uid the jailer would otherwise provide; firecracker self-installs its
// seccomp filters regardless. The in-bwrap command is `exec-sandbox fc-launch <bundle>`, which
// drives the REST API and exits with the guest's exit code.
//
// Bind-mounts (all read-only except /dev/kvm and the bundle):
//   - the exec-sandbox binary (self) — so fc-launch can run in the sandbox,
//   - the firecracker binary,
//   - the verified kernel + rootfs (read-only),
//   - the per-run bundle dir READ-WRITE (the api + vsock sockets + payload drive live there),
//   - /dev/kvm — the ONE device permission Tier-3 needs (rw); no other host device is exposed,
//   - minimal system dirs for the dynamic loader.
//
// No --share-net ever appears: --unshare-all removes the network namespace entirely, the microVM
// analogue of the no-NIC config (the guest's only egress is the vsock bridge, host-side).
func firecrackerArgv(bundle string, art guestArtifacts, lim Limits) ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve exec-sandbox binary path: %w", err)
	}
	fcBin, err := exec.LookPath("firecracker")
	if err != nil {
		return nil, fmt.Errorf("firecracker binary not found on PATH: %w (Tier-3 requires firecracker + /dev/kvm)", err)
	}

	argv := []string{"bwrap",
		"--unshare-all", "--die-with-parent",
		// Non-host uid (A1.Q3, jailer-equivalent identity): --unshare-all creates a new user
		// namespace; --uid/--gid run the firecracker child as an unprivileged 65534 (nobody) INSIDE
		// that namespace — the no-jailer analogue of the jailer's setuid-to-an-unprivileged-uid step.
		// The host-side observable is /proc/<pid>/uid_map "65534 <hostuid> 1" (the in-namespace uid is
		// 65534, not the invoking host uid); the kernel still reports the OWNING host uid in
		// /proc/<pid>/status Uid, so the uid_map — not the status Uid — is what proves the non-host
		// identity (see TC-015-05). /dev/kvm stays reachable: the dev-bind owner maps to nobody in the
		// userns and the device is mode rw for it.
		"--uid", "65534", "--gid", "65534",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc", "/etc",
		"--proc", "/proc",
		"--dev", "/dev",
		// /dev/kvm is the ONE device Tier-3 needs (rw). --dev-bind exposes exactly it (and nothing
		// else) into the otherwise-minimal /dev. No root, no setuid, no caps beyond this (A1.Q3).
		"--dev-bind", "/dev/kvm", "/dev/kvm",
		// The exec-sandbox binary (self) + the firecracker binary, read-only.
		"--ro-bind", self, self,
		"--ro-bind", fcBin, fcBin,
		// The verified kernel + rootfs, read-only.
		"--ro-bind", art.kernelPath, art.kernelPath,
		"--ro-bind", art.rootfsPath, art.rootfsPath,
		// The per-run bundle dir READ-WRITE: the api socket, the vsock bridge socket, and the payload
		// drive all live here; firecracker creates the api socket and connects the vsock here.
		"--bind", bundle, bundle,
	}
	for _, d := range []string{"/bin", "/lib", "/lib64", "/sbin"} {
		if _, err := os.Stat(d); err == nil {
			argv = append(argv, "--ro-bind", d, d)
		}
	}
	// PATH must include the firecracker binary's dir so fc-launch resolves it inside the sandbox
	// (firecracker commonly lives in /usr/local/bin, which is not in the bare default).
	fcDir := filepath.Dir(fcBin)
	path := "/usr/bin:/bin"
	if fcDir != "/usr/bin" && fcDir != "/bin" {
		path = fcDir + ":" + path
	}
	argv = append(argv, "--clearenv", "--setenv", "PATH", path)
	// The in-sandbox command: the launcher drives firecracker and exits with the guest exit code.
	// FC_BINARY names the absolute firecracker path so fc-launch need not rely on PATH resolution.
	argv = append(argv, "--setenv", "FC_BINARY", fcBin)
	argv = append(argv, self, "fc-launch", bundle)
	return argv, nil
}

// addPayloadDrive appends the per-run writable payload drive (vdb) to the firecracker config's
// drives list. It is the second drive (after the read-only root), is_root_device=false; the guest
// init mounts /dev/vdb read-only and runs payload.sh from it. Keeping the payload off the root drive
// is what keeps the base.ext4 digest stable across runs (A1.Q1).
func addPayloadDrive(cfg map[string]any, payloadDrive string) {
	drives, _ := cfg["drives"].([]map[string]any)
	cfg["drives"] = append(drives, map[string]any{
		"drive_id":       "payload",
		"path_on_host":   payloadDrive,
		"is_root_device": false,
		"is_read_only":   true,
	})
}

// addWorkdirDrive appends the writable /work drive (vdc) to the config's drives list. It is the
// SINGLE writable drive (is_read_only:false) — the microVM analogue of bwrap's --bind workdir /work
// (ADR 004 / F-006). is_root_device=false; the guest init mounts it READ-WRITE at /work and cd's
// there before the payload.
func addWorkdirDrive(cfg map[string]any, workImage string) {
	drives, _ := cfg["drives"].([]map[string]any)
	cfg["drives"] = append(drives, map[string]any{
		"drive_id":       workDriveID,
		"path_on_host":   workImage,
		"is_root_device": false,
		"is_read_only":   false,
	})
}

// fileReadDriveID returns the firecracker drive_id for the i-th FileRead drive. Firecracker requires
// drive IDs to be alphanumeric + underscore ONLY (a hyphen is rejected with a 400 at the REST PUT),
// so the id uses an underscore: fileread_0, fileread_1, … The drive_id is distinct from the image
// FILENAME (which may use a hyphen — it is a host path, not an API resource id).
func fileReadDriveID(i int) string {
	return fmt.Sprintf("fileread_%d", i)
}

// fileReadGuestDev returns the guest block-device path for the i-th FileRead drive. FileRead drives
// follow vda(root), vdb(payload), vdc(/work): the first FileRead is vdd, the second vde, and so on.
// The mapping is deterministic in the FileRead path order so the cmdline and the drive list agree.
func fileReadGuestDev(i int) string {
	// 'd' is vdd (index 0); 'e' vde (1); … virtio enumerates drives in config order.
	return "/dev/vd" + string(rune('d'+i))
}

// addFileReadDrives appends one READ-ONLY drive per validated FileRead path (vdd, vde, …,
// is_read_only:true) seeded from the host path, and emits the device→mountpoint mapping on the
// kernel cmdline (guestFileReadBootArg) so the guest init mounts each one read-only at its host
// path. The images are written into the same bundle dir as the /work + payload drives — the build
// happens here so a mkfs failure surfaces as a spawn error. A FileRead path that cannot be imaged is
// a hard error (the guest would otherwise run with the path silently absent — the no-silent-skip
// stance of ADR 005). NO copy-out is wired for these drives: a guest write fails at the virtio layer
// and the host file is untouched (F-007).
func addFileReadDrives(cfg map[string]any, fileReads []string) error {
	if len(fileReads) == 0 {
		return nil
	}
	drives, _ := cfg["drives"].([]map[string]any)
	var mapping []string
	for i, p := range fileReads {
		// The image lives next to the other per-run drives (path_on_host of the root drive's dir is
		// the bundle). Derive the bundle dir from the rootfs drive is fragile; instead place FileRead
		// images alongside the /work + payload images via the config's existing drive paths is not
		// available here, so the bundle dir is recovered from an existing drive path_on_host.
		bundleDir := filepath.Dir(driveHostPath(drives, "payload"))
		img := filepath.Join(bundleDir, fmt.Sprintf("fileread-%d.ext4", i))
		if err := buildFileReadDrive(p, img); err != nil {
			return fmt.Errorf("build FileRead drive for %q: %w", p, err)
		}
		drives = append(drives, map[string]any{
			"drive_id":       fileReadDriveID(i),
			"path_on_host":   img,
			"is_root_device": false,
			"is_read_only":   true,
		})
		mapping = append(mapping, fileReadGuestDev(i)+":"+p)
	}
	cfg["drives"] = drives

	// Emit the device→mountpoint mapping on the trusted kernel cmdline (appended LAST so the no-
	// FileRead cmdline is byte-for-byte unchanged). The guest init parses it and mounts each device
	// read-only at its host path.
	bs, _ := cfg["boot-source"].(map[string]any)
	if bs != nil {
		args, _ := bs["boot_args"].(string)
		bs["boot_args"] = args + " " + guestFileReadBootArg + "=" + strings.Join(mapping, ",")
	}
	return nil
}

// driveHostPath returns the path_on_host of the drive with the given drive_id, or "" if absent.
func driveHostPath(drives []map[string]any, id string) string {
	for _, d := range drives {
		if d["drive_id"] == id {
			s, _ := d["path_on_host"].(string)
			return s
		}
	}
	return ""
}

// validateDriveReadOnly is the config-level single-writable-surface guard (TC-017-06/07). It asserts
// that the generated config has EXACTLY ONE writable drive (is_read_only:false) — the /work surface,
// drive_id "work" — and that every other drive (rootfs, payload, every FileRead) is read-only. A
// FileRead drive accidentally made writable, or any second writable drive, is a hard error: the
// single-writable-surface invariant (ADR 004 / F-006) and FileRead-read-only (ADR 005 / F-007) are
// enforced structurally here, before the config is ever written or booted. The negative case (a
// writable FileRead drive) proves the check bites and is not a no-op.
func validateDriveReadOnly(cfg map[string]any, fileReads []string) error {
	drives, _ := cfg["drives"].([]map[string]any)
	// Build the set of FileRead drive_ids so a writable one is caught by name in the error.
	freadIDs := map[string]bool{}
	for i := range fileReads {
		freadIDs[fileReadDriveID(i)] = true
	}
	writable := 0
	for _, d := range drives {
		ro, _ := d["is_read_only"].(bool)
		id, _ := d["drive_id"].(string)
		if !ro {
			writable++
			if id != workDriveID {
				return fmt.Errorf("microVM config has a writable non-/work drive %q (is_read_only:false): only the /work surface may be writable (ADR 004/005, F-006/F-007)", id)
			}
		}
		// A FileRead drive must always be read-only (defense in depth: even if it were somehow not the
		// extra writable drive, a writable FileRead surface is forbidden).
		if freadIDs[id] && !ro {
			return fmt.Errorf("microVM config presents FileRead drive %q WRITABLE (is_read_only:false): FileRead paths must be read-only (ADR 005 / F-007)", id)
		}
	}
	if writable > 1 {
		return fmt.Errorf("microVM config has %d writable drives: /work must be the ONLY writable surface (F-006)", writable)
	}
	return nil
}

// buildWorkdirDrive creates a WRITABLE ext4 image at out seeded from the host workdir (copy-in via
// `mkfs.ext4 -d`, unprivileged — no root/loop-mount). A host-seeded /work file is therefore readable
// in-guest. diskMB sizes the writable surface (same disk_mb cap as the payload drive); below the
// floor (or unset/degraded) uses the floor so the ext4 metadata fits. A missing mkfs.ext4 is a hard
// error (the guest cannot get its /work), surfaced as a spawn failure.
func buildWorkdirDrive(workdir, out string, diskMB int) error {
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return fmt.Errorf("cannot build /work drive: mkfs.ext4 not found on PATH: %w", err)
	}
	sizeMB := payloadDriveSizeMB(diskMB)
	// -b 1024 keeps small images viable (the default 4K block size needs a larger minimum); -F forces
	// non-interactive; -d seeds from the host workdir without root/loop-mount.
	cmd := exec.Command(mkfs, "-q", "-F", "-b", "1024", "-d", workdir, out, fmt.Sprintf("%dM", sizeMB))
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 /work drive: %w: %s", err, strings.TrimSpace(string(outBytes)))
	}
	return nil
}

// buildFileReadDrive creates a READ-ONLY-presented ext4 image at out seeded from a single host
// FileRead path (copy-in via `mkfs.ext4 -d`, unprivileged). The host path may be a file or a
// directory; for a file the image contains it at /<basename> and the guest mounts the device and
// exposes the file at the same absolute host path (the init binds the device's file to it). The
// drive carries is_read_only:true so a guest write fails at the virtio layer and the host file is
// never touched (F-007). The copy-in is a SNAPSHOT — there is no copy-out, so the host file cannot
// be modified through this path.
func buildFileReadDrive(hostPath, out string) error {
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return fmt.Errorf("cannot build FileRead drive: mkfs.ext4 not found on PATH: %w", err)
	}
	info, err := os.Stat(hostPath)
	if err != nil {
		return fmt.Errorf("stat FileRead path %s: %w", hostPath, err)
	}
	staging, err := os.MkdirTemp("", "exec-sandbox-fc-fileread-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if info.IsDir() {
		// A directory FileRead surface: image the directory contents under /<basename>.
		dst := filepath.Join(staging, filepath.Base(hostPath))
		if err := copyTree(hostPath, dst); err != nil {
			return err
		}
	} else {
		// A file FileRead surface: stage it under /<basename> so the init can expose it at hostPath.
		data, err := os.ReadFile(hostPath)
		if err != nil {
			return fmt.Errorf("read FileRead path %s: %w", hostPath, err)
		}
		if err := os.WriteFile(filepath.Join(staging, filepath.Base(hostPath)), data, info.Mode().Perm()); err != nil {
			return err
		}
	}
	cmd := exec.Command(mkfs, "-q", "-F", "-b", "1024", "-d", staging, out, "4M")
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 FileRead drive: %w: %s", err, strings.TrimSpace(string(outBytes)))
	}
	return nil
}

// copyTree recursively copies src to dst (files + dirs + symlinks-as-regular), preserving perms. It
// stages a host directory for the unprivileged `mkfs.ext4 -d` seed (copy-in); it is read-only host
// I/O on the source.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode().Perm()|0o700); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

// copyOutWorkdir copies the guest's writes back from the writable /work ext4 image into the host
// workdir at TEARDOWN, using `debugfs -R "rdump / <hostdir>"` — an UNPRIVILEGED userspace read of
// the ext4 image (NO loop-mount, NO root/CAP_SYS_ADMIN). It is best-effort: a copy-out failure is
// logged-by-omission (the host workdir simply keeps its pre-run state) and never aborts teardown —
// the run's stdout/exit_code are already captured by the time cleanup runs. This is the persist-at-
// teardown half of ADR 004's "writes persist to the host work dir" under the block-device mechanism
// (ADR 010 Q2): live persistence is not available without a host share, so writes land at teardown.
func copyOutWorkdir(image, hostDir string) {
	debugfs, err := exec.LookPath("debugfs")
	if err != nil {
		return // no debugfs: the host workdir keeps its pre-run state (best-effort copy-out)
	}
	// rdump extracts the whole filesystem tree (seeded + guest-written files) into a staging dir, then
	// we sync it back into the host workdir. Extracting to a staging dir (not directly onto hostDir)
	// avoids debugfs's chown-to-in-image-uid warnings clobbering host ownership and lets us skip
	// ext4's lost+found.
	staging, err := os.MkdirTemp("", "exec-sandbox-fc-workout-")
	if err != nil {
		return
	}
	defer os.RemoveAll(staging)
	cmd := exec.Command(debugfs, "-R", fmt.Sprintf("rdump / %q", staging), image)
	// debugfs emits non-fatal "Operation not permitted while changing ownership" warnings as an
	// unprivileged user; the files are still extracted with the invoking uid + correct content. We
	// ignore the exit status and sync whatever landed.
	_ = cmd.Run()
	entries, err := os.ReadDir(staging)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.Name() == "lost+found" {
			continue // ext4 metadata dir, not a guest write
		}
		_ = copyTree(filepath.Join(staging, e.Name()), filepath.Join(hostDir, e.Name()))
	}
}

// reapFirecrackerOrphans is the defensive half of the Tier-3 teardown (ADR 010 D5): it SIGKILLs any
// LIVE process whose argv references this run's bundle dir — a firecracker VMM (its --api-sock and
// vsock paths live in the bundle) or an exec-sandbox fc-launch driving it. On the normal exit paths
// the child is already dead (fc-launch is the spawned child; firecracker runs under
// bwrap --die-with-parent; Run() SIGKILLs the process group on timeout), so this is belt-and-
// suspenders for an orphan that outlived a crashed launcher — no firecracker process may outlive the
// run.
//
// The match is SCOPED to this run's bundle path (a per-run MkdirTemp dir, unique to this run), NOT a
// broad pkill of "firecracker": a concurrent run's child references a DIFFERENT bundle and is never
// touched. bundle must be a non-empty, absolute per-run dir; an empty/"/" bundle is refused so a
// degenerate call can never match every process.
func reapFirecrackerOrphans(bundle string) {
	if bundle == "" || bundle == "/" || !filepath.IsAbs(bundle) {
		return // never reap on a degenerate bundle path — scope-safety over completeness
	}
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid == self {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		if err != nil {
			continue
		}
		// /proc/<pid>/cmdline is NUL-separated; join with spaces to substring-scan for the bundle path.
		joined := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if !strings.Contains(joined, bundle) {
			continue
		}
		// A process whose argv references THIS run's bundle is a survivor (the firecracker VMM with its
		// --api-sock/vsock under the bundle, or the fc-launch driving it). SIGKILL it — it must not
		// outlive the run. A kill failure (already-exited / race) is ignored; the RemoveAll that follows
		// is the load-bearing reclaim and the survivor probe re-checks.
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// payloadDriveFloorMB is the minimum payload-drive size (the filesystem metadata + the tiny
// payload.sh must fit). A disk_mb below the floor is raised to it; disk_mb == 0 (unset, or degraded
// away) uses the floor as the default. The writable /work surface is task 017's concern.
const payloadDriveFloorMB = 2

// payloadDriveSizeMB maps disk_mb onto the writable-drive size in MiB: disk_mb above the floor sizes
// the drive to exactly disk_mb; disk_mb == 0 (unset, or degraded away by the diskQuotaSupported
// check) or below the floor uses the floor default so the ext4 metadata always fits. It is a pure
// function so the disk_mb → drive-size mapping is unit-testable without mkfs.ext4 (TC-016-03).
func payloadDriveSizeMB(diskMB int) int {
	if diskMB > payloadDriveFloorMB {
		return diskMB
	}
	return payloadDriveFloorMB
}

// buildPayloadDrive creates an ext4 image at out containing payload.sh (copied from scriptPath). It
// uses `mkfs.ext4 -d` to populate the image without root/loop-mount. diskMB sizes the writable drive
// presented to the guest (the guest's writable layer): disk_mb > floor sizes the image to exactly
// disk_mb; 0 (unset or degraded) uses the floor default. A missing mkfs.ext4 is a hard error (the
// guest cannot get its payload), surfaced as a spawn failure.
func buildPayloadDrive(scriptPath, out string, diskMB int) error {
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return fmt.Errorf("cannot build payload drive: mkfs.ext4 not found on PATH: %w", err)
	}
	staging, err := os.MkdirTemp("", "exec-sandbox-fc-payload-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("read payload %s: %w", scriptPath, err)
	}
	if err := os.WriteFile(filepath.Join(staging, "payload.sh"), data, 0o644); err != nil {
		return err
	}
	sizeMB := payloadDriveSizeMB(diskMB)
	cmd := exec.Command(mkfs, "-q", "-F", "-d", staging, out, fmt.Sprintf("%dM", sizeMB))
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 payload drive: %w: %s", err, strings.TrimSpace(string(outBytes)))
	}
	return nil
}

// firecrackerConfig builds the Firecracker microVM configuration as a pure function of the
// supplied on-host paths. It issues NO network-interface body (ADR 010 D2): the no-NIC
// invariant is enforced by omission — the key is never present, not set to nil or false.
//
// The returned map is serialization-deterministic: field order is fixed (struct-like ordering
// via a deterministic map key sequence), mirroring gvisorOCISpec's reproducibility. Identical
// inputs produce a byte-for-byte identical JSON output (TC-013-07).
//
// Config sections:
//   - machine-config: vcpu_count ← cpu_count (a REAL vCPU cap), mem_size_mib ← memory_mb (the
//     guest's RAM ceiling). Zero/absent ⇒ the Firecracker default (1 vCPU / 128 MiB) — the "zero =
//     no cap" contract; the default IS the no-cap shape (ADR 010 D4).
//   - boot-source:    kernel image path + boot args running /usr/bin/sh /payload.sh; pids ← an
//     `exec_sandbox.nproc=N` cmdline arg the guest init applies as an in-guest RLIMIT_NPROC.
//   - drives:         root drive (rootfs, read-only, root_device=true)
//   - vsock:          host-side UDS path bridging to the EgressProxy (task 014)
//
// timeout_sec and max_output_bytes are DELIBERATELY ABSENT — they are enforced host-side in Run()
// (above the tier seam), so the firecracker config is byte-for-byte identical whether or not those
// two caps are set (TC-016-07/08).
//
// Deliberately absent: network-interfaces (D2 — no NIC by omission).
func firecrackerConfig(kernelPath, rootfsPath, scriptPath, vsockUDS string, lim Limits) map[string]any {
	// machine-config: cpu_count → vcpu_count (a real vCPU cap, NOT a taskset affinity hint — stronger
	// than the namespace tiers), memory_mb → mem_size_mib (the guest's hard RAM ceiling). A zero field
	// leaves the Firecracker default (1 vCPU / 128 MiB): the default IS the "no cap" shape, so the
	// emitted config is unchanged from the task-015 no-limits boot.
	vcpuCount := 1
	if lim.CPUCount > 0 {
		vcpuCount = lim.CPUCount
	}
	memMiB := 128
	if lim.MemoryMB > 0 {
		memMiB = lim.MemoryMB
	}

	// boot-source: PID 1 is /sbin/init (baked into the read-only base), which starts the vsock shim,
	// mounts the per-run payload drive (/dev/vdb) read-only, and runs /usr/bin/sh /payload.sh — the
	// same payload entry point as bwrap and gVisor. Boot args are exactly `console=ttyS0 reboot=k
	// panic=1` (A1.Q1) — reboot=k + panic=1 make a reboot/panic terminate firecracker cleanly — plus
	// pci=off (Firecracker has no PCI) and i8042 quirks to skip the absent legacy keyboard probe.
	// There is deliberately NO `ip=` arg: with no virtio-net device the guest has no NIC to configure
	// (reinforces D2's no-NIC-by-construction at the kernel-cmdline level). scriptPath is unused in
	// the cmdline (the payload arrives on /dev/vdb, not the cmdline).
	_ = scriptPath
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/sbin/init"

	// pids → in-guest RLIMIT_NPROC, delivered as a kernel boot-arg the guest init parses and applies
	// with `ulimit -p` BEFORE running the payload (the guest owns its pid space — analogous to
	// prlimitWrap, limits.go:88). pids is NOT a machine-config field; it rides the cmdline because the
	// guest, not the host, must set the rlimit. Zero/absent ⇒ no arg ⇒ no in-guest NPROC cap. The arg
	// is appended LAST so the no-pids cmdline is byte-for-byte the task-015 boot shape.
	if lim.PidsLimit > 0 {
		bootArgs += " " + guestNprocBootArg + "=" + strconv.Itoa(lim.PidsLimit)
	}

	return map[string]any{
		"machine-config": map[string]any{
			"vcpu_count":        vcpuCount,
			"mem_size_mib":      memMiB,
			"smt":               false,
			"track_dirty_pages": false,
		},
		"boot-source": map[string]any{
			"kernel_image_path": kernelPath,
			"boot_args":         bootArgs,
		},
		"drives": []map[string]any{
			{
				"drive_id":       "rootfs",
				"path_on_host":   rootfsPath,
				"is_root_device": true,
				"is_read_only":   true,
			},
		},
		"vsock": map[string]any{
			"vsock_id":  "proxy",
			"guest_cid": 3,
			"uds_path":  vsockUDS,
		},
		// network-interfaces is intentionally absent — no-NIC by omission (ADR 010 D2).
		// DO NOT add a network-interfaces key here; see configHasNoNIC.
	}
}

// configHasNoNIC asserts that cfg contains no network-interface or network-interfaces key at any
// level, in both its structured form and its JSON serialization. It returns a non-nil error if
// either check fails, and nil if the config is NIC-free.
//
// This helper is reused by:
//   - firecracker_test.go TC-013-04 (base config) and TC-013-05 (negative: detector bites on a
//     crafted NIC config)
//   - task 014's vsock-bridge egress tests
//   - task 018's fitness-no-nic target (the microVM analogue of fitness F-001)
func configHasNoNIC(cfg map[string]any) error {
	// Structural check: scan every top-level key and descend one level.
	nicKeys := []string{"network-interface", "network-interfaces", "network_interface", "network_interfaces"}
	for _, k := range nicKeys {
		if _, present := cfg[k]; present {
			return fmt.Errorf("microVM config contains forbidden NIC key %q (ADR 010 D2: no-NIC by omission)", k)
		}
	}

	// Serialization check: stringify and scan for the substring — catches a future refactor
	// that serializes a nested key under a different top-level name.
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("configHasNoNIC: marshal failed: %w", err)
	}
	s := string(b)
	for _, needle := range []string{"network-interface", "network_interface"} {
		if strings.Contains(s, needle) {
			return errors.New("microVM config JSON contains network-interface substring (ADR 010 D2)")
		}
	}
	return nil
}
