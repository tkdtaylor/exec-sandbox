package main

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// limitsReq builds a minimal RunRequest carrying profile.limits. No NetConnect capability is set
// (these payloads don't use the network), so the egress allowlist is empty — orthogonal to limits.
func limitsReq(tier, payload string, limits map[string]any) RunRequest {
	var req RunRequest
	req.Run.Payload = payload
	req.Run.Tier = tier
	req.Run.Profile = map[string]any{"limits": limits}
	req.Wiring.RequestID = "limits-test"
	return req
}

func sandboxStatus(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	st, ok := res["sandbox_status"].(map[string]any)
	if !ok {
		t.Fatalf("sandbox_status missing/wrong type in %v", res)
	}
	return st
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what was written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	fn()
	w.Close()
	os.Stderr = orig
	return <-done
}

// TC-001: parseLimits maps the contract fields; absent/zero/non-numeric ⇒ unset.
func TestParseLimits(t *testing.T) {
	full := parseLimits(map[string]any{"limits": map[string]any{
		"cpu_count": 2.0, "memory_mb": 64.0, "pids": 32.0, "disk_mb": 8.0, "timeout_sec": 5.0,
	}})
	want := Limits{CPUCount: 2, MemoryMB: 64, PidsLimit: 32, DiskMB: 8, Timeout: 5 * time.Second}
	if full != want {
		t.Fatalf("full limits = %+v, want %+v", full, want)
	}

	partial := parseLimits(map[string]any{"limits": map[string]any{"memory_mb": 128.0}})
	if (partial != Limits{MemoryMB: 128}) {
		t.Fatalf("partial limits = %+v, want only MemoryMB=128", partial)
	}

	for _, p := range []map[string]any{
		{},                           // no limits key
		{"limits": map[string]any{}}, // empty limits
		{"limits": map[string]any{"memory_mb": -4.0, "pids": 0.0, "cpu_count": "x"}}, // junk ⇒ unset
	} {
		if got := parseLimits(p); (got != Limits{}) {
			t.Fatalf("parseLimits(%v) = %+v, want zero Limits", p, got)
		}
	}
}

// TC-009: the gVisor OCI spec + argv carry the limits — the host-side authoritative record by which
// cpu_count is verified under gVisor (ADR 003 / ADR 028). No runsc required (inspects the spec).
func TestGvisorOCISpecCarriesLimits(t *testing.T) {
	spec := gvisorOCISpec("/work/payload.sh", "/work/proxy.sock")
	degrades := applyLimitsToOCISpec(spec, Limits{CPUCount: 2, MemoryMB: 64, PidsLimit: 40, DiskMB: 8})
	if len(degrades) != 0 {
		t.Fatalf("unexpected degrades on an enforcing host: %v", degrades)
	}

	proc := spec["process"].(map[string]any)
	rlimits, ok := proc["rlimits"].([]map[string]any)
	if !ok {
		t.Fatal("process.rlimits missing")
	}
	gotAS, gotNPROC := uint64(0), uint64(0)
	for _, rl := range rlimits {
		switch rl["type"] {
		case "RLIMIT_AS":
			gotAS = rl["hard"].(uint64)
		case "RLIMIT_NPROC":
			gotNPROC = rl["hard"].(uint64)
		}
	}
	if gotAS != 64*1024*1024 {
		t.Fatalf("RLIMIT_AS = %d, want %d", gotAS, 64*1024*1024)
	}
	if gotNPROC != 40 {
		t.Fatalf("RLIMIT_NPROC = %d, want 40", gotNPROC)
	}

	// /tmp tmpfs carries a size= cap.
	mounts := spec["mounts"].([]map[string]any)
	var tmpSized bool
	for _, m := range mounts {
		if m["destination"] == "/tmp" {
			for _, o := range m["options"].([]string) {
				if strings.HasPrefix(o, "size=") {
					tmpSized = true
				}
			}
		}
	}
	if !tmpSized {
		t.Fatal("/tmp tmpfs has no size= option for disk_mb")
	}

	// cpu_count is verified host-side: the runsc argv must be prefixed with taskset -c 0-1.
	argv, cleanup, _, err := gvisorBackend{}.Argv("/work/payload.sh", "/work/proxy.sock",
		Limits{CPUCount: 2})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("gvisor Argv error: %v", err)
	}
	if len(argv) < 3 || argv[0] != "taskset" || argv[1] != "-c" || argv[2] != "0-1" {
		t.Fatalf("gvisor argv not taskset-prefixed for cpu_count=2: %v", argv[:min(4, len(argv))])
	}

	// Edge: zero limits leave the base spec unchanged (no rlimits, no size=, no taskset).
	base := gvisorOCISpec("/work/payload.sh", "/work/proxy.sock")
	if d := applyLimitsToOCISpec(base, Limits{}); len(d) != 0 {
		t.Fatalf("zero limits produced degrades: %v", d)
	}
	if _, present := base["process"].(map[string]any)["rlimits"]; present {
		t.Fatal("zero limits added process.rlimits")
	}
	zargv, zc, _, _ := gvisorBackend{}.Argv("/work/payload.sh", "/work/proxy.sock", Limits{})
	if zc != nil {
		defer zc()
	}
	if zargv[0] != "runsc" {
		t.Fatalf("zero-limit gvisor argv should start with runsc, got %q", zargv[0])
	}
}

// TC-002: timeout_sec terminates an over-running payload; status == "timeout".
func TestTimeoutTerminatesPayload(t *testing.T) {
	requireBwrap(t)
	res := Run(limitsReq("bubblewrap", "echo START\nsleep 30\necho DONE\n",
		map[string]any{"timeout_sec": 1.0}))

	st := sandboxStatus(t, res)
	if st["status"] != "timeout" {
		t.Fatalf("status = %v, want \"timeout\"", st["status"])
	}
	if dur := res["sandbox_status"].(map[string]any)["duration_ms"].(int64); dur > 10000 {
		t.Fatalf("duration_ms = %d, expected the sleep to be killed near 1s (well under 30s)", dur)
	}
	if ec := res["exit_code"].(int); ec == 0 {
		t.Fatal("exit_code = 0, expected non-zero for a timed-out payload")
	}
	out := res["stdout"].(string)
	if !strings.Contains(out, "START") || strings.Contains(out, "DONE") {
		t.Fatalf("expected START but not DONE (payload killed mid-sleep), stdout=%q", out)
	}

	// Edge: a payload that finishes before the timeout is clean, not a false timeout.
	ok := Run(limitsReq("bubblewrap", "echo quick\n", map[string]any{"timeout_sec": 30.0}))
	okSt := sandboxStatus(t, ok)
	if okSt["status"] != "clean" || ok["exit_code"].(int) != 0 {
		t.Fatalf("fast payload should be clean/0, got status=%v exit=%v", okSt["status"], ok["exit_code"])
	}
}

// TC-003: memory_mb kills a payload that exceeds RLIMIT_AS under bwrap.
func TestMemoryLimitKillsPayload_Bwrap(t *testing.T) {
	requireBwrap(t)
	const payload = `perl -e 'my $x = "A" x (256*1024*1024); print "ALLOCATED ", length($x), "\n";' 2>&1` + "\n"

	// Control: without a memory cap the 256MB allocation succeeds (proves the cap is the cause).
	ctrl := Run(limitsReq("bubblewrap", payload, map[string]any{}))
	if !strings.Contains(ctrl["stdout"].(string), "ALLOCATED") {
		t.Skipf("control allocation did not succeed (perl missing?), stdout=%q stderr=%q",
			ctrl["stdout"], ctrl["stderr"])
	}

	capped := Run(limitsReq("bubblewrap", payload, map[string]any{"memory_mb": 64.0}))
	combined := capped["stdout"].(string) + capped["stderr"].(string)
	if strings.Contains(combined, "ALLOCATED 268435456") {
		t.Fatalf("256MB allocation succeeded under a 64MB cap; not enforced. out=%q", combined)
	}
}

// TC-004: pids rejects a fork bomb under bwrap (RLIMIT_NPROC, per-sandbox via the userns).
func TestPidsLimitRejectsForkBomb_Bwrap(t *testing.T) {
	requireBwrap(t)
	const payload = "i=0; while [ $i -lt 80 ]; do sleep 3 & i=$((i+1)); done 2>&1 | sort -u | head -3\necho SPAWNDONE\n"

	capped := Run(limitsReq("bubblewrap", payload, map[string]any{"pids": 20.0}))
	combined := strings.ToLower(capped["stdout"].(string) + capped["stderr"].(string))
	if !strings.Contains(combined, "fork") && !strings.Contains(combined, "resource temporarily") {
		t.Fatalf("expected a fork failure under pids=20, got out=%q", combined)
	}

	// Edge: without the cap the loop completes without a fork failure.
	ctrl := Run(limitsReq("bubblewrap", payload, map[string]any{}))
	cc := strings.ToLower(ctrl["stdout"].(string) + ctrl["stderr"].(string))
	if strings.Contains(cc, "cannot fork") {
		t.Fatalf("uncapped run hit a fork failure unexpectedly: %q", cc)
	}
}

// TC-005: disk_mb blocks writes past the cap under bwrap (tmpfs --size).
func TestDiskLimitBlocksWrites_Bwrap(t *testing.T) {
	requireBwrap(t)
	const payload = "dd if=/dev/zero of=/tmp/big bs=1M count=4 2>&1\necho DDDONE\n"

	capped := Run(limitsReq("bubblewrap", payload, map[string]any{"disk_mb": 1.0}))
	if !strings.Contains(capped["stdout"].(string)+capped["stderr"].(string), "No space left") {
		t.Fatalf("expected ENOSPC writing 4MB to a 1MB /tmp, got out=%q err=%q",
			capped["stdout"], capped["stderr"])
	}

	// Edge: without the cap the 4MB write succeeds.
	ctrl := Run(limitsReq("bubblewrap", payload, map[string]any{}))
	if strings.Contains(ctrl["stdout"].(string)+ctrl["stderr"].(string), "No space left") {
		t.Fatalf("uncapped 4MB write hit ENOSPC unexpectedly: out=%q", ctrl["stdout"])
	}
}

// TC-006: cpu_count applies taskset affinity, visible in-box under bwrap.
func TestCPUAffinity_Bwrap(t *testing.T) {
	requireBwrap(t)
	if runtime.NumCPU() < 2 {
		t.Skip("need >= 2 host cores to observe a cpu_count=1 affinity cap")
	}
	capped := Run(limitsReq("bubblewrap", "nproc\n", map[string]any{"cpu_count": 1.0}))
	if got := strings.TrimSpace(capped["stdout"].(string)); got != "1" {
		t.Fatalf("in-box nproc = %q under cpu_count=1, want \"1\" (stderr=%q)", got, capped["stderr"])
	}
	// Edge: without the cap, in-box nproc reflects the host's cores (> 1 here).
	ctrl := Run(limitsReq("bubblewrap", "nproc\n", map[string]any{}))
	if got := strings.TrimSpace(ctrl["stdout"].(string)); got == "1" {
		t.Fatalf("uncapped in-box nproc = 1; expected the host's %d cores", runtime.NumCPU())
	}
}

// TC-007 + TC-010: a non-enforcing host degrades disk_mb (warn + continue), the run still succeeds,
// and sandbox_status.limits records the degraded cap. The contract shape (including the additive
// limits report) is preserved.
func TestDiskQuotaDegradesGracefully_Bwrap(t *testing.T) {
	requireBwrap(t)
	orig := diskQuotaSupported
	diskQuotaSupported = func() bool { return false }
	defer func() { diskQuotaSupported = orig }()

	const payload = "dd if=/dev/zero of=/tmp/big bs=1M count=4 2>&1\necho DDDONE\n"
	var res map[string]any
	warn := captureStderr(t, func() {
		res = Run(limitsReq("bubblewrap", payload, map[string]any{"disk_mb": 1.0}))
	})

	// The run still succeeds — a secondary control degrades, it does not fail the run (ADR 027).
	if ec := res["exit_code"].(int); ec != 0 {
		t.Fatalf("degraded-disk run exit_code = %d, want 0 (run must still succeed)", ec)
	}
	if !strings.Contains(res["stdout"].(string)+res["stderr"].(string), "DDDONE") {
		t.Fatal("expected the 4MB write to be allowed (quota dropped), payload did not complete")
	}
	if !strings.Contains(warn, "WARNING") || !strings.Contains(warn, "disk_mb") {
		t.Fatalf("expected a stderr WARNING naming disk_mb, got %q", warn)
	}

	// sandbox_status.limits is the additive record; degraded lists disk_mb.
	st := sandboxStatus(t, res)
	lr, ok := st["limits"].(map[string]any)
	if !ok {
		t.Fatal("sandbox_status.limits missing")
	}
	deg, _ := lr["degraded"].([]string)
	if !contains(deg, "disk_mb") {
		t.Fatalf("limits.degraded = %v, want it to contain disk_mb", deg)
	}
	// Contract shape preserved: the established keys are all present.
	for _, k := range []string{"sandbox_id", "tier", "duration_ms", "secrets_injected", "status", "limits"} {
		if _, ok := st[k]; !ok {
			t.Fatalf("sandbox_status missing key %q; contract shape changed", k)
		}
	}
}

// TC-008: gVisor enforces memory_mb, pids, and disk_mb via process.rlimits + tmpfs size. Each cap
// is proven in its own run so a fork bomb's lingering PIDs can't starve a later disk step.
func TestGvisorEnforcesLimits(t *testing.T) {
	requireRunsc(t)

	mem := Run(limitsReq("gvisor",
		`perl -e 'my $x = "A" x (256*1024*1024); print "ALLOCATED\n";' 2>&1 | tail -1`+"\n",
		map[string]any{"memory_mb": 64.0}))
	if sandboxStatus(t, mem)["tier"] != "gvisor" {
		t.Fatalf("tier = %v, want gvisor", sandboxStatus(t, mem)["tier"])
	}
	if out := mem["stdout"].(string) + mem["stderr"].(string); strings.Contains(out, "ALLOCATED") {
		t.Fatalf("memory_mb not enforced under gVisor: 256MB allocated under 64MB cap. out=%q", out)
	}

	pids := Run(limitsReq("gvisor",
		"i=0; while [ $i -lt 80 ]; do sleep 3 & i=$((i+1)); done 2>&1 | sort -u | head -3\necho PDONE\n",
		map[string]any{"pids": 40.0}))
	if out := strings.ToLower(pids["stdout"].(string) + pids["stderr"].(string)); !strings.Contains(out, "fork") {
		t.Fatalf("pids not enforced under gVisor: no fork failure. out=%q", out)
	}

	disk := Run(limitsReq("gvisor",
		"dd if=/dev/zero of=/tmp/big bs=1M count=4 2>&1\necho DDONE\n",
		map[string]any{"disk_mb": 1.0}))
	if out := disk["stdout"].(string) + disk["stderr"].(string); !strings.Contains(out, "No space left") {
		t.Fatalf("disk_mb not enforced under gVisor: 4MB write to a 1MB /tmp succeeded. out=%q", out)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
