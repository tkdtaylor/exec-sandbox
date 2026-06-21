// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Task 018 — teardown (D5). At the end of a firecracker run the microVM is terminated and its
// per-run bundle dir + any surviving firecracker/fc-launch process + cgroup/namespace artifacts are
// reclaimed, on EVERY exit path (clean, non-zero, timeout, launch error). The teardown is the
// backend `cleanup` func on the existing `defer cleanup()` path in Run(); snapshot.go (the host-side
// baseline) is UNCHANGED. The L5 TCs boot a real guest and skip-guard via requireKVM.

// bundlesUnderTemp lists the per-run firecracker bundle dirs currently present under os.TempDir()
// (they are MkdirTemp("", "exec-sandbox-fc-")). It is the filesystem half of the no-survivor probe:
// after a run, the bundle this run created must be gone. We snapshot before/after so the assertion is
// about THIS run's bundle, robust to any unrelated leftover.
func bundlesUnderTemp() map[string]bool {
	out := map[string]bool{}
	entries, _ := os.ReadDir(os.TempDir())
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "exec-sandbox-fc-") {
			out[filepath.Join(os.TempDir(), e.Name())] = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// TC-018-01 / TC-018-02: clean exit — no firecracker/fc-launch process and no bundle dir survive.
// ---------------------------------------------------------------------------

func TestFirecrackerTeardown_CleanExitNoSurvivors_E2E(t *testing.T) {
	requireKVM(t)
	before := bundlesUnderTemp()

	res := fcRun(t, "echo TEARDOWN-CLEAN", nil)
	if code, _ := res["exit_code"].(int); code != 0 {
		t.Fatalf("TC-018-01: exit_code = %v, want 0; res=%v", res["exit_code"], res)
	}

	assertNoFirecrackerSurvivors(t, "TC-018-01/02 (clean)", before)
}

// ---------------------------------------------------------------------------
// TC-018-01 (non-zero): the same no-survivor property on a non-zero (still clean) exit path.
// ---------------------------------------------------------------------------

func TestFirecrackerTeardown_NonZeroExitNoSurvivors_E2E(t *testing.T) {
	requireKVM(t)
	before := bundlesUnderTemp()

	res := fcRun(t, "echo before; exit 7", nil)
	if code, _ := res["exit_code"].(int); code != 7 {
		t.Fatalf("TC-018-01 (non-zero): exit_code = %v, want 7; res=%v", res["exit_code"], res)
	}

	assertNoFirecrackerSurvivors(t, "TC-018-01 (non-zero)", before)
}

// ---------------------------------------------------------------------------
// TC-018-03 (timeout): teardown fires on the wall-clock-kill path — no firecracker process and no
// bundle dir survive even when the guest is SIGKILLed mid-run.
// ---------------------------------------------------------------------------

func TestFirecrackerTeardown_TimeoutNoSurvivors_E2E(t *testing.T) {
	requireKVM(t)
	before := bundlesUnderTemp()

	res := fcRun(t, "sleep 30", map[string]any{"timeout_sec": float64(1)})
	ss, _ := res["sandbox_status"].(map[string]any)
	if ss["status"] != "timeout" {
		t.Fatalf("TC-018-03 (timeout): status = %v, want timeout; res=%v", ss["status"], res)
	}

	// Give the deferred cleanup a beat to reap after Run() returns; firecracker dies with bwrap (the
	// process-group SIGKILL) but the cleanup belt-and-suspenders reap + RemoveAll runs on return.
	assertNoFirecrackerSurvivors(t, "TC-018-03 (timeout)", before)
}

// ---------------------------------------------------------------------------
// TC-018-03 (launch error): teardown fires on the launch-error path. A launch error (e.g. a
// mkfs failure or a missing prereq) returns through Argv's cleanup() call — no bundle is left behind.
// This half runs WITHOUT /dev/kvm (the failure is at bundle-build time, before any boot).
// ---------------------------------------------------------------------------

func TestFirecrackerTeardown_LaunchErrorReclaimsBundle(t *testing.T) {
	withRepoRootArtifacts(t)
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not on PATH; Argv cannot build the payload drive here")
	}
	before := bundlesUnderTemp()

	// Force a launch error AFTER the bundle is created: an unreadable scriptPath makes buildPayloadDrive
	// fail, which calls cleanup() (removing the bundle) and returns an error. This exercises the
	// error-exit reclaim path of Argv directly (the same cleanup the defer in Run() would run).
	scriptPath := filepath.Join(t.TempDir(), "does-not-exist.sh")
	_, cleanup, _, _, err := firecrackerBackend{}.Argv(scriptPath, filepath.Join(t.TempDir(), "p.sock"), "", nil, nil, nil, fcLimits)
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("TC-018-03 (launch error): expected Argv to error on an unreadable payload, got nil")
	}

	// No bundle dir from this attempt survives — Argv's error path ran cleanup().
	after := bundlesUnderTemp()
	for b := range after {
		if !before[b] {
			t.Fatalf("TC-018-03 (launch error): bundle %q survived a failed Argv — cleanup() did not run on the error path", b)
		}
	}
}

// TC-018-03 (launch error, via Run): a missing /dev/kvm or firecracker binary surfaces as exit 127
// AND leaves no bundle behind. This drives the FULL Run() path so the deferred cleanup() is what
// reclaims the bundle on a launch failure. Runs everywhere (it asserts the no-leak property whether
// the launch fails at prereq-check or at boot).
func TestFirecrackerTeardown_RunLaunchErrorNoBundle(t *testing.T) {
	withRepoRootArtifacts(t)
	before := bundlesUnderTemp()

	req := RunRequest{}
	req.Run.Payload = "echo hi"
	req.Run.Tier = "firecracker"
	res := Run(req)

	// Whether the run booted cleanly (kvm present) or failed at a prereq (kvm/firecracker/mkfs absent),
	// NO per-run bundle dir may outlive Run() — the deferred cleanup() reclaims it on every exit path.
	after := bundlesUnderTemp()
	var leaked []string
	for b := range after {
		if !before[b] {
			leaked = append(leaked, b)
		}
	}
	if len(leaked) > 0 {
		t.Fatalf("TC-018-03 (run launch path): bundle(s) %v survived Run() — teardown did not reclaim them; res=%v", leaked, res)
	}
}

// assertNoFirecrackerSurvivors is the shared post-run no-survivor assertion (TC-018-01/02/03). It
// fails if (a) any firecracker/fc-launch process referencing a NEW bundle (one not present before the
// run) is still alive, or (b) any NEW exec-sandbox-fc- bundle dir survived. before is the bundle set
// snapshotted before the run.
func assertNoFirecrackerSurvivors(t *testing.T, tc string, before map[string]bool) {
	t.Helper()
	after := bundlesUnderTemp()
	var leakedBundles []string
	for b := range after {
		if !before[b] {
			leakedBundles = append(leakedBundles, b)
		}
	}
	if len(leakedBundles) > 0 {
		// Re-check after a brief settle: the cleanup RemoveAll is synchronous on Run() return, so a
		// surviving dir here is a genuine leak.
		time.Sleep(200 * time.Millisecond)
		after = bundlesUnderTemp()
		leakedBundles = leakedBundles[:0]
		for b := range after {
			if !before[b] {
				leakedBundles = append(leakedBundles, b)
			}
		}
	}
	if len(leakedBundles) > 0 {
		t.Fatalf("%s: per-run bundle dir(s) survived the run: %v — teardown did not reclaim them", tc, leakedBundles)
	}
	// No live firecracker/fc-launch process references any surviving (new) bundle. Since the bundles
	// are gone, scan for any firecracker child whose api-sock would have lived under TempDir's
	// exec-sandbox-fc- prefix and is still alive.
	deadline := time.Now().Add(2 * time.Second)
	for {
		alive := firecrackerSurvivorPids()
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: firecracker/fc-launch process(es) %v still alive after the run — teardown did not reap them", tc, alive)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// firecrackerSurvivorPids returns the pids of any live process whose executable basename is
// firecracker and whose --api-sock argument points into an exec-sandbox-fc- bundle under TempDir.
// These are exactly the VMMs a run would have spawned; after teardown there must be none.
func firecrackerSurvivorPids() []int {
	var pids []int
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
		cmdline, err := os.ReadFile(filepath.Join("/proc", name, "cmdline"))
		if err != nil {
			continue
		}
		joined := strings.ReplaceAll(string(cmdline), "\x00", " ")
		if strings.Contains(joined, filepath.Join(os.TempDir(), "exec-sandbox-fc-")) ||
			strings.Contains(joined, "exec-sandbox-fc-") {
			pid := 0
			for _, c := range name {
				pid = pid*10 + int(c-'0')
			}
			pids = append(pids, pid)
		}
	}
	return pids
}

// ---------------------------------------------------------------------------
// TC-018-02 (unit): the backend cleanup func removes the bundle dir AND defensively reaps a surviving
// firecracker child. This drives the reap path directly with a stand-in long-lived process so it runs
// WITHOUT /dev/kvm: it proves reapFirecrackerOrphans kills a process whose argv references the bundle.
// ---------------------------------------------------------------------------

func TestFirecrackerTeardown_ReapsOrphanReferencingBundle(t *testing.T) {
	bundle := t.TempDir()
	// A stand-in orphan: `sleep 300 <bundle>` — its argv references the bundle, mimicking a firecracker
	// child whose --api-sock lives in the bundle. reapFirecrackerOrphans must SIGKILL it.
	orphan := exec.Command("sleep", "300", bundle)
	if err := orphan.Start(); err != nil {
		t.Fatalf("start stand-in orphan: %v", err)
	}
	t.Cleanup(func() {
		if orphan.Process != nil {
			_ = orphan.Process.Kill()
			_, _ = orphan.Process.Wait()
		}
	})

	reapFirecrackerOrphans(bundle)

	// The orphan must now be dead (or dying). Wait() returns once it is reaped.
	done := make(chan error, 1)
	go func() { done <- orphan.Wait() }()
	select {
	case <-done:
		// reaped — the SIGKILL landed.
	case <-time.After(3 * time.Second):
		t.Fatal("TC-018-02 (reap): the bundle-referencing orphan survived reapFirecrackerOrphans — the defensive reap is a no-op (BUG)")
	}
}

// TC-018-02 (unit, scoping): reapFirecrackerOrphans does NOT kill a process whose argv does not
// reference the bundle — the reap is scoped to THIS run, never a broad pkill that could hit a
// concurrent run's child.
func TestFirecrackerTeardown_ReapDoesNotKillUnrelated(t *testing.T) {
	bundle := t.TempDir()
	otherBundle := t.TempDir()
	// An UNRELATED long-lived process referencing a DIFFERENT bundle. The reap of `bundle` must leave
	// it alone.
	unrelated := exec.Command("sleep", "300", otherBundle)
	if err := unrelated.Start(); err != nil {
		t.Fatalf("start unrelated: %v", err)
	}
	defer func() {
		_ = unrelated.Process.Kill()
		_, _ = unrelated.Process.Wait()
	}()

	reapFirecrackerOrphans(bundle)

	// The unrelated process is still alive: Signal(0) succeeds.
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("TC-018-02 (scoping): reapFirecrackerOrphans killed an UNRELATED process (referencing %q, not the reaped bundle %q) — the reap is not scoped (BUG): %v", otherBundle, bundle, err)
	}
}

// TC-018-04: snapshot.go (the host-side baseline) is byte-for-byte unchanged by this task. The
// firecracker teardown is additive (in the backend cleanup func in firecracker.go), NOT a
// modification of the tier-independent host-side baseline. The F-010 snapshot/restore tests are run
// by `make fitness-snapshot-restore`; here we assert snapshot.go has not gained any firecracker
// teardown symbol (the additive teardown lives in firecracker.go).
func TestTeardown_SnapshotBaselineUnchanged(t *testing.T) {
	b, err := os.ReadFile("snapshot.go")
	if err != nil {
		t.Fatalf("read snapshot.go: %v", err)
	}
	body := string(b)
	// snapshot.go must remain the host-side baseline only — no firecracker/microVM teardown leaked in.
	for _, forbidden := range []string{"firecracker", "reapFirecracker", "fc-launch", "microVM", "bundle"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("TC-018-04: snapshot.go contains %q — the firecracker teardown must live in firecracker.go, not the tier-independent host-side baseline", forbidden)
		}
	}
}
