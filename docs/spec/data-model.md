# Data Model

**Project:** exec-sandbox
**Last updated:** 2026-06-18

What data exists, how it's structured, where it lives. exec-sandbox holds **no persistent
state** — every run is ephemeral. The data model is therefore mostly wire/interchange formats
(stdin/stdout JSON, IPC messages) plus a small amount of per-run in-memory state.

Not in this file: operations on the data ([behaviors.md](behaviors.md)), how it's accessed
([interfaces.md](interfaces.md)), tunable parameters ([configuration.md](configuration.md)).

---

## Persistent state

**None.** exec-sandbox creates a temp dir per run (`os.MkdirTemp`), writes `payload.sh` and a
proxy socket into it, and removes the whole dir on return (`defer os.RemoveAll`). Nothing
survives the process.

---

## In-memory state

### State: `EgressProxy.creds` (`map[string]Credential`, host → credential)

- **Shape:** `map[string]Credential` guarded by `EgressProxy.mu sync.Mutex`. `Credential = {Value, Header, Scheme string}`.
- **Owner:** the per-run `EgressProxy`, constructed in `Run()` (`proxy.go`).
- **Lifetime:** populated by `SetCredential` after each proxy-mode `vault.inject`; read under lock in `handle` when forwarding; cleared by `Wipe()` at teardown (`defer proxy.Stop(); proxy.Wipe()`).
- **Concurrency rules:** all access goes through `mu`. `SetCredential`/`Wipe` are write paths; `handle` takes the lock to read the credential for a host. The HTTP server runs in its own goroutine.
- **Bounds:** at most one credential per allowlisted host.

### State: `EgressProxy.allowlist` (`map[string]bool`) and `originMap` (`map[string][2]string`)

- **Shape:** `allowlist` is a set of bare hostnames (ports stripped); `originMap` maps `host -> {ip, port}`.
- **Owner:** the per-run `EgressProxy`; both are set at construction and read-only thereafter.
- **Lifetime:** per run.

---

## Wire / interchange formats

### Format: `RunRequest` (stdin → `exec-sandbox run`)

- **Producer:** the calling agent / orchestrator.
- **Consumer:** `main.go` (unmarshal) → `Run()`.
- **Schema:**

```
{
  "run": {
    "payload":     string,            // shell script run as payload.sh
    "profile":     object,            // capabilities + limits; see below
    "tier":        string,            // "bubblewrap" | "gvisor" | "firecracker" (v0 wires bubblewrap)
    "secret_refs": [ string ]         // opaque vault handles
  },
  "wiring": {
    "vault_socket":   string,         // Unix socket path for vault.inject ("" → skip)
    "audit_socket":   string,         // Unix socket path for audit emit ("" → no-op)
    "origin_map":     { host: [ip, port] },  // resolves allowlisted hosts to real origins
    "request_id":     string,         // correlation id echoed into audit context
    "injection_mode": string          // mode passed to vault.inject (e.g. "proxy" | "env")
  }
}
```

`profile.capabilities` is an array of capability objects. exec-sandbox reads only the
`NetConnect` entries: `{ "type": "NetConnect", "allowlist": [ "host:port", … ] }` — the port is
stripped to derive the egress allowlist. Other capability types (e.g. `FileRead{paths}`) and
`profile.limits` are part of the v1 contract but not consumed by v0 (TODO: `limits` —
cpu/mem/disk/timeout — are documented in the contract but not yet enforced in code).

- **Versioning:** v1 contract (`the ecosystem's v1 interface contract §2`). The `run` object is the
  contract proper; `wiring` is deploy/test plumbing.
- **Example:**

```json
{
  "run": {
    "payload": "curl -s --unix-socket /proxy.sock http://api.example.com/ping\n",
    "profile": {"capabilities": [{"type": "NetConnect", "allowlist": ["api.example.com:443"]}]},
    "tier": "bubblewrap",
    "secret_refs": ["vault://handle/abc"]
  },
  "wiring": {
    "vault_socket": "/run/vault.sock",
    "audit_socket": "/run/audit.sock",
    "origin_map": {"api.example.com": ["10.0.0.5", "8443"]},
    "request_id": "req-123",
    "injection_mode": "proxy"
  }
}
```

### Format: Run result (stdout ← `exec-sandbox run`)

- **Producer:** `Run()`.
- **Consumer:** the calling agent.
- **Schema:**

```
{
  "stdout":    string,
  "stderr":    string,
  "exit_code": int,                  // 0 = success; 127 = bwrap failed to start; else payload exit
  "sandbox_status": {
    "sandbox_id":       string,      // "sbx-" + 6 random hex bytes
    "tier":             string,      // echoes run.tier
    "duration_ms":      int,
    "secrets_injected": [ { "handle_prefix": string, "delivery": "proxy" | "env" } ],
    "status":           string       // "clean" in v0
  }
}
```

On an early failure (proxy could not start) the result is instead `{ "error": string }`.

### Format: `vault.inject` request / response (Unix-socket JSON-line)

- **Producer / consumer:** `vaultInject` (`run.go`) ⇄ vault.
- **Request:** `{ "op": "inject", "handle": string, "sandbox_identity": {sandbox_id, attestation}, "mode": string }`
- **Response (proxy mode):** `{ "delivery": "proxy", "credential": string, "binding": { "host": string, "header": string, "scheme": string } }` — `header` defaults to `Authorization`, `scheme` to `Bearer` if absent.
- **Response (env mode):** `{ "delivery": "env", … }` (recorded but not loaded onto the proxy).
- **Error:** a non-nil `error` field, or a transport error, triggers an `inject_failed` audit event and the handle is skipped.

### Format: audit event (Unix-socket JSON-line)

- **Producer:** `emit` (`run.go`).
- **Consumer:** audit-trail.
- **Schema:** `{ "op": "emit", "event": { "actor": "exec-sandbox", "action": "spawn"|"inject_failed"|"exit", "target": sandbox_id, "decision": "allow"|"deny", "context": { … } } }`
  - `spawn` context: `{tier, request_id}`
  - `inject_failed` context: `{request_id}`
  - `exit` context: `{exit_code, duration_ms, request_id}`

### `sandbox_identity`

`{ "sandbox_id": "sbx-<6 hex bytes>", "attestation": "<16 hex bytes>" }` — minted per run with
`crypto/rand`. (TODO: the attestation is currently random bytes, not a signed attestation; v1
adds signatures per the README.)

---

## Derived data

| Derived | Source | Recompute trigger | Staleness tolerance |
|---------|--------|-------------------|---------------------|
| Egress allowlist | `profile.capabilities[NetConnect].allowlist` (ports stripped) | Once at start of each run (`netAllowlist`) | N/A — recomputed every run |

---

## Data invariants

- **`secrets_injected` never contains a full handle or a credential** — only an 8-char `handle_prefix` and the `delivery` mode. (`prefix(handle, 8)` in `run.go`.)
- **A `Credential` value lives only in `EgressProxy.creds`** and is wiped at teardown; it is never serialized into the result, the audit events, or the sandbox.
- **`sandbox_id` is unique per run** (random hex) and is the `target` of every audit event for that run.
