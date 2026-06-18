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

This project follows **Unix philosophy** as its default design approach — favoring **composability over monolithic design**. The operating-system analogy is deliberate: Unix's lasting contribution is not any particular tool but the principle that complex behavior should emerge from combining small, independent components that communicate through standardized interfaces. Complex systems are built by chaining simple ones, not by growing one large one.

### The four structural properties to design for

- **Modularity** — break the system into independent units that can be built, understood, changed, and tested on their own. A module that "does two related things" is two modules. A function whose name needs an "and" in it is two functions.
- **Interface standardization** — components communicate through **stable, well-defined contracts**: typed function signatures, versioned APIs, plain-text formats (JSON, YAML, TOML, TSV), Unix-style pipes where it makes sense. A universal interface makes pieces composable without coordination. Bespoke interfaces make pieces captive.
- **Maintainability** — changes to one module should not require rewriting or redeploying the rest. If a one-line bug fix touches three unrelated services, the boundaries are wrong.
- **Reusability** — small, focused components should be combinable in ways the original author did not anticipate. Test: could this piece be lifted out and dropped into another project, or is it entangled with this one's specifics?

### Derived working rules

- **One thing, well** — each module, function, and service has a single clear responsibility.
- **Small, composable pieces over large configurable ones.** A 300-line function that branches on options is usually three 100-line functions that call each other. Composition is reversible; configuration flags accumulate and rarely get removed.
- **Plain text where possible.** Configs, intermediate artifacts, and data interchange in plain text. Plain text is inspectable with standard tools, greppable, version-controllable, diffable, and readable by both humans and agents.
- **Explicit over implicit.** Surface assumptions in code and types, not in comments or README paragraphs you hope someone reads.
- **Fail fast, crash loudly.** Unexpected state should raise or return an error, not be silently papered over with a default. Silent continuation is how subtle bugs become load-bearing.
- **Test in isolation.** Every component should be runnable and testable without spinning up the whole system. If you need the full stack to exercise one piece, the piece is too coupled.
- **Defer premature decisions.** No abstractions, plugin points, or configurability until the second or third concrete use case demands them. The cost of under-abstracting is one refactor; the cost of over-abstracting is a permanent tax on every future change.

### When composability is the wrong answer

Not every subsystem should be userspace-composable. **The Linux kernel itself is monolithic, and that is the right choice** — the performance, correctness, and safety properties of a kernel require tight internal coupling that a plug-in architecture would undermine. The same reasoning can apply to a hot-path runtime core, a tightly-coupled state machine, a cryptographic primitive, or a real-time control loop.

The principle is not *"always decompose"* — it is *"prefer composability for everything that lives at a user-facing or cross-module boundary, and be deliberate (with a documented ADR) when you go monolithic."* A monolithic core with composable userspace around it is the Unix pattern itself. **Monolithic is a legitimate choice; accidental monolithic is not.**

The `tier` seam is exec-sandbox's composability boundary: isolation backends plug in behind it
without touching the `run()` contract or the egress/credential model. When reviewing a change,
the architect agent weighs it against these principles and flags deviations.

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
