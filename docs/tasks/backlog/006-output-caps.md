# Task 006: per-run stdout/stderr output caps (complete per-run resource bounding)

**Status:** ⬜ backlog
**Branch:** `task/006-output-caps`
**Spec:** [`docs/tasks/test-specs/006-output-caps-test-spec.md`](../test-specs/006-output-caps-test-spec.md)
**ADR:** ADR 007 (to be written during implementation — see Verification plan)

## Problem

The roadmap (`docs/architecture/prior-art.md`, "Net-new candidates" #2) calls for **per-run
resource bounding enforcement**: execution timeout, CPU/memory quotas, **and output caps**.
policy-engine *decides* obligations and explicitly does **not** enforce workload budgets; armor
and memory-guard disclaim it too — so enforcement is exec-sandbox's to own.

Task 002 already enforced the first half: `profile.limits` `{cpu_count, memory_mb, pids,
disk_mb, timeout_sec}` on both wired tiers (bwrap + gVisor). **The remaining gap is the output
cap.** `Run()` captures the payload's stdout/stderr into two **unbounded** `bytes.Buffer`s
(`run.go:147-150`). A chatty or adversarial payload that writes gigabytes to stdout can exhaust
the host's memory — an anti-DoS hole on the *host* side, distinct from the in-sandbox rlimits
task 002 added. This task closes it with a byte ceiling on captured output, enforced **identically
across both tiers** because the capture happens host-side, above the `tier` seam.

This task **extends** task 002's `Limits` machinery; it does not duplicate it. Out of scope are
all the caps task 002 already covers.

## Scope

- **Add `max_output_bytes` to the limits shape.** Extend `parseLimits` (`limits.go`) to read
  `profile.limits.max_output_bytes` into `Limits.MaxOutputBytes`. Missing/zero/non-positive ⇒
  "no cap" (current unbounded behavior). All other limit fields parse unchanged.
- **Enforce the cap in the host capture path** (`run.go`, around `run.go:147-150`): replace the
  bare `bytes.Buffer` stdout/stderr targets with a capping writer that retains at most
  `MaxOutputBytes` per stream and **discards** the overflow (it must keep accepting writes so the
  payload never blocks or gets a write error that would change its exit code — the cap is a host
  memory guard, not a payload signal). stdout and stderr are capped **independently** at the same
  ceiling.
- **Make truncation observable, not silent.** Add `sandbox_status.limits.output_truncated`: a
  deterministic-order list of the streams that were capped (`[]`, `["stdout"]`, or
  `["stdout","stderr"]`). Mirror the existing `degraded` array in `limitsReport`.
- **Tier-independence is the point.** The cap lives in `Run()`'s capture path, **above** the
  backend seam — the bwrap argv and the OCI spec are **unchanged** by this task. A payload
  emitting N≫cap bytes yields the same truncated length and same `output_truncated` record under
  bwrap and gVisor.
- **Preserve the invariant.** This task touches only host-side capture; it adds no network, no
  `--share-net`, no new mount, no egress. The proxy socket remains the only path out.
- **Spec + contract update in the same commit:** `docs/CONTRACT.md`,
  `docs/spec/data-model.md` (add `max_output_bytes` to the `profile.limits` block and
  `output_truncated` to the result's `sandbox_status.limits`), `docs/spec/configuration.md` (add
  the `max_output_bytes` row to the limits table), `docs/spec/behaviors.md` (note output capping
  in the run flow), `docs/spec/fitness-functions.md` (extend F-005, or add F-008, to assert the
  output cap is enforced and tier-independent — settle which in ADR 007). Rewrite in place.

Out of scope: caps task 002 already enforces (cpu/mem/pids/disk/timeout); persisting truncated
output to disk/audit (the result returns truncated output; full output is not retained); rate
limiting / bytes-per-second (this is a total-bytes ceiling, not a throughput limit).

## Verification plan

- **Highest level achievable: L5/L6.** This host has **both** `bwrap` and `runsc` (the tier
  task 002/003/004 reached), so a real >1 MiB payload truncated to the cap is observable
  end-to-end on both tiers.
- **Harness command:** `go test -count=1 ./...`
- **Runtime observation (L6):** a payload writing ~1 MiB to stdout under `max_output_bytes:1024`
  yields `len(stdout)==1024` with `output_truncated:["stdout"]`, the payload still exits with its
  natural code (no hang, no crash), and the **identical** result is produced under both bwrap and
  runsc. A run with no cap captures full output with `output_truncated:[]`. The capping writer is
  unit-tested for exact-cap (no flag) vs cap+1 (flag) and for chunked writes.
- **Fitness (L3):** F-005 extended (or F-008 added) asserting the output cap is enforced
  host-side and tier-independent; check command is the output-cap test set.
- **ADR 007 written during implementation:** records that the output cap is enforced **above**
  the `tier` seam (host capture path) deliberately — so it is identical across backends and does
  not touch any argv/OCI wiring — and that overflow is **dropped** (not buffered, not errored to
  the payload). Records the `output_truncated` observability decision and whether the fitness rule
  is a new F-008 or an extension of F-005.

## Definition of done

- `profile.limits.max_output_bytes` parsed and enforced host-side; stdout/stderr each truncated
  independently at the ceiling; overflow dropped without affecting the payload's exit.
- `sandbox_status.limits.output_truncated` records the capped streams; uncapped run records `[]`.
- The cap is proven tier-independent — same truncated length + record under bwrap and gVisor.
- The bwrap argv and OCI spec are unchanged (the cap is host-side); the no-network +
  proxy-only-egress invariant is intact.
- Backward compatible: no `max_output_bytes` ⇒ full output, prior behavior byte-for-byte;
  existing tests green.
- Spec + CONTRACT updated in place; F-005 extended or F-008 added; **ADR 007** written.
- spec-verifier APPROVE before promotion to ✅.
