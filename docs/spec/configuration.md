# Configuration

**Project:** exec-sandbox
**Last updated:** 2026-06-18

Every knob the system exposes. exec-sandbox has **no config files and reads no application
environment variables** — all configuration arrives inside the stdin `RunRequest`. The tunable
surface is therefore the `wiring` object plus the parts of `run` that shape execution.

Not in this file: what gets configured (the behaviors live in [behaviors.md](behaviors.md));
how values get into the process (the parsing is in code; the *contract* is here).

---

## Configuration files

**None.** exec-sandbox is configured entirely per-request via the stdin JSON. There is no
`config.toml`/`.env`/etc.

---

## Request configuration (the `wiring` object)

These fields ride alongside the `run()` contract under `wiring` and tune how a single run is
wired into the surrounding ecosystem. They are part of the stdin `RunRequest`
([data-model.md](data-model.md)), not process-level config.

| Key | Type | Default | Required | Effect |
|-----|------|---------|----------|--------|
| `wiring.vault_socket` | string (path) | `""` | no | Unix socket for `vault.inject`. Empty → injection calls are skipped (no secrets loaded). |
| `wiring.audit_socket` | string (path) | `""` | no | Unix socket for audit `emit`. Empty → emission is a no-op. |
| `wiring.origin_map` | object `{host: [ip, port]}` | `{}` | no | Resolves allowlisted hosts to real origins. A host without a route returns `502 no-route`. |
| `wiring.request_id` | string | `""` | no | Correlation id echoed into every audit event's context. |
| `wiring.injection_mode` | string | `""` | no | Passed verbatim to `vault.inject` as `mode`. `"proxy"` keeps the secret out of the sandbox (loaded onto the proxy); `"env"` is recorded but not loaded onto the proxy in v0. |

Execution-shaping fields under `run`:

| Key | Type | Default | Effect |
|-----|------|---------|--------|
| `run.tier` | string | `""`/`bubblewrap` → Tier-1 (bwrap) | `bubblewrap \| gvisor \| firecracker`. `""`/`bubblewrap` runs bwrap, `gvisor` runs runsc; `firecracker` (or any other value) returns `{error: "tier not implemented: <tier>"}`. The value is echoed into `sandbox_status.tier` and the spawn audit context. |
| `run.profile.capabilities[NetConnect].allowlist` | `[string]` ("host:port") | `[]` | The egress allowlist (ports stripped). Hosts not listed are `403`-blocked by the proxy. |
| `run.profile.capabilities` (other types) | array | — | Part of the v1 contract; not consumed by v0. (TODO: `FileRead{paths}` etc. not yet enforced.) |
| `run.profile.limits` | object | — | cpu/mem/disk/timeout — documented in the contract; **not yet enforced in v0** (TODO). |
| `run.secret_refs` | `[string]` | `[]` | Opaque vault handles to inject at spawn. |

---

## Environment variables

The **application** reads no environment variables. (Inside the sandbox, the environment is
cleared — `bwrap --clearenv` — and only `PATH=/usr/bin:/bin` is set.)

**Hook profile env vars** (consumed by `.claude/scripts/`, not the application itself):
- `CLAUDE_HOOK_PROFILE` — `minimal` / `standard` / `strict` (default `standard`)
- `CLAUDE_DISABLED_HOOKS` — comma-separated list of hook names to disable

---

## Runtime flags

None beyond the `run` subcommand — see [interfaces.md](interfaces.md). All runtime input is the
stdin JSON.

---

## Secrets

exec-sandbox never stores or mints secrets. Credentials flow transiently from vault → the
egress proxy and are wiped at teardown.

| Secret | Source | Used for |
|--------|--------|----------|
| (per-request credentials) | `vault.inject` over `wiring.vault_socket` | Injected into allowlisted egress requests (proxy mode); never written to disk, never enters the sandbox |

**Rule:** secrets are never pasted into the chat, never logged, never written into the repo,
and (the project-specific invariant) never enter the sandbox in proxy mode. The
`protect-secrets` hook blocks writes to common credential filenames.

---

## Deployment configuration

| Aspect | Value | Notes |
|--------|-------|-------|
| Binary | `bin/exec-sandbox` (`make build`) | Single static-ish Go binary |
| Runtime dependency | `bwrap` on `PATH` | Tier-1 isolation; integration tests skip if absent |
| Ports exposed | none | Egress proxy listens on a per-run Unix socket, not a TCP port |
| Persistent volumes | none | Per-run temp dir, removed on exit |

---

## Defaults policy

Defaults are **safe and closed**: an empty `vault_socket`/`audit_socket` disables that
integration rather than failing; an empty allowlist blocks all egress (default-deny); the
sandbox always runs with no network regardless of any field. Nothing in the request can widen
the sandbox's network access beyond the proxy + allowlist.
