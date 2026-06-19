# Test Spec 008: snapshot/restore for clean-slate sandbox reuse

**Linked task:** [`docs/tasks/backlog/008-snapshot-restore.md`](../backlog/008-snapshot-restore.md)
**ADR:** ADR 009 (to be written during implementation — this task carries open design questions)
**Written:** 2026-06-19

## Context for the test author

Inspired by hyperlight's `snapshot()`/`restore()`. The goal is to **reset a sandbox to a
pristine baseline between invocations** instead of a full teardown + rebuild per run, to improve
per-call hygiene and throughput at scale. This is a **performance/hygiene** task, **not** a
security gap — and it must honor the "every run is ephemeral; no state leaks between runs"
non-goal. The hard correctness bar for any reuse mechanism is therefore: **a reused sandbox must
be byte-for-byte indistinguishable from a freshly built one** — no file, no env var, no leftover
process, no credential from a prior run survives a reset.

Scope is deliberately conservative. The open design questions (what exactly is the "baseline":
the temp dir + payload + proxy? a snapshot of the bwrap/gVisor root? does this require a
long-lived sandbox process, which the current one-shot `Run()` does not have?) are recorded in
the task file and ADR 009 — **not** pre-decided here. The first increment this spec pins down is
the **reset/baseline contract and its leak-proof property**, behind the existing one-shot
`run()` so the contract is unchanged and the no-network + proxy-only-egress invariant holds.

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-008-01 | A `snapshot` captures the pristine per-run baseline (the freshly-prepared sandbox state before the payload runs): the empty/seeded workdir state, the written `payload.sh`, the fresh proxy socket, and the cleared credential map — the exact components `Run()` builds today | TC-008-01 (unit) | ⏳ |
| REQ-008-02 | A `restore` returns the sandbox to the captured baseline: any file the payload wrote under the writable surface is gone, env is reset, and the credential map is cleared — the restored state equals the snapshot | TC-008-02 (unit), TC-008-04 (bwrap) | ⏳ |
| REQ-008-03 | **No state leaks across a restore (the load-bearing property):** a second run on a restored sandbox cannot read any file, env var, or credential written by the first run; a reused sandbox is indistinguishable from a freshly built one (the ephemeral non-goal is preserved, not weakened) | TC-008-03 (unit, diff), TC-008-04 (bwrap), TC-008-06 (credential leak) | ⏳ |
| REQ-008-04 | snapshot/restore **preserves the no-network + proxy-only-egress invariant**: a restored sandbox still has no network namespace (`--unshare-all` / path-less OCI netns) and its only egress is a fresh `/proxy.sock`; restore never re-binds a stale credential or a stale socket from a prior run | TC-008-05 (argv/spec + proxy-state unit) | ⏳ |
| REQ-008-05 | The `run()` contract is **unchanged**: snapshot/restore is an internal reuse mechanism. A one-shot `run()` with no reuse behaves byte-for-byte as today (snapshot+immediate-teardown is the degenerate, default path); the result schema gains nothing externally observable beyond an optional internal/telemetry marker settled in ADR 009 | TC-008-07 (regression) | ⏳ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Edge cases and error paths are covered
- [x] Every REQ-ID has at least one test case
- [x] Success criteria are unambiguous
- [x] Open design questions are recorded in the task file / ADR 009 rather than over-specified here

---

## Test cases

### TC-008-01: snapshot captures the pristine baseline

- **Requirement:** REQ-008-01
- **Type:** unit (no sandbox)
- **Input:** prepare a sandbox baseline (temp dir + `payload.sh` written + a fresh proxy with an
  empty credential map); take a snapshot.
- **Expected:** the snapshot records the baseline components — the workdir's pristine file set,
  the `payload.sh` contents, and an empty credential map. The snapshot is taken **before** any
  payload execution.
- **Edge cases:** snapshotting twice yields equal baselines (idempotent capture of a pristine
  state).

### TC-008-02: restore returns the sandbox to the captured baseline

- **Requirement:** REQ-008-02
- **Type:** unit (no sandbox)
- **Input:** snapshot a baseline; mutate the writable surface (create `scratch.txt`, set a
  credential, write an env marker); call restore.
- **Expected:** after restore, `scratch.txt` is gone, the credential map is empty, the env marker
  is cleared — the restored state is equal to the snapshot.
- **Edge cases:** restoring an already-pristine sandbox is a no-op (no error).

### TC-008-03: no file/env state leaks across a restore (diff)

- **Requirement:** REQ-008-03
- **Type:** unit (state diff, no sandbox)
- **Input:** snapshot; run "phase 1" mutations (write a secret-looking file + env var to the
  writable surface); restore; capture "phase 2" state.
- **Expected:** the phase-2 state is byte-for-byte equal to a freshly-built baseline — no
  phase-1 file or env survives. The assertion is a direct diff: `restored == fresh`.

### TC-008-04: a second real run on a restored sandbox cannot see the first run's files (bwrap)

- **Requirement:** REQ-008-02, REQ-008-03
- **Type:** integration (bwrap; `requireBwrap`)
- **Input:** run 1 writes `/work/leak.txt` (writable surface); restore the sandbox; run 2's
  payload attempts to read `/work/leak.txt`.
- **Expected:** run 2 cannot find `leak.txt` (it was removed by restore) — the writable surface
  is pristine. Both runs complete with `exit_code` reflecting their own payload; run 2 is
  indistinguishable from a fresh first run.
- **Edge cases:** if the conservative first increment does not reuse the *kernel* sandbox (only
  the host-side baseline), this test still asserts the file-level leak-proofing on the writable
  host surface; note the boundary in ADR 009.

### TC-008-05: restored sandbox keeps the no-network + proxy-only invariant (argv/spec + proxy unit)

- **Requirement:** REQ-008-04
- **Type:** unit (argv + OCI spec + proxy-state inspection)
- **Input:** a sandbox after restore; inspect its spawn argv / OCI spec and its proxy state.
- **Expected:** the argv still carries `--unshare-all` with no `--share-net`; the OCI `network`
  namespace is still path-less; the proxy's credential map is **empty** after restore (no stale
  credential re-bound) and its socket is a fresh per-run path (no stale socket reused). Restore
  never widens egress.

### TC-008-06: no credential leaks across a restore

- **Requirement:** REQ-008-03
- **Type:** unit (proxy-state)
- **Input:** load a credential for a host (simulating a proxy-mode injection in run 1); restore.
- **Expected:** after restore the proxy's credential map is empty (equivalent to `Wipe()` plus a
  pristine baseline) — a run-2 request to the same host carries no run-1 credential. This is the
  credential half of the ephemeral non-goal.

### TC-008-07: a one-shot run with no reuse is byte-for-byte unchanged (regression)

- **Requirement:** REQ-008-05
- **Type:** integration (bwrap) + unit
- **Input:** a normal one-shot `Run()` (no reuse requested).
- **Expected:** the result schema, the argv/spec, and the teardown behavior are unchanged from
  before this task; `go test ./...` green; `run_test.go`/`gvisor_test.go`/`limits_test.go`/
  `workdir_test.go`/`fileread_test.go` stay green. snapshot+immediate-teardown is the default
  degenerate path and is observationally identical to today's behavior.

---

## Post-implementation verification

- [ ] Unit TCs pass (TC-008-01, TC-008-02, TC-008-03, TC-008-05, TC-008-06)
- [ ] bwrap integration TCs pass on a box with bwrap (TC-008-04, TC-008-07)
- [ ] L5/L6: a restored sandbox observed to NOT expose a prior run's `/work/leak.txt` or
      credential, and to be indistinguishable from a fresh build
- [ ] No regressions in existing tests (TC-008-07)
- [ ] ADR 009 records the resolved scope of the first increment and the deferred open questions

## Test framework notes

- Standard Go `testing`. Reuse `requireBwrap`. New tests live in `snapshot_test.go`; existing
  test files are not modified beyond any mechanical signature touch.
- The leak-proof assertions (TC-008-03, TC-008-06) are the heart of this task — write them as
  direct `restored == fresh` equality / "credential map empty" checks so a regression is loud.
- If ADR 009 concludes the conservative first increment is "factor the baseline-build + reset out
  of `Run()` without a long-lived sandbox process," these tests target that factored unit; the
  long-lived-process variant (true throughput gain) is a follow-on noted in the task's deferred
  questions.
