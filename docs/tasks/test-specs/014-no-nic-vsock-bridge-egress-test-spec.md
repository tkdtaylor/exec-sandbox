# Test Spec 014: No-NIC + vsock-bridge egress enforcement

**Linked task:** [`docs/tasks/backlog/014-no-nic-vsock-bridge-egress.md`](../backlog/014-no-nic-vsock-bridge-egress.md)
**ADR:** ADR 010 D2 (the egress crux: no virtio-net device + a vsock bridge to the host proxy; rejected TAP+nftables alternative). Resolves ADR 010 **Q4** (vsock shim location/lifecycle) as an in-task decision recorded in `docs/spec/` + ADR-010 note.
**Written:** 2026-06-20

## Context for the test author

This is the **egress crux** of the Firecracker tier and the **highest-assurance** task in the epic
(target L6). It re-expresses the project's two load-bearing invariants — **no network** (F-001) and
**credential never enters the sandbox** (F-002) — in microVM terms.

A microVM has neither `--unshare-all` nor an OCI netns. Per ADR 010 D2 the invariants are
re-enforced by:

1. **No NIC, ever.** The config issues no `network-interfaces` body, so the guest has no network
   device at all (no TAP, no bridge, no route). "No network" is true *by omission* — the analogue
   of `bwrap --unshare-all`. Adding a NIC is forbidden by the same rule that forbids `--share-net`
   (CLAUDE.md invariant). The **rejected** alternative is TAP+nftables (D2: re-introduces a real
   netstack and fails *open* on a missing rule).
2. **Proxy reached over virtio-vsock, not a bind-mount.** A microVM cannot bind-mount the host
   `/proxy.sock` (separate kernel, separate VFS). The config wires a `vsock` device with a host-side
   `uds_path`; the host side of the vsock terminates at the **existing `EgressProxy`** (`proxy.go`),
   unchanged — it still enforces the domain + per-host verb allowlist and injects credentials
   host-side.
3. **A dumb guest-side shim presents `/proxy.sock` to the payload.** A tiny guest-side forwarder
   listens on `/proxy.sock` inside the guest and pumps bytes over the vsock to the host proxy. It is
   a **byte pump only** — it does NOT parse HTTP, hold credentials, or make allowlist decisions. The
   payload's contract is unchanged across tiers: it always talks to a Unix socket at `/proxy.sock`.
4. **The credential never enters the guest.** The proxy injects the credential header host-side,
   *after* the request crosses the vsock back to the host — identical to the bind-mount case. The
   credential value never appears in guest env, args, stdout, or memory. The vsock bridge is a
   *transport substitution* for the bind-mount, not a change to the trust boundary (F-002 verbatim).

**ADR 010 Q4 must be decided in this task** (and recorded): does the `/proxy.sock` shim ship inside
the rootfs image, get injected at boot, or *be* the guest init — and how is its dumbness audited.
Record the disposition in the task's readiness note, in `docs/spec/behaviors.md` (the egress flow),
and as a note on ADR-010.

Ground truth to mirror:
- `EgressProxy.handle` injects the credential at the proxy edge via `out.Header.Set(...)`
  (`proxy.go:126`); the sandbox never sees `EgressProxy.creds`. This is unchanged here.
- `firecrackerConfig(...)` from task 013 already omits the NIC and wires the vsock `uds_path`; this
  task wires the **host side** of that vsock to the live `EgressProxy` and ships the **guest side**
  shim.
- The proxy-mode F-002 leak surfaces are: spawn argv, sandbox env, payload, returned stdout (task
  009 TC-009-07). For the microVM these extend to: guest env, guest args, guest stdout — the
  credential must appear in none.

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-014-01 | The generated config wires the vsock device's host `uds_path` to a path served by the live `EgressProxy`; the proxy is reached over vsock with no code change to `proxy.go`'s allowlist/verb/inject logic | TC-014-01, TC-014-02 | ✅ |
| REQ-014-02 | No `network-interfaces` body is ever issued and the config has no NIC — re-asserted in the wired (vsock-live) shape, not just the bare skeleton from task 013; an attempt to add a NIC is rejected by the same guard (microVM F-001) | TC-014-03 (positive), TC-014-04 (negative) | ✅ |
| REQ-014-03 | The guest-side `/proxy.sock` shim is a dumb byte pump: it forwards bytes between an in-guest `/proxy.sock` listener and the vsock channel, and does NOT parse HTTP, hold/inspect credentials, or make allowlist/verb decisions (those stay host-side in `EgressProxy`) | TC-014-05, TC-014-06 | ✅ |
| REQ-014-04 | A proxy-mode credential value injected host-side appears in NONE of: the guest env, the guest args, the guest stdout, the spawn argv, or the returned `stdout` — the credential never crosses the vsock into the guest (microVM F-002) | TC-014-07 (positive end-to-end), TC-014-08 (negative leak-detector) | ✅ |
| REQ-014-05 | A payload talking to `/proxy.sock` inside the microVM reaches an allowlisted host (200) and is blocked for a non-allowlisted host (403) — identical observable behavior to bubblewrap/gVisor; the proxy is the sole egress (no NIC, no second route) | TC-014-09 (allow), TC-014-10 (block) | ✅ |
| REQ-014-06 | ADR 010 Q4 (shim location/lifecycle) is resolved and recorded in `docs/spec/behaviors.md` (the microVM egress flow) + an ADR-010 note; the shim's dumbness is documented as an auditable property | TC-014-11 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] The no-NIC and credential-leak negative cases are specified (provably-not-no-op detectors)
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: `proxy.go` allowlist/verb/inject logic is UNCHANGED — vsock is a transport swap only
- [x] Confirmed: Q4 (shim location/lifecycle) is an in-task decision to record, not pre-decided here
- [x] Target verification level: L6 (operator-observed end-to-end: allowed host 200, blocked 403, credential absent from the guest) — requires `/dev/kvm` + firecracker; integration tests skip-guard when absent (mirroring `requireBwrap`)

---

## Test cases

### TC-014-01: vsock host uds_path is wired to the live EgressProxy

- **Requirement:** REQ-014-01
- **Type:** unit/integration (Go test)
- **Input:** build the run wiring for the firecracker tier with a fresh `EgressProxy` started on a
  host socket, and the vsock `uds_path` pointed at the host side of that bridge.
- **Expected:** the config's vsock `uds_path` resolves to a path the host bridge serves and that
  forwards to the started `EgressProxy`; no field of `proxy.go` (allowlist, verbAllowlist, creds,
  handle) is modified to make this work — the proxy is reached as-is over the vsock transport.

### TC-014-02: proxy.go allowlist/verb/inject logic is byte-for-byte unchanged

- **Requirement:** REQ-014-01
- **Type:** inspection + unit (Go test)
- **Input:** diff `proxy.go` against the pre-task baseline; run the existing proxy unit tests
  (verb allowlist, host allowlist, credential injection — F-009/F-002 tests).
- **Expected:** `proxy.go`'s `handle`/`SetCredential`/`Wipe`/allowlist logic is unchanged; all
  existing proxy tests still pass. The vsock bridge is new host-side wiring + a guest shim, NOT a
  fork of the proxy.

### TC-014-03: wired (vsock-live) config still carries no NIC

- **Requirement:** REQ-014-02
- **Type:** unit (Go test)
- **Input:** the full firecracker config as wired for a live run (vsock pointed at the proxy
  bridge), serialized.
- **Expected:** no `network-interface`/`network-interfaces` key anywhere — re-asserts TC-013-04 on
  the *wired* shape (not just the bare skeleton), proving the vsock wiring did not sneak in a NIC.

### TC-014-04: adding a NIC is rejected by the microVM no-NIC guard (negative)

- **Requirement:** REQ-014-02
- **Type:** unit (Go test, negative)
- **Input:** feed the no-NIC guard a config mutated to include a `network-interfaces` entry.
- **Expected:** the guard returns a non-nil error / the run-build path refuses it — there is no
  flag, field, or code path that produces a NIC. This is the microVM equivalent of "there is no
  `--share-net`."

### TC-014-05: the guest-side shim forwards bytes both directions (dumb pump)

- **Requirement:** REQ-014-03
- **Type:** unit (Go test, the shim in isolation)
- **Input:** drive the shim with a fake in-guest `/proxy.sock` client on one end and a fake vsock
  endpoint on the other; send a byte sequence each direction.
- **Expected:** every byte written to the `/proxy.sock` side appears unmodified on the vsock side
  and vice versa — a transparent bidirectional pump. The shim does not buffer-and-parse, rewrite,
  or drop bytes.

### TC-014-06: the shim never parses HTTP, holds a credential, or makes an allowlist decision

- **Requirement:** REQ-014-03
- **Type:** inspection + unit (Go test)
- **Input:** read the shim source; drive it with a request to a NON-allowlisted host and with a
  request carrying no credential.
- **Expected:** the shim contains no HTTP parsing, no allowlist/verb map, no credential storage —
  it forwards the bytes regardless, and the **host-side `EgressProxy`** is what returns
  `403 blocked-by-allowlist` (the decision stays host-side). The shim has no knowledge of which
  hosts are allowed; it cannot leak a credential because it never holds one.

### TC-014-07: credential value never reaches the guest (positive, end-to-end)

- **Requirement:** REQ-014-04
- **Type:** integration (Go test) — **load-bearing, target L6**
- **Input:** a firecracker run with a proxy-mode credential `SetCredential(host,
  Credential{Value:"SENTINEL-SECRET-abc123", …})` and a payload that dumps its env, args, and
  echoes a request to `/proxy.sock`. Requires `/dev/kvm` + firecracker (skip-guard otherwise).
- **Expected:** the sentinel value appears in NONE of: the guest `env` output, the guest `args`,
  the guest stdout, the spawn argv, or the returned `result["stdout"]`. The allowlisted upstream
  request still succeeds (credential injected host-side, observed by the origin), proving the
  credential was injected *after* the vsock hop, never inside the guest.

### TC-014-08: the leak-detector catches a credential that did reach the guest (negative)

- **Requirement:** REQ-014-04
- **Type:** unit (Go test, negative)
- **Input:** feed the F-002-microVM leak-scan a constructed guest-surface set (guest env / args /
  stdout) that DOES contain the sentinel value.
- **Expected:** the scan returns a non-nil error / a test using it fails — proving the guest-side
  leak check is not vacuous. Mirrors task 009 TC-009-08.

### TC-014-09: payload reaches an allowlisted host over /proxy.sock in the microVM (200)

- **Requirement:** REQ-014-05
- **Type:** integration (Go test) — target L6
- **Input:** a firecracker run whose profile allowlists `api.example.test`, with a stub origin and
  a payload that GETs `http://api.example.test/...` via `/proxy.sock`. Requires `/dev/kvm` +
  firecracker.
- **Expected:** the request returns 200; the stub origin observes exactly one request — identical
  observable behavior to the bubblewrap/gVisor allowlisted-host tests. The egress went over the
  vsock to the host proxy and out; there is no NIC and no second route.

### TC-014-10: payload is blocked for a non-allowlisted host (403)

- **Requirement:** REQ-014-05
- **Type:** integration (Go test) — target L6
- **Input:** same run; the payload GETs a host NOT on the allowlist.
- **Expected:** `403 blocked-by-allowlist`; the origin observes zero requests. A direct
  network attempt (bypassing `/proxy.sock`) fails because there is no NIC — assert a raw
  TCP/connect from the guest fails (no route), the microVM analogue of the gVisor
  "direct net FAILED-no-network" assertion.

### TC-014-11: Q4 (shim location/lifecycle) resolved and recorded

- **Requirement:** REQ-014-06
- **Type:** inspection (spec + ADR)
- **Input:** read `docs/spec/behaviors.md` (the microVM egress flow) and the ADR-010 Q4 note after
  the feat commit.
- **Expected:** the chosen shim location/lifecycle (rootfs-resident vs boot-injected vs guest-init)
  is stated as the current truth in `behaviors.md` (present tense, no future tense), the shim's
  dumbness is described as an auditable property, and ADR-010's Q4 carries a resolution note
  pointing to the spec. The credential-never-in-guest invariant is restated in microVM terms in the
  spec.

---

## Post-implementation verification

- [ ] TC-014-01..02: vsock wired to the live proxy; `proxy.go` unchanged + its tests green
- [ ] TC-014-03..04: wired config carries no NIC; the no-NIC guard rejects a constructed NIC
- [ ] TC-014-05..06: the shim is a dumb bidirectional pump with no HTTP/credential/allowlist logic
- [ ] TC-014-07: credential value absent from guest env/args/stdout + argv + result (L6)
- [ ] TC-014-08: the guest-side leak-detector fails on a constructed leak (not a no-op)
- [ ] TC-014-09..10: allowlisted host 200, non-allowlisted 403, direct net fails (no NIC)
- [ ] TC-014-11: Q4 resolved and recorded in `behaviors.md` + ADR-010 note

## Test framework notes

- Standard Go `testing`. The shim-pump tests (TC-014-05/06) and the no-NIC guard tests
  (TC-014-03/04) run without `/dev/kvm`. The end-to-end credential and allow/block tests
  (TC-014-07/09/10) need `/dev/kvm` + firecracker and MUST skip-guard when absent (mirror
  `requireBwrap` / the runsc skip), never silently pass.
- The microVM F-002 leak-scan helper should extend the host-side surface set (argv/env/payload/
  stdout) with the **guest** surfaces (guest env/args/stdout) so task 018's `fitness-cred-not-in-guest`
  can reuse it.
- Depends on task 013 (the config skeleton + the no-NIC helper) landing first.
