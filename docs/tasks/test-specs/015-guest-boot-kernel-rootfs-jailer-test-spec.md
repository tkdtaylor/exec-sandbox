# Test Spec 015: Guest boot — kernel image + rootfs + jailer launch

**Linked task:** [`docs/tasks/backlog/015-guest-boot-kernel-rootfs-jailer.md`](../backlog/015-guest-boot-kernel-rootfs-jailer.md)
**ADR:** ADR 010 D1 (drive Firecracker via REST-over-Unix-socket under the jailer), D3 (workload → guest kernel + rootfs + jailer, mirroring the OCI bundle). **BLOCKED on ADR 010 Q1 + Q3** (see Readiness) — likely an ADR-010 amendment before implementation.
**Written:** 2026-06-20

## Context for the test author

This task makes the microVM actually **boot and run the payload**: launch Firecracker under its
`jailer` via the REST-over-Unix-socket API (`PUT /machine-config`, `/boot-source`, `/drives/...`,
`/vsock`, then `PUT /actions {InstanceStart}`), boot the guest kernel + rootfs, run
`/usr/bin/sh /payload.sh`, and capture stdout/stderr/exit through the **unchanged** host-side path
in `Run()` (`run.go:154-194` — the `exec.CommandContext` + `capWriter` capture, the process-group
kill, the exit-code mapping). The Firecracker process is the spawned child; the host-side capture,
timeout kill, and output cap apply with no backend involvement (ADR 010 D4 — `timeout_sec` /
`max_output_bytes` stay above the seam).

This is the seam between the pure config generator (task 013) + egress wiring (task 014) and a real
running guest. It is gated on two unresolved ADR-010 open questions.

### PRECONDITION — BLOCKED on Q1 + Q3 (flagged prominently)

- **Q1 — Guest kernel + rootfs sourcing.** Where the `vmlinux` and the minimal rootfs come from
  (build from source / vendor a pinned prebuilt / generate at first run) is **not established
  anywhere in the repo**. It affects reproducibility, supply-chain scanning (`dep-scan` does not
  cover a kernel image), and binary size. **Must be decided before this task is implemented** —
  likely an ADR-010 amendment.
- **Q3 — Jailer privilege/runtime model.** The jailer expects specific chroot/cgroup/uid setup and
  often elevated setup privileges; how that reconciles with exec-sandbox's unprivileged
  (`--rootless`-style) operation — where the namespace tiers run unprivileged — is **unresolved**.
  It may constrain which hosts can run Tier-3.

The test cases below are written so they are **executable once Q1+Q3 are resolved**; the precise
kernel/rootfs provenance assertions (TC-015-07) and the jailer privilege-model assertions
(TC-015-05) are stated as placeholders to be tightened by the Q1/Q3 disposition.

Ground truth to mirror:
- The host-side capture path is tier-independent and already correct (`run.go:160-188`): stdout/
  stderr through `capWriter`, `Setpgid` process group, `cmd.Cancel` SIGKILLs the group on the
  deadline, exit-code mapping (`ExitError` → code; other error → 127; timeout → 137 + `status:
  "timeout"`). This task does NOT modify it — it makes the firecracker child a well-behaved member
  of it.
- Absence of `firecracker`/`jailer`/`/dev/kvm` is a spawn error (exit 127), never a silent
  fall-back (ADR 010 D1; mirrors `gvisor.go:18-20`).

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-015-01 | `firecrackerBackend.Argv` returns an argv that launches `firecracker` under the `jailer` pointed at the per-run bundle (config + api socket); a cleanup func tears the bundle down (mirroring the gVisor bundle + cleanup) | TC-015-01, TC-015-02 | ✅ |
| REQ-015-02 | The backend drives the Firecracker REST API over the Unix socket in order — machine-config → boot-source → drives → vsock → `InstanceStart` — and the guest boots and runs `/usr/bin/sh /payload.sh` | TC-015-03, TC-015-04 | ✅ |
| REQ-015-03 | Firecracker runs under the jailer (cgroup/namespace barrier + chroot + privilege drop) per the Q3-resolved privilege model; the jailer is part of the launch, not optional | TC-015-05 | ✅ |
| REQ-015-04 | stdout/stderr/exit_code flow through the UNCHANGED host-side capture path in `Run()` — `capWriter` caps, process-group kill on timeout, exit-code mapping (clean exit, non-zero exit, timeout=137) — with no change to that path | TC-015-06, TC-015-08, TC-015-09 | ✅ |
| REQ-015-05 | The guest kernel image + rootfs are sourced per the Q1-resolved provenance (pinned/reproducible), and their provenance is recorded; absence of the kernel/rootfs or `/dev/kvm`/`firecracker`/`jailer` is a spawn error (exit 127), never a silent fall-back to a weaker tier | TC-015-07, TC-015-10 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] **BLOCKED marker recorded:** Q1 (kernel/rootfs sourcing) + Q3 (jailer privilege model) must be resolved (ADR-010 amendment) before implementation
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: the host-side capture path in `Run()` is NOT modified by this task
- [x] Target verification level: L5 (validation harness boots a guest, runs a payload, captures stdout/exit end-to-end) — requires `/dev/kvm` + firecracker + jailer + the Q1 kernel/rootfs; integration tests skip-guard when absent

---

## Test cases

### TC-015-01: Argv returns a jailer-wrapped firecracker launch + a bundle cleanup func

- **Requirement:** REQ-015-01
- **Type:** unit (Go test)
- **Input:** `firecrackerBackend.Argv(scriptPath, proxySock, workdir, fileReads, env, lim)`.
- **Expected:** `argv[0]` is the `jailer` (or a launcher that invokes the jailer); the argv names
  the `firecracker` binary, the per-run api socket, and the config/bundle dir; a non-nil cleanup
  func is returned that removes the bundle dir (mirroring `gvisorBackend.Argv`'s `cleanup` at
  `gvisor.go:22-26`).

### TC-015-02: a per-run bundle dir is created and torn down

- **Requirement:** REQ-015-01
- **Type:** unit (Go test)
- **Input:** call `Argv`, note the bundle dir, invoke the returned cleanup.
- **Expected:** the bundle dir exists after `Argv` (contains the generated config) and is gone
  after cleanup. No bundle survives the run (the ephemeral non-goal; ADR 010 D5/D6).

### TC-015-03: the backend drives the REST API in the correct order

- **Requirement:** REQ-015-02
- **Type:** unit/integration (Go test against a fake REST endpoint or a recording transport)
- **Input:** point the backend's REST client at a recording Unix-socket server; run the launch
  sequence.
- **Expected:** the recorded PUT order is machine-config → boot-source → drives (root) → vsock →
  `actions{InstanceStart}`. **No `PUT /network-interfaces`** appears in the recorded sequence (the
  no-NIC invariant observed at the API level, complementing task 014's config-level check).

### TC-015-04: the guest boots and runs the payload (end-to-end)

- **Requirement:** REQ-015-02
- **Type:** integration (Go test) — target L5, requires `/dev/kvm` + firecracker + jailer + Q1 kernel/rootfs
- **Input:** a firecracker run whose payload is `echo HELLO-FROM-GUEST`.
- **Expected:** the run returns `exit_code 0` and `result["stdout"]` contains `HELLO-FROM-GUEST` —
  the payload genuinely executed as `/usr/bin/sh /payload.sh` inside the booted guest. Skip-guard
  when prerequisites are absent.

### TC-015-05: Firecracker runs under the jailer (Q3 privilege model)

- **Requirement:** REQ-015-03
- **Type:** integration (Go test) + inspection — target L5
- **Input:** inspect the launch argv and (when running) the live process tree / cgroup placement.
- **Expected:** the firecracker process is a child of the jailer, placed in the jailer's
  cgroup/namespace barrier and chroot, with privileges dropped per the **Q3-resolved** model
  (tighten this assertion to the chosen privilege model once Q3 is settled). The jailer is never
  bypassed.

### TC-015-06: clean payload exit maps to exit_code 0 via the unchanged capture path

- **Requirement:** REQ-015-04
- **Type:** integration (Go test) — target L5
- **Input:** a payload that exits 0 after printing to stdout and stderr.
- **Expected:** `result["exit_code"] == 0`, `status == "clean"`; stdout/stderr captured through the
  same `capWriter` path as the other tiers (`run.go:160-161`). The host-side capture code is
  unchanged (assert by diff that `run.go`'s capture block is untouched).

### TC-015-07: kernel + rootfs provenance recorded (Q1 disposition)

- **Requirement:** REQ-015-05
- **Type:** inspection (spec/ADR) — placeholder, tighten per Q1
- **Input:** read the Q1 disposition (ADR-010 amendment) + the spec note on kernel/rootfs sourcing.
- **Expected:** the `vmlinux` + rootfs provenance is pinned and recorded (a pinned prebuilt hash, a
  build recipe, or a generation procedure — whichever Q1 chooses), with the supply-chain stance
  noted (dep-scan does not cover a kernel image, so the pin is the control). No unpinned
  fetch-at-run.

### TC-015-08: non-zero payload exit is propagated

- **Requirement:** REQ-015-04
- **Type:** integration (Go test) — target L5
- **Input:** a payload that runs `exit 3`.
- **Expected:** `result["exit_code"] == 3`, `status == "clean"` (a clean non-zero exit, not a
  timeout) — proving the guest's exit code surfaces through the host capture path unchanged.

### TC-015-09: an over-running payload is killed on the wall-clock deadline (timeout=137)

- **Requirement:** REQ-015-04
- **Type:** integration (Go test) — target L5
- **Input:** a payload `sleep 30` with `profile.limits.timeout_sec = 1`.
- **Expected:** the run terminates in ≈1s with `status == "timeout"`, `exit_code == 137` — the
  host-side process-group SIGKILL (`run.go:166-188`) terminates the firecracker child and no guest
  outlives the deadline. This proves `timeout_sec` stays host-side above the seam (ADR 010 D4),
  identical to the bubblewrap/gVisor behavior.

### TC-015-10: missing firecracker/jailer/kvm is a spawn error (exit 127), no fall-back

- **Requirement:** REQ-015-05
- **Type:** unit/integration (Go test)
- **Input:** run the firecracker tier on a host where `firecracker`/`jailer` is not on PATH (or
  `/dev/kvm` is absent).
- **Expected:** the run yields `exit_code 127` (a spawn error surfaced through the host path), NOT a
  silent fall-back to bubblewrap or gVisor. Mirrors the gVisor `runsc`-absent behavior.

---

## Post-implementation verification

- [ ] **Q1 + Q3 resolved (ADR-010 amendment) BEFORE implementation** — this is the unblock gate
- [ ] TC-015-01..02: jailer-wrapped argv + bundle create/teardown
- [ ] TC-015-03: REST PUT order correct, NO `network-interfaces` PUT
- [ ] TC-015-04: guest boots, payload prints HELLO-FROM-GUEST, exit 0 (L5)
- [ ] TC-015-05: firecracker runs under the jailer per the Q3 model
- [ ] TC-015-06/08/09: exit-code mapping (0, non-zero, timeout=137) via the unchanged capture path
- [ ] TC-015-07: kernel/rootfs provenance pinned + recorded (Q1)
- [ ] TC-015-10: missing prereq → exit 127, no fall-back

## Test framework notes

- Standard Go `testing`. The unit-level tests (TC-015-01/02/03 against a fake REST endpoint, and
  TC-015-10) run without `/dev/kvm`. The boot/run/timeout tests (TC-015-04/05/06/08/09) need
  `/dev/kvm` + firecracker + jailer + the Q1 kernel/rootfs and MUST skip-guard when absent.
- Do NOT modify `Run()`'s host-side capture block — TC-015-06 asserts it is untouched by diff.
- **Depends on task 013 (config skeleton) and task 014 (egress wiring) landing first, and is BLOCKED
  on Q1 + Q3 being resolved.** Mark the coverage row `⚠️ planned, BLOCKED on Q1+Q3`.
