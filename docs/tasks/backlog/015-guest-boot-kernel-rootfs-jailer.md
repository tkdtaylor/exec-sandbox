# Task 015: Guest boot — kernel image + rootfs + jailer launch

**Status:** ⬜ backlog
**Branch:** `task/015-guest-boot-kernel-rootfs-jailer`
**Spec:** [`docs/tasks/test-specs/015-guest-boot-kernel-rootfs-jailer-test-spec.md`](../test-specs/015-guest-boot-kernel-rootfs-jailer-test-spec.md)
**ADR:** ADR 010 D1 (drive Firecracker via REST-over-Unix-socket under the jailer), D3 (workload → guest kernel + rootfs + jailer, mirroring the OCI bundle). **BLOCKED — needs an ADR-010 amendment resolving Q1 + Q3 before implementation.**

## Readiness — BLOCKED on Q1 + Q3 (FLAGGED)

**This task is BLOCKED. Do NOT start implementation until ADR-010 Q1 and Q3 are resolved**
(most likely as an ADR-010 amendment). Both are flagged in ADR-010's "Open questions" as
*not settleable from the current repository*:

- **Q1 — Guest kernel + rootfs sourcing.** Where the `vmlinux` and the minimal rootfs come from
  (build from source / vendor a pinned prebuilt / generate at first run) is **not established
  anywhere in the repo**. It drives reproducibility, supply-chain scanning (`dep-scan` does **not**
  cover a kernel image — the pin is the control), and binary size. **Decide before this task.**
- **Q3 — Jailer privilege/runtime model.** The jailer expects specific chroot/cgroup/uid setup and
  often elevated setup privileges. How that reconciles with exec-sandbox's unprivileged
  (`--rootless`-style) operation — where the namespace tiers run unprivileged — is **unresolved** and
  may constrain which hosts can run Tier-3. **Decide before this task.**

Coverage-tracker status: `⚠️ planned, BLOCKED on Q1+Q3`. The spec's TCs for kernel/rootfs provenance
(TC-015-07) and the jailer privilege model (TC-015-05) are placeholders to be tightened by the Q1/Q3
disposition.

**Dependency position:** 013 → 014 → **015** → {016, 017} → 018. Needs 013 (config skeleton) and 014
(egress wiring) landed; this task makes the guest actually boot and run.

## Problem

Tasks 013/014 produce a NIC-less, vsock-bridged microVM **config** but **nothing boots**. This task
launches Firecracker under its `jailer` via the REST-over-Unix-socket API
(`PUT /machine-config`, `/boot-source`, `/drives/...`, `/vsock`, then `PUT /actions {InstanceStart}`),
boots the guest kernel + rootfs, runs `/usr/bin/sh /payload.sh`, and captures stdout/stderr/exit
through the **unchanged** host-side path in `Run()` (`run.go:154-194` — `exec.CommandContext` +
`capWriter` + process-group SIGKILL on the deadline + exit-code mapping). The Firecracker process is
the spawned child; `timeout_sec`/`max_output_bytes` stay host-side above the seam (ADR 010 D4) with
no backend involvement.

## Scope

- **`firecrackerBackend.Argv` returns the jailer-wrapped launch**: `argv[0]` is the `jailer` (per the
  Q3-resolved model) naming the `firecracker` binary, the per-run api socket, and the bundle dir; a
  `cleanup` func removes the bundle (mirroring `gvisorBackend.Argv`).
- **Drive the REST API in order** (machine-config → boot-source → drives → vsock → `InstanceStart`)
  over the Unix socket, the way `gvisor.go` shells out to `runsc`. **No `PUT /network-interfaces`**
  ever appears in the sequence (the no-NIC invariant at the API level).
- **Source the guest kernel + rootfs per the Q1 decision** (pinned/reproducible), with the rootfs
  carrying the task-014 vsock→`/proxy.sock` shim. Record the provenance + supply-chain stance.
- **Run under the jailer per the Q3 decision** (cgroup/namespace barrier + chroot + privilege drop);
  the jailer is never bypassed.
- **Capture flows through the UNCHANGED `Run()` path**: do NOT modify the host-side capture block —
  the firecracker child becomes a well-behaved member of it (clean exit → its code; non-zero → its
  code; timeout → process-group SIGKILL → 137 + `status:"timeout"`).
- **Missing `firecracker`/`jailer`/`/dev/kvm`/kernel/rootfs → spawn error (exit 127)**, never a
  silent fall-back to a weaker tier (mirrors the gVisor `runsc`-absent behavior).

Out of scope: the limits→machine-config mapping semantics (task 016); `/work`/FileRead presentation
(task 017); teardown reclaim + fitness umbrella (task 018 — basic bundle cleanup is here, the jailer
chroot/cgroup reclaim is 018); VMM-native snapshot (D6, out of scope entirely). Do NOT modify the
`Run()` host-side capture block.

## Verification plan

- **Highest level achievable: L5 (per ADR-010 decomposition).** A validation harness boots a guest,
  runs a payload, and captures stdout/exit end-to-end. Requires `/dev/kvm` + firecracker + jailer +
  the Q1 kernel/rootfs; the REST-order test (against a fake endpoint) and the missing-prereq test are
  L2 and run without `/dev/kvm`.
- **Harness command:** `go test -count=1 -run 'Firecracker|Jailer|GuestBoot|RestOrder' ./...`; the
  boot/run/timeout TCs under `/dev/kvm`; `go test -count=1 ./...`; `gofmt -l .`.
- **Runtime observation (L5):** paste the `HELLO-FROM-GUEST` stdout + `exit 0` line (TC-015-04); the
  non-zero `exit 3` line (TC-015-08); the `status:"timeout"` + `exit 137` line for `sleep 30` under
  `timeout_sec=1` (TC-015-09); the recorded REST PUT order with **no** `network-interfaces` PUT
  (TC-015-03); the `exit 127` on a host without firecracker (TC-015-10).
- **ADR:** the **Q1 + Q3 resolution is an ADR-010 amendment** written before implementation; record
  the kernel/rootfs provenance + the jailer privilege model there and in the spec.

## Definition of done

- **Q1 + Q3 resolved (ADR-010 amendment) before any code** — the unblock gate.
- `firecrackerBackend.Argv` returns a jailer-wrapped launch + a bundle cleanup func; the bundle is
  created and torn down per run.
- The backend drives the REST API in the correct order with **no** `network-interfaces` PUT.
- The guest boots and runs `/usr/bin/sh /payload.sh`: `HELLO-FROM-GUEST` at exit 0; non-zero exit
  propagated; `sleep 30` under `timeout_sec=1` killed at ≈1s with `status:"timeout"` / exit 137.
- Firecracker runs under the jailer per the Q3 model; the host-side capture block in `Run()` is
  unchanged (proven by diff).
- The kernel/rootfs provenance is pinned + recorded (Q1); missing prereq → exit 127, no fall-back.
- `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE + recorded L5 evidence before promotion to ✅.
