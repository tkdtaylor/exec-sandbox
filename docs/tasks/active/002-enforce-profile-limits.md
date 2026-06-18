# Task 002: enforce `profile.limits` in `run()`

**Status:** 🟡 in progress
**Branch:** `task/002-enforce-profile-limits`
**Spec:** [`docs/tasks/test-specs/002-enforce-profile-limits-test-spec.md`](../test-specs/002-enforce-profile-limits-test-spec.md)
**ADR:** [`docs/architecture/decisions/003-profile-limits-enforcement.md`](../../architecture/decisions/003-profile-limits-enforcement.md)

## Problem

`profile.limits = { cpu_count, memory_mb, disk_mb, timeout_sec }` is in the v1 contract but v0
**accepts the fields and ignores them** (the "not yet enforced (TODO)" caveat in
`docs/spec/data-model.md` and `docs/spec/configuration.md`). A sandbox that drops its resource caps
is an anti-DoS hole. This task closes the gap: `run()` must **enforce** each cap on every tier the
repo supports (bubblewrap + gVisor) and **prove** enforcement with passing tests.

Reference: agent-builder's launcher (`containment/execution-box/run.sh`,
`internal/sandbox/podman/run.go`) — the mechanism is **adapted** to exec-sandbox's non-Podman
backends per ADR 003 (POSIX rlimits + tmpfs sizing + affinity + host-side kill), not blind-copied.

## Scope

- Parse `profile.limits` into a typed `Limits` (`parseLimits`).
- Enforce `timeout_sec` host-side in `Run()` (`context.WithTimeout` + process-group kill; `status`
  becomes `"timeout"`).
- Enforce `memory_mb` (RLIMIT_AS), `pids` (RLIMIT_NPROC), `disk_mb` (tmpfs size), `cpu_count`
  (`taskset` affinity) per backend (bwrap: in-sandbox `prlimit` + `--size` tmpfs + outer `taskset`;
  gVisor: OCI `process.rlimits` + tmpfs `size=` + outer `taskset`).
- Degrade `cpu_count`/`disk_mb` gracefully (warn + continue) when the host can't enforce them
  (ADR 027); never silently drop a load-bearing cap.
- Report applied/degraded caps in an additive `sandbox_status.limits`.
- Update `docs/CONTRACT.md`, `docs/spec/{data-model,configuration,behaviors,fitness-functions}.md`
  in the same commit (remove the "not enforced" caveat; state the enforced behavior; add F-005).

Out of scope: cgroup-delegated/privileged RSS+bandwidth enforcement (Option C, deferred);
`FileRead` capability enforcement; attestation signing.

## Verification plan

- **Highest level achievable: L5/L6.** This host has **both** `bwrap` and `runsc` installed, plus
  `taskset` and `prlimit`, so real enforcement is observable end-to-end (not just argv inspection).
- **Harness command:** `go test -count=1 ./...`
- **Runtime observation (L6):** the integration tests drive real sandboxes and assert the cap
  fires — memory alloc OOMs, fork bomb prints "Cannot fork", a 4 MB write to a 1 MB `/tmp` hits
  ENOSPC, a `sleep 30` under a 1 s timeout is killed in ≈1 s with `status == "timeout"`, and an
  unprivileged `runsc` run enforces the same via `process.rlimits`. The degrade path is observed by
  forcing `diskQuotaSupported=false` and confirming the run still succeeds with a stderr WARNING.
- **Fitness (L3):** new F-005 row asserts the limits-enforcement invariant; check command is the
  limits test set.

## Definition of done

- `profile.limits` enforced on bubblewrap **and** gVisor; each cap has a passing test that proves
  enforcement; the non-enforcing disk-quota degrade path is tested.
- Spec no longer says "not enforced"; CONTRACT/data-model/configuration/behaviors rewritten in place.
- F-005 fitness row added (invariant + check command + asserting test).
- spec-verifier APPROVE before promotion to ✅.
