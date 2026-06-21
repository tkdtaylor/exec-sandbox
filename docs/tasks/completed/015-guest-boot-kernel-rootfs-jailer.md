# Task 015: Guest boot — kernel image + rootfs + direct firecracker launch (no jailer)

**Status:** ⬜ backlog
**Branch:** `task/015-guest-boot-kernel-rootfs-jailer`
**Spec:** [`docs/tasks/test-specs/015-guest-boot-kernel-rootfs-jailer-test-spec.md`](../test-specs/015-guest-boot-kernel-rootfs-jailer-test-spec.md)
**ADR:** ADR 010 D1 (drive Firecracker via REST-over-Unix-socket), D3 (workload → guest kernel + rootfs, mirroring the OCI bundle), **Amendment 1 (2026-06-20)** — A1.Q1 (kernel/rootfs sourcing) + A1.Q3 (**no jailer**: run `firecracker` directly under the existing unprivileged `bwrap --unshare-all` + `limits.go` model).

> **Filename note:** the file/branch still say `...-jailer` for stability (renaming task/source files
> is an "ask first" action), but the **jailer is dropped** per ADR-010 Amendment 1 A1.Q3. Read every
> "jailer" reference in older artifacts as "direct firecracker under bwrap `--unshare-all`."

## Readiness — READY (Q1 + Q3 resolved)

**This task is READY.** ADR-010 Amendment 1 (2026-06-20) resolved the two open questions that
previously blocked it:

- **A1.Q1 — Guest kernel + rootfs sourcing (RESOLVED).** Build the **uncompressed x86_64 ELF
  `vmlinux`** (`make vmlinux`; **not** `bzImage`) and a minimal **Alpine ext4** rootfs **from source
  as build-time tooling** (zero runtime third-party deps). Target the newest upstream-supported kernel
  line per Firecracker's `kernel-policy.md`, **floor linux 6.1**. **FLAG:** linux 6.1's upstream
  support ends **2026-09-02** (~2.5 months out) — pin the **newest non-EOL line available in upstream
  `resources/guest_configs/` at build time**, preferring 6.1's successor if published; do not hard-code
  6.1 if a newer supported line exists. **Vendor three pinned things** into the repo: the `vmlinux`
  artifact, its `sha256`, and a copied-in `microvm-kernel-ci-x86_64-<ver>.config` + a `PROVENANCE`
  note (upstream Firecracker commit + linux git tag). Mount the rootfs **`is_read_only: true`**, baking
  in **only** the trusted vsock→`/proxy.sock` shim (task 014, part of the TCB) + `/sbin/init`; pin
  `base.ext4` by `sha256`. The **untrusted payload is never baked into the base** — it is copied in to
  a separate writable drive / the writable `/work` surface (Q2 / task 017). A stdlib `crypto/sha256`
  Go loader verifies **both** digests before use and **fails fast / crashes loudly** on mismatch. The
  pin is the supply-chain control (`dep-scan` does not cover a kernel image); both artifacts are
  scannable by `code-scanner`.

  Recommended layout (per Amendment 1):
  ```
  guest/
    kernel/  vmlinux-<ver>  vmlinux.sha256  config/microvm-kernel-ci-x86_64-<ver>.config  config/PROVENANCE
    rootfs/  base.ext4  base.ext4.sha256  build.sh   # RO base: vsock shim + /sbin/init; build.sh is build-time only
  ```

- **A1.Q3 — Privilege model (RESOLVED): NO jailer.** Run the `firecracker` binary **directly** under
  exec-sandbox's existing unprivileged model, reconstructing jailer-equivalent isolation with
  `bwrap --unshare-all` + `limits.go`. Firecracker **self-installs its seccomp filters regardless** of
  the jailer (the highest-value syscall-surface reduction is not lost); bwrap supplies the chroot +
  mnt/pid/ipc/net namespaces + cgroup limits the jailer would otherwise provide. This keeps Tier-3
  **unprivileged**, consistent with Tier-1/Tier-2 — adopting the jailer would have made Tier-3 the one
  tier demanding root, a structural regression on the highest-risk tier.

  **Host requirement (verbatim, per Amendment 1):**
  > Tier-3 (Firecracker) requires KVM-capable hardware and the exec-sandbox host user to be a member
  > of the `kvm` group (or an equivalent ACL granting rw on `/dev/kvm`). It requires NO root, NO
  > setuid launcher, and NO elevated capabilities beyond `/dev/kvm` access — preserving the Tier-1/2
  > unprivileged invariant. The bwrap `--unshare-all` wrapper supplies the chroot + mnt/pid/ipc/net
  > namespaces + cgroup limits the jailer would otherwise provide; firecracker self-installs its
  > seccomp filters regardless.

  **Accepted risk / test obligation:** by skipping the jailer, this tier owns the responsibility of
  reproducing jailer-equivalent constraints. The **constraints ≥ jailer** fitness rule is registered
  in **task 018**; its assertion is **exercised in this task's Verification plan** (TC-015-05).

**Dependency position:** 013 → 014 → **015** → {016, 017} → 018. Needs 013 (config skeleton) and 014
(egress wiring) landed; this task makes the guest actually boot and run.

## Problem

Tasks 013/014 produce a NIC-less, vsock-bridged microVM **config** but **nothing boots**. This task
launches Firecracker **directly** (no jailer — A1.Q3) under exec-sandbox's existing
`bwrap --unshare-all` + `limits.go` unprivileged wrapper, via the REST-over-Unix-socket API
(`PUT /machine-config`, `/boot-source`, `/drives/...`, `/vsock`, then `PUT /actions {InstanceStart}`),
boots the verified guest kernel + rootfs (A1.Q1), runs `/usr/bin/sh /payload.sh`, and captures
stdout/stderr/exit through the **unchanged** host-side path in `Run()` (`run.go:154-194` —
`exec.CommandContext` + `capWriter` + process-group SIGKILL on the deadline + exit-code mapping). The
Firecracker process is the spawned child; `timeout_sec`/`max_output_bytes` stay host-side above the
seam (ADR 010 D4) with no backend involvement.

## Scope

- **`firecrackerBackend.Argv` returns the launch under the unprivileged bwrap wrapper** (A1.Q3 —
  **no jailer**): the argv names the `firecracker` binary, the per-run api socket, and the bundle dir,
  wrapped by exec-sandbox's existing `bwrap --unshare-all` + `limits.go` machinery (chroot +
  mnt/pid/ipc/net namespaces + cgroup limits + non-host uid); a `cleanup` func removes the bundle
  (mirroring `gvisorBackend.Argv`).
- **Verify the pinned kernel + rootfs before use (A1.Q1):** a stdlib `crypto/sha256` loader checks
  `vmlinux.sha256` and `base.ext4.sha256` and **fails fast / crashes loudly** on mismatch — no boot
  from an unverified artifact. The rootfs is mounted `is_read_only: true` and carries the task-014
  vsock→`/proxy.sock` shim + `/sbin/init`; the untrusted payload is copied in to a separate writable
  surface, never baked into the RO base. Record the provenance (`PROVENANCE` note: upstream Firecracker
  commit + linux git tag) and the supply-chain stance (the pin is the control).
- **Drive the REST API in order** (machine-config → boot-source → drives → vsock → `InstanceStart`)
  over the Unix socket, the way `gvisor.go` shells out to `runsc`. **No `PUT /network-interfaces`**
  ever appears in the sequence (the no-NIC invariant at the API level). Boot args
  `console=ttyS0 reboot=k panic=1`; **no `ip=` arg** (no NIC to configure — reinforces no-NIC).
- **Capture flows through the UNCHANGED `Run()` path**: do NOT modify the host-side capture block —
  the firecracker child becomes a well-behaved member of it (clean exit → its code; non-zero → its
  code; timeout → process-group SIGKILL → 137 + `status:"timeout"`).
- **Missing `firecracker`/`/dev/kvm`/kernel/rootfs, or a `/dev/kvm` the host user cannot access
  (not in `kvm` group) → spawn error (exit 127)**, never a silent fall-back to a weaker tier (mirrors
  the gVisor `runsc`-absent behavior). **No jailer binary is a prerequisite.**

Out of scope: the limits→machine-config mapping semantics (task 016); `/work`/FileRead presentation
(task 017); teardown reclaim + fitness umbrella (task 018 — basic bundle cleanup is here, the
constraints-≥-jailer + cgroup/namespace reclaim fitness is 018); VMM-native snapshot (D6, out of scope
entirely). Do NOT modify the `Run()` host-side capture block.

## Verification plan

- **Highest level achievable: L5 (per ADR-010 decomposition).** A validation harness boots a guest,
  runs a payload, and captures stdout/exit end-to-end. Requires `/dev/kvm` (host user in the `kvm`
  group) + firecracker + the A1.Q1 verified kernel/rootfs; the REST-order test (against a fake
  endpoint) and the missing-prereq test are L2 and run without `/dev/kvm`. **No jailer binary is
  required** (A1.Q3).
- **Harness command:** `go test -count=1 -run 'Firecracker|GuestBoot|RestOrder|Constraints' ./...`;
  the boot/run/timeout TCs under `/dev/kvm`; `go test -count=1 ./...`; `gofmt -l .`.
- **Runtime observation (L5):** paste the `HELLO-FROM-GUEST` stdout + `exit 0` line (TC-015-04); the
  non-zero `exit 3` line (TC-015-08); the `status:"timeout"` + `exit 137` line for `sleep 30` under
  `timeout_sec=1` (TC-015-09); the recorded REST PUT order with **no** `network-interfaces` PUT
  (TC-015-03); the `exit 127` on a host without firecracker / without `/dev/kvm` access (TC-015-10).
- **Constraints ≥ jailer (A1.Q3, TC-015-05):** exercise the task-018 fitness assertion against a live
  Tier-3 run — the firecracker child runs as a **non-host uid**, with **all namespaces unshared**
  (none shared with the host), **cgroup limits applied**, **chroot/`pivot_root` in effect**, and (with
  A1.Q1) the credential / host FS never leaking into the guest. The jailer-equivalent constraints are
  observed, not assumed.
- **ADR:** the Q1 + Q3 resolution is recorded in **ADR-010 Amendment 1 (2026-06-20)** — kernel/rootfs
  provenance (A1.Q1) + the no-jailer unprivileged model (A1.Q3). No further ADR needed for this task.

## Definition of done

- The kernel + rootfs are verified by a stdlib `crypto/sha256` loader before boot, **failing fast on
  mismatch** (A1.Q1); the RO rootfs carries only the trusted shim + `/sbin/init`; the payload is copied
  in to a writable surface, never baked into the base. Provenance is pinned + recorded.
- `firecrackerBackend.Argv` returns a launch under the unprivileged `bwrap --unshare-all` + `limits.go`
  wrapper (**no jailer**, A1.Q3) + a bundle cleanup func; the bundle is created and torn down per run.
- The backend drives the REST API in the correct order with **no** `network-interfaces` PUT.
- The guest boots and runs `/usr/bin/sh /payload.sh`: `HELLO-FROM-GUEST` at exit 0; non-zero exit
  propagated; `sleep 30` under `timeout_sec=1` killed at ≈1s with `status:"timeout"` / exit 137.
- The firecracker child's effective constraints are **≥ jailer** (non-host uid, all namespaces
  unshared, cgroup limits applied, chroot/`pivot_root` in effect) — the task-018 fitness assertion
  passes against the live run (TC-015-05); the host-side capture block in `Run()` is unchanged (proven
  by diff).
- Missing `firecracker`/`/dev/kvm`/kernel/rootfs or an inaccessible `/dev/kvm` → exit 127, no
  fall-back.
- `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE + recorded L5 evidence before promotion to ✅.
