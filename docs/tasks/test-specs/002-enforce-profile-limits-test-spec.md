# Test Spec 002: enforce `profile.limits` in `run()`

**Linked task:** [`docs/tasks/active/002-enforce-profile-limits.md`](../active/002-enforce-profile-limits.md)
**ADR:** [`docs/architecture/decisions/003-profile-limits-enforcement.md`](../../architecture/decisions/003-profile-limits-enforcement.md)
**Written:** 2026-06-18

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-001 | `profile.limits` is parsed into a typed `Limits` (cpu_count, memory_mb, pids, disk_mb, timeout_sec); absent/zero ⇒ unset | TC-001 | ⏳ |
| REQ-002 | `timeout_sec` terminates an over-running payload (host-side kill); `status` reports `"timeout"` | TC-002 | ⏳ |
| REQ-003 | `memory_mb` kills a payload that exceeds it (RLIMIT_AS), on **both** backends | TC-003 (bwrap), TC-008 (gvisor) | ⏳ |
| REQ-004 | `pids` rejects a fork bomb (RLIMIT_NPROC), on **both** backends | TC-004 (bwrap), TC-008 (gvisor) | ⏳ |
| REQ-005 | `disk_mb` blocks writes past the cap (tmpfs size), on **both** backends | TC-005 (bwrap), TC-008 (gvisor) | ⏳ |
| REQ-006 | `cpu_count` is applied as `taskset` affinity (in-box visible under bwrap; host-side argv-record under gvisor per ADR 028) | TC-006 (bwrap in-box), TC-009 (gvisor argv) | ⏳ |
| REQ-007 | A non-enforcing host (no `taskset` / disk quota unsupported) **warns + degrades**, run still succeeds | TC-007 | ⏳ |
| REQ-008 | The `run()` contract shape is preserved; `sandbox_status.limits` is additive; no limits ⇒ behaviour unchanged | TC-010, TC-011 | ⏳ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Edge cases and error paths are covered
- [x] Every REQ-ID has at least one test case
- [x] Success criteria are unambiguous
- [x] Enforcement mechanisms reproduced live before authoring (see ADR 003 Context)

---

## Test cases

### TC-001: `parseLimits` maps the contract fields and treats absent/zero as unset

- **Requirement:** REQ-001
- **Type:** unit (no sandbox; runs everywhere)
- **Input:** `profile["limits"]` maps:
  - full: `{cpu_count: 2, memory_mb: 64, pids: 32, disk_mb: 8, timeout_sec: 5}`
  - partial: `{memory_mb: 128}` (others absent)
  - empty / missing `limits` key
- **Expected:** `parseLimits` returns a `Limits` with `CPUCount=2, MemoryMB=64, PidsLimit=32, DiskMB=8, Timeout=5s` for the full case; only `MemoryMB=128` set (others zero/`Timeout=0`) for partial; all-zero for empty/missing. JSON numbers decoded from `float64` correctly.
- **Edge cases:** a non-numeric or negative value is treated as unset (≤0 ⇒ no limit); a missing `limits` key never panics.

### TC-002: `timeout_sec` terminates an over-running payload and reports `status == "timeout"`

- **Requirement:** REQ-002
- **Type:** integration (bwrap; `requireBwrap`)
- **Input:** payload `sleep 30`; `limits.timeout_sec = 1`.
- **Expected:** `Run` returns in ≈1 s (well under 30 s — assert `duration_ms < 10000`); the payload is killed (non-zero `exit_code`); `sandbox_status.status == "timeout"`. The whole process group is killed (no orphaned `sleep`).
- **Edge cases:** a payload that finishes **before** the timeout returns normally with `status == "clean"` and `exit_code == 0` (no false timeout).

### TC-003: `memory_mb` kills a payload that exceeds it under bwrap (RLIMIT_AS)

- **Requirement:** REQ-003
- **Type:** integration (bwrap)
- **Input:** `limits.memory_mb = 64`; payload allocates ~256 MB (`perl -e '"A" x (256*1024*1024)'`).
- **Expected:** the allocation fails (out-of-memory / non-zero from the allocator); the run does **not** succeed in allocating 256 MB. Assert the payload's allocation step reports failure (stderr contains an OOM/allocation-failure marker, or the alloc command exits non-zero).
- **Edge cases:** with `memory_mb` **unset**, the same allocation succeeds (proves the cap is what kills it, not the environment).

### TC-004: `pids` rejects a fork bomb under bwrap (RLIMIT_NPROC, per-sandbox)

- **Requirement:** REQ-004
- **Type:** integration (bwrap)
- **Input:** `limits.pids = 20`; payload spawns 80 background processes in a loop.
- **Expected:** the shell hits the cap and prints a fork-failure ("Cannot fork" / "Resource temporarily unavailable"); the run does not spawn 80 processes. The cap is **per-sandbox** (bwrap user namespace), so it fires regardless of how many processes the host user already has.
- **Edge cases:** `pids` unset ⇒ the loop completes without a fork failure.

### TC-005: `disk_mb` blocks writes past the cap under bwrap (tmpfs `--size`)

- **Requirement:** REQ-005
- **Type:** integration (bwrap)
- **Input:** `limits.disk_mb = 1`; payload writes a 4 MB file to `/tmp` (`dd … of=/tmp/big bs=1M count=4`).
- **Expected:** the write fails with "No space left on device"; `/tmp` is size-capped at 1 MB. The bwrap argv carries `--size <bytes> --tmpfs /tmp`.
- **Edge cases:** `disk_mb` unset ⇒ `/tmp` is an unsized tmpfs and the 4 MB write succeeds.

### TC-006: `cpu_count` applies `taskset` affinity, visible in-box under bwrap

- **Requirement:** REQ-006
- **Type:** integration (bwrap), gated on host having ≥2 cores
- **Input:** `limits.cpu_count = 1`; payload prints `nproc` (or reads `/proc/self/status` `Cpus_allowed_list`).
- **Expected:** in-box `nproc == 1` (the affinity pins the sandbox to one core), even though the host has more. The constructed argv begins with `taskset -c 0-0 bwrap …`.
- **Edge cases:** `cpu_count` unset ⇒ no `taskset` prefix; in-box `nproc` equals the host's.

### TC-007: degrade-gracefully — disk quota unsupported / `taskset` absent ⇒ warn + continue

- **Requirement:** REQ-007
- **Type:** integration (bwrap) using the overridable `diskQuotaSupported` seam (no env var)
- **Input:** `limits.disk_mb = 1` with `diskQuotaSupported` forced to `false`; a payload that writes 4 MB.
- **Expected:** the `--size` flag is **omitted**, a stderr `WARNING` naming the degraded disk-quota control is emitted, the run **still succeeds** (the 4 MB write is allowed because the cap was dropped), `exit_code == 0`, and `sandbox_status.limits.degraded` lists `disk_mb`. The run is **not** failed over a secondary control (ADR 027).
- **Edge cases:** an absent `taskset` similarly degrades `cpu_count` (warn + continue), not a hard error. Assert the degraded list and that the run still completes. The wall-clock and memory/pids (load-bearing) caps are **never** silently dropped — only `cpu_count`/`disk_mb` degrade.

### TC-008: gVisor enforces `memory_mb`, `pids`, and `disk_mb` via `process.rlimits` + tmpfs size

- **Requirement:** REQ-003, REQ-004, REQ-005 (gvisor path)
- **Type:** integration (runsc; `requireRunsc`)
- **Input:** a `tier: "gvisor"` run with `limits = {memory_mb: 64, pids: 40, disk_mb: 1}`; payload that (a) allocates 256 MB, (b) fork-bombs 80, (c) writes 4 MB to `/tmp`.
- **Expected:** under gVisor each cap fires — allocation OOMs, fork bomb hits "Cannot fork", disk write hits ENOSPC — proving the sentry honors `process.rlimits` and the tmpfs `size=` option. `sandbox_status.tier == "gvisor"` and the contract shape is unchanged.
- **Edge cases:** with all limits unset, the same payload runs unbounded (regression baseline). Skips cleanly when runsc is absent.

### TC-009: gVisor OCI spec carries the limits (host-side authoritative record, ADR 028)

- **Requirement:** REQ-006, REQ-003/004/005 (host-side record)
- **Type:** unit (no runsc; inspects the generated spec/argv)
- **Input:** `Limits{CPUCount:2, MemoryMB:64, PidsLimit:40, DiskMB:8}` applied to the gVisor OCI spec/argv.
- **Expected:** `process.rlimits` contains `RLIMIT_AS` (= 64 MiB) and `RLIMIT_NPROC` (= 40); the `/tmp` tmpfs mount options include `size=` (= 8 MiB); the constructed `runsc` argv is prefixed with `taskset -c 0-1`. This is the host-side record exec-sandbox verifies cpu_count by, since gVisor does not surface cpu/pids reliably in-box (ADR 028). Existing `gvisorOCISpec(scriptPath, proxySock)` 2-arg callers and `TestGvisorSpec*` tests remain green (the base spec is unchanged when no limits are applied).
- **Edge cases:** zero limits ⇒ no `process.rlimits` entries added, no `size=` option, no `taskset` prefix — base spec identical to today.

### TC-010: the `run()` contract shape is preserved; `sandbox_status.limits` is additive

- **Requirement:** REQ-008
- **Type:** integration (bwrap)
- **Input:** any successful run with limits set.
- **Expected:** the result keeps `{stdout, stderr, exit_code, sandbox_status:{sandbox_id, tier, duration_ms, secrets_injected, status}}`; a new **additive** `sandbox_status.limits` object reports the applied caps (`{cpu_count, memory_mb, pids, disk_mb, timeout_sec}`) and a `degraded` list. No existing field is removed or renamed.
- **Edge cases:** `secrets_injected` and the proxy flow are unchanged (limits are orthogonal to credential injection).

### TC-011: no-limits runs are byte-for-byte unchanged (regression guard)

- **Requirement:** REQ-008
- **Type:** integration (bwrap) — the existing, **unmodified** `run_test.go` suite
- **Expected:** `go build ./... && go test ./...` green; `TestSandboxReachesAllowlistedHostViaProxy`, `TestProxyBlocksNonAllowlistedHost`, `TestNetAllowlistParsing`, and all `gvisor_test.go` `TestGvisorSpec*`/`TestBackendFor*` tests pass unchanged. A request with no `limits` produces the same argv/spec as before this task (modulo the additive `sandbox_status.limits` reporting zeros).
- **Edge cases:** none — this is the regression guard. `run_test.go` is **not** edited.

---

## Post-implementation verification

- [ ] All unit TCs pass everywhere (TC-001, TC-009)
- [ ] All bwrap integration TCs pass on a box with bwrap (TC-002..007, TC-010, TC-011)
- [ ] gVisor integration TC passes on a box with runsc (TC-008), skips cleanly otherwise
- [ ] L5/L6: real enforcement observed — OOM, "Cannot fork", ENOSPC, timeout-kill — on this host (bwrap **and** runsc both present)
- [ ] No regressions in existing tests (TC-011)

## Test framework notes

- Standard Go `testing`. Reuse `requireBwrap` / `requireRunsc`; add a `≥2 cores` guard for the
  affinity test (`runtime.NumCPU() < 2 ⇒ t.Skip`).
- Enforcement assertions are **behavioral** (the payload observes the cap), which is stronger than
  inspecting flags. cpu_count under gVisor is the one exception verified by argv-record (ADR 028).
- The degrade path (TC-007) uses the package-level `diskQuotaSupported` function variable as the
  testability seam — **no environment variable** is introduced (honors the no-env-var invariant).
- New tests live in a new file (`limits_test.go`); `run_test.go` is not modified.
