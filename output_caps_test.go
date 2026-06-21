// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// Test spec: docs/tasks/test-specs/006-output-caps-test-spec.md
// ADR 007: the per-run output cap is enforced host-side, above the tier seam — the same capWriter
// truncates captured stdout/stderr identically under bubblewrap and gVisor; overflow is dropped
// without erroring the child's pipe; sandbox_status.limits.output_truncated records the capped
// streams. F-008.

// TC-006-01: max_output_bytes parsing — a positive int sets the cap; absent/zero/negative ⇒ no cap.
// The other limit fields parse unchanged (no regression to parseLimits).
func TestParseLimits_MaxOutputBytes(t *testing.T) {
	// (a) a positive value is parsed into MaxOutputBytes.
	got := parseLimits(map[string]any{"limits": map[string]any{"max_output_bytes": 1024.0}})
	if got.MaxOutputBytes != 1024 {
		t.Fatalf("MaxOutputBytes = %d, want 1024", got.MaxOutputBytes)
	}
	if (got != Limits{MaxOutputBytes: 1024}) {
		t.Fatalf("a lone max_output_bytes perturbed another field: %+v", got)
	}

	// (b) absent, (c) zero, (d) negative ⇒ 0 / no cap, no panic.
	for _, raw := range []map[string]any{
		{},                           // (b) field absent
		{"max_output_bytes": 0.0},    // (c) zero
		{"max_output_bytes": -5.0},   // (d) negative
		{"max_output_bytes": "1024"}, // junk (string) ⇒ unset
	} {
		if g := parseLimits(map[string]any{"limits": raw}); g.MaxOutputBytes != 0 {
			t.Fatalf("parseLimits(limits=%v).MaxOutputBytes = %d, want 0 (no cap)", raw, g.MaxOutputBytes)
		}
	}

	// No regression: max_output_bytes coexists with the other caps and parses them unchanged.
	full := parseLimits(map[string]any{"limits": map[string]any{
		"cpu_count": 2.0, "memory_mb": 64.0, "pids": 32.0, "disk_mb": 8.0,
		"timeout_sec": 5.0, "max_output_bytes": 1024.0,
	}})
	if full.CPUCount != 2 || full.MemoryMB != 64 || full.PidsLimit != 32 ||
		full.DiskMB != 8 || full.MaxOutputBytes != 1024 || full.Timeout.Seconds() != 5 {
		t.Fatalf("mixed limits parsed wrong: %+v", full)
	}
}

// TC-006-02: the capping writer truncates at the byte ceiling and drops the rest, never erroring
// the child's pipe; exactly cap bytes does not flag, cap+1 does; chunking does not matter.
func TestCapWriter(t *testing.T) {
	// Write 25 bytes into a cap-10 writer in one shot: first 10 retained, overflow flagged, the
	// Write reports all 25 consumed with no error (the child's pipe must not see a short write).
	w := newCapWriter(10)
	n, err := w.Write([]byte("0123456789ABCDEFGHIJKLMNO")) // 25 bytes
	if err != nil {
		t.Fatalf("Write returned an error (%v); the cap must never error the payload's pipe", err)
	}
	if n != 25 {
		t.Fatalf("Write reported %d bytes consumed, want 25 (full count so the child never blocks)", n)
	}
	if w.String() != "0123456789" {
		t.Fatalf("retained %q, want the first 10 bytes %q", w.String(), "0123456789")
	}
	if !w.overflowed {
		t.Fatal("overflowed = false after writing 25 bytes into a cap-10 writer")
	}

	// Chunked writes: 5 writes of 6 bytes (30 total) into a cap-10 writer retain exactly 10 bytes.
	wc := newCapWriter(10)
	for i := 0; i < 5; i++ {
		if _, err := wc.Write([]byte("abcdef")); err != nil {
			t.Fatalf("chunked Write errored: %v", err)
		}
	}
	if len(wc.String()) != 10 {
		t.Fatalf("chunked retained %d bytes, want exactly 10 (cap)", len(wc.String()))
	}
	if wc.String() != "abcdefabcd" {
		t.Fatalf("chunked retained %q, want %q", wc.String(), "abcdefabcd")
	}
	if !wc.overflowed {
		t.Fatal("chunked writer did not flag overflow after 30 bytes into cap 10")
	}

	// Edge — exactly cap bytes does NOT flag truncation (the flag means "bytes dropped").
	exact := newCapWriter(10)
	exact.Write([]byte("0123456789")) // exactly 10
	if exact.overflowed {
		t.Fatal("writing exactly cap bytes flagged overflow; it must not")
	}
	if exact.String() != "0123456789" {
		t.Fatalf("exact-cap retained %q, want all 10 bytes", exact.String())
	}

	// Edge — cap+1 DOES flag.
	plus1 := newCapWriter(10)
	plus1.Write([]byte("0123456789X")) // 11
	if !plus1.overflowed {
		t.Fatal("writing cap+1 bytes did not flag overflow")
	}
	if plus1.String() != "0123456789" {
		t.Fatalf("cap+1 retained %q, want the first 10", plus1.String())
	}

	// cap <= 0 ⇒ unbounded (prior behavior): everything retained, never flagged.
	un := newCapWriter(0)
	big := bytes.Repeat([]byte("z"), 100000)
	un.Write(big)
	if un.overflowed {
		t.Fatal("an uncapped (cap<=0) writer flagged overflow")
	}
	if len(un.String()) != 100000 {
		t.Fatalf("uncapped writer retained %d bytes, want all 100000", len(un.String()))
	}
}

// TC-006-03: output_truncated lists which streams were capped, in deterministic order, alongside
// the existing degraded array under sandbox_status.limits.
func TestOutputTruncatedRecord(t *testing.T) {
	cases := []struct {
		name             string
		stdoutOverflowed bool
		stderrOverflowed bool
		want             []string
	}{
		{"neither", false, false, []string{}},
		{"stdout only", true, false, []string{"stdout"}},
		{"stderr only", false, true, []string{"stderr"}},
		{"both", true, true, []string{"stdout", "stderr"}}, // stdout always first
	}
	for _, c := range cases {
		so := newCapWriter(1)
		so.overflowed = c.stdoutOverflowed
		se := newCapWriter(1)
		se.overflowed = c.stderrOverflowed
		got := outputTruncated(so, se)
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: outputTruncated = %v, want %v", c.name, got, c.want)
		}
	}

	// The field appears under sandbox_status.limits alongside degraded.
	report := limitsReport(Limits{MaxOutputBytes: 1024}, []string{}, []string{"stdout"})
	ot, ok := report["output_truncated"].([]string)
	if !ok {
		t.Fatalf("limitsReport has no []string output_truncated field: %v", report["output_truncated"])
	}
	if !reflect.DeepEqual(ot, []string{"stdout"}) {
		t.Fatalf("limitsReport output_truncated = %v, want [stdout]", ot)
	}
	if _, ok := report["degraded"]; !ok {
		t.Fatal("limitsReport dropped the degraded array")
	}
	if report["max_output_bytes"] != 1024 {
		t.Fatalf("limitsReport max_output_bytes = %v, want 1024", report["max_output_bytes"])
	}
}

// limitsOf is a small accessor for sandbox_status.limits in an integration result.
func limitsOf(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	lr, ok := sandboxStatus(t, res)["limits"].(map[string]any)
	if !ok {
		t.Fatalf("sandbox_status.limits missing/wrong type in %v", res)
	}
	return lr
}

// chattyPayload writes ~1 MiB to stdout and a short marker to stderr.
const chattyPayload = "head -c 1048576 /dev/zero | tr '\\0' a\nprintf MARK 1>&2\n"

// TC-006-04: the output cap truncates real payload output under bwrap; the payload still exits
// naturally; output_truncated contains "stdout".
func TestOutputCapTruncates_Bwrap(t *testing.T) {
	requireBwrap(t)
	res := Run(limitsReq("bubblewrap", chattyPayload, map[string]any{"max_output_bytes": 1024.0}))

	if got := len(res["stdout"].(string)); got != 1024 {
		t.Fatalf("len(stdout) = %d under max_output_bytes=1024, want exactly 1024 (truncated)", got)
	}
	// The payload still completes with its natural exit (the cap is not a payload signal): the
	// pipeline's `head`/`tr` exit 0, so exit_code is 0 and stderr's marker survives (it is short).
	if ec := res["exit_code"].(int); ec != 0 {
		t.Fatalf("exit_code = %d, want 0 — the output cap must not change the payload's exit", ec)
	}
	if se := res["stderr"].(string); se != "MARK" {
		t.Fatalf("stderr = %q, want %q (short stderr is under the cap, not truncated)", se, "MARK")
	}
	ot, _ := limitsOf(t, res)["output_truncated"].([]string)
	if !contains(ot, "stdout") {
		t.Fatalf("output_truncated = %v, want it to contain \"stdout\"", ot)
	}
	if contains(ot, "stderr") {
		t.Fatalf("output_truncated = %v, must not contain stderr (the 4-byte marker is under the cap)", ot)
	}
}

// TC-006-05: the output cap preserves the no-network invariant — it is a host-side capture concern,
// so the bwrap argv is byte-for-byte the same with and without the cap (no new flag, no new mount),
// still --unshare-all with no --share-net and the proxy socket the only egress bind.
func TestOutputCapDoesNotTouchBwrapArgv(t *testing.T) {
	withCap := bwrapArgv("/w/payload.sh", "/w/proxy.sock", "", nil, nil, 0, []string{"/usr/bin/sh", "/payload.sh"}, -1, 3)
	withoutCap := bwrapArgv("/w/payload.sh", "/w/proxy.sock", "", nil, nil, 0, []string{"/usr/bin/sh", "/payload.sh"}, -1, 3)
	// The cap never reaches bwrapArgv at all — it lives in Run()'s capture path. The argv is
	// identical regardless of MaxOutputBytes, which is exactly the tier-independence property.
	if !reflect.DeepEqual(withCap, withoutCap) {
		t.Fatalf("bwrap argv differs:\n with = %v\n base = %v", withCap, withoutCap)
	}
	joined := strings.Join(withCap, " ")
	if !strings.Contains(joined, "--unshare-all") {
		t.Fatal("bwrap argv lost --unshare-all")
	}
	if strings.Contains(joined, "--share-net") {
		t.Fatal("bwrap argv gained --share-net — the no-network invariant is broken")
	}
	// The only egress bind is the proxy socket.
	if !strings.Contains(joined, "--bind /w/proxy.sock /proxy.sock") {
		t.Fatalf("proxy socket bind missing from argv: %v", withCap)
	}

	// The OCI spec is likewise untouched by the cap — gvisorOCISpec takes no max_output_bytes and
	// the netns stays path-less (no host networking).
	spec := gvisorOCISpec("/w/payload.sh", "/w/proxy.sock")
	nss, _ := spec["linux"].(map[string]any)["namespaces"].([]map[string]any)
	for _, ns := range nss {
		if ns["type"] == "network" {
			if p, hasPath := ns["path"]; hasPath && p != "" {
				t.Fatalf("OCI network namespace gained a path %q — no longer an empty netns", p)
			}
		}
	}
}

// TC-006-06: the output cap truncates identically under gVisor — same truncated length, same
// output_truncated record as bwrap, proving the cap is tier-independent (it lives above the seam).
func TestOutputCapTruncates_Gvisor(t *testing.T) {
	requireRunsc(t)
	res := Run(limitsReq("gvisor", chattyPayload, map[string]any{"max_output_bytes": 1024.0}))

	if tier := sandboxStatus(t, res)["tier"]; tier != "gvisor" {
		t.Fatalf("tier = %v, want gvisor", tier)
	}
	if got := len(res["stdout"].(string)); got != 1024 {
		t.Fatalf("len(stdout) = %d under gVisor max_output_bytes=1024, want 1024 (identical to bwrap)", got)
	}
	ot, _ := limitsOf(t, res)["output_truncated"].([]string)
	if !contains(ot, "stdout") {
		t.Fatalf("gVisor output_truncated = %v, want it to contain \"stdout\"", ot)
	}
}

// TC-006-07: no cap ⇒ full output captured, output_truncated == [], behavior unchanged (regression).
func TestNoOutputCapFullOutput_Bwrap(t *testing.T) {
	requireBwrap(t)
	// A known multi-KiB string: 50000 'a' bytes plus a trailing newline from echo? Use printf to
	// avoid a newline ambiguity — emit exactly 50000 bytes.
	const payload = "head -c 50000 /dev/zero | tr '\\0' a\n"
	res := Run(limitsReq("bubblewrap", payload, map[string]any{})) // no max_output_bytes

	if got := len(res["stdout"].(string)); got != 50000 {
		t.Fatalf("uncapped len(stdout) = %d, want the full 50000 bytes", got)
	}
	ot, ok := limitsOf(t, res)["output_truncated"].([]string)
	if !ok {
		t.Fatalf("output_truncated missing/wrong type: %v", limitsOf(t, res)["output_truncated"])
	}
	if len(ot) != 0 {
		t.Fatalf("uncapped output_truncated = %v, want [] (nothing capped)", ot)
	}
}
