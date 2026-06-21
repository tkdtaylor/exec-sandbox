# Test Spec 015: Guest boot — kernel image + rootfs + direct firecracker launch (no jailer)

**Linked task:** [`docs/tasks/backlog/015-guest-boot-kernel-rootfs-jailer.md`](../backlog/015-guest-boot-kernel-rootfs-jailer.md)
**ADR:** ADR 010 D1 (drive Firecracker via REST-over-Unix-socket), D3 (workload → guest kernel + rootfs, mirroring the OCI bundle), **Amendment 1 (2026-06-20)** — A1.Q1 (kernel/rootfs sourcing) + A1.Q3 (**no jailer**: direct `firecracker` under the unprivileged `bwrap --unshare-all` + `limits.go` model). **READY** (Q1 + Q3 resolved).
**Written:** 2026-06-20

> **Filename note:** the file still says `...-jailer` for stability (renaming is "ask first"), but the
> **jailer is dropped** per ADR-010 Amendment 1 A1.Q3. Read "jailer" in older artifacts as "direct
> firecracker under bwrap `--unshare-all` + `limits.go`."

## Context for the test author

This task makes the microVM actually **boot and run the payload**: launch Firecracker **directly**
(no jailer — A1.Q3) under exec-sandbox's existing `bwrap --unshare-all` + `limits.go` unprivileged
wrapper, via the REST-over-Unix-socket API (`PUT /machine-config`, `/boot-source`, `/drives/...`,
`/vsock`, then `PUT /actions {InstanceStart}`), boot the **verified** guest kernel + rootfs (A1.Q1),
run `/usr/bin/sh /payload.sh`, and capture stdout/stderr/exit through the **unchanged** host-side path
in `Run()` (`run.go:154-194` — the `exec.CommandContext` + `capWriter` capture, the process-group
kill, the exit-code mapping). The Firecracker process is the spawned child; the host-side capture,
timeout kill, and output cap apply with no backend involvement (ADR 010 D4 — `timeout_sec` /
`max_output_bytes` stay above the seam).

This is the seam between the pure config generator (task 013) + egress wiring (task 014) and a real
running guest. ADR-010 Amendment 1 resolved the two open questions that previously gated it.

### PRECONDITION — RESOLVED by ADR-010 Amendment 1 (2026-06-20)

- **Q1 — Guest kernel + rootfs sourcing (RESOLVED).** Build the **uncompressed x86_64 ELF `vmlinux`**
  (`make vmlinux`; **not** `bzImage`) + a minimal **Alpine ext4** rootfs **from source as build-time
  tooling** (zero runtime third-party deps). Newest upstream-supported kernel line per Firecracker's
  `kernel-policy.md`, **floor linux 6.1**; **FLAG:** 6.1 EOL **2026-09-02** — pin the newest non-EOL
  line in upstream `resources/guest_configs/` at build time, preferring 6.1's successor if published.
  **Vendor + pin** `vmlinux` + `vmlinux.sha256` + `microvm-kernel-ci-x86_64-<ver>.config` +
  `PROVENANCE`; rootfs `is_read_only: true`, baking only the trusted vsock shim + `/sbin/init`, pinned
  by `base.ext4.sha256`. A stdlib `crypto/sha256` loader verifies both digests and **fails fast** on
  mismatch. The pin is the supply-chain control (`dep-scan` does not cover a kernel image).
- **Q3 — Privilege model (RESOLVED): NO jailer.** Run `firecracker` **directly** under
  `bwrap --unshare-all` + `limits.go`. Firecracker self-installs its seccomp filters regardless; bwrap
  supplies the chroot + mnt/pid/ipc/net namespaces + cgroup limits the jailer would otherwise provide,
  keeping Tier-3 **unprivileged** (consistent with Tier-1/2). **Host requirement:** KVM-capable
  hardware + the host user in the `kvm` group (rw on `/dev/kvm`); **no root, no setuid launcher, no
  caps beyond `/dev/kvm`**.

The precise kernel/rootfs provenance assertions (TC-015-07) and the **constraints ≥ jailer**
assertions (TC-015-05) are now tightened to the Amendment 1 disposition (no longer placeholders).

Ground truth to mirror:
- The host-side capture path is tier-independent and already correct (`run.go:160-188`): stdout/
  stderr through `capWriter`, `Setpgid` process group, `cmd.Cancel` SIGKILLs the group on the
  deadline, exit-code mapping (`ExitError` → code; other error → 127; timeout → 137 + `status:
  "timeout"`). This task does NOT modify it — it makes the firecracker child a well-behaved member
  of it.
- Absence of `firecracker`/`/dev/kvm` (or a `/dev/kvm` the host user cannot access) is a spawn error
  (exit 127), never a silent fall-back (ADR 010 D1; mirrors `gvisor.go:18-20`). **No jailer binary is
  a prerequisite** (A1.Q3).

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-015-01 | `firecrackerBackend.Argv` returns an argv that launches `firecracker` **directly** (NO jailer — A1.Q3) under the existing `bwrap --unshare-all` + `limits.go` unprivileged wrapper, pointed at the per-run bundle (config + api socket); a cleanup func tears the bundle down (mirroring the gVisor bundle + cleanup) | TC-015-01, TC-015-02 | ✅ |
| REQ-015-02 | The backend drives the Firecracker REST API over the Unix socket in order — machine-config → boot-source → drives → vsock → `InstanceStart` — and the guest boots and runs `/usr/bin/sh /payload.sh` | TC-015-03, TC-015-04 | ✅ |
| REQ-015-03 | The firecracker child's effective constraints are **≥ jailer** (A1.Q3): runs as a non-host uid, all namespaces unshared (none shared with host), cgroup limits applied, chroot/`pivot_root` in effect — supplied by `bwrap --unshare-all` + `limits.go`, with firecracker self-installing seccomp regardless. Exercises the task-018 constraints-≥-jailer fitness assertion against a live run | TC-015-05 | ✅ |
| REQ-015-04 | stdout/stderr/exit_code flow through the UNCHANGED host-side capture path in `Run()` — `capWriter` caps, process-group kill on timeout, exit-code mapping (clean exit, non-zero exit, timeout=137) — with no change to that path | TC-015-06, TC-015-08, TC-015-09 | ✅ |
| REQ-015-05 | The guest kernel image + rootfs are sourced per A1.Q1 (built-from-source, vendored + pinned by sha256, RO rootfs) and **verified by a stdlib `crypto/sha256` loader that fails fast on mismatch**; provenance is recorded (`PROVENANCE`); absence of the kernel/rootfs or `/dev/kvm`/`firecracker`, or an inaccessible `/dev/kvm` (host user not in `kvm` group), is a spawn error (exit 127), never a silent fall-back to a weaker tier. NO jailer binary is a prerequisite | TC-015-07, TC-015-10 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] **UNBLOCKED:** Q1 (kernel/rootfs sourcing) + Q3 (no jailer; unprivileged firecracker) resolved by ADR-010 Amendment 1 (2026-06-20) — this task is READY
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: the host-side capture path in `Run()` is NOT modified by this task
- [x] Target verification level: L5 (validation harness boots a guest, runs a payload, captures stdout/exit end-to-end) — requires `/dev/kvm` (host user in `kvm` group) + firecracker + the A1.Q1 verified kernel/rootfs; **no jailer binary**; integration tests skip-guard when absent

---

## Test cases

### TC-015-01: Argv returns a direct firecracker launch under bwrap (NO jailer) + a bundle cleanup func

- **Requirement:** REQ-015-01
- **Type:** unit (Go test)
- **Input:** `firecrackerBackend.Argv(scriptPath, proxySock, workdir, fileReads, env, lim)`.
- **Expected:** the argv launches the **`firecracker`** binary **directly** under exec-sandbox's
  existing `bwrap --unshare-all` + `limits.go` wrapper (A1.Q3) — **no `jailer` in `argv[0]` or
  anywhere in the argv**; the argv names the `firecracker` binary, the per-run api socket, and the
  config/bundle dir; `--unshare-all` is present and **no `--share-net`** appears; a non-nil cleanup
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

### TC-015-05: firecracker's effective constraints are ≥ jailer (A1.Q3, no jailer)

- **Requirement:** REQ-015-03
- **Type:** integration (Go test) + inspection — target L5
- **Input:** inspect the launch argv and (when running) the live process tree / namespace + cgroup +
  uid placement of the firecracker child. This exercises the **task-018 constraints-≥-jailer fitness
  assertion** against a live run.
- **Expected:** there is **no jailer** in the launch; instead the firecracker child runs under
  `bwrap --unshare-all` + `limits.go` with effective constraints **≥ jailer**:
  - runs as a **non-host uid** (not the host user's uid),
  - **all namespaces unshared** — mnt/pid/ipc/**net**/user — none shared with the host,
  - **cgroup limits applied** (the `limits.go` machinery),
  - **chroot / `pivot_root` in effect** (the guest VMM cannot see the host FS root),
  - and (with A1.Q1) the credential / host FS never leaks into the guest.

  Firecracker self-installs its seccomp filters regardless of any launcher; assert the filters are
  active. The point is jailer-*equivalent* (or stronger) containment without the jailer binary or any
  elevated privilege.

### TC-015-06: clean payload exit maps to exit_code 0 via the unchanged capture path

- **Requirement:** REQ-015-04
- **Type:** integration (Go test) — target L5
- **Input:** a payload that exits 0 after printing to stdout and stderr.
- **Expected:** `result["exit_code"] == 0`, `status == "clean"`; stdout/stderr captured through the
  same `capWriter` path as the other tiers (`run.go:160-161`). The host-side capture code is
  unchanged (assert by diff that `run.go`'s capture block is untouched).

### TC-015-07: kernel + rootfs are pinned, verified by sha256, and fail fast on mismatch (A1.Q1)

- **Requirement:** REQ-015-05
- **Type:** inspection (spec/ADR) + unit (Go test)
- **Input:** read the A1.Q1 disposition (ADR-010 Amendment 1) + the vendored artifacts
  (`guest/kernel/vmlinux-<ver>` + `vmlinux.sha256` + `config/PROVENANCE`,
  `guest/rootfs/base.ext4` + `base.ext4.sha256`); unit-test the stdlib `crypto/sha256` loader by
  feeding it a good artifact (passes) and a tampered/wrong-digest artifact (errors).
- **Expected:**
  - `vmlinux` is the **uncompressed x86_64 ELF** (built `make vmlinux`, not `bzImage`); the rootfs is
    a minimal **Alpine ext4** mounted `is_read_only: true`, carrying only the trusted shim +
    `/sbin/init` (no baked-in payload).
  - both artifacts are **vendored + pinned by sha256**, with `PROVENANCE` recording the upstream
    Firecracker commit + linux git tag; the config is `microvm-kernel-ci-x86_64-<ver>.config`.
  - the loader **verifies both digests before the backend uses the paths** and **fails fast / crashes
    loudly** on mismatch (no boot from an unverified artifact). No unpinned fetch-at-run; the pin is
    the supply-chain control (`dep-scan` does not cover a kernel image).

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

### TC-015-10: missing firecracker / inaccessible kvm is a spawn error (exit 127), no fall-back

- **Requirement:** REQ-015-05
- **Type:** unit/integration (Go test)
- **Input:** run the firecracker tier on a host where `firecracker` is not on PATH, or `/dev/kvm` is
  absent, or `/dev/kvm` exists but the host user is **not in the `kvm` group** (no rw access). **No
  jailer binary is checked for — it is not a prerequisite (A1.Q3).**
- **Expected:** the run yields `exit_code 127` (a spawn error surfaced through the host path), NOT a
  silent fall-back to bubblewrap or gVisor. Mirrors the gVisor `runsc`-absent behavior.

---

## Post-implementation verification

- [x] **Q1 + Q3 resolved (ADR-010 Amendment 1, 2026-06-20)** — the unblock gate is cleared
- [x] TC-015-01..02: direct-firecracker (no jailer) argv under bwrap + bundle create/teardown — `TestFirecrackerArgvIsBwrapDirectNoJailer`, `TestFirecrackerBundleCreatedAndTornDown`
- [x] TC-015-03: REST PUT order correct, NO `network-interfaces` PUT — `TestFirecrackerRESTOrderNoNIC`
- [x] TC-015-04: guest boots, payload prints HELLO-FROM-GUEST, exit 0 (L5) — `TestFirecrackerGuestBoot_E2E` (real boot on `/dev/kvm`)
- [x] TC-015-05: firecracker's effective constraints ≥ jailer (no jailer; A1.Q3) — `TestFirecrackerConstraintsGEJailer_Live` **observes the live HOST-SIDE firecracker child** from `/proc/<pid>/*` (all 7 namespaces differ from an unwrapped host process; `uid_map "65534 <hostuid> 1"` = non-host in-namespace uid; `CapEff=0`/`NoNewPrivs=1`; root mount is a `pivot_root` tmpfs newroot, not the host `/`) via `observeFirecrackerChild` + `assertConstraintsGEJailer`; `_Argv` checks the requested argv; `TestFirecrackerConstraintsCheckerRejectsWeakChild` + `...RejectsWeakArgv` prove both checkers are non-vacuous (each weakening — shared ns, host uid, no userns, regained caps, no pivot_root — is rejected). The non-host uid required a wrapper fix (`--uid 65534 --gid 65534`); see ADR 010 Amendment 2.
- [x] TC-015-06/08/09: exit-code mapping (0, non-zero, timeout=137) via the unchanged capture path — `TestFirecrackerCleanExit_E2E`, `TestFirecrackerNonZeroExit_E2E` (exit 3), `TestFirecrackerTimeout_E2E` (1.066s → 137)
- [x] TC-015-07: kernel/rootfs pinned + sha256-verified, fails fast on mismatch (A1.Q1) — `TestGuestArtifactsVerifyAndFailFast`, `TestGuestKernelProvenanceVendored`
- [x] TC-015-10: missing firecracker / inaccessible kvm → exit 127, no fall-back — `TestFirecrackerMissingArtifactsIsHardError`, `TestFirecrackerRunMissingPrereqIsExit127`

## Test framework notes

- Standard Go `testing`. The unit-level tests (TC-015-01/02/03 against a fake REST endpoint, the
  sha256-loader half of TC-015-07, and TC-015-10) run without `/dev/kvm`. The boot/run/timeout/
  constraints tests (TC-015-04/05/06/08/09) need `/dev/kvm` (host user in the `kvm` group) +
  firecracker + the A1.Q1 verified kernel/rootfs and MUST skip-guard when absent. **No jailer binary
  is required** (A1.Q3).
- Do NOT modify `Run()`'s host-side capture block — TC-015-06 asserts it is untouched by diff.
- **Depends on task 013 (config skeleton) and task 014 (egress wiring) landing first.** Q1 + Q3 are
  resolved (ADR-010 Amendment 1), so this task is **no longer blocked** — mark the coverage row
  `📋 planned (ready)`.
