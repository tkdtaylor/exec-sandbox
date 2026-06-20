# Test Spec 016: profile.limits → machine-config mapping

**Linked task:** [`docs/tasks/backlog/016-profile-limits-machine-config-mapping.md`](../backlog/016-profile-limits-machine-config-mapping.md)
**ADR:** ADR 010 D4 (limits mapping: `profile.limits` → Firecracker machine-config vCPU/mem; disk → drive sizing; pids → in-guest rlimit; timeout/output stay host-side above the seam). No new ADR required — this applies the existing limits model (ADR 003) to the new backend.
**Written:** 2026-06-20

## Context for the test author

ADR 003 (`limits.go`) defines `Limits{CPUCount, MemoryMB, PidsLimit, DiskMB, Timeout,
MaxOutputBytes}`. Each tier maps these to its own enforcement mechanism: bubblewrap uses
`prlimit` + tmpfs `--size` + `taskset`; gVisor uses OCI `process.rlimits` + tmpfs `size=` +
`taskset` (`gvisor.go:140-178`). Firecracker's machine config is a **better fit** for the
CPU/memory caps than the rlimit/tmpfs approximations (ADR 010 D4):

- `cpu_count` → `machine-config.vcpu_count` — the guest literally has that many vCPUs (a real cap,
  not a host-side `taskset` affinity hint; **stronger** than Tier-1/Tier-2).
- `memory_mb` → `machine-config.mem_size_mib` — the guest's total RAM ceiling (the microVM cannot
  exceed it, vs the namespace tiers' `RLIMIT_AS`).
- `disk_mb` → the size of the writable drive / writable layer presented to the guest.
- `pids` → an in-guest `RLIMIT_NPROC` applied by the guest-side launcher (the guest kernel owns its
  own pid space — analogous to the `prlimit` wrap under bubblewrap).
- `timeout_sec` and `max_output_bytes` are enforced **host-side, above the tier seam, UNCHANGED** —
  `Run()` kills the process group on the deadline (`run.go:166-188`) and caps each stream via
  `capWriter` (`run.go:160-161`). The firecracker process is the spawned child; no backend
  involvement. A load-bearing cap the host genuinely cannot apply is a hard error; a secondary cap
  degrades loudly (warn + continue) exactly as today.

The mapping is a **pure function** of `Limits` onto the machine-config fields — unit-testable
without `/dev/kvm`, mirroring `applyLimitsToOCISpec` (`gvisor.go:140`). Behavioral enforcement
(an over-memory payload OOMs; a fork bomb hits NPROC) is the L5 evidence and needs a booted guest.

Ground truth to mirror:
- `parseLimits` coerces JSON numbers; a zero/absent field means "no cap" (`limits.go:26-51`).
- `applyLimitsToOCISpec` returns `[]degrade` and applies caps in place; `disk_mb` degrades when the
  writable layer can't be sized (`gvisor.go:155-163`). The firecracker mapping follows the same
  degrade contract.
- `cpu_count` under the namespace tiers is a host-side affinity hint (`cpuAffinityPrefix`,
  `limits.go:71-80`); under firecracker it becomes a **real** `vcpu_count` — record that this is a
  stronger enforcement, not a degrade.

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-016-01 | `cpu_count` maps to `machine-config.vcpu_count` (a real vCPU cap, not a taskset hint); zero/absent ⇒ the field is left at the Firecracker default (no cap requested) | TC-016-01, TC-016-05 | ✅ |
| REQ-016-02 | `memory_mb` maps to `machine-config.mem_size_mib`; zero/absent ⇒ left at default | TC-016-02, TC-016-05 | ✅ |
| REQ-016-03 | `disk_mb` maps to the size of the writable drive presented to the guest; if the host cannot size the writable layer it degrades (warn + continue) with `degraded:[disk_mb]`, never a silent drop | TC-016-03, TC-016-06 | ✅ |
| REQ-016-04 | `pids` maps to an in-guest `RLIMIT_NPROC` applied by the guest-side launcher (not a host machine-config field) | TC-016-04 | ✅ |
| REQ-016-05 | `timeout_sec` and `max_output_bytes` are NOT mapped into the machine-config — they remain host-side, above the tier seam, enforced by the unchanged `Run()` path; the firecracker config is byte-for-byte independent of these two caps | TC-016-07, TC-016-08 | ✅ |
| REQ-016-06 | Behavioral enforcement holds on a booted guest: an over-memory payload is OOM-killed under `memory_mb`; a fork bomb hits the pids cap; the vCPU cap is observable in-guest (`nproc`); the over-running payload is killed host-side (`status:"timeout"`) | TC-016-09, TC-016-10, TC-016-11 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] The disk_mb degrade path is specified (warn + continue, not silent drop)
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: timeout_sec + max_output_bytes do NOT enter the machine-config (host-side, unchanged)
- [x] Target verification level: L5 (validation harness: over-memory payload OOMs, fork bomb hits NPROC, `nproc` reflects vcpu_count) — requires a booted guest; mapping unit tests run without `/dev/kvm`

---

## Test cases

### TC-016-01: cpu_count → machine-config.vcpu_count

- **Requirement:** REQ-016-01
- **Type:** unit (Go test)
- **Input:** build the firecracker machine-config from `Limits{CPUCount: 2}`.
- **Expected:** `machine-config.vcpu_count == 2`. **No** `taskset` prefix is added to the argv for
  the firecracker tier (the vCPU count is the cap; the host-side affinity hint is unnecessary and
  must not be applied — record that cpu_count is a *stronger* enforcement here, not a degrade).

### TC-016-02: memory_mb → machine-config.mem_size_mib

- **Requirement:** REQ-016-02
- **Type:** unit (Go test)
- **Input:** build the machine-config from `Limits{MemoryMB: 128}`.
- **Expected:** `machine-config.mem_size_mib == 128`.

### TC-016-03: disk_mb → writable drive size

- **Requirement:** REQ-016-03
- **Type:** unit (Go test)
- **Input:** build the drive config from `Limits{DiskMB: 64}` on a host where the writable layer is
  sizeable (`diskQuotaSupported() == true`).
- **Expected:** the writable drive presented to the guest is sized to 64 MiB; no degrade is
  recorded.

### TC-016-04: pids → in-guest RLIMIT_NPROC

- **Requirement:** REQ-016-04
- **Type:** unit (Go test)
- **Input:** build the launch config from `Limits{PidsLimit: 20}`.
- **Expected:** the guest-side launcher applies `RLIMIT_NPROC = 20` (an in-guest rlimit, e.g. a
  `prlimit --nproc=20 --` wrap around `/usr/bin/sh /payload.sh` in the guest, analogous to
  `prlimitWrap` at `limits.go:88-106`). `pids` is NOT a machine-config field.

### TC-016-05: zero/absent caps leave machine-config at defaults

- **Requirement:** REQ-016-01, REQ-016-02
- **Type:** unit (Go test)
- **Input:** build the machine-config from a zero `Limits{}`.
- **Expected:** `vcpu_count` and `mem_size_mib` are left at the Firecracker default (no explicit cap
  emitted), matching the "zero field = no limit" contract (`limits.go` comment). The base config is
  byte-for-byte the no-limits shape.

### TC-016-06: disk_mb degrades when the writable layer can't be sized

- **Requirement:** REQ-016-03
- **Type:** unit (Go test)
- **Input:** force `diskQuotaSupported = func() bool { return false }` (the test seam at
  `limits.go:65`) and build the drive config from `Limits{DiskMB: 64}`.
- **Expected:** the returned `[]degrade` contains a `disk_mb` entry with a WARNING reason; the run
  proceeds (warn + continue), the drive is unsized — never a silent drop. Mirrors
  `applyLimitsToOCISpec`'s disk degrade (`gvisor.go:155-163`).

### TC-016-07: timeout_sec is NOT in the machine-config

- **Requirement:** REQ-016-05
- **Type:** unit (Go test)
- **Input:** build the full firecracker config from `Limits{Timeout: 5s}` vs `Limits{}`.
- **Expected:** the two configs are byte-for-byte identical — `timeout_sec` does not appear in the
  machine-config or anywhere in the firecracker config. It is enforced host-side in `Run()`
  (unchanged).

### TC-016-08: max_output_bytes is NOT in the machine-config

- **Requirement:** REQ-016-05
- **Type:** unit (Go test)
- **Input:** build the full firecracker config from `Limits{MaxOutputBytes: 1024}` vs `Limits{}`.
- **Expected:** the two configs are byte-for-byte identical — `max_output_bytes` does not appear in
  the firecracker config. It is enforced host-side by `capWriter` (unchanged), exactly as the gVisor
  output-cap test (F-008) asserts the OCI spec is unchanged by it.

### TC-016-09: memory_mb is behaviorally enforced (over-memory payload OOMs)

- **Requirement:** REQ-016-06
- **Type:** integration (Go test) — target L5, requires a booted guest
- **Input:** a firecracker run with `memory_mb = 64` and a payload that allocates ~256 MB.
- **Expected:** the payload is OOM-killed inside the guest (the microVM's `mem_size_mib` ceiling),
  analogous to `TestMemoryLimitKillsPayload_Bwrap`. Skip-guard when prerequisites are absent.

### TC-016-10: pids is behaviorally enforced (fork bomb hits NPROC)

- **Requirement:** REQ-016-06
- **Type:** integration (Go test) — target L5
- **Input:** a firecracker run with `pids = 20` and a fork-bomb payload.
- **Expected:** the fork bomb hits "Cannot fork" / the NPROC ceiling inside the guest, analogous to
  `TestPidsLimitRejectsForkBomb_Bwrap`.

### TC-016-11: cpu_count is behaviorally observable in-guest

- **Requirement:** REQ-016-06
- **Type:** integration (Go test) — target L5
- **Input:** a firecracker run with `cpu_count = 1` and a payload that prints `nproc`.
- **Expected:** the guest reports `nproc == 1` — the guest genuinely has one vCPU (a real cap, the
  ADR 010 D4 improvement over the namespace tiers' host-side affinity hint).

---

## Post-implementation verification

- [ ] TC-016-01..05: cpu/mem/disk/pids map to the right targets; zero ⇒ defaults
- [ ] TC-016-06: disk_mb degrades (warn + continue) when unsizeable
- [ ] TC-016-07..08: timeout_sec + max_output_bytes absent from the config (host-side, unchanged)
- [ ] TC-016-09..11: behavioral enforcement on a booted guest (OOM, NPROC, nproc==1) (L5)

## Test framework notes

- Standard Go `testing`. The mapping tests (TC-016-01..08) are pure-function unit tests — no
  `/dev/kvm`. The behavioral tests (TC-016-09..11) need a booted guest (depends on task 015) and
  MUST skip-guard when prerequisites are absent.
- `Limits` is the existing parsed type (`limits.go`) — reuse it; `diskQuotaSupported` is the
  existing package-level test seam (`limits.go:65`) — reuse it for TC-016-06.
- **Depends on task 013 (config skeleton) and task 015 (guest boot) landing first.** Once those land
  this task is READY (no open-question block of its own).
