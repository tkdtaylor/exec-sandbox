# Architecture — C4 Element Catalog

**Project:** exec-sandbox
**Last updated:** 2026-06-20 (task 015: Tier-3 Firecracker boots end-to-end — firecrackerBackend/fclaunch/fcartifacts + vsock bridge components; no jailer)

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
| exec-sandbox CLI | Go (`main` package) | Parse argv, read `RunRequest` on stdin, orchestrate the run, write result on stdout | `main.go`, `run.go`, `snapshot.go`, `limits.go` | Egress proxy, isolation sandbox, vault, audit-trail |
| Egress proxy | Go `net/http` over a Unix socket | Enforce domain allowlist; inject credentials; forward allowlisted requests to the origin | `proxy.go` | Allowlisted origin |
| isolation sandbox | `bwrap --unshare-all` (Tier 1), `runsc` over an OCI bundle (Tier 2), or a Firecracker microVM booted directly under `bwrap --unshare-all` (Tier 3 — no jailer), selected by `run.tier` | Run `payload.sh` with no network namespace; only `/proxy.sock` bind-mounted (Tier 1/2) or vsock-bridged (Tier 3) for egress | (runtime; argv/spec built in `run.go` `bwrapArgv`, `gvisor.go` `gvisorOCISpec`, `firecracker.go`/`fclaunch.go`) | bubblewrap (`bwrap`) / gVisor (`runsc`) / Firecracker (`firecracker` + `/dev/kvm`), Egress proxy |

**Invariants for this table**
- All three containers run within (or as a subprocess of) the single `exec-sandbox` process; there is no separate deployable artifact. The Egress proxy and the isolation sandbox are spawned by the CLI per run and torn down at the end.
- Every `Depends on` entry resolves to another row here or in Section 2.

---

## 4. Components

| Container | Component | Source path | Responsibility | Depends on |
|-----------|-----------|-------------|----------------|------------|
| exec-sandbox CLI | `main` | `main.go` | CLI entry: argv check, read stdin, call `Run()`, marshal result to stdout | `Run()` |
| exec-sandbox CLI | `Run()` | `run.go` | Orchestration core: allowlist parse, identity mint, audit emit, baseline snapshot, vault.inject loop, proxy start, backend exec, result assembly, teardown | `backendFor`, `snapshotBaseline`, `vaultInject`, `emit`, `EgressProxy` |
| exec-sandbox CLI | `backendFor` / `Backend` | `run.go` | Tier seam: select `bubblewrapBackend`, `gvisorBackend`, or `firecrackerBackend` by `run.tier`; unknown tier → `tier not implemented` error | `bwrapArgv`, `gvisorBackend`, `firecrackerBackend` |
| exec-sandbox CLI | `bwrapArgv` | `run.go` | Build the Tier-1 bubblewrap argv (`--unshare-all`, no network namespace) | bubblewrap |
| exec-sandbox CLI | `gvisorBackend` / `gvisorOCISpec` | `gvisor.go` | Build the Tier-2 OCI bundle (empty netns, `/proxy.sock` only egress) and the `runsc run` argv | gVisor (`runsc`) |
| exec-sandbox CLI | `firecrackerBackend` / `firecrackerConfig` | `firecracker.go` | Build the Tier-3 microVM config (no `network-interface` — no-NIC by omission) + payload drive; verify the pinned kernel/rootfs; return the bwrap-wrapped `fc-launch` argv (no jailer) | `loadGuestArtifacts`, `startVsockBridge`, firecracker (`firecracker`) + `/dev/kvm` |
| exec-sandbox CLI | `fcLaunchMain` / `driveFirecrackerAPI` / `streamConsole` | `fclaunch.go` | In-bwrap launcher: spawn firecracker, drive the REST API in order (no `network-interfaces`), parse the console for guest stdout + the exit sentinel, exit with the guest exit code | firecracker REST socket |
| exec-sandbox CLI | `loadGuestArtifacts` / `verifyPinnedDigest` | `fcartifacts.go` | Resolve + sha256-verify the vendored pinned guest kernel + rootfs before boot; fail fast on mismatch | `guest/kernel/`, `guest/rootfs/` |
| exec-sandbox CLI | `vsockBridge` / `guestShim` | `vsockbridge.go` / `vsockshim.go` | Host vsock bridge forwards the vsock uds to the live `EgressProxy`; dumb guest-side byte pump presents `/proxy.sock` in the microVM (B-014) | `EgressProxy` |
| exec-sandbox CLI | `ipcCall` / `vaultInject` / `emit` | `run.go` | Unix-socket JSON-lines IPC to vault and audit-trail | vault, audit-trail |
| exec-sandbox CLI | `netAllowlist` | `run.go` | Parse the egress allowlist from `profile.capabilities[NetConnect]` | — |
| exec-sandbox CLI | `sandboxBaseline` / `snapshotBaseline` / `restore` | `snapshot.go` | Snapshot/restore reset boundary: build the pristine per-run baseline (work dir + `payload.sh` + fresh proxy), one-shot `teardown`, and leak-proof `restore` to a clean slate (ADR 009) | `EgressProxy` |
| Egress proxy | `EgressProxy` | `proxy.go` | Host + per-host verb allowlist enforcement, credential injection, request forwarding, credential wipe | Allowlisted origin |

---

## 5. Cross-cutting decisions

- **No-network + proxy-only egress** spans the sandbox and proxy containers — the load-bearing security control. (ADR-001 D3/D4)
- **Credential ownership split** — exec-sandbox owns the boundary; vault owns injection; the secret never enters the sandbox. (ADR-001 D5)
- **Tier seam** — `tier = bubblewrap | gvisor | firecracker`, dispatched by `backendFor`; bubblewrap (Tier 1), gVisor/runsc (Tier 2), and Firecracker (Tier 3, a KVM microVM booted directly under `bwrap --unshare-all` — no jailer) all run end-to-end; backends plug in without changing the `run()` contract. (ADR-001 D7, ADR-002, ADR-010 + Amendment 1)
- **JSON-lines Unix-socket IPC** is the single convention for both external blocks (`ipcCall`). (ADR-001 D2)

---

## Maintenance

- **Update in the same commit as `../architecture/diagrams.md`** when the structure changes.
- **Supersede in place. Never append.**
- **Don't list every file.** Components in Section 4 are the load-bearing ones.
- The drift-audit mode of the `architect` agent uses this catalog as its primary check against the import graph and the deployable artifact list.
