# exec-sandbox — Authoritative Spec

**Project:** exec-sandbox
**Last updated:** 2026-06-20

## What this directory is

`docs/spec/` is the **authoritative current-state snapshot** of exec-sandbox. It answers the question:

> "If the code were deleted tomorrow, what would I need to write down to rebuild it?"

The spec is dual-natured:

- **Output of current sessions** — every completed task that changes externally-observable behavior, the data model, an interface, or configuration must update the relevant spec file in the same commit.
- **Input to future sessions** — used for onboarding, drift audits against the code, and (in the limit) regenerating the codebase from scratch.

The code is one *realization* of this spec. If the spec and code disagree, one of them is wrong — fix the wrong one in that same change.

## Spec vs. ADRs vs. overview

| Doc | Purpose | Lifecycle |
|-----|---------|-----------|
| [`docs/spec/`](.) | What the system **does and is** today | Snapshot — supersede in place, never append |
| [`docs/architecture/decisions/`](../architecture/decisions/) | **Why** decisions were made | Append-only history; ADRs can be superseded by later ADRs |
| [`docs/architecture/overview.md`](../architecture/overview.md) | Narrative tour of the system | Snapshot, but optimized for human reading |
| [`docs/architecture/diagrams.md`](../architecture/diagrams.md) | Visual structure and flows | Snapshot, part of the spec |

## The seven sub-files

| File | Covers |
|------|--------|
| [behaviors.md](behaviors.md) | What the system does — the `run()` flow, audit emission, proxy egress, vault.inject |
| [architecture.md](architecture.md) | C4 element catalog — persons, systems, containers, components |
| [data-model.md](data-model.md) | RunRequest/response JSON shapes, audit event shape, in-memory proxy state |
| [interfaces.md](interfaces.md) | The `run` CLI surface + the vault/audit IPC contracts |
| [configuration.md](configuration.md) | The wiring fields: vault_socket, audit_socket, origin_map, injection_mode |
| [fitness-functions.md](fitness-functions.md) | Executable architectural invariants (no `--share-net`; credential never in sandbox) |

## Maintenance rules

1. **Update in the same commit as the code change.**
2. **Supersede in place. Never append.** The ADR carries history; the spec carries current truth.
3. **No future tense.** Roadmap and planned work live in `docs/plans/` and `docs/tasks/`.
4. **No implementation rationale.** "We chose X because Y" belongs in an ADR.
5. **Audit drift periodically** with the `architect` agent's drift-audit mode.

## Project summary

exec-sandbox is the OS execution-isolation block of the secure-agent ecosystem. It is a
single-binary Go CLI (`exec-sandbox run`) that reads a JSON `RunRequest` on stdin and runs the
supplied agent-generated payload in a sandbox with **no network**. The sandbox's only path out
is a host-side egress proxy on a Unix socket that enforces a domain allowlist and injects
credentials obtained from `vault`. In proxy mode the credential value never enters the sandbox.
spawn/inject/exit events are emitted to `audit-trail`. Two isolation tiers are wired behind a
`tier` seam — Tier-1 bubblewrap (`bwrap`) and Tier-2 gVisor (`runsc`) — both enforcing the same
no-network + proxy-only-egress invariant; Tier-3 Firecracker is accepted by the `tier` field but
not yet implemented. The seam keeps the `run()` contract stable across tiers.

## Top-level invariants

- **No network in the sandbox.** The payload runs with no network namespace regardless of tier —
  bubblewrap via `bwrap --unshare-all`, gVisor via an OCI spec declaring an empty `network`
  namespace plus `runsc --network=none`. There is no `--share-net` and no direct route out.
  Enforced in code by `bwrapArgv` and `gvisorOCISpec`; proposed as fitness rule F-001.
- **Tier-1 runs under a default-deny seccomp profile.** The bubblewrap backend installs a
  default-deny + allowlist seccomp-BPF filter via `bwrap --seccomp <fd>`: the dangerous syscall
  family (`keyctl`, `add_key`, `request_key`, `ptrace`, `process_vm_readv`/`writev`, `userfaultfd`,
  `bpf`, `perf_event_open`, the `mount`/`umount2`/`pivot_root`, `kexec_*`, and `*_module` families,
  …) returns `EPERM`, while the common-case syscalls a payload shell + the proxy client need stay
  allowed. The filter is a build-time-generated, sha256-pinned cBPF blob loaded fail-fast (a
  mismatch aborts the run — never an unfiltered spawn); it **adds to** the no-network model
  (`--unshare-all` kept, `--share-net` absent). Tier-2 (gVisor) and Tier-3 (Firecracker) self-filter
  every syscall and do not get `--seccomp` (ADR 016). Enforced in code by `bwrapArgv` +
  `loadTier1Seccomp`; fitness rule F-011.
- **The bind-mounted proxy socket is the only egress.** `/proxy.sock` is the sole path out of
  the sandbox. The egress proxy enforces the domain allowlist; non-allowlisted hosts get `403`.
- **exec-sandbox owns the network boundary; vault owns credential injection.** exec-sandbox
  never mints or stores credentials — it presents `{handle, sandbox_identity}` to `vault.inject`
  and loads what vault returns.
- **A proxy-mode credential value never enters the sandbox** — not in env, args, or stdout. It
  lives only on the host-side proxy and is wiped at teardown. Proposed as fitness rule F-002.
- **The `run()` contract is stable across tiers.** `run(payload, profile, tier, secret_refs) ->
  {stdout, stderr, exit_code, sandbox_status}` does not change when a new isolation backend is
  added behind the `tier` seam.

## Non-goals

- **Not credential storage or minting** — that is vault's responsibility.
- **Not audit storage or querying** — exec-sandbox emits events and forgets them.
- **Not a generic dev container or general process runner** — it is the execution-box profile
  for agent-generated code.
- **No persistent state** — every run is ephemeral (fresh temp dir, fresh proxy, wiped at
  teardown).
- **Tier 3 config-generating backend wired, VMM launch not yet wired** — `firecracker`
  dispatches to `firecrackerBackend` (ADR 010 D1); the backend generates the microVM config
  (machine-config, boot-source, root-drive, vsock) as a pure function of on-host paths, with
  no `network-interface` key (no-NIC by omission, D2). The one-shot VMM launch over the
  Firecracker REST socket is not yet wired (task 015). Tier-1 (`bubblewrap`), Tier-2 (`gvisor`),
  and the config-generating Tier-3 (`firecracker`) backend are all dispatched by `backendFor`.
