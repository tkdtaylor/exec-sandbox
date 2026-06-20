# Task 014: No-NIC + vsock-bridge egress enforcement

**Status:** ⬜ backlog
**Branch:** `task/014-no-nic-vsock-bridge-egress`
**Spec:** [`docs/tasks/test-specs/014-no-nic-vsock-bridge-egress-test-spec.md`](../test-specs/014-no-nic-vsock-bridge-egress-test-spec.md)
**ADR:** ADR 010 D2 (the egress crux: no virtio-net + vsock bridge to the host proxy; TAP+nftables rejected). **Resolves ADR 010 Q4** (vsock shim location/lifecycle) as an in-task decision recorded in `docs/spec/behaviors.md` + an ADR-010 note.

## Readiness

**READY after task 013 lands** (it needs the config skeleton + the reusable no-NIC helper). It has
**no external blocker** — Q4 (the shim location/lifecycle) is an **in-task decision this task makes
and records**, not a precondition. This is the **egress crux** of the epic and the
**highest-assurance** task (target **L6**).

**Dependency position:** 013 → **014** → 015 → {016, 017} → 018.

## Problem

A microVM has neither `bwrap --unshare-all` nor a gVisor OCI netns, and it **cannot bind-mount** the
host `/proxy.sock` (separate kernel, separate VFS). The two load-bearing invariants must be
re-expressed in microVM terms (ADR 010 D2):

- **No network (F-001).** The config issues no `network-interfaces` body, so the guest has **no NIC
  at all** — no TAP, no bridge, no route. "No network" is true by omission. The **rejected**
  alternative is TAP + nftables (fails *open* on a missing rule); never add a real NIC or route.
- **Credential never enters the sandbox (F-002).** The proxy is reached over a **virtio-vsock**
  device whose host side terminates at the **existing `EgressProxy`** (`proxy.go`, unchanged). A
  **dumb guest-side shim** presents `/proxy.sock` to the payload and pumps bytes over the vsock; it
  does NOT parse HTTP, hold credentials, or make allowlist decisions. The credential is injected
  host-side, *after* the vsock hop — it never crosses into the guest.

Task 013 produced a config that omits the NIC and names a vsock `uds_path`; this task **wires the
host side of that vsock to the live `EgressProxy`** and **ships the guest-side shim**.

## Q4 — in-task decision to make and record

**Q4 (ADR 010): vsock shim location and lifecycle.** Decide whether the `/proxy.sock` shim ships
inside the rootfs image, is injected at boot, or *is* the guest `init` — and how its dumbness is
audited. Record the disposition in `docs/spec/behaviors.md` (the microVM egress flow), as a
resolution note on ADR-010 Q4, and keep the shim a **byte pump only** (auditably no HTTP/credential/
allowlist logic).

## Scope

- **Wire the host side of the vsock to the live `EgressProxy`**: the vsock `uds_path` from task 013's
  config resolves to a host bridge that forwards to the started proxy. **`proxy.go` is NOT modified**
  — allowlist, verb, credential-injection logic stay exactly as today (the vsock is a transport swap
  for the bind-mount, nothing more).
- **Ship the guest-side `/proxy.sock` shim** (per the Q4 decision): a dumb bidirectional byte pump
  between an in-guest `/proxy.sock` listener and the vsock channel. No HTTP parsing, no credential
  storage, no allowlist/verb map.
- **Re-assert the no-NIC invariant on the wired shape** using the task-013 helper (the vsock wiring
  must not sneak in a NIC); the guard rejects any attempt to add a `network-interfaces` entry.
- **Extend the F-002 leak-scan** (the task-009 surface set: argv/env/payload/stdout) with the
  **guest** surfaces (guest env/args/stdout), as a reusable helper task 018's
  `fitness-cred-not-in-guest` consumes.
- **Spec update in the same commit:** `docs/spec/behaviors.md` gains the microVM egress flow (guest
  `/proxy.sock` shim → vsock → host `EgressProxy`; credential injected host-side after the hop; no
  NIC), Q4 resolved; ADR-010 carries a Q4 resolution note.

Out of scope: booting the guest / driving the REST API end-to-end (task 015 — but the integration
TCs here ride on 015's launch path once it lands; until then the shim-pump + no-NIC-guard +
leak-scan halves run without a guest); limits mapping (016); mounts (017); teardown + fitness
umbrella (018). Do NOT add a `--share-net`, a NIC, a TAP, or any route.

## Verification plan

- **Highest level achievable: L6 (per ADR-010 decomposition — the highest-assurance task).**
  Operator-observed end-to-end: an allowlisted host returns 200 over `/proxy.sock`-in-guest, a
  non-allowlisted host returns 403, a direct (NIC-less) network attempt fails, and a proxy-mode
  credential value appears in **none** of the guest env/args/stdout/argv/result. Requires `/dev/kvm`
  + firecracker (rides on task 015's launch path); the shim-pump, no-NIC-guard, and leak-scan
  negative cases are L2 and run everywhere.
- **Harness command:** `go test -count=1 -run 'Vsock|Shim|NoNIC|CredNotInGuest|FirecrackerEgress' ./...`;
  the end-to-end allow/block/credential TCs under `/dev/kvm`; `go test -count=1 ./...`; `gofmt -l .`.
- **Runtime observation (L6):** paste the allowlisted-host `200` + origin-hit line, the
  non-allowlisted `403 blocked-by-allowlist` + zero-origin-hits line, the direct-net-fails line
  (no NIC), and the assertion that the sentinel credential value is absent from guest
  env/args/stdout, the spawn argv, and `result["stdout"]`. Show the shim contains no HTTP/credential/
  allowlist logic (inspection + the dumb-pump test). Show the leak-scan negative case fails on a
  constructed guest leak.
- **ADR:** no new ADR; record the **Q4 resolution** as a note on ADR-010 + the spec.

## Definition of done

- The vsock host side is wired to the live `EgressProxy`; `proxy.go` is byte-for-byte unchanged and
  its existing tests still pass.
- The guest-side `/proxy.sock` shim is a dumb bidirectional pump with no HTTP/credential/allowlist
  logic (proven by the pump test + inspection).
- The wired config still carries no `network-interface` key; the no-NIC guard rejects a constructed
  NIC config.
- A proxy-mode credential value appears in none of: guest env, guest args, guest stdout, spawn argv,
  returned `stdout` (L6, end-to-end); the microVM leak-scan fails on a constructed guest leak.
- An allowlisted host returns 200 and a non-allowlisted host 403 over `/proxy.sock`-in-guest; a
  direct network attempt fails (no NIC).
- Q4 resolved and recorded in `docs/spec/behaviors.md` (present tense, no future tense) + an
  ADR-010 note; the credential-never-in-guest invariant restated in microVM terms.
- `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE + recorded L6 evidence before promotion to ✅.
