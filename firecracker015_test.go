// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Task 015 — guest boot. The unit-level TCs (argv shape, bundle lifecycle, REST order, sha256
// loader, missing-prereq) run on any host. The boot/run/timeout TCs (TC-015-04/05/06/08/09) need
// /dev/kvm + firecracker + the verified kernel/rootfs and skip-guard via requireKVM when absent.

// withRepoRootArtifacts points the guest-artifact resolver at the worktree root so loadGuestArtifacts
// finds the vendored guest/ tree regardless of the test's cwd, and restores it on cleanup.
func withRepoRootArtifacts(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	prev := guestArtifactRoots
	guestArtifactRoots = func() []string { return []string{wd} }
	t.Cleanup(func() { guestArtifactRoots = prev })
}

// fcLimits is a zero-Limits convenience for the argv/bundle TCs.
var fcLimits = Limits{}

// ---------------------------------------------------------------------------
// TC-015-01: Argv returns a DIRECT firecracker launch under bwrap (NO jailer) + a cleanup func.
// ---------------------------------------------------------------------------

func TestFirecrackerArgvIsBwrapDirectNoJailer(t *testing.T) {
	withRepoRootArtifacts(t)
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not on PATH; payload-drive build (and thus Argv) cannot run here")
	}

	scriptPath := writeTempScript(t, "echo hi")
	dir := t.TempDir()
	proxySock := filepath.Join(dir, "egress.sock")

	argv, cleanup, _, _, err := firecrackerBackend{}.Argv(scriptPath, proxySock, "", nil, nil, nil, fcLimits)
	if err != nil {
		t.Fatalf("Argv returned error: %v", err)
	}
	if cleanup == nil {
		t.Fatal("TC-015-01: Argv returned a nil cleanup func (the bundle would leak)")
	}
	defer cleanup()

	joined := strings.Join(argv, " ")

	// Launches firecracker DIRECTLY under bwrap — no jailer BINARY anywhere in the argv (A1.Q3).
	// Check the basename of each token (not a substring of the whole line: the worktree path itself
	// can contain the literal "jailer", which is not a jailer invocation).
	for _, tok := range argv {
		if base := filepath.Base(tok); base == "jailer" {
			t.Fatalf("TC-015-01: argv contains a jailer (%q) — A1.Q3 forbids the jailer; argv=%v", tok, argv)
		}
	}
	// bwrap is the wrapper; --unshare-all present; --share-net absent (the no-network invariant).
	if argv[0] != "bwrap" && filepath.Base(argv[0]) != "bwrap" && !strings.HasPrefix(argv[0], "taskset") {
		t.Fatalf("TC-015-01: argv[0] = %q, want bwrap (the unprivileged wrapper)", argv[0])
	}
	if !strings.Contains(joined, "--unshare-all") {
		t.Fatalf("TC-015-01: argv missing --unshare-all (no-network invariant): %s", joined)
	}
	if strings.Contains(joined, "--share-net") {
		t.Fatalf("TC-015-01: argv contains --share-net — forbidden: %s", joined)
	}
	// The argv names the firecracker binary (bound in) and the launcher's fc-launch + bundle dir.
	if !strings.Contains(joined, "firecracker") {
		t.Fatalf("TC-015-01: argv does not name the firecracker binary: %s", joined)
	}
	if !strings.Contains(joined, "fc-launch") {
		t.Fatalf("TC-015-01: argv does not invoke `fc-launch` (the REST-driving launcher): %s", joined)
	}
	// /dev/kvm is exposed (the one device permission Tier-3 needs) — no other host device.
	if !strings.Contains(joined, "/dev/kvm") {
		t.Fatalf("TC-015-01: argv does not bind /dev/kvm (the only Tier-3 device permission): %s", joined)
	}
}

// ---------------------------------------------------------------------------
// TC-015-02: a per-run bundle dir is created and torn down by cleanup.
// ---------------------------------------------------------------------------

func TestFirecrackerBundleCreatedAndTornDown(t *testing.T) {
	withRepoRootArtifacts(t)
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not on PATH; payload-drive build (and thus Argv) cannot run here")
	}
	scriptPath := writeTempScript(t, "echo hi")
	dir := t.TempDir()
	proxySock := filepath.Join(dir, "egress.sock")

	argv, cleanup, _, _, err := firecrackerBackend{}.Argv(scriptPath, proxySock, "", nil, nil, nil, fcLimits)
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	// The bundle dir is the last argv element (the fc-launch <bundle> argument).
	bundle := argv[len(argv)-1]
	if fi, err := os.Stat(bundle); err != nil || !fi.IsDir() {
		t.Fatalf("TC-015-02: bundle dir %q does not exist after Argv: %v", bundle, err)
	}
	// It contains the generated vm-config.json.
	if _, err := os.Stat(filepath.Join(bundle, "vm-config.json")); err != nil {
		t.Fatalf("TC-015-02: bundle missing vm-config.json: %v", err)
	}
	cleanup()
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Fatalf("TC-015-02: bundle dir survived cleanup (err=%v) — no bundle may outlive the run", err)
	}
}

// ---------------------------------------------------------------------------
// TC-015-03: the boot sequence drives the REST API in order, with NO /network-interfaces PUT.
// ---------------------------------------------------------------------------

func TestFirecrackerRESTOrderNoNIC(t *testing.T) {
	// Build a representative config (the wired shape), decode it, and assert the boot sequence order.
	cfgMap := wiredFirecrackerConfig("/boot/vmlinux", "/rootfs.ext4", "/payload.sh",
		"/tmp/vsock.sock", "/tmp/egress.sock", Limits{CPUCount: 1, MemoryMB: 128})
	addPayloadDrive(cfgMap, "/tmp/payload.ext4")
	raw, err := json.Marshal(cfgMap)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	var cfg vmConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("decode vmConfig: %v", err)
	}

	steps := firecrackerBootSequence(&cfg)
	var order []string
	for _, s := range steps {
		order = append(order, s.path)
	}

	// The required order: machine-config -> boot-source -> drives... -> vsock -> actions.
	want := []string{"/machine-config", "/boot-source", "/drives/rootfs", "/drives/payload", "/vsock", "/actions"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("TC-015-03: REST PUT order = %v, want %v", order, want)
	}

	// NO /network-interfaces PUT ever appears (no NIC at the API level — ADR 010 D2).
	for _, p := range order {
		if strings.Contains(p, "network-interface") {
			t.Fatalf("TC-015-03: boot sequence issues a network-interfaces PUT (%q) — forbidden (no NIC)", p)
		}
	}
	// The last step is the InstanceStart action.
	last := steps[len(steps)-1]
	if last.path != "/actions" || !strings.Contains(string(last.body), "InstanceStart") {
		t.Fatalf("TC-015-03: final step is %q body=%q, want PUT /actions InstanceStart", last.path, last.body)
	}
}

// TC-015-03b: firecrackerBootSequence with NO drives/vsock still ends in InstanceStart and never
// emits a network-interfaces step — the no-NIC property holds structurally for any config shape.
func TestFirecrackerRESTSequenceNeverEmitsNIC(t *testing.T) {
	cfg := &vmConfig{
		MachineConfig: json.RawMessage(`{"vcpu_count":1,"mem_size_mib":128}`),
		BootSource:    json.RawMessage(`{"kernel_image_path":"/k","boot_args":"console=ttyS0"}`),
	}
	for _, s := range firecrackerBootSequence(cfg) {
		if strings.Contains(s.path, "network") {
			t.Fatalf("TC-015-03b: boot sequence path %q references network — no NIC path may exist", s.path)
		}
	}
}

// ---------------------------------------------------------------------------
// TC-015-07: kernel + rootfs are pinned, verified by sha256, fail fast on mismatch (A1.Q1).
// ---------------------------------------------------------------------------

func TestGuestArtifactsVerifyAndFailFast(t *testing.T) {
	withRepoRootArtifacts(t)

	// Positive: the vendored artifacts verify and resolve.
	art, err := loadGuestArtifacts()
	if err != nil {
		t.Fatalf("TC-015-07: loadGuestArtifacts failed on the vendored pinned artifacts: %v", err)
	}
	if art.kernelPath == "" || art.rootfsPath == "" {
		t.Fatalf("TC-015-07: loadGuestArtifacts returned empty paths: %+v", art)
	}
	// The kernel is an uncompressed x86_64 ELF (built `make vmlinux`, not bzImage).
	head, err := os.ReadFile(art.kernelPath)
	if err != nil {
		t.Fatalf("read kernel: %v", err)
	}
	if len(head) < 4 || head[0] != 0x7f || head[1] != 'E' || head[2] != 'L' || head[3] != 'F' {
		t.Fatalf("TC-015-07: vmlinux is not an ELF (got magic %x) — must be the uncompressed ELF, not bzImage", head[:4])
	}

	// Negative: a tampered artifact (wrong bytes for the pin) must FAIL fast.
	dir := t.TempDir()
	good := filepath.Join(dir, "artifact.bin")
	pin := filepath.Join(dir, "artifact.sha256")
	if err := os.WriteFile(good, []byte("the-real-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pin, []byte(sum+"  artifact.bin\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Good content matches the pin.
	if err := verifyPinnedDigest(good, pin); err != nil {
		t.Fatalf("TC-015-07: verifyPinnedDigest rejected a matching artifact: %v", err)
	}
	// Tamper the content; the pin no longer matches → hard error (fail fast, no boot).
	if err := os.WriteFile(good, []byte("TAMPERED-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedDigest(good, pin); err == nil {
		t.Fatal("TC-015-07: verifyPinnedDigest accepted a TAMPERED artifact — the sha256 gate is a no-op (BUG)")
	}
	// A missing pin file is also a hard error (no unpinned boot).
	if err := verifyPinnedDigest(good, filepath.Join(dir, "nonexistent.sha256")); err == nil {
		t.Fatal("TC-015-07: verifyPinnedDigest accepted a missing pin — an unpinned artifact must be rejected")
	}
}

// TC-015-07b: the pinned config + PROVENANCE are vendored alongside the kernel.
func TestGuestKernelProvenanceVendored(t *testing.T) {
	withRepoRootArtifacts(t)
	for _, p := range []string{
		"guest/kernel/config/microvm-kernel-ci-x86_64-6.1.config",
		"guest/kernel/config/PROVENANCE",
		"guest/kernel/vmlinux.sha256",
		"guest/rootfs/base.ext4.sha256",
		"guest/rootfs/build.sh",
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("TC-015-07b: vendored artifact %q missing: %v", p, err)
		}
	}
	prov, err := os.ReadFile("guest/kernel/config/PROVENANCE")
	if err != nil {
		t.Fatal(err)
	}
	ps := string(prov)
	// PROVENANCE records the upstream linux git tag + firecracker commit (the supply-chain pin).
	if !strings.Contains(ps, "v6.1.176") {
		t.Fatal("TC-015-07b: PROVENANCE does not record the linux git tag")
	}
	if !strings.Contains(ps, "Firecracker commit") {
		t.Fatal("TC-015-07b: PROVENANCE does not record the upstream Firecracker commit")
	}
}

// ---------------------------------------------------------------------------
// TC-015-10: missing firecracker / inaccessible kvm / missing artifacts → spawn error 127.
// ---------------------------------------------------------------------------

// TC-015-10a: when the pinned artifacts cannot be resolved, Argv returns an error (which Run() maps
// to exit 127) — never a silent fall-back to a weaker tier.
func TestFirecrackerMissingArtifactsIsHardError(t *testing.T) {
	prev := guestArtifactRoots
	guestArtifactRoots = func() []string { return []string{t.TempDir()} } // empty root → no artifacts
	t.Cleanup(func() { guestArtifactRoots = prev })

	scriptPath := writeTempScript(t, "echo hi")
	dir := t.TempDir()
	_, cleanup, _, _, err := firecrackerBackend{}.Argv(scriptPath, filepath.Join(dir, "p.sock"), "", nil, nil, nil, fcLimits)
	if cleanup != nil {
		cleanup()
	}
	if err == nil {
		t.Fatal("TC-015-10a: Argv succeeded with no pinned artifacts — must be a hard error (no fall-back)")
	}
	if !strings.Contains(err.Error(), "kernel") && !strings.Contains(err.Error(), "rootfs") {
		t.Fatalf("TC-015-10a: error %q does not name the missing artifact", err.Error())
	}
}

// TC-015-10b: an actual `run.tier=firecracker` run on a host without firecracker / /dev/kvm yields
// exit_code 127 (spawn error surfaced through the unchanged Run() path), NOT a fall-back. This drives
// the full Run() so it also proves the no-fall-back contract end-to-end.
func TestFirecrackerRunMissingPrereqIsExit127(t *testing.T) {
	// Skip only when the tier WOULD boot (firecracker + kvm both present) — then exit 127 is not the
	// expected outcome. When either is absent (the common CI host), the run must surface 127.
	_, fcErr := exec.LookPath("firecracker")
	_, kvmErr := os.Stat("/dev/kvm")
	if fcErr == nil && kvmErr == nil {
		t.Skip("firecracker + /dev/kvm both present; this TC covers the ABSENT case (boot is TC-015-04)")
	}
	withRepoRootArtifacts(t)

	req := RunRequest{}
	req.Run.Payload = "echo hi"
	req.Run.Tier = "firecracker"
	res := Run(req)
	// Either Argv failed (firecracker binary absent → error from firecrackerArgv) or the spawn
	// failed (kvm absent → exec error → 127). Both must surface as a non-fall-back failure.
	if errStr, ok := res["error"].(string); ok {
		if !strings.Contains(errStr, "firecracker") {
			t.Fatalf("TC-015-10b: error %q does not name firecracker (no-fall-back expected)", errStr)
		}
		return
	}
	code, _ := res["exit_code"].(int)
	if code != 127 {
		t.Fatalf("TC-015-10b: exit_code = %v, want 127 (spawn error, no fall-back); result=%v", res["exit_code"], res)
	}
	// And it must NOT have silently run under another tier — tier echoes firecracker.
	ss, _ := res["sandbox_status"].(map[string]any)
	if ss != nil && ss["tier"] != "firecracker" {
		t.Fatalf("TC-015-10b: sandbox_status.tier = %v, want firecracker (no silent fall-back)", ss["tier"])
	}
}

// writeTempScript writes body to a temp payload.sh and returns its path.
func writeTempScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "payload.sh")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return p
}
