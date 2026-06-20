# ADR 001 — Foundational stack (bootstrap)

**Status:** Accepted
**Date:** 2026-06-18

## Context

exec-sandbox predates this ADR log. A working v0 (commit `206bcc7`,
*"exec-sandbox v0: bubblewrap isolation + egress proxy + vault.inject (v1 contract)"*) already
exists, ported from the tracer-bullet. This bootstrap ADR consolidates the decisions the
codebase already commits to as of 2026-06-18, so that subsequent ADRs (002, 003, …) have a
coherent baseline to amend or supersede rather than free-floating in a vacuum.

These are decisions **as observed in the code**, not new proposals. The authoritative design
they realize is `exec-sandbox.md` and `interface-contracts.md`
(v1).

## Decisions

### D1 — Language and packaging: single-binary Go CLI

Go 1.26, module `github.com/tkdtaylor/exec-sandbox`. One `main` package at the repo root
(`main.go`, `run.go`, `proxy.go`, `run_test.go`); no internal package split. Standard library
only — no third-party Go dependencies. Built with `go build -o bin/exec-sandbox ./...`.
Rationale: Go is the native ecosystem for bubblewrap/OCI/containerd; the tool is small enough
that root-level files are the right unit of organization.

### D2 — Invocation contract: JSON `RunRequest` on stdin, JSON result on stdout

`exec-sandbox run` reads one JSON `RunRequest` from stdin and writes one JSON result to stdout.
The contract (v1) is:

```
run(payload, profile, tier, secret_refs) -> { stdout, stderr, exit_code, sandbox_status }
```

Plain-text JSON over stdio is the standardized interface — inspectable, pipeable, and free of a
bespoke RPC layer. Deploy/test wiring (`vault_socket`, `audit_socket`, `origin_map`,
`request_id`, `injection_mode`) rides under a separate `wiring` object so it stays distinct
from the contract proper.

### D3 — Isolation substrate (Tier 1): bubblewrap with no network namespace

The payload runs under `bwrap --unshare-all --die-with-parent --clearenv` with a minimal
read-only root (`/usr`, `/etc`, conditionally `/bin /lib /lib64 /sbin`), `--proc`, `--dev`,
`--tmpfs /tmp`, the payload bind-mounted read-only as `/payload.sh`, and the egress proxy socket
bind-mounted as `/proxy.sock`. `--unshare-all` removes the network namespace entirely; there is
**no** `--share-net`. This is the load-bearing security control: the only path out of the
sandbox is the bind-mounted proxy socket.

### D4 — Network boundary owned by exec-sandbox: host-side egress proxy + allowlist

exec-sandbox owns the network boundary. The host-side `EgressProxy` (`proxy.go`) listens on a
Unix socket, enforces a domain allowlist derived from `profile.capabilities[NetConnect]`,
resolves allowlisted hosts to real origins via the `origin_map`, and forwards. A
non-allowlisted host gets `403 blocked-by-allowlist`; a host with no origin route gets `502
no-route`. v0 speaks HTTP over the Unix socket (single-layer); TLS-terminating + SOCKS5 and the
two-layer network-namespace egress filter are deferred to v1 (see README).

> **Superseded in part by ADR-011 (2026-06-20):** the forward-looking prediction that v1 adds a
> TLS-terminating + SOCKS5 proxy and the two-layer network-namespace egress filter is superseded —
> SOCKS5 and the two-layer filter are **rejected**; HTTPS via `CONNECT` is the real tracked gap.
> The v0 decision body above (single-layer HTTP proxy, domain allowlist, origin-map forwarding)
> stands unchanged.

### D5 — Credential injection owned by vault: `vault.inject` at spawn, proxy mode

`secret_refs` carries only opaque handles. At spawn, exec-sandbox calls
`vault.inject(handle, sandbox_identity, mode)` itself (pull-triggered push). In proxy mode vault
returns `{credential, binding:{host,header,scheme}}`, which exec-sandbox loads onto the egress
proxy via `SetCredential`. The proxy injects it into allowlisted outbound requests
(`Header: Scheme Value`, defaulting to `Authorization: Bearer`). **The credential value never
enters the sandbox** (not in env, args, or stdout) — it lives only at the proxy injection edge,
and the proxy's credentials are wiped at teardown. An env-mode delivery is noted in the
response shape but not the primary path. vault and audit-trail are reached over Unix-socket
JSON-lines IPC (`ipcCall`).

### D6 — Audit emission to audit-trail

exec-sandbox emits `spawn`, `inject_failed` (on a failed/denied injection), and `exit` events
to audit-trail over its Unix socket. Events carry `{actor, action, target (sandbox_id),
decision, context}`. Emission is best-effort: an empty `audit_socket` makes `emit` a no-op,
and failures do not abort the run.

### D7 — Tier seam: bubblewrap | gvisor | firecracker (v0 wires bubblewrap only)

The `tier` field accepts `bubblewrap | gvisor | firecracker`. v0 wires **bubblewrap only**
(`bwrapArgv`); the field is plumbed through to `sandbox_status.tier` and the spawn audit
context. gVisor (runsc, Tier 2) and Firecracker/Kata (Tier 3) are intended to plug in behind
this seam — modeled on the OCI Runtime Spec — without changing the `run()` contract or the
no-network + proxy-only-egress invariant. This is the project's deliberate composability
boundary.

### D8 — License: Apache-2.0

Apache-2.0 — free to use, modify, and distribute, including in commercial and proprietary
products. Exposes the block as a pluggable adapter seam without commercial-use restrictions.

## Consequences

- The no-network + proxy-only-egress invariant (D3/D4) and the secret-never-in-sandbox invariant
  (D5) are the security model. Future ADRs that touch the sandbox argv, the egress path, or the
  injection flow must preserve them or explicitly supersede this ADR with reasoning.
- Adding an isolation backend is an additive change behind the `tier` seam (D7), not a contract
  change.
- The stdlib-only constraint (D1) means any future dependency is a deliberate decision requiring
  its own ADR and a dep-scan pass.

## Future ADRs supersede or refine these

Each decision above is *what the codebase already commits to as of 2026-06-18*. ADR-002 onward
get their own files and may supersede a specific section here (e.g. wiring a gVisor backend
refines D7; adding the two-layer egress filter refines D4).
