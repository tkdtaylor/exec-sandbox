# Architecture Overview

**Project:** exec-sandbox
**Last updated:** 2026-06-18

## What this is

exec-sandbox is the OS execution-isolation block of the secure-agent ecosystem. It answers
one question: *when agent-generated code runs, is its execution boundary isolated from the
host and from other sandboxes?* It runs the code in a sandbox with **no network**, and its
**only** path out is a host-side egress proxy with a domain allowlist. `vault` plugs
credential injection into that proxy — in proxy mode the secret never enters the sandbox at
all. spawn/inject/exit events are emitted to `audit-trail`.

It is a single-binary Go CLI: `exec-sandbox run` reads a JSON `RunRequest` on stdin and writes
a JSON result on stdout. v0 implements **Tier-1 isolation (bubblewrap) only**, behind a `tier`
seam that is wired to accept `gvisor` and `firecracker` in later versions without changing the
`run()` contract.

## High-level design

The system is one process with three load-bearing modules at the repo root:

- **`main.go`** — the CLI entrypoint. Parses `argv` (requires the `run` subcommand), reads the
  JSON `RunRequest` from stdin, calls `Run()`, marshals the result to stdout.
- **`run.go`** — `Run()`, the orchestration core. In order: parse the egress allowlist from
  `profile.capabilities[NetConnect]`; mint a `sandbox_identity` (`sandbox_id` + random
  attestation); emit a `spawn` audit event; for each `secret_ref` call `vault.inject` and, in
  proxy mode, load the returned credential onto the egress proxy; start the Unix-socket egress
  proxy; write the payload to `payload.sh`; run it under `bwrap --unshare-all --die-with-parent
  --clearenv` with the proxy socket bind-mounted in (no network namespace); capture
  stdout/stderr/exit_code; emit an `exit` audit event; return the result. `run.go` also holds
  the bubblewrap argv builder (`bwrapArgv`) and the Unix-socket JSON-lines IPC helper
  (`ipcCall`) used to reach vault and audit-trail.
- **`proxy.go`** — `EgressProxy`, the host-side egress proxy. Listens on a Unix socket
  (bind-mounted into the sandbox as `/proxy.sock`), enforces the domain allowlist, injects
  credentials into allowlisted outbound requests, and forwards to the real origin resolved via
  the `origin_map`. The sandbox never possesses the credential — it lives only here, at the
  injection edge.

The diagrams for these flows live in [diagrams.md](diagrams.md). The structured element
catalog is in [`../spec/architecture.md`](../spec/architecture.md). The full source-of-truth
spec lives in [`docs/spec/`](../spec/) — start there to know what the system *does and is*
today (behaviors, data model, interfaces, configuration).

## Key decisions

| Decision | Choice | ADR |
|----------|--------|-----|
| Isolation substrate (Tier 1) | bubblewrap (`bwrap --unshare-all`) — no network namespace | [001](decisions/001-foundational-stack.md) |
| Network boundary | exec-sandbox owns it: `--unshare-all` + host-side egress proxy on a Unix socket + domain allowlist | [001](decisions/001-foundational-stack.md) |
| Credential injection | vault owns it: `vault.inject(handle, sandbox_identity, mode)`; proxy mode keeps the secret out of the sandbox | [001](decisions/001-foundational-stack.md) |
| Tier seam | `tier = bubblewrap \| gvisor \| firecracker`; v0 wires bubblewrap only, behind an OCI-style seam | [001](decisions/001-foundational-stack.md) |
| Language | Go (bubblewrap / OCI / containerd ecosystem) | [001](decisions/001-foundational-stack.md) |
| IPC to vault / audit | Unix-socket JSON-lines (`ipcCall`) | [001](decisions/001-foundational-stack.md) |

## Data flow

A request enters as a JSON `RunRequest` on stdin (`{run:{payload,profile,tier,secret_refs},
wiring:{vault_socket,audit_socket,origin_map,request_id,injection_mode}}`). `Run()` derives the
egress allowlist from the profile, mints a sandbox identity, and emits a `spawn` audit event.
For each secret handle it calls `vault.inject` over the vault socket; a proxy-mode response
carries `{credential, binding:{host,header,scheme}}`, which is loaded onto the egress proxy and
**never** flows into the sandbox. The egress proxy starts on a Unix socket; the payload runs
under bubblewrap with no network namespace and that socket bind-mounted as `/proxy.sock`.
Allowlisted HTTP requests from the payload go out through the proxy (which injects the
credential and forwards to the origin); non-allowlisted hosts get a `403`. On exit, `Run()`
emits an `exit` audit event and returns `{stdout, stderr, exit_code, sandbox_status}` as JSON
on stdout. The proxy is stopped and its credentials wiped at teardown.

## External dependencies

| Dependency | Purpose | Notes |
|------------|---------|-------|
| bubblewrap (`bwrap`) | Tier-1 sandbox: process/namespace isolation, no network namespace | Runtime binary, looked up on `PATH`; integration tests skip if absent |
| vault (block) | Credential injection via `vault.inject` | Reached over a Unix socket (`vault_socket`), JSON-lines IPC; optional — skipped if no `secret_refs` / socket |
| audit-trail (block) | spawn / inject / exit event sink | Reached over a Unix socket (`audit_socket`), JSON-lines IPC; optional — `emit` is a no-op if the socket is empty |
| Go standard library | Everything else (`net/http`, `os/exec`, `encoding/json`, `crypto/rand`) | No third-party Go dependencies |

## Design principles

exec-sandbox follows **Unix philosophy** — composability over monolithic design. The full
statement lives in `AGENTS.md`; the load-bearing instance here is the `tier` seam: a small,
well-defined boundary that lets isolation backends (bubblewrap → gVisor → Firecracker) plug in
without touching the `run()` contract or the egress/credential model. The execution core itself is
deliberately cohesive (a monolithic choice for correctness on the isolation path) — composability
lives at the `tier` boundary, not inside `run()`. When reviewing a change, the architect agent
weighs it against these principles and flags deviations.

## Constraints and non-goals

- **Does not own credential storage or minting** — that is vault's job. exec-sandbox only
  presents `{handle, sandbox_identity}` and loads what vault returns onto the proxy.
- **Does not own audit storage / querying** — it emits events to audit-trail and forgets them.
- **Does not implement Tier 2/3 yet** — gVisor/Firecracker/Kata are accepted by the `tier`
  field but not wired in v0.
- **Not a generic dev container or a general process runner** — it is the execution-box profile
  for agent-generated code, with the no-network + proxy-only-egress invariant as its reason to
  exist.
- **No persistent state** — every run is ephemeral (a fresh temp dir, a fresh proxy, wiped at
  teardown).
