# Test Coverage Tracker

**Project:** exec-sandbox

## Rules

- Test specs are written **before** implementation begins — no exceptions
- A task is **not** "complete" because the feat commit landed and tests passed. See the verification ladder below.
- Each row maps a task ID to its spec file, current test status, and the verification level achieved

## Coverage

| Task ID | Feature | Spec file | Tests written | Status | Verified by |
|---------|---------|-----------|---------------|--------|-------------|
| 001 | gVisor (runsc) Tier-2 backend behind the `tier` seam | [`001-gvisor-tier2-backend-test-spec.md`](001-gvisor-tier2-backend-test-spec.md) | TC-001..007 written + passing | ✅ verified | L6 (spec-verifier APPROVE): `go test -count=1 ./...` 8/8 pass; `TestGvisorRunReachesAllowlistedHostAndBlocksOthers` ran (not skipped) under runsc — `allow=200`, `block=403`, direct net `FAILED-no-network`, `tier=gvisor`. Verifier confirmed no-network OCI netns, proxy-only egress, and `runsc --host-uds=open` connect-only confinement against authoritative flag semantics. |
| 002 | Enforce `profile.limits` (cpu/mem/pids/disk/timeout) on bwrap + gVisor | [`002-enforce-profile-limits-test-spec.md`](002-enforce-profile-limits-test-spec.md) | TC-001..011 written + passing | ✅ verified | L6 (spec-verifier APPROVE): `go test -count=1 ./...` 17 PASS / 0 SKIP on a host with bwrap+runsc+taskset+prlimit — every cap proven behaviorally: memory 256MB alloc OOM'd under 64MB (RLIMIT_AS); fork bomb hit "Cannot fork" under pids=20 (RLIMIT_NPROC); 4MB write to a 1MB `/tmp` hit ENOSPC; `sleep 30` killed in ≈1s with `status="timeout"`; in-box `nproc==1` under cpu_count=1; `diskQuotaSupported=false` ⇒ run still succeeds with a stderr WARNING + `degraded:[disk_mb]`; gVisor enforced mem/pids/disk via OCI `process.rlimits`+tmpfs `size=`. `run_test.go`/`gvisor_test.go` unmodified & green; `gofmt -l .` clean. No "not yet enforced" caveat remains for limits in docs/spec or CONTRACT. |
| 003 | Writable working-directory mount (`run.workdir` → `/work` rw, cwd=/work) on bwrap + gVisor | [`003-writable-workdir-mount-test-spec.md`](003-writable-workdir-mount-test-spec.md) | TC-001..010 written + passing | ✅ verified | L6 (spec-verifier APPROVE): `go test -count=1 ./...` 26 PASS / 0 SKIP / 0 FAIL on a bwrap+runsc host — workdir behaviorally proven on BOTH tiers: host-seeded `/work/seed.txt` read back, payload's `/work/out.txt` write persisted to the host dir, `pwd==/work`; `TestWorkdirEndToEnd_Gvisor` ran (not skipped). Writability proven negatively: `/work` is `--bind`/non-`ro` (not `--ro-bind`), a `/usr` write hit read-only, `--unshare-all` kept, no `--share-net`, OCI netns path-less. Bad path → `{error}` before proxy/vault (no side effect); absent workdir → no `/work`, prior behavior intact. `run_test.go` unmodified vs ab03804; `gofmt -l .` clean. |
| 004 | `FileRead{paths}` read-only host mounts + payload PATH/env provisioning (bwrap + gVisor) | [`004-toolchain-mount-and-path-test-spec.md`](004-toolchain-mount-and-path-test-spec.md) | TC-001..011 written + passing | 🟡 code merged | L5: `go test -count=1 ./...` → `ok github.com/tkdtaylor/exec-sandbox` 37 PASS / 0 SKIP / 0 FAIL on a bwrap+runsc host. FileRead behaviorally proven on BOTH tiers: a host marker tool mounted read-only is read+executed and resolves via `command -v` on `run.env["PATH"]` (`TestFileReadOnPathResolves_Bwrap`, `TestFileReadEndToEnd_Gvisor` ran — not skipped). Read-only proven negatively: a write under the FileRead mount fails and the host `evil.txt` is never created while `/work` write persists (`TestFileReadMountIsReadOnly_Bwrap`); argv uses `--ro-bind` not `--bind`, OCI options contain `"ro"`, `--unshare-all` kept, no `--share-net`, netns path-less. Bad path (relative/nonexistent) → `{error}` before proxy/vault. Empty FileRead/env ⇒ base argv/spec byte-for-byte unchanged. ADR 005 written; `gofmt -l .` clean; `run_test.go`/`gvisor_test.go` unmodified. Awaiting spec-verifier APPROVE before ✅. |

## Status key

| Symbol | Meaning |
|--------|---------|
| ✅ | **Verified** — validation harness exercised the live runtime path, or operator observed the targeted behaviour |
| 🟡 | **Code merged** — feat-commit landed, unit tests + fitness + CI green, but runtime/live behaviour not yet observed |
| ⏳ | In progress |
| ❌ | Not started |
| ⚠️ | Blocked |

## Verification ladder

A task earns 🟡 at levels 1–4 and ✅ only at level 5 or 6. The `Verified by` column records which level the row reached.

| Level | Evidence | Status this earns |
|-------|----------|-------------------|
| 1 | Code merged | 🟡 |
| 2 | Unit tests pass (paste verbatim final line of `make check`) | 🟡 |
| 3 | `make fitness` passes (verbatim closing line) | 🟡 |
| 4 | CI passes (`gh run watch <id> --exit-status` → success) | 🟡 |
| 5 | **Validation harness** exercises the live runtime path end-to-end — paste the command and the final assertion line | ✅ |
| 6 | **Operator-observed** — operator (or executor via `cargo run` / `npm start` / etc.) saw the targeted behaviour in stdout / logs / UI | ✅ |

If the task targets runtime-observable behaviour (logging, CLI args, TUI, server endpoints, file outputs, side effects), level 5 or 6 is **required** before flipping to ✅. If the task only adds an internal helper covered by unit tests, level 2 may be sufficient — but in that case the row's `Verified by` should explicitly say "unit-test-only; no runtime surface" so future readers don't mistake silence for verification.

## Rule

**The task-executor commits at 🟡 by default.** Only the main session (after spec-verifier APPROVE + the appropriate level-5/6 evidence) updates the row to ✅, in a separate commit titled `verify: confirm task NNN — <level-5/6 evidence>`. This keeps the verification step visible in git history and prevents "merged ≠ done" drift.
