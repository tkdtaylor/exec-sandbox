# Architecture Diagrams

**Project:** exec-sandbox
**Last updated:** 2026-06-18

C4-structured Mermaid diagrams covering the system at progressively detailed levels (Context → Container → Component), plus the runtime sequence flow that shows how those pieces collaborate. See [overview.md](overview.md) for prose context, [decisions/](decisions/) for the ADRs referenced here, and [`../spec/architecture.md`](../spec/architecture.md) for the structured element catalog these diagrams render.

These diagrams are part of the **authoritative spec** for this project. Code changes that contradict a diagram either invalidate the change or invalidate the diagram; one must be updated to match the other in the same commit.

GitHub and most IDE markdown previewers render Mermaid natively — no build step required.

> **Scaling rule.** exec-sandbox is a single deployable binary, so the Container view is small (one process plus the host-side proxy it runs and the two external blocks it talks to). The Component view is where the load-bearing structure lives.

---

## 1. System Context — who uses it and what it touches

```mermaid
C4Context
    title System Context for exec-sandbox

    Person(agent, "Calling agent / orchestrator", "Submits a JSON RunRequest with agent-generated payload, profile, tier, secret_refs")

    System(execsandbox, "exec-sandbox", "OS execution-isolation block: runs untrusted code with no network; only egress is a credential-injecting proxy")

    System_Ext(vault, "vault", "Mints/binds credentials; serves vault.inject over a Unix socket")
    System_Ext(audit, "audit-trail", "Receives spawn/inject/exit audit events over a Unix socket")
    System_Ext(origin, "Allowlisted origin", "The real HTTP service the payload is permitted to reach")

    Rel(agent, execsandbox, "Sends RunRequest / receives result", "JSON over stdio")
    Rel(execsandbox, vault, "vault.inject(handle, sandbox_identity, mode)", "Unix socket, JSON-lines")
    Rel(execsandbox, audit, "emit(event)", "Unix socket, JSON-lines")
    Rel(execsandbox, origin, "Forwards allowlisted requests (credential injected)", "HTTP")
```

---

## 2. Containers — runnable units inside the system

```mermaid
C4Container
    title Container view of exec-sandbox

    Person(agent, "Calling agent / orchestrator")

    System_Boundary(boundary, "exec-sandbox process") {
        Container(cli, "exec-sandbox run", "Go / main package", "Reads RunRequest on stdin, orchestrates the run, writes result on stdout")
        Container(proxy, "Egress proxy", "Go / net/http on a Unix socket", "Domain allowlist + credential injection; the sandbox's only path out")
        Container(sandbox, "bubblewrap sandbox", "bwrap --unshare-all", "Runs payload.sh with no network namespace; /proxy.sock bind-mounted in")
    }

    System_Ext(vault, "vault")
    System_Ext(audit, "audit-trail")
    System_Ext(origin, "Allowlisted origin")

    Rel(agent, cli, "RunRequest / result", "JSON over stdio")
    Rel(cli, vault, "vault.inject", "Unix socket")
    Rel(cli, audit, "emit", "Unix socket")
    Rel(cli, proxy, "Starts, loads credentials, stops/wipes", "in-process")
    Rel(cli, sandbox, "Runs payload, captures stdout/stderr/exit", "exec bwrap")
    Rel(sandbox, proxy, "Outbound HTTP (only egress)", "Unix socket /proxy.sock")
    Rel(proxy, origin, "Forwards allowlisted request with injected credential", "HTTP")
```

---

## 3. Components — modules inside the exec-sandbox process

```mermaid
C4Component
    title Component view of the exec-sandbox process

    Container_Boundary(boundary, "exec-sandbox") {
        Component(main, "main", "main.go", "CLI entry: parse argv, read stdin RunRequest, call Run(), write result")
        Component(run, "Run()", "run.go", "Orchestration: allowlist parse, identity mint, audit emit, vault.inject loop, proxy start, bwrap exec, result assembly")
        Component(bwrap, "bwrapArgv", "run.go", "Builds the Tier-1 bubblewrap argv (no network namespace)")
        Component(ipc, "ipcCall / vaultInject / emit", "run.go", "Unix-socket JSON-lines IPC to vault and audit-trail")
        Component(egress, "EgressProxy", "proxy.go", "Allowlist enforcement + credential injection on a Unix socket")
    }

    System_Ext(vault, "vault")
    System_Ext(audit, "audit-trail")

    Rel(main, run, "Invokes", "Run(req)")
    Rel(run, bwrap, "Builds argv", "")
    Rel(run, ipc, "vault.inject / emit", "")
    Rel(run, egress, "NewEgressProxy / SetCredential / Start / Stop / Wipe", "")
    Rel(ipc, vault, "inject", "Unix socket")
    Rel(ipc, audit, "emit", "Unix socket")
```

**Key contracts**
- The sandbox has **no network namespace** (`bwrap --unshare-all`); `/proxy.sock` is the only egress. (ADR-001 D3)
- exec-sandbox owns the network boundary + proxy + allowlist; vault owns credential injection. The proxy-mode credential **never** enters the sandbox. (ADR-001 D4/D5)
- The `tier` seam selects the isolation backend; v0 wires bubblewrap only. (ADR-001 D7)

---

## 4. Primary runtime flow — `Run()` end-to-end

```mermaid
sequenceDiagram
    autonumber
    participant Agent as Calling agent
    participant Run as exec-sandbox Run()
    participant Vault as vault
    participant Proxy as Egress proxy
    participant Box as bubblewrap sandbox
    participant Audit as audit-trail

    Agent->>Run: RunRequest {payload, profile, tier, secret_refs} on stdin
    Run->>Run: parse NetConnect allowlist; mint sandbox_identity
    Run->>Audit: emit spawn {actor, action:spawn, target:sandbox_id, decision:allow}
    loop for each secret_ref handle
        Run->>Vault: vault.inject(handle, sandbox_identity, mode)
        alt proxy-mode success
            Vault-->>Run: {credential, binding:{host,header,scheme}}
            Run->>Proxy: SetCredential(host, cred)  %% never enters sandbox
        else failure / deny
            Vault-->>Run: error
            Run->>Audit: emit inject_failed {decision:deny}
        end
    end
    Run->>Proxy: Start(proxy.sock)
    Run->>Box: exec bwrap --unshare-all (payload.sh, /proxy.sock bind-mounted)
    Box->>Proxy: outbound HTTP via /proxy.sock (only egress)
    Proxy->>Proxy: allowlist check; inject credential
    Proxy-->>Box: forwarded response (or 403 blocked / 502 no-route)
    Box-->>Run: stdout, stderr, exit_code
    Run->>Audit: emit exit {action:exit, exit_code, duration_ms}
    Run->>Proxy: Stop() + Wipe()
    Run-->>Agent: {stdout, stderr, exit_code, sandbox_status} on stdout
```

---

## Adding more diagrams

Add additional numbered sections (5., 6., …) for any of:

- **Per-flow sequence diagrams** — e.g. the gVisor Tier-2 dispatch path once it lands (it should reuse this same sequence with the `bwrap` exec step replaced by `runsc`, preserving every other edge).
- **State machines** — if a subsystem grows explicit states with transitions.
- **Deployment topology** — `C4Deployment` if the runtime layout becomes non-obvious.

One concept per diagram.

---

## Maintaining these diagrams

- **Trigger to update:** any time a new actor, container, or component appears; a boundary moves; an external dependency is added or removed; an ADR changes a diagrammed flow. Keep [`../spec/architecture.md`](../spec/architecture.md) in sync — the catalog and these diagrams describe the same elements.
- **Edit existing over adding new.** Duplicates rot independently. If a diagram has grown unwieldy, split it by extracting a self-contained subflow into its own numbered section.
- **Note ADRs that don't change diagrams.** When an ADR introduces a refactor that preserves the diagrammed runtime shape, add a one-line note here saying so.
- **Update the date at the top** when you change anything substantive.
