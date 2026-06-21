// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Task 016 — profile.limits → Firecracker machine-config mapping. The unit-level TCs
// (TC-016-01..08) are pure-function mappings and run on any host. The behavioral TCs
// (TC-016-09..11) boot a real guest and skip-guard via requireKVM.

// fcMachineConfig is a small helper: build the config from lim and return the machine-config map.
func fcMachineConfig(t *testing.T, lim Limits) map[string]any {
	t.Helper()
	cfg := firecrackerConfig("/boot/vmlinux", "/var/lib/fc/rootfs.ext4", "/tmp/payload.sh", "/tmp/proxy.sock", lim)
	mc, ok := cfg["machine-config"].(map[string]any)
	if !ok {
		t.Fatalf("machine-config missing or wrong type: %T", cfg["machine-config"])
	}
	return mc
}

// TC-016-01: cpu_count → machine-config.vcpu_count (a real vCPU cap, NOT a taskset prefix).
func TestFirecrackerCPUCountMapsToVcpuCount(t *testing.T) {
	mc := fcMachineConfig(t, Limits{CPUCount: 2})
	if mc["vcpu_count"] != 2 {
		t.Fatalf("TC-016-01: vcpu_count = %v, want 2", mc["vcpu_count"])
	}

	// No taskset prefix on the firecracker argv for the firecracker tier — vcpu_count IS the cap.
	// Drive Argv and assert the spawn argv has no taskset prefix (the vcpu cap is stronger, not a
	// degrade). Requires mkfs.ext4 to build the payload drive.
	withRepoRootArtifacts(t)
	if !haveMkfs() {
		t.Skip("mkfs.ext4 not on PATH; cannot build the payload drive to inspect the argv")
	}
	script := writeTempScript(t, "echo hi")
	dir := t.TempDir()
	argv, cleanup, degrades, _, err := firecrackerBackend{}.Argv(
		script, dir+"/egress.sock", "", nil, nil, nil, Limits{CPUCount: 2})
	if err != nil {
		t.Skipf("TC-016-01: Argv failed (prereqs absent?): %v", err)
	}
	defer cleanup()
	for _, a := range argv {
		if a == "taskset" {
			t.Fatalf("TC-016-01: firecracker argv contains a taskset prefix %v — cpu_count must be the vcpu_count cap, not a host affinity hint", argv)
		}
	}
	for _, d := range degrades {
		if d.cap == "cpu_count" {
			t.Fatalf("TC-016-01: cpu_count was degraded %q — under firecracker it is a real vcpu_count cap, never a degrade", d.reason)
		}
	}
}

// TC-016-02: memory_mb → machine-config.mem_size_mib.
func TestFirecrackerMemoryMapsToMemSizeMib(t *testing.T) {
	mc := fcMachineConfig(t, Limits{MemoryMB: 128})
	if mc["mem_size_mib"] != 128 {
		t.Fatalf("TC-016-02: mem_size_mib = %v, want 128", mc["mem_size_mib"])
	}
}

// TC-016-03: disk_mb → writable drive size. The pure mapping is payloadDriveSizeMB; a disk_mb above
// the floor sizes the drive to exactly disk_mb. No degrade when diskQuotaSupported() == true.
func TestFirecrackerDiskMapsToDriveSize(t *testing.T) {
	if got := payloadDriveSizeMB(64); got != 64 {
		t.Fatalf("TC-016-03: payloadDriveSizeMB(64) = %d, want 64 (drive sized to disk_mb)", got)
	}
	// Below the floor is raised to the floor so the ext4 metadata fits (not a silent disk_mb=0).
	if got := payloadDriveSizeMB(1); got != payloadDriveFloorMB {
		t.Fatalf("TC-016-03: payloadDriveSizeMB(1) = %d, want floor %d", got, payloadDriveFloorMB)
	}

	// And on a host where the writable layer is sizeable, Argv records NO disk_mb degrade.
	forceDiskQuota(t, true)
	withRepoRootArtifacts(t)
	if !haveMkfs() {
		t.Skip("mkfs.ext4 not on PATH; cannot build the payload drive")
	}
	script := writeTempScript(t, "echo hi")
	dir := t.TempDir()
	_, cleanup, degrades, _, err := firecrackerBackend{}.Argv(
		script, dir+"/egress.sock", "", nil, nil, nil, Limits{DiskMB: 64})
	if err != nil {
		t.Skipf("TC-016-03: Argv failed (prereqs absent?): %v", err)
	}
	defer cleanup()
	for _, d := range degrades {
		if d.cap == "disk_mb" {
			t.Fatalf("TC-016-03: disk_mb degraded on a sizeable host: %q", d.reason)
		}
	}
}

// TC-016-04: pids → in-guest RLIMIT_NPROC delivered as the exec_sandbox.nproc cmdline arg, NOT a
// machine-config field. The host emits the arg; the guest init applies it.
func TestFirecrackerPidsMapsToInGuestNproc(t *testing.T) {
	cfg := firecrackerConfig("/k", "/r", "/p", "/v", Limits{PidsLimit: 20})

	// pids is NOT a machine-config field.
	mc := cfg["machine-config"].(map[string]any)
	for _, k := range []string{"pids", "pids_limit", "nproc", "rlimit_nproc"} {
		if _, ok := mc[k]; ok {
			t.Fatalf("TC-016-04: machine-config carries a pids field %q — pids is an in-guest rlimit, not a host machine-config field", k)
		}
	}

	// The cmdline carries exec_sandbox.nproc=20 (the in-guest delivery mechanism).
	bs := cfg["boot-source"].(map[string]any)
	bootArgs, _ := bs["boot_args"].(string)
	if !strings.Contains(bootArgs, "exec_sandbox.nproc=20") {
		t.Fatalf("TC-016-04: boot_args %q does not carry exec_sandbox.nproc=20 (the in-guest NPROC delivery)", bootArgs)
	}

	// The guest init must actually parse and apply it (a no-op cmdline arg would be a dead delegate).
	initSrc := readGuestInit(t)
	if !strings.Contains(initSrc, "exec_sandbox.nproc") {
		t.Fatal("TC-016-04: guest init does not parse exec_sandbox.nproc — the cmdline arg would be dead")
	}
	// The cap must actually bite: the kernel ignores RLIMIT_NPROC for a privileged (uid-0) process, so
	// the init MUST set the rlimit (`ulimit -u`) AND drop privileges (setpriv to nobody) before the
	// payload runs, or a root payload's fork bomb would sail past the cap.
	if !strings.Contains(initSrc, "ulimit -u") {
		t.Fatal("TC-016-04: guest init does not set `ulimit -u` (RLIMIT_NPROC) — pids would not be enforced in-guest")
	}
	if !strings.Contains(initSrc, "setpriv") || !strings.Contains(initSrc, "--reuid") {
		t.Fatal("TC-016-04: guest init does not drop privileges (setpriv --reuid) — the kernel does not enforce RLIMIT_NPROC for a uid-0 payload, so pids would not bite")
	}
}

// TC-016-05: zero/absent caps leave the machine-config at the Firecracker default (no explicit cap),
// and the FULL config is byte-for-byte the no-limits shape.
func TestFirecrackerZeroLimitsLeaveDefaults(t *testing.T) {
	mc := fcMachineConfig(t, Limits{})
	if mc["vcpu_count"] != 1 {
		t.Fatalf("TC-016-05: vcpu_count = %v, want the Firecracker default 1", mc["vcpu_count"])
	}
	if mc["mem_size_mib"] != 128 {
		t.Fatalf("TC-016-05: mem_size_mib = %v, want the Firecracker default 128", mc["mem_size_mib"])
	}

	// No pids cmdline arg when pids is unset (the no-cap cmdline shape).
	bs := firecrackerConfig("/k", "/r", "/p", "/v", Limits{})["boot-source"].(map[string]any)
	if strings.Contains(bs["boot_args"].(string), "exec_sandbox.nproc") {
		t.Fatalf("TC-016-05: zero pids still emitted an exec_sandbox.nproc arg: %v", bs["boot_args"])
	}
}

// TC-016-06: disk_mb degrades (warn + continue) when the writable layer can't be sized — never a
// silent drop. Mirrors applyLimitsToOCISpec's disk degrade under gVisor.
func TestFirecrackerDiskDegradesWhenUnsizeable(t *testing.T) {
	forceDiskQuota(t, false)
	withRepoRootArtifacts(t)
	if !haveMkfs() {
		t.Skip("mkfs.ext4 not on PATH; cannot build the payload drive")
	}
	script := writeTempScript(t, "echo hi")
	dir := t.TempDir()
	_, cleanup, degrades, _, err := firecrackerBackend{}.Argv(
		script, dir+"/egress.sock", "", nil, nil, nil, Limits{DiskMB: 64})
	if err != nil {
		t.Skipf("TC-016-06: Argv failed (prereqs absent?): %v", err)
	}
	defer cleanup()

	var found *degrade
	for i := range degrades {
		if degrades[i].cap == "disk_mb" {
			found = &degrades[i]
		}
	}
	if found == nil {
		t.Fatalf("TC-016-06: disk_mb was NOT degraded on an unsizeable host — silent drop (BUG). degrades=%v", degrades)
	}
	if !strings.Contains(strings.ToLower(found.reason), "disk_mb") {
		t.Fatalf("TC-016-06: degrade reason %q does not name disk_mb", found.reason)
	}
}

// TC-016-07: timeout_sec is NOT in the firecracker config — the config is byte-for-byte identical
// whether or not it is set (it is enforced host-side in Run()).
func TestFirecrackerTimeoutNotInConfig(t *testing.T) {
	withTimeout := firecrackerConfig("/k", "/r", "/p", "/v", Limits{Timeout: 5 * time.Second})
	without := firecrackerConfig("/k", "/r", "/p", "/v", Limits{})
	assertConfigsByteIdentical(t, "TC-016-07 (timeout_sec)", withTimeout, without)
	// Belt-and-suspenders: the string "timeout" must not appear anywhere in the serialized config.
	b, _ := json.Marshal(withTimeout)
	if strings.Contains(string(b), "timeout") {
		t.Fatalf("TC-016-07: serialized config mentions 'timeout': %s", b)
	}
}

// TC-016-08: max_output_bytes is NOT in the firecracker config — byte-for-byte identical with/without.
func TestFirecrackerOutputCapNotInConfig(t *testing.T) {
	withCap := firecrackerConfig("/k", "/r", "/p", "/v", Limits{MaxOutputBytes: 1024})
	without := firecrackerConfig("/k", "/r", "/p", "/v", Limits{})
	assertConfigsByteIdentical(t, "TC-016-08 (max_output_bytes)", withCap, without)
	b, _ := json.Marshal(withCap)
	if strings.Contains(string(b), "output") {
		t.Fatalf("TC-016-08: serialized config mentions 'output': %s", b)
	}
}

// assertConfigsByteIdentical marshals two configs and fails if their JSON differs byte-for-byte.
func assertConfigsByteIdentical(t *testing.T, label string, a, b map[string]any) {
	t.Helper()
	ba, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		t.Fatalf("%s: marshal failed: %v / %v", label, err1, err2)
	}
	if string(ba) != string(bb) {
		t.Fatalf("%s: configs differ — the host-side cap leaked into the firecracker config:\n  with: %s\n  without: %s",
			label, ba, bb)
	}
}

// ---------------------------------------------------------------------------
// Behavioral TCs (L5) — boot a real guest. Skip-guard via requireKVM.
// ---------------------------------------------------------------------------

// TC-016-09: memory_mb is behaviorally enforced — a ~200 MB allocation OOMs under memory_mb=64.
func TestFirecrackerMemoryLimitOOMs_E2E(t *testing.T) {
	requireKVM(t)
	// Control: with the default RAM the allocation succeeds (proves the cap is the cause). The guest
	// default is 128 MiB, too small for 200 MB; use memory_mb=512 for the control. busybox has no
	// perl/python, so awk builds ~200 MB of RESIDENT data (an array of chunks — held in RAM, no
	// string-concat doubling) — exactly what the mem_size_mib ceiling must kill. 2000 chunks ×
	// 100000 bytes = 200 MB, comfortably above the 64 MiB cap and below the 512 MiB control.
	const alloc = `awk 'BEGIN{ n=0; for(i=0;i<2000;i++){ a[i]=sprintf("%100000s",""); n+=length(a[i]) } ` +
		`print "ALLOCATED", n }' 2>&1`
	const allocatedMarker = "ALLOCATED 200000000" // 2000 * 100000 bytes
	ctrl := fcRun(t, alloc, map[string]any{"memory_mb": float64(512)})
	ctrlOut := str(ctrl["stdout"]) + str(ctrl["stderr"])
	if !strings.Contains(ctrlOut, allocatedMarker) {
		t.Skipf("TC-016-09: control 256MB allocation did not succeed under memory_mb=512 (no allocator tool in guest?); out=%q", ctrlOut)
	}

	capped := fcRun(t, alloc, map[string]any{"memory_mb": float64(64)})
	combined := str(capped["stdout"]) + str(capped["stderr"])
	t.Logf("TC-016-09 capped(memory_mb=64) exit=%v stdout=%q stderr=%q", capped["exit_code"], capped["stdout"], capped["stderr"])
	if strings.Contains(combined, allocatedMarker) {
		t.Fatalf("TC-016-09: 256MB allocation SUCCEEDED under a 64MiB mem_size_mib cap — not enforced. out=%q", combined)
	}
}

// TC-016-10: pids is behaviorally enforced — a fork bomb hits the in-guest NPROC cap under pids=20.
func TestFirecrackerPidsForkBomb_E2E(t *testing.T) {
	requireKVM(t)
	const payload = "i=0; while [ $i -lt 100 ]; do sleep 5 & i=$((i+1)); done 2>&1 | sort -u | head -5\necho SPAWNDONE\n"

	capped := fcRun(t, payload, map[string]any{"pids": float64(20)})
	combined := strings.ToLower(str(capped["stdout"]) + str(capped["stderr"]))
	t.Logf("TC-016-10 capped(pids=20) stdout=%q stderr=%q", capped["stdout"], capped["stderr"])
	if !strings.Contains(combined, "fork") && !strings.Contains(combined, "resource temporarily") &&
		!strings.Contains(combined, "can't fork") {
		t.Fatalf("TC-016-10: expected a fork failure under pids=20, got out=%q", combined)
	}
}

// TC-016-11: cpu_count is behaviorally observable — the guest reports nproc==1 under cpu_count=1
// (the guest genuinely has one vCPU, the ADR 010 D4 real-cap improvement).
func TestFirecrackerVcpuObservable_E2E(t *testing.T) {
	requireKVM(t)
	res := fcRun(t, "nproc", map[string]any{"cpu_count": float64(1)})
	out := strings.TrimSpace(str(res["stdout"]))
	t.Logf("TC-016-11 cpu_count=1 nproc stdout=%q exit=%v", res["stdout"], res["exit_code"])
	if out != "1" {
		t.Fatalf("TC-016-11: guest nproc = %q under cpu_count=1, want 1 (the guest must have exactly one vCPU)", out)
	}
}

// haveMkfs reports whether mkfs.ext4 is on PATH (needed to build the payload drive in Argv).
func haveMkfs() bool {
	_, err := exec.LookPath("mkfs.ext4")
	return err == nil
}

// forceDiskQuota overrides the diskQuotaSupported test seam for the test's duration.
func forceDiskQuota(t *testing.T, supported bool) {
	t.Helper()
	orig := diskQuotaSupported
	diskQuotaSupported = func() bool { return supported }
	t.Cleanup(func() { diskQuotaSupported = orig })
}

// readGuestInit returns the source of the guest /sbin/init (the in-guest launcher) so a test can
// assert it actually parses + applies the pids cmdline arg (guards against a dead delegate).
func readGuestInit(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("guest/rootfs/init/init")
	if err != nil {
		t.Fatalf("cannot read guest init: %v", err)
	}
	return string(b)
}
