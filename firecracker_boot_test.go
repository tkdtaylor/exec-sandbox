// SPDX-License-Identifier: Apache-2.0
package main

import (
	"strings"
	"testing"
	"time"
)

// Task 015 end-to-end boot TCs (L5). These actually boot the guest, so they require /dev/kvm +
// firecracker + the verified kernel/rootfs and skip-guard via requireKVM when absent. They drive the
// FULL Run() so the unchanged host-side capture/exit-mapping/timeout path is exercised against a real
// firecracker child.

// fcRun is a small helper: run a payload under the firecracker tier with the given limits and return
// the result map. It points the artifact resolver at the worktree root for the duration.
func fcRun(t *testing.T, payload string, lim map[string]any) map[string]any {
	t.Helper()
	withRepoRootArtifacts(t)
	req := RunRequest{}
	req.Run.Payload = payload
	req.Run.Tier = "firecracker"
	if lim != nil {
		req.Run.Profile = map[string]any{"limits": lim}
	}
	return Run(req)
}

// TC-015-04: the guest boots and runs /usr/bin/sh /payload.sh — HELLO-FROM-GUEST on stdout, exit 0.
func TestFirecrackerGuestBoot_E2E(t *testing.T) {
	requireKVM(t)
	start := time.Now()
	res := fcRun(t, "echo HELLO-FROM-GUEST", nil)
	t.Logf("TC-015-04 boot+run took %s; result=%v", time.Since(start), res)

	if code, _ := res["exit_code"].(int); code != 0 {
		t.Fatalf("TC-015-04: exit_code = %v, want 0; result=%v", res["exit_code"], res)
	}
	stdout, _ := res["stdout"].(string)
	if !strings.Contains(stdout, "HELLO-FROM-GUEST") {
		t.Fatalf("TC-015-04: stdout %q does not contain HELLO-FROM-GUEST — payload did not run in the guest", stdout)
	}
	ss, _ := res["sandbox_status"].(map[string]any)
	if ss["status"] != "clean" {
		t.Fatalf("TC-015-04: status = %v, want clean", ss["status"])
	}
	if ss["tier"] != "firecracker" {
		t.Fatalf("TC-015-04: tier = %v, want firecracker", ss["tier"])
	}
}

// TC-015-06: a clean payload that writes stdout then exits 0 maps to exit_code 0 / status clean via
// the unchanged capture path.
func TestFirecrackerCleanExit_E2E(t *testing.T) {
	requireKVM(t)
	res := fcRun(t, "echo line-one; echo line-two; exit 0", nil)
	if code, _ := res["exit_code"].(int); code != 0 {
		t.Fatalf("TC-015-06: exit_code = %v, want 0; result=%v", res["exit_code"], res)
	}
	stdout, _ := res["stdout"].(string)
	if !strings.Contains(stdout, "line-one") || !strings.Contains(stdout, "line-two") {
		t.Fatalf("TC-015-06: stdout %q missing payload lines", stdout)
	}
	ss, _ := res["sandbox_status"].(map[string]any)
	if ss["status"] != "clean" {
		t.Fatalf("TC-015-06: status = %v, want clean", ss["status"])
	}
}

// TC-015-08: a non-zero payload exit (exit 3) propagates through the unchanged capture path.
func TestFirecrackerNonZeroExit_E2E(t *testing.T) {
	requireKVM(t)
	res := fcRun(t, "echo before-exit; exit 3", nil)
	if code, _ := res["exit_code"].(int); code != 3 {
		t.Fatalf("TC-015-08: exit_code = %v, want 3; result=%v", res["exit_code"], res)
	}
	ss, _ := res["sandbox_status"].(map[string]any)
	if ss["status"] != "clean" {
		t.Fatalf("TC-015-08: status = %v, want clean (a clean non-zero exit, not a timeout)", ss["status"])
	}
}

// TC-015-09: an over-running payload (sleep 30) under timeout_sec=1 is killed on the wall-clock
// deadline — status timeout, exit_code 137 — by the unchanged host-side process-group SIGKILL.
func TestFirecrackerTimeout_E2E(t *testing.T) {
	requireKVM(t)
	start := time.Now()
	res := fcRun(t, "sleep 30", map[string]any{"timeout_sec": float64(1)})
	elapsed := time.Since(start)
	t.Logf("TC-015-09 timeout run took %s; result=%v", elapsed, res)

	ss, _ := res["sandbox_status"].(map[string]any)
	if ss["status"] != "timeout" {
		t.Fatalf("TC-015-09: status = %v, want timeout; result=%v", ss["status"], res)
	}
	if code, _ := res["exit_code"].(int); code != 137 {
		t.Fatalf("TC-015-09: exit_code = %v, want 137 (128+SIGKILL)", res["exit_code"])
	}
	// The kill must be on the deadline, not after sleep 30 finishes — well under 30s, with margin for
	// boot + WaitDelay.
	if elapsed > 20*time.Second {
		t.Fatalf("TC-015-09: run took %s — the wall-clock deadline did not terminate the guest near 1s", elapsed)
	}
}
