# ADR 007: per-run stdout/stderr output caps (`profile.limits.max_output_bytes`)

**Date:** 2026-06-19
**Status:** Accepted
**Task:** 006 (per-run stdout/stderr output caps — completing per-run resource bounding)
**Related:** ADR 001 (foundational stack: no-network, proxy-only egress), ADR 002 (gVisor Tier-2
backend behind the `tier` seam), ADR 003 (profile.limits enforcement: cpu/mem/pids/disk/timeout),
ADR 004 (writable `/work` mount), ADR 005 (FileRead read-only mounts + env provisioning).

## Context

The roadmap (`docs/architecture/prior-art.md`, "Net-new candidates" #2) calls for **per-run
resource bounding**: execution timeout, CPU/memory quotas, **and output caps**. policy-engine
*decides* obligations and explicitly does **not** enforce workload budgets; armor and memory-guard
disclaim it too — so enforcement is exec-sandbox's to own.

ADR 003 (task 002) enforced the first half — `profile.limits` `{cpu_count, memory_mb, pids,
disk_mb, timeout_sec}` on both wired tiers. The remaining gap is the **output cap**. `Run()`
captures the payload's stdout/stderr into two **unbounded** `bytes.Buffer`s. A chatty or
adversarial payload that writes gigabytes to stdout can exhaust the *host's* memory — an anti-DoS
hole on the host side, distinct from the in-sandbox `RLIMIT_AS` (`memory_mb`) which bounds the
*payload's* address space, not the host buffer that retains its output.

The in-sandbox memory cap does not close this hole: a payload streaming output uses negligible
in-sandbox memory while the host accumulates every byte. The cap therefore has to live where the
host *retains* the bytes — `Run()`'s capture path.

## Decision

### 1. The cap is enforced **above** the `tier` seam (host capture path), not in any backend

The capture of the child's stdout/stderr happens in `Run()`, in the host process, identically for
every backend — `cmd.Stdout`/`cmd.Stderr` are set before `backendFor(tier)` is even consulted for
*how* to spawn. The output cap is enforced there, by wrapping the capture target in a **capping
writer** (`capWriter`).

Consequences, all deliberate:

- **The bwrap argv and the OCI spec are unchanged by this task.** `bwrapArgv` and `gvisorOCISpec`
  carry no new flag, no new mount, no new rlimit. The cap is not a sandbox-confinement concern; it
  is a host-memory-guard concern. (Asserted by TC-006-05.)
- **The cap is tier-independent for free.** A payload emitting N≫cap bytes yields the *same*
  truncated length and the *same* `output_truncated` record under bubblewrap and gVisor, because
  the same host-side `capWriter` sits on the pipe regardless of backend. (Asserted by TC-006-04 /
  TC-006-06.)
- **No new isolation surface.** This touches host-side capture only — no network, no `--share-net`,
  no new mount, no egress. The proxy socket remains the only path out. The no-network +
  proxy-only-egress invariant is untouched.

The alternative — pushing the cap into each backend (e.g. a per-tier output limiter) — was
rejected: it would duplicate the same logic per backend, drift between tiers, and entangle a
host-memory guard with sandbox confinement. The tier seam is for *isolation* backends; an
above-seam host concern stays above the seam.

### 2. Overflow is **dropped**, never buffered and never errored back to the payload

`capWriter` retains at most `MaxOutputBytes` bytes per stream and **discards** everything past the
ceiling. It keeps accepting `Write` calls — `Write` always reports the full byte count as written
and returns a nil error — so the child's pipe never sees a short write or an error. This matters:
if the writer returned an error or a short count, `os/exec`'s output-copy goroutine could surface a
broken pipe to the child, which could change the payload's exit code or deadlock it. The cap is a
**host memory guard, not a payload signal** — the payload runs to its natural completion and
exits with its own code; only the host's *retained* copy is truncated.

Rejected alternatives:

- **Buffer-then-truncate** (let the buffer grow, trim at the end): defeats the entire purpose —
  the unbounded growth is the DoS we are closing.
- **Kill the payload on overflow** (like `timeout_sec`): wrong semantics. Output volume is not a
  liveness fault; truncating the host's copy is the minimal, non-destructive response. The payload
  is not misbehaving by being verbose, and we do not want a verbose-but-correct payload to fail.
- **Error the pipe on overflow:** can perturb the payload's exit code / cause a deadlock (above).

`max_output_bytes` missing / zero / non-positive ⇒ **no cap** (unbounded — exactly today's
behavior). Backward compatible byte-for-byte: an uncapped run captures full output and records
`output_truncated: []`.

### 3. Truncation is **observable** via `sandbox_status.limits.output_truncated`

Silent truncation would be a correctness trap — a consumer parsing truncated JSON output would have
no way to know it was cut. So the result records which streams were capped:
`sandbox_status.limits.output_truncated`, a deterministic-order list mirroring the existing
`degraded` array in `limitsReport`:

- `[]` — neither stream overflowed (the uncapped/under-cap case),
- `["stdout"]` — only stdout overflowed,
- `["stdout","stderr"]` — both overflowed (stdout always listed first — deterministic order).

stdout and stderr are capped **independently** at the same ceiling: each gets its own `capWriter`
with the same `MaxOutputBytes`. One can overflow without the other.

Writing *exactly* `cap` bytes does **not** flag truncation; writing `cap+1` does — the flag means
"bytes were dropped," not "the cap was reached."

### 4. Fitness rule: **new F-008**, not an extension of F-005

F-005 asserts that every `profile.limits` cap is enforced **on every wired tier** — its defining
property is *per-tier* enforcement (memory/pids via rlimits, disk via tmpfs, cpu via taskset),
including the degrade path for secondary controls. The output cap's defining property is the
**opposite**: it is enforced **once, above the tier seam, identically** on every backend, with no
per-tier wiring and no degrade path (a tmpfs-less or taskset-less host changes nothing — the host
buffer cap always applies). Folding it into F-005 would blur "per-tier enforcement" with
"tier-independent host guard," which are different invariants with different check commands and
different failure modes.

So **F-008** is added: "the per-run output cap is enforced host-side and is tier-independent —
stdout/stderr each truncate at `max_output_bytes`, overflow is dropped without changing the
payload's exit, `output_truncated` records the capped streams, and the result is identical under
bwrap and gVisor; the bwrap argv and OCI spec are unchanged by the cap." Check command: the
output-cap test set (`go test -run 'OutputCap|CapWriter|MaxOutputBytes' ./...`).

## Consequences

- `Limits` gains `MaxOutputBytes int` (0 = no cap), parsed by `parseLimits` from
  `profile.limits.max_output_bytes` exactly like the other caps (missing/zero/non-positive ⇒
  unset).
- `Run()` wraps its `bytes.Buffer` stdout/stderr capture in `capWriter` when `MaxOutputBytes > 0`;
  `limitsReport` gains the `output_truncated` field built from the two writers' overflow flags.
- The bwrap argv and OCI spec are unchanged — proven by TC-006-05 (argv/spec byte-for-byte equal
  with and without the cap).
- Spec/contract updated in the same feat commit: `docs/CONTRACT.md`, `docs/spec/data-model.md`,
  `docs/spec/configuration.md`, `docs/spec/behaviors.md` (B-009 extended to name the output cap),
  `docs/spec/fitness-functions.md` (F-008 added).
- Out of scope (unchanged): the caps task 002 already enforces; persisting truncated output to
  disk/audit (full output is not retained — the result returns the truncated copy); rate limiting /
  bytes-per-second (this is a total-bytes ceiling, not a throughput limit).
