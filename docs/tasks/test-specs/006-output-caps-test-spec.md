# Test Spec 006: per-run stdout/stderr output caps (completing per-run resource bounding)

**Linked task:** [`docs/tasks/backlog/006-output-caps.md`](../backlog/006-output-caps.md)
**ADR:** ADR 007 (to be written during implementation)
**Written:** 2026-06-19

## Context for the test author

Task 002 already enforces `profile.limits` `{cpu_count, memory_mb, pids, disk_mb, timeout_sec}`
on both wired tiers (bwrap + gVisor). The **one remaining per-run resource bound** the roadmap
calls for is an **output cap**: a byte ceiling on captured `stdout`/`stderr` so a chatty or
adversarial payload cannot exhaust host memory via the unbounded `bytes.Buffer` in `Run()`
(`run.go:147`). This task adds `profile.limits.max_output_bytes` and enforces it host-side,
**identically across both tiers** (the cap lives in the host capture path, not in the backend).

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-006-01 | `profile.limits.max_output_bytes` is parsed into `Limits` (mirroring `parseLimits` for the other caps): an int; missing/zero/non-positive ⇒ "no cap" (current unbounded behavior) | TC-006-01 (unit) | ⏳ |
| REQ-006-02 | When `max_output_bytes` > 0, captured `stdout` is truncated at the cap; captured `stderr` is **independently** truncated at the same cap; bytes beyond the cap are dropped, not buffered (no unbounded growth) | TC-006-02 (unit), TC-006-04 (bwrap), TC-006-06 (gvisor) | ⏳ |
| REQ-006-03 | Truncation is **observable, not silent**: the result records that output was truncated — `sandbox_status.limits.output_truncated` lists which streams were capped (e.g. `["stdout"]`, `["stdout","stderr"]`); an uncapped run records `[]` | TC-006-03 (unit), TC-006-04 (bwrap) | ⏳ |
| REQ-006-04 | The cap is enforced **identically on both tiers** — the same host-side capture path applies to bwrap and gVisor; a payload emitting N≫cap bytes yields the same truncated length and the same `output_truncated` record under each tier | TC-006-04 (bwrap), TC-006-06 (gvisor) | ⏳ |
| REQ-006-05 | The output cap **preserves the no-network + proxy-only-egress invariant**: adding the cap touches only the host capture path; the bwrap argv still carries `--unshare-all` with no `--share-net` and the OCI netns stays path-less; the proxy socket remains the only egress | TC-006-05 (argv/spec unit) | ⏳ |
| REQ-006-06 | Backward compatible: a request with no `max_output_bytes` (or 0) captures full output exactly as before this task; `output_truncated` is `[]`; existing tests stay green | TC-006-07 (regression) | ⏳ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Edge cases and error paths are covered
- [x] Every REQ-ID has at least one test case
- [x] Success criteria are unambiguous
- [x] Confirmed task 002 covers the other caps; this task is scoped to the output cap only (no overlap)

---

## Test cases

### TC-006-01: `max_output_bytes` parsing

- **Requirement:** REQ-006-01
- **Type:** unit (no sandbox)
- **Input:** limits objects with (a) `max_output_bytes: 1024`; (b) field absent; (c)
  `max_output_bytes: 0`; (d) `max_output_bytes: -5`.
- **Expected:** (a) `Limits.MaxOutputBytes == 1024`; (b)(c)(d) ⇒ 0 / "no cap" (no panic). The
  other limit fields parse unchanged (no regression to `parseLimits`).

### TC-006-02: a capped writer truncates at the byte ceiling and drops the rest

- **Requirement:** REQ-006-02
- **Type:** unit (no sandbox — exercise the capping writer directly)
- **Input:** a capping writer with cap 10; write 25 bytes in one or several `Write` calls.
- **Expected:** the captured content is exactly the first 10 bytes; subsequent writes are
  accepted (no error returned to the child's pipe — the payload must not deadlock or get a write
  error that changes its exit code) but discarded; the writer reports it overflowed. Total
  retained ≤ cap regardless of chunking.
- **Edge cases:** writing exactly `cap` bytes does **not** flag truncation; writing `cap+1`
  does.

### TC-006-03: `output_truncated` records which streams were capped

- **Requirement:** REQ-006-03
- **Type:** unit (no sandbox)
- **Input:** the limits-report builder given (a) neither stream overflowed; (b) only stdout
  overflowed; (c) both overflowed.
- **Expected:** (a) `output_truncated == []`; (b) `["stdout"]`; (c) `["stdout","stderr"]`
  (deterministic order). The field appears under `sandbox_status.limits` alongside the existing
  `degraded` array.

### TC-006-04: output cap truncates real payload output (bwrap)

- **Requirement:** REQ-006-02, REQ-006-03, REQ-006-04
- **Type:** integration (bwrap; `requireBwrap`)
- **Input:** a run with `max_output_bytes: 1024` and a payload that writes ~1 MiB to stdout
  (e.g. `head -c 1048576 /dev/zero | tr '\0' a`) and a marker to stderr.
- **Expected:** `len(stdout) == 1024` (truncated); the run still completes with the payload's
  natural `exit_code` (the cap does not crash or hang the payload);
  `sandbox_status.limits.output_truncated` contains `"stdout"`.
- **Edge cases:** the payload does not block forever when the host stops retaining its output.

### TC-006-05: output cap preserves the no-network invariant (argv/spec unit)

- **Requirement:** REQ-006-05
- **Type:** unit (argv + OCI spec inspection; no sandbox)
- **Input:** build the bwrap argv and the OCI spec for a run carrying `max_output_bytes`.
- **Expected:** the bwrap argv is byte-for-byte the same as without the cap (the cap is a
  host-side capture concern, **not** an argv/spec concern) — still `--unshare-all`, no
  `--share-net`; the OCI `network` namespace stays path-less; the only socket bind is still
  `/proxy.sock`. The cap changes no backend wiring.

### TC-006-06: output cap truncates identically under gVisor

- **Requirement:** REQ-006-02, REQ-006-04
- **Type:** integration (runsc; `requireRunsc`)
- **Input:** the TC-006-04 run with `tier:"gvisor"`.
- **Expected:** `len(stdout) == 1024`, `output_truncated` contains `"stdout"`, identical to the
  bwrap result — proving the cap is tier-independent. `sandbox_status.tier == "gvisor"`.
- **Edge cases:** skips cleanly when runsc is absent.

### TC-006-07: no cap ⇒ full output, behavior unchanged (regression)

- **Requirement:** REQ-006-06
- **Type:** integration (bwrap) + unit
- **Input:** a run with **no** `max_output_bytes`; a payload that prints a known multi-KiB
  string.
- **Expected:** full output captured (length matches the emitted bytes), `output_truncated ==
  []`, `go test ./...` green; `run_test.go`/`gvisor_test.go`/`limits_test.go` unmodified and
  passing.

---

## Post-implementation verification

- [ ] Unit TCs pass everywhere (TC-006-01, TC-006-02, TC-006-03, TC-006-05)
- [ ] bwrap integration TCs pass on a box with bwrap (TC-006-04, TC-006-07)
- [ ] gVisor integration TC passes on a box with runsc (TC-006-06), skips cleanly otherwise
- [ ] L5/L6: a >1 MiB payload truncated to the cap observed under bwrap **and** runsc
- [ ] No regressions in existing tests (TC-006-07)

## Test framework notes

- Standard Go `testing`. Reuse `requireBwrap` / `requireRunsc` and the limits-request helper from
  `limits_test.go`. New tests live in a new file (`output_caps_test.go`); existing test files are
  not modified beyond any mechanical `parseLimits`/report-builder signature touch.
- The capping writer must be safe under the goroutine `cmd.Run` uses to copy child output and
  must never return an error that would change the payload's exit code (the cap is a host memory
  guard, not a payload signal).
