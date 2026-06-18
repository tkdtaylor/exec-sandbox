# ADR 003: profile.limits enforcement — POSIX rlimits + tmpfs sizing + host-side kill, per backend

**Date:** 2026-06-18
**Status:** Accepted
**Task:** 002 (enforce `profile.limits` in `run()`)
**Related:** ADR 001 (foundational stack: stdlib-only, unprivileged bwrap), ADR 002 (gVisor Tier-2
backend). Adapts agent-builder ADR 027 (disk-quota graceful degrade) and ADR 028 (runtime-aware
in-box cap verification).

## Context

`profile.limits = { cpu_count, memory_mb, disk_mb, timeout_sec }` is part of the v1 contract
(`docs/CONTRACT.md`) but was **accepted and ignored** by v0 — `data-model.md` and
`configuration.md` both carried a "not yet enforced (TODO)" caveat. A sandbox that silently drops
its resource caps is an anti-DoS hole: agent-generated code could fork-bomb, exhaust memory, fill
the writable layer, or spin forever, and exec-sandbox would neither bound nor report it.

A complete reference implementation exists in the sibling consumer repo **agent-builder**
(`containment/execution-box/run.sh` + `internal/sandbox/podman/run.go`). That launcher targets
**rootless Podman**, so it maps caps to Podman/OCI flags: `--cpus`, `--memory`, `--pids-limit`,
`--storage-opt size=` (writable-layer quota), plus a host-side `context.WithTimeout` for
wall-clock and a `podman inspect` of `NanoCpus/Memory/PidsLimit/ShmSize` to verify enforcement.

exec-sandbox does **not** use Podman. Its backends are **bubblewrap** (Tier-1, unprivileged) and
**gVisor `runsc`** (Tier-2, run directly as `runsc run`, also unprivileged). The agent-builder
flags do not exist on these paths, so the mechanism must be **adapted, not blind-copied**. Three
facts from this host (rootless, cgroup-v2) shaped the decision, each reproduced live before
choosing:

1. **Unprivileged cgroup-v2 delegation is not available.** A child cgroup can be `mkdir`'d under
   our scope, but `cgroup.subtree_control` is empty and `cpu`/`memory`/`pids` controller files are
   not writable (`permission denied`). So exec-sandbox **cannot** create-and-populate a cgroup the
   way Podman (with a systemd scope) does. Any design that writes `memory.max`/`pids.max`
   ourselves fails on the common operator host.
2. **`runsc --ignore-cgroups` ignores `linux.resources`.** The gVisor backend must pass
   `--ignore-cgroups` to run unprivileged (it has no cgroup write access — see fact 1). Under that
   flag, a `linux.resources.memory.limit` of 64 MiB did **not** stop a 300 MiB allocation. So the
   OCI `linux.resources` block — the natural "cgroup limits on the OCI/runsc path" — is a **no-op**
   on the unprivileged gVisor path. (Kept in the spec as a secondary signal/host-record, but it is
   not the enforcer.)
3. **`RLIMIT_NPROC` set on the bwrap *parent* is counted system-wide per-UID and breaks bwrap.**
   `prlimit --nproc=25 -- bwrap …` failed at *bwrap's own* `clone()` with "Resource temporarily
   unavailable" because RLIMIT_NPROC counts every process the real UID owns, not just the sandbox.
   The cap must be applied **inside** the sandbox, after bwrap establishes its user namespace.

What **does** work, unprivileged, reproduced live on both backends:

- **POSIX rlimits applied in-sandbox.** Under bwrap, `prlimit --as --nproc -- sh payload.sh` caps
  memory (RLIMIT_AS) and pids (RLIMIT_NPROC) *per-sandbox* — the bwrap user namespace gives a fresh
  `user_struct`, so the NPROC count starts at ~1 and the fork bomb hits the cap ("Cannot fork").
  Under gVisor, the OCI **`process.rlimits`** block is honored by the sentry directly: RLIMIT_AS
  OOM'd a 300 MiB alloc at a 64 MiB cap and RLIMIT_NPROC stopped a fork bomb — **without**
  `linux.resources` and **with** `--ignore-cgroups`.
- **tmpfs size cap for the writable layer.** bwrap `--size <bytes> --tmpfs /tmp` returned
  "No space left on device" on a write past the cap; the OCI `/tmp` tmpfs mount takes the same
  `size=` option. The writable layer is always a tmpfs (rootfs is read-only), so this never depends
  on an XFS-backed overlay the way agent-builder's `--storage-opt size=` does.
- **CPU-affinity for core count.** `taskset -c 0-(N-1)` on the spawned process is inherited into
  the sandbox: under bwrap, in-box `nproc` reported `1` and `Cpus_allowed_list: 0` for `-c 0`. It
  pins the sandbox (and, for gVisor, the sentry threads) to N cores.
- **Host-side wall-clock kill.** `exec.CommandContext` + `context.WithTimeout`, with the child in
  its own process group (`Setpgid`) so the whole tree is killed on the deadline — identical to
  agent-builder's Go reference, and backend-agnostic.

## Decision

Enforce every `profile.limits` field on **both** backends using mechanisms that work unprivileged
and are kernel/sentry-enforced, not cgroup-delegation-dependent:

| Cap | Bubblewrap (Tier-1) | gVisor `runsc` (Tier-2) |
|-----|---------------------|--------------------------|
| `timeout_sec` | host-side `context.WithTimeout` + process-group `SIGKILL` (in `Run()`) | identical — host-side, backend-agnostic |
| `memory_mb` | in-sandbox `prlimit --as=<bytes>` (RLIMIT_AS) | OCI `process.rlimits` `RLIMIT_AS` (sentry-enforced) |
| `pids` | in-sandbox `prlimit --nproc=<n>` (RLIMIT_NPROC, per-sandbox via userns) | OCI `process.rlimits` `RLIMIT_NPROC` |
| `cpu_count` | `taskset -c 0-(N-1)` wrapping the bwrap argv | `taskset -c 0-(N-1)` wrapping the `runsc` argv |
| `disk_mb` | bwrap `--size <bytes> --tmpfs /tmp` | OCI `/tmp` tmpfs mount `size=<bytes>` option |

An unset (zero/absent) field means "no limit" — the cap is simply not applied. Limits are a
closed default: nothing in the request can *widen* the sandbox beyond the host's resources.

### Memory is RLIMIT_AS (address space), stated explicitly

`memory_mb` maps to `RLIMIT_AS` — the virtual address-space ceiling — because it is the only
unprivileged, runtime-portable memory knob available on both backends without cgroup write access.
This is a coarser bound than a cgroup `memory.max` RSS limit: a process that reserves large virtual
mappings without faulting them in can be killed earlier than its RSS would warrant. We accept this:
it is a conservative anti-DoS bound, it is enforced by the kernel (bwrap) and the sentry (gVisor),
and it is the same trade-off any `ulimit -v`-based sandbox makes. If a future privileged/delegated
deployment wants RSS semantics, it can add a cgroup `memory.max` behind the same `Limits` struct
without changing the contract.

### Graceful degradation (adapts agent-builder ADR 027)

`cpu_count` and `disk_mb` depend on host affordances (`taskset` on `PATH`; the writable-layer being
size-cappable). When the affordance is **absent**, exec-sandbox **omits that one cap, emits a
`WARNING` on stderr naming the degraded control, and continues** — it never hard-fails a run over a
*secondary* anti-DoS control. The load-bearing controls (no-network, proxy-only egress, memory/pids
rlimits, wall-clock kill) are never silently dropped; a failure to apply *them* is a real error.
This is exactly ADR 027's stance: a secondary, reversible control degrades loudly rather than
blocking the box. Disk-quota enforceability is resolved through an **overridable detection function**
(`diskQuotaSupported`), the exec-sandbox analogue of agent-builder's `EXEC_BOX_STORAGE_QUOTA_SUPPORTED`
env seam — a function variable rather than an env var, because exec-sandbox reads **no** application
environment variables (ADR 001 / `configuration.md`). Tests flip it to exercise the degrade path.

### Verification is runtime-aware (adapts agent-builder ADR 028)

agent-builder verifies caps via `podman inspect` (host-side) because gVisor does not surface
cpu/pids in-box. exec-sandbox runs `runsc` directly — there is no `podman inspect` — so the
**authoritative host-side record is the argv and the OCI `config.json` exec-sandbox itself
constructs**:

- **memory / pids / disk** are enforced by the kernel (bwrap) or sentry (gVisor) and are
  **behaviorally observable in-box on both backends** — a process that exceeds the cap dies. The
  integration tests assert the behavior (OOM, "Cannot fork", ENOSPC), which is the strongest proof.
- **cpu_count** is applied via `taskset` affinity. Under bwrap it is visible in-box
  (`nproc`/`Cpus_allowed_list`). Under gVisor the in-box cpu view is virtualized and unreliable
  (the ADR 028 situation), so cpu_count is verified **host-side**: a unit test asserts the
  constructed argv carries the `taskset -c 0-(N-1)` prefix. No cap goes unchecked; the unreliable
  in-box *signal* is deferred to the authoritative host-side *record*.

The applied/degraded caps are also reported in `sandbox_status.limits` (additive field) so a
consumer and the audit trail can see exactly which caps were enforced and which degraded.

## Options considered

### Option A — POSIX rlimits + tmpfs size + affinity + host-kill, per backend (chosen)

- **Pros:** works unprivileged on both backends (reproduced live); no cgroup-delegation dependency;
  kernel/sentry-enforced; behaviorally testable on the dev box (bwrap + runsc both present); honors
  the stdlib-only and no-env-var invariants; degrades a secondary control gracefully per ADR 027.
- **Cons:** memory is RLIMIT_AS (address space, coarser than RSS — stated above); cpu_count is an
  affinity bound, not a cgroup `cpu.max` quota (pins cores rather than throttling bandwidth);
  gVisor cpu_count is verified host-side, not in-box (ADR 028 trade-off); requires `prlimit`/
  `taskset` on the host (degraded if absent).

### Option B — OCI `linux.resources` (cgroup limits) on the gVisor path, as the task's literal wording suggests

- **Pros:** matches "cpu/mem/pids map to cgroup limits on the OCI/runsc paths" verbatim; would be
  the right answer for a *privileged* or cgroup-delegated deployment.
- **Cons:** **does not enforce** under the unprivileged `runsc --ignore-cgroups` path this repo runs
  (reproduced: 64 MiB `memory.limit` did not stop a 300 MiB alloc). Adopting it alone would
  re-create the exact "accepted but ignored" bug this task exists to close. **Rejected as the
  enforcer**; `linux.resources` is still emitted as a secondary host-record so a future
  cgroup-capable deployment enforces automatically.

### Option C — exec-sandbox creates and populates a cgroup itself

- **Pros:** true `memory.max`/`pids.max`/`cpu.max` RSS-accurate enforcement.
- **Cons:** requires cgroup-v2 delegation, which is **absent** on the common operator host
  (reproduced: empty `subtree_control`, controller files not writable). Would hard-fail or silently
  no-op on most dev machines — the ADR 027 anti-pattern. Deferred until a delegated/privileged
  deployment profile exists.

## Consequences

**What becomes true.** `profile.limits` is enforced on every tier the repo supports. A
fork-bombing, memory-hungry, disk-filling, or non-terminating payload is bounded and the run is
reported (`sandbox_status.status` becomes `"timeout"` on a wall-clock kill; `sandbox_status.limits`
records the applied/degraded caps). This is the prerequisite that lets agent-builder retire its own
in-repo `run.sh` launcher and adopt exec-sandbox's `run()` binary.

**Trade-offs, stated.** Memory is an address-space (RLIMIT_AS) bound, not RSS. cpu_count is a core
affinity bound, not a cgroup bandwidth quota. cpu_count under gVisor is verified host-side
(argv-record), not in-box. `cpu_count`/`disk_mb` degrade with a stderr `WARNING` when `taskset`/the
size-cappable writable layer is unavailable — a secondary control degrading loudly, never silently.

**Reopening condition.** If exec-sandbox gains a privileged or cgroup-delegated deployment profile,
add cgroup `memory.max`/`pids.max`/`cpu.max` behind the same `Limits` struct (Option C) for RSS and
bandwidth semantics, and tighten gVisor cpu verification to in-box where a future runsc exposes it
as a stable contract (the ADR 028 reopening condition).

**Spec updates land with the code in task 002.** `docs/CONTRACT.md`, `docs/spec/data-model.md`, and
`docs/spec/configuration.md` have their "not yet enforced (TODO)" caveats **rewritten in place** to
state the enforced behavior; `docs/spec/behaviors.md` gains the limits-enforcement behavior; and
`docs/spec/fitness-functions.md` gains a row (F-005) naming the invariant, its check command, and
the test that asserts it.
