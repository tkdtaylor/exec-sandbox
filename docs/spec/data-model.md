# Data Model

**Project:** exec-sandbox
**Last updated:** 2026-06-20

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

### State: `EgressProxy.allowlist` (`map[string]bool`), `verbAllowlist` (`map[string]map[string]bool`) and `originMap` (`map[string][2]string`)

- **Shape:** `allowlist` is a set of bare hostnames (ports stripped); `verbAllowlist` maps a bare host to its allowed-method set (canonical upper-case keys) — a host **absent** from the map, or with an **empty** set, is **unconstrained** (all verbs allowed); `originMap` maps `host -> {ip, port}`.
- **Owner:** the per-run `EgressProxy`; all three are set at construction and read-only thereafter.
- **Lifetime:** per run.
- **Notes:** `verbAllowlist` carries the per-host HTTP-verb constraint (ADR 008). It is the *enforcement* state for a verb **decision** made by policy-engine; exec-sandbox only enforces. The verb check in `handle` runs **after** the host check and only ever **narrows** egress (a non-allowlisted verb is blocked with `403 blocked-by-method`, no outbound connection).

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
    "tier":        string,            // "bubblewrap" | "gvisor" wired; "firecracker" → tier not implemented
    "secret_refs": [ string ],        // opaque vault handles
    "workdir":     string,            // optional host dir → bind-mounted rw at /work, cwd=/work; "" → no mount
    "env":         { string: string } // env exported into the sandbox; "PATH" replaces the bare default; {} → unchanged
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

`profile.capabilities` is an array of capability objects. exec-sandbox reads two entry types:
- `{ "type": "NetConnect", "allowlist": [ "host:port", … ], "methods": [ "GET", … ] }` — the port is
  stripped to derive the egress allowlist. The optional `methods` array (ADR 008) constrains the
  HTTP verbs permitted to **every** host in that entry's `allowlist`; it is parsed into a
  `host → allowed-method-set` map (canonical upper-case). **No `methods` / an empty `methods: []` ⇒
  unconstrained** (all verbs allowed — backward compatible); a non-empty set denies any verb not in
  it. Per-host verb sets are expressed by emitting multiple `NetConnect` entries. The *decision* of
  which verbs to allow is policy-engine's; exec-sandbox carries the shape and **enforces** it at the
  proxy.
- `{ "type": "FileRead", "paths": [ "/abs/host/path", … ] }` — **implemented** (ADR 005): each path
  is bind-mounted **read-only** at the **same** path inside the sandbox. Multiple `FileRead` entries
  **union** their path lists (`fileReadPaths`). Each path is validated before spawn — it must be
  **absolute** and **exist**; a relative or nonexistent path is a hard `{error}` (no run, no silent
  skip). An empty/absent `FileRead` adds no mounts.

`run.workdir` (ADR 004) is the **writable working-directory** input: a host path that, when
non-empty, is bind-mounted **read-write** at `/work` and becomes the payload's cwd (validated
before spawn — must be an existing directory; a bad path is a hard `{error}`). It is distinct from
`FileRead{paths}`: `run.workdir` is a single read-**write** working dir at a fixed mountpoint;
`FileRead{paths}` is the read-**only** list-of-same-path mounts. They compose — a run can mount a
writable repo at `/work` *and* a read-only toolchain dir via `FileRead`. Empty/absent `run.workdir`
⇒ no `/work` mount (backward compatible).

`run.env` (ADR 005) is the env-provisioning input: a `map[string]string` exported into the sandbox.
A `PATH` entry **replaces** the bare default `PATH=/usr/bin:/bin`; every other entry is exported as
`k=v`. Env entries are emitted in a deterministic order (PATH first, then sorted keys) so the
spawn argv / OCI spec are reproducible. Empty/absent `run.env` ⇒ the bare default PATH, no other
env (backward compatible). `run.env` carries no secret — proxy-mode credentials never enter the
sandbox (they live only at the proxy edge). Combined with `FileRead`, `run.env["PATH"]` puts a
mounted read-only toolchain dir on PATH so a payload can `command -v <tool>` and run it.

`profile.limits` **is enforced** (parsed by `parseLimits` into the `Limits` struct):

```
"limits": {
  "cpu_count":        int,   // cores; enforced as taskset CPU affinity
  "memory_mb":        int,   // RLIMIT_AS ceiling (MiB)
  "pids":             int,   // RLIMIT_NPROC
  "disk_mb":          int,   // writable-layer (/tmp tmpfs) size cap (MiB)
  "timeout_sec":      int,   // wall-clock; host-side process-group kill
  "max_output_bytes": int    // per-stream host capture ceiling (bytes); host-side, above the tier seam
}
```

Every field is optional; a missing, zero, or non-positive value means "no limit" for that cap. The
per-backend enforcement mechanism (bubblewrap rlimits/tmpfs/affinity vs gVisor OCI
`process.rlimits`/tmpfs `size=`) is ADR 003. `cpu_count` and `disk_mb` are secondary controls that
degrade gracefully (recorded in `sandbox_status.limits.degraded`) on hosts that can't enforce them.
`max_output_bytes` (ADR 007) is enforced **host-side, above the `tier` seam** — `Run()` captures
each of stdout/stderr through a writer that retains at most `max_output_bytes` bytes per stream and
**drops** the overflow without erroring the payload's pipe (so the payload's exit is unaffected).
stdout and stderr are capped **independently** at the same ceiling; the cap is identical under
bubblewrap and gVisor (the backend argv/OCI spec are unchanged by it). Truncation is recorded in
`sandbox_status.limits.output_truncated` (see the result shape below).

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
  "exit_code": int,                  // 0 = success; 137 = killed by timeout_sec; 127 = runtime failed to start; else payload exit
  "sandbox_status": {
    "sandbox_id":       string,      // "sbx-" + 6 random hex bytes
    "tier":             string,      // echoes run.tier
    "duration_ms":      int,
    "secrets_injected": [ { "handle_prefix": string, "delivery": "proxy" | "env" } ],
    "status":           string,      // "clean" | "timeout" (timeout = killed by the wall-clock deadline)
    "limits": {                      // applied profile.limits (zeros = not requested)
      "cpu_count":   int, "memory_mb": int, "pids": int, "disk_mb": int, "timeout_sec": int,
      "max_output_bytes": int,       // applied per-stream output ceiling (0 = no cap)
      "degraded":         [ string ], // secondary caps the host could not enforce (e.g. "disk_mb")
      "output_truncated": [ string ]  // streams whose output cap dropped bytes; deterministic order: [], ["stdout"], ["stdout","stderr"]
    }
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
  - `exit` (success) context: `{exit_code, duration_ms, status, request_id}` (`status` is `"clean"` or `"timeout"`; `decision` is `"allow"`)
  - `exit` (early proxy-start failure) context: `{status:"proxy_start_failed", error:<msg>, request_id}` (`decision` is `"deny"`); no `exit_code` or `duration_ms` (ADR 013). Every run that emits `spawn` emits a matching `exit` event — either the success shape or the failure shape.

### `sandbox_identity`

`{ "sandbox_id": "sbx-<6 hex bytes>", "attestation": "<16 hex bytes>" }` — minted per run with
`crypto/rand`. (TODO: the attestation is currently random bytes, not a signed attestation; v1
adds signatures per the README.)

---

## Derived data

| Derived | Source | Recompute trigger | Staleness tolerance |
|---------|--------|-------------------|---------------------|
| Egress allowlist | `profile.capabilities[NetConnect].allowlist` (ports stripped) | Once at start of each run (`netAllowlist`) | N/A — recomputed every run |
| Per-host verb allowlist | `profile.capabilities[NetConnect].methods` (canonical upper-case; absent/empty ⇒ unconstrained) | Once at start of each run (`netVerbAllowlist`) | N/A — recomputed every run |

---

## Data invariants

- **`secrets_injected` never contains a full handle or a credential** — only an 8-char `handle_prefix` and the `delivery` mode. (`prefix(handle, 8)` in `run.go`.)
- **A `Credential` value lives only in `EgressProxy.creds`** and is wiped at teardown; it is never serialized into the result, the audit events, or the sandbox.
- **`sandbox_id` is unique per run** (random hex) and is the `target` of every audit event for that run.
