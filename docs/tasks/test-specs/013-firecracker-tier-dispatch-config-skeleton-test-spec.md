# Test Spec 013: Firecracker tier dispatch + config-generator skeleton

**Linked task:** [`docs/tasks/backlog/013-firecracker-tier-dispatch-config-skeleton.md`](../backlog/013-firecracker-tier-dispatch-config-skeleton.md)
**ADR:** ADR 010 (Firecracker Tier-3 backend) — D1 (backend behind `backendFor`), D2 (no-NIC invariant by omission), D3 (config as a pure function of on-host paths). No new ADR required for this slice.
**Written:** 2026-06-20

## Context for the test author

ADR-001 D7 established the `tier` seam; ADR-002 D7.1 made `backendFor(tier)` the real dispatch
point. Today `backendFor` (`run.go:258-267`) wires `""`/`bubblewrap` → `bubblewrapBackend`,
`gvisor` → `gvisorBackend`, and **every other tier (including `firecracker`) returns
`tier not implemented: <tier>`**. SPEC.md Non-goals records `firecracker` as "accepted by the
`tier` field but returns `tier not implemented`."

This task is the **thin first slice** of ADR 010: wire a `firecrackerBackend` into `backendFor`
and add a **pure config generator** that produces the microVM configuration (machine-config,
boot-source, root-drive, vsock) as a function of on-host paths — mirroring how `gvisorOCISpec`
(`gvisor.go:190-240`) is a pure function unit-testable without `runsc`. **No VMM is launched in
this task** (that is task 015); this slice is config generation only, unit-testable without
`/dev/kvm` or the `firecracker`/`jailer` binaries.

The **load-bearing assertion** is the no-NIC invariant **by construction** (ADR 010 D2): the
generated config must carry **no `network-interface` key**. This is the microVM analogue of
`bwrap --unshare-all` / the gVisor empty netns — "no network" is achieved by *omission*, not by a
disable flag. A fitness function asserting this lands in task 018; this task's tests prove the
generator never emits the key in the first place.

Ground truth to mirror:
- `gvisorBackend.Argv(scriptPath, proxySock, workdir, fileReads, env, lim)` is the `Backend`
  interface signature (`run.go:251-253`); `firecrackerBackend` must satisfy it.
- `gvisorOCISpec(scriptPath, proxySock)` returns a `map[string]any` built purely from paths
  (`gvisor.go:190`); `firecrackerConfig(...)` should follow the same pure-function shape so it is
  unit-testable with no host prerequisite.
- The absence of `firecracker`/`jailer`/`runsc` surfaces as a spawn error (exit 127), never a
  silent fall-back (`gvisor.go:18-20` comment; ADR 010 D1).
- Firecracker's REST config bodies are `machine-config`, `boot-source`, `drives` (root drive),
  `vsock`; `network-interfaces` is the opt-in NIC body that this generator must **never** produce.

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-013-01 | `backendFor("firecracker")` returns a `firecrackerBackend` (satisfying the `Backend` interface), not the `tier not implemented` error; `""`/`bubblewrap`/`gvisor` dispatch is unchanged | TC-013-01, TC-013-02 | ✅ |
| REQ-013-02 | A pure `firecrackerConfig(...)` generates the microVM config (machine-config, boot-source, root-drive, vsock) as a function of on-host paths only — no `/dev/kvm`, no `firecracker`/`jailer` binary, no network call needed to build it | TC-013-03 | ✅ |
| REQ-013-03 | The generated config contains **no `network-interface`/`network-interfaces` key** — the no-NIC invariant by construction (ADR 010 D2); this holds for the base config and for every shape this slice can produce | TC-013-04 (base), TC-013-05 (negative: a detector proves it would catch a NIC if one were present) | ✅ |
| REQ-013-04 | The generated config wires the vsock device with the host-side `uds_path` (the bridge to the EgressProxy, ADR 010 D2) and the root drive + boot-source point at the supplied on-host paths; the payload entry point is `/usr/bin/sh /payload.sh` (matching every other tier) | TC-013-06 | ✅ |
| REQ-013-05 | The config generator is deterministic — identical inputs produce a byte-for-byte identical serialized config (reproducible, like `envSetenvPairs`/`gvisorOCISpec`) | TC-013-07 | ✅ |
| REQ-013-06 | SPEC.md Non-goals is updated in the same commit: `firecracker` is no longer "returns `tier not implemented`" — it now dispatches to a config-generating backend (VMM launch still pending, stated without future tense as the current boundary) | TC-013-08 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] The no-NIC negative case (a detector that would catch a NIC) is specified so the invariant check is provably not a no-op
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: NO VMM launch in this task — config generation + dispatch only
- [x] Confirmed: generator is a pure function, unit-testable without `/dev/kvm` or the firecracker binary

---

## Test cases

### TC-013-01: `backendFor("firecracker")` returns a backend, not an error

- **Requirement:** REQ-013-01
- **Type:** unit (Go test)
- **Input:** `backendFor("firecracker")`.
- **Expected:** returns a non-nil `Backend` and a nil error; the concrete type is
  `firecrackerBackend`. (Contrast: pre-task it returned `nil, errors.New("tier not implemented: firecracker")`.)

### TC-013-02: existing tier dispatch is unchanged

- **Requirement:** REQ-013-01
- **Type:** unit (Go test)
- **Input:** `backendFor("")`, `backendFor("bubblewrap")`, `backendFor("gvisor")`, and an unknown
  tier `backendFor("qemu")`.
- **Expected:** `""`/`bubblewrap` → `bubblewrapBackend`; `gvisor` → `gvisorBackend`; `qemu` →
  `nil, "tier not implemented: qemu"` (the default arm still rejects genuinely unknown tiers).

### TC-013-03: `firecrackerConfig` builds without any host prerequisite

- **Requirement:** REQ-013-02
- **Type:** unit (Go test)
- **Input:** call `firecrackerConfig(kernelPath, rootfsPath, scriptPath, vsockUDS, lim)` with
  plausible on-host paths (the files need not exist — it is a pure builder) and a zero `Limits`.
- **Expected:** returns a populated config structure (machine-config, boot-source, root drive,
  vsock) with no error, no call to `/dev/kvm`, no exec of `firecracker`/`jailer`, no network I/O.
  Runs green on a host with none of the Firecracker prerequisites installed.

### TC-013-04: generated config carries NO `network-interface` key (the crux, base shape)

- **Requirement:** REQ-013-03
- **Type:** unit (Go test) — **load-bearing**
- **Input:** serialize the config from TC-013-03 to JSON (or walk the structure) and scan its keys.
- **Expected:** **no** `network-interface` or `network-interfaces` key appears anywhere in the
  config — not at the top level, not nested. This is the microVM analogue of `--unshare-all` /
  the empty OCI netns: no NIC = no network, by construction (ADR 010 D2). Assert on the serialized
  bytes (`!strings.Contains(json, "network-interface")`) AND on the structured keys so a future
  refactor of the serialization can't silently reintroduce it.

### TC-013-05: the no-NIC detector would catch a NIC if one were present (negative)

- **Requirement:** REQ-013-03
- **Type:** unit (Go test, negative)
- **Input:** feed the no-NIC assertion helper a **constructed** config that *does* carry a
  `network-interfaces` entry (simulating a regression that added a NIC).
- **Expected:** the assertion helper returns a non-nil error / a test using it fails — proving the
  no-NIC check is not vacuous (it actually rejects a config with a NIC). This mirrors the
  positive/negative idiom task 009 uses for the F-001 bwrap check.

### TC-013-06: config wires vsock uds, root drive, boot source, and the sh /payload.sh entry

- **Requirement:** REQ-013-04
- **Type:** unit (Go test)
- **Input:** the config from TC-013-03, built with a specific `vsockUDS`, `rootfsPath`,
  `kernelPath`.
- **Expected:** the vsock device's host-side `uds_path` equals `vsockUDS` (the bridge to the
  EgressProxy); the root drive `path_on_host` equals `rootfsPath` and is the root device; the
  boot-source `kernel_image_path` equals `kernelPath`; the boot args / guest entry runs
  `/usr/bin/sh /payload.sh` (the same payload entry point as bubblewrap and gVisor). The vsock is
  a device, NOT a network-interface (re-assert TC-013-04 on this shape).

### TC-013-07: config generation is deterministic / reproducible

- **Requirement:** REQ-013-05
- **Type:** unit (Go test)
- **Input:** call `firecrackerConfig(...)` twice with identical arguments; serialize both.
- **Expected:** the two serialized configs are byte-for-byte equal (`reflect.DeepEqual` on the
  structures and equal JSON bytes). No map-iteration-order nondeterminism leaks into the output
  (mirrors `gvisorOCISpec`'s reproducibility and `envSetenvPairs`' sorted-key order).

### TC-013-08: SPEC.md Non-goals updated — firecracker dispatches, no longer "tier not implemented"

- **Requirement:** REQ-013-06
- **Type:** inspection (spec)
- **Input:** read `docs/spec/SPEC.md` Non-goals (and the project-summary tier sentence) after the
  feat commit.
- **Expected:** the line "`firecracker` … returns `tier not implemented`" is rewritten in place to
  reflect that `firecracker` now dispatches to a config-generating Tier-3 backend, with VMM launch
  noted as the current boundary (present tense — "the backend generates the microVM config; the
  one-shot VMM launch is not yet wired"). No future-tense roadmap language in the spec.

---

## Post-implementation verification

- [ ] TC-013-01..02: `backendFor("firecracker")` returns a backend; other tiers unchanged
- [ ] TC-013-03: config builds with no Firecracker host prerequisite
- [ ] TC-013-04: base config carries no `network-interface` key (load-bearing)
- [ ] TC-013-05: the no-NIC detector fails on a constructed NIC config (not a no-op)
- [ ] TC-013-06: vsock uds / root drive / boot source / `sh /payload.sh` entry wired correctly
- [ ] TC-013-07: config generation is deterministic
- [ ] TC-013-08: SPEC.md Non-goals rewritten in place, no future tense

## Test framework notes

- Standard Go `testing`. All tests in this task run **without** `/dev/kvm` or the
  `firecracker`/`jailer` binaries — the config generator is a pure function (the whole point of the
  thin slice).
- Keep the no-NIC assertion in a single helper so task 018's `fitness-no-nic` target can reuse it
  (the microVM analogue of F-001). The negative case (TC-013-05) calls that helper on a bad config.
- Put the new code in a new `firecracker.go` (mirroring `gvisor.go`) and tests in
  `firecracker_test.go`; do not modify `gvisor.go`. `backendFor` in `run.go` gains one case arm.
