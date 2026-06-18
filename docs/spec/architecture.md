# Architecture — C4 Element Catalog

**Project:** exec-sandbox
**Last updated:** 2026-06-18

The structured catalog of architectural elements that the diagrams in [`../architecture/diagrams.md`](../architecture/diagrams.md) render. Tables here are the **machine-readable spec** for the system's structure.

## How this file relates to the diagrams

| File | Form | Use when |
|------|------|----------|
| [`../architecture/diagrams.md`](../architecture/diagrams.md) | Visual (Mermaid C4 + sequence) | You want to *see* the structure |
| `architecture.md` (this file) | Tabular (rows + columns) | You want to *check, query, or regenerate* the structure |

When the structure changes, both files update in the same commit.

---

## 1. Persons (actors)

| Name | Description | Goals |
|------|-------------|-------|
| Calling agent / orchestrator | The automated client that submits a `RunRequest` (agent-builder is the first consumer) | Run agent-generated code safely; get back stdout/stderr/exit; never leak credentials to the payload |
| Operator | Human running or debugging exec-sandbox directly via the CLI | Verify isolation; inspect a run's result and audit trail |

---

## 2. Systems

| Name | Type | Description | Owner |
|------|------|-------------|-------|
| exec-sandbox | In-scope | OS execution-isolation block: no-network sandbox + credential-injecting egress proxy + audit emission | This team |
| vault | External | Mints/binds credentials; serves `vault.inject` over a Unix socket | secure-agent ecosystem block |
| audit-trail | External | Receives spawn/inject/exit audit events over a Unix socket | secure-agent ecosystem block |
| Allowlisted origin | External | The real HTTP service an allowlisted host resolves to (via `origin_map`) | Third party |
| bubblewrap (`bwrap`) | External (runtime tool) | Provides the Tier-1 sandbox; invoked as a subprocess | Upstream OSS |

---

## 3. Containers

| Name | Technology | Responsibility | Source path | Depends on |
|------|------------|----------------|-------------|------------|
| exec-sandbox CLI | Go (`main` package) | Parse argv, read `RunRequest` on stdin, orchestrate the run, write result on stdout | `main.go`, `run.go` | Egress proxy, isolation sandbox, vault, audit-trail |
| Egress proxy | Go `net/http` over a Unix socket | Enforce domain allowlist; inject credentials; forward allowlisted requests to the origin | `proxy.go` | Allowlisted origin |
| isolation sandbox | `bwrap --unshare-all` (Tier 1) or `runsc` over an OCI bundle (Tier 2), selected by `run.tier` | Run `payload.sh` with no network namespace; only `/proxy.sock` bind-mounted for egress | (runtime; argv/spec built in `run.go` `bwrapArgv` and `gvisor.go` `gvisorOCISpec`) | bubblewrap (`bwrap`) / gVisor (`runsc`), Egress proxy |

**Invariants for this table**
- All three containers run within (or as a subprocess of) the single `exec-sandbox` process; there is no separate deployable artifact. The Egress proxy and the isolation sandbox are spawned by the CLI per run and torn down at the end.
- Every `Depends on` entry resolves to another row here or in Section 2.

---

## 4. Components

| Container | Component | Source path | Responsibility | Depends on |
|-----------|-----------|-------------|----------------|------------|
| exec-sandbox CLI | `main` | `main.go` | CLI entry: argv check, read stdin, call `Run()`, marshal result to stdout | `Run()` |
| exec-sandbox CLI | `Run()` | `run.go` | Orchestration core: allowlist parse, identity mint, audit emit, vault.inject loop, proxy start, backend exec, result assembly, teardown | `backendFor`, `vaultInject`, `emit`, `EgressProxy` |
| exec-sandbox CLI | `backendFor` / `Backend` | `run.go` | Tier seam: select `bubblewrapBackend` or `gvisorBackend` by `run.tier`; unknown tier → `tier not implemented` error | `bwrapArgv`, `gvisorBackend` |
| exec-sandbox CLI | `bwrapArgv` | `run.go` | Build the Tier-1 bubblewrap argv (`--unshare-all`, no network namespace) | bubblewrap |
| exec-sandbox CLI | `gvisorBackend` / `gvisorOCISpec` | `gvisor.go` | Build the Tier-2 OCI bundle (empty netns, `/proxy.sock` only egress) and the `runsc run` argv | gVisor (`runsc`) |
| exec-sandbox CLI | `ipcCall` / `vaultInject` / `emit` | `run.go` | Unix-socket JSON-lines IPC to vault and audit-trail | vault, audit-trail |
| exec-sandbox CLI | `netAllowlist` | `run.go` | Parse the egress allowlist from `profile.capabilities[NetConnect]` | — |
| Egress proxy | `EgressProxy` | `proxy.go` | Allowlist enforcement, credential injection, request forwarding, credential wipe | Allowlisted origin |

---

## 5. Cross-cutting decisions

- **No-network + proxy-only egress** spans the sandbox and proxy containers — the load-bearing security control. (ADR-001 D3/D4)
- **Credential ownership split** — exec-sandbox owns the boundary; vault owns injection; the secret never enters the sandbox. (ADR-001 D5)
- **Tier seam** — `tier = bubblewrap | gvisor | firecracker`, dispatched by `backendFor`; bubblewrap (Tier 1) and gVisor/runsc (Tier 2) are wired, Firecracker (Tier 3) returns `tier not implemented`; backends plug in without changing the `run()` contract. (ADR-001 D7, ADR-002)
- **JSON-lines Unix-socket IPC** is the single convention for both external blocks (`ipcCall`). (ADR-001 D2)

---

## Maintenance

- **Update in the same commit as `../architecture/diagrams.md`** when the structure changes.
- **Supersede in place. Never append.**
- **Don't list every file.** Components in Section 4 are the load-bearing ones.
- The drift-audit mode of the `architect` agent uses this catalog as its primary check against the import graph and the deployable artifact list.
