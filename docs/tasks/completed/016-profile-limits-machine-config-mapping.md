# Task 016: profile.limits → machine-config mapping

**Status:** ⬜ backlog
**Branch:** `task/016-profile-limits-machine-config-mapping`
**Spec:** [`docs/tasks/test-specs/016-profile-limits-machine-config-mapping-test-spec.md`](../test-specs/016-profile-limits-machine-config-mapping-test-spec.md)
**ADR:** ADR 010 D4 (limits mapping: `profile.limits` → machine-config vCPU/mem; disk → drive sizing; pids → in-guest rlimit; timeout/output stay host-side). Applies the existing ADR 003 limits model to the new backend — no new ADR.

## Readiness

**READY once tasks 013 and 015 land** (it needs the config skeleton and a booted guest for the
behavioral L5 evidence). It has **no open-question block of its own** — Q1/Q3 are inherited via the
015 dependency (resolved before 015 can land), and this task adds no new question.

**Dependency position:** 013 → 014 → 015 → **{016, 017}** → 018. Siblings of 017; both depend on
013 + 015.

## Problem

`profile.limits` (ADR 003, `Limits` in `limits.go`) is enforced per-tier: bubblewrap uses
`prlimit` + tmpfs `--size` + `taskset`; gVisor uses OCI `process.rlimits` + tmpfs `size=` + `taskset`
(`gvisor.go:140-178`). The Firecracker backend has **no limits mapping** — task 013 generates the
base config structure but does not apply caps. Firecracker's machine config is a *better* fit for the
CPU/memory caps than the rlimit/tmpfs approximations (ADR 010 D4):

- `cpu_count` → `machine-config.vcpu_count` (a **real** vCPU cap, not a host-side `taskset` affinity
  hint — stronger than Tier-1/Tier-2).
- `memory_mb` → `machine-config.mem_size_mib` (the guest's RAM ceiling, vs `RLIMIT_AS`).
- `disk_mb` → the size of the writable drive presented to the guest.
- `pids` → an in-guest `RLIMIT_NPROC` applied by the guest-side launcher (the guest owns its pid
  space — analogous to `prlimitWrap`, `limits.go:88-106`).
- `timeout_sec` + `max_output_bytes` stay **host-side, above the seam, UNCHANGED** — `Run()` kills
  the process group on the deadline and caps each stream via `capWriter`. The firecracker config must
  be **byte-for-byte independent** of these two caps.

## Scope

- **Map `Limits` onto the machine-config** in `firecracker.go`: `cpu_count → vcpu_count`,
  `memory_mb → mem_size_mib`, `disk_mb →` writable-drive size, `pids →` an in-guest `RLIMIT_NPROC`
  applied by the guest launcher. Zero/absent fields leave the Firecracker defaults (the "zero = no
  cap" contract).
- **For the firecracker tier, `cpu_count` is the vCPU count, NOT a `taskset` prefix** — do not add the
  host-side affinity prefix this tier; record that it is a *stronger* enforcement, not a degrade.
- **`disk_mb` follows the existing degrade contract**: when the writable layer can't be sized
  (`diskQuotaSupported() == false`), return a `disk_mb` degrade (warn + continue), never a silent
  drop — mirror `applyLimitsToOCISpec` (`gvisor.go:155-163`).
- **Keep `timeout_sec` + `max_output_bytes` out of the config** — the config must be byte-for-byte
  identical whether or not those two caps are set (assert it).
- **Spec update in the same commit:** `docs/spec/configuration.md` (`run.profile.limits` per-tier
  enforcement table) + `docs/spec/behaviors.md` gain the Firecracker mapping (vcpu/mem/drive/in-guest
  NPROC; timeout/output host-side); note the cpu_count *real-cap* improvement over the namespace
  tiers.

Out of scope: the mount mechanism for the writable drive itself (task 017 owns `/work`/FileRead
presentation — this task sizes the writable drive, 017 decides what backs it); the host-side
timeout/output caps (already correct in `Run()`, unchanged). Do NOT modify `limits.go`'s `Limits`
type or `parseLimits` (reuse them).

## Verification plan

- **Highest level achievable: L5 (per ADR-010 decomposition).** Behavioral enforcement on a booted
  guest: an over-memory payload OOMs under `memory_mb`, a fork bomb hits the pids cap, `nproc`
  reflects `vcpu_count`. Requires `/dev/kvm` + firecracker (rides on task 015); the mapping tests are
  L2 pure-function and run without `/dev/kvm`.
- **Harness command:** `go test -count=1 -run 'FirecrackerLimit|MachineConfig|VcpuMem|FirecrackerDisk' ./...`;
  the OOM/forkbomb/nproc TCs under `/dev/kvm`; `go test -count=1 ./...`; `gofmt -l .`.
- **Runtime observation (L5):** paste the OOM-kill line for a 256 MB alloc under `memory_mb=64`
  (TC-016-09); the "Cannot fork" line for the fork bomb under `pids=20` (TC-016-10); the `nproc==1`
  line under `cpu_count=1` (TC-016-11); the unit-level assertions that `vcpu_count`/`mem_size_mib`/
  drive-size map correctly, that zero ⇒ defaults, that disk_mb degrades when unsizeable, and that the
  config is byte-for-byte identical with vs without `timeout_sec`/`max_output_bytes` (TC-016-07/08).
- **No ADR.** ADR 010 D4 already specifies the mapping.

## Definition of done

- `cpu_count → vcpu_count`, `memory_mb → mem_size_mib`, `disk_mb →` writable-drive size,
  `pids →` in-guest `RLIMIT_NPROC`; zero/absent ⇒ Firecracker defaults.
- No `taskset` prefix on the firecracker argv (vcpu_count is the cap); recorded as a stronger
  enforcement, not a degrade.
- `disk_mb` degrades (warn + continue, `degraded:[disk_mb]`) when the writable layer can't be sized.
- The firecracker config is byte-for-byte independent of `timeout_sec` + `max_output_bytes` (those
  stay host-side, unchanged).
- Behavioral L5: 256 MB alloc OOMs under `memory_mb=64`; fork bomb hits NPROC under `pids=20`;
  `nproc==1` under `cpu_count=1`.
- `configuration.md` + `behaviors.md` updated in place with the Firecracker limits mapping; no future
  tense.
- `go test -count=1 ./...` green; `gofmt -l .` clean; `limits.go` unmodified.
- spec-verifier APPROVE + recorded L5 evidence before promotion to ✅.
