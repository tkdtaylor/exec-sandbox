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
| `run.profile.capabilities[FileRead].paths` | `[string]` (abs host paths) | `[]` | **Read-only host mounts** (ADR 005). Each path is bind-mounted **read-only** at the **same** path inside the sandbox; multiple `FileRead` entries union their paths. Validated before spawn (each must be absolute + exist, else a hard `{error}`). The read-**write** host dir is `run.workdir`, not `FileRead`. |
| `run.profile.limits` | object | — | **Enforced** resource caps (ADR 003). See the per-field table below. |
| `run.workdir` | string (host path) | `""` | **Writable working directory** (ADR 004). Non-empty → the host dir is bind-mounted **read-write** at `/work` and the payload's cwd is `/work`; validated before spawn (must be an existing directory, else a hard `{error}`). Empty → no `/work` mount (backward compatible). The one writable host surface — system dirs stay read-only, the network stays unshared. |
| `run.env` | object `{string: string}` | `{}` | **Env provisioning** (ADR 005). Each entry is exported into the sandbox; a `PATH` entry **replaces** the bare default `PATH=/usr/bin:/bin`, every other entry is exported as `k=v`. Combined with `FileRead`, `run.env["PATH"]` puts a mounted toolchain dir on PATH. Empty → bare default PATH, no other env (backward compatible). Carries no secret. |
| `run.secret_refs` | `[string]` | `[]` | Opaque vault handles to inject at spawn. |

`run.profile.limits` fields (each optional; missing/zero/non-positive ⇒ that cap is not applied):

| Key | Type | Enforced by | Effect |
|-----|------|-------------|--------|
| `cpu_count` | int (cores) | `taskset -c 0-(N-1)` affinity on the spawn argv | Pins the sandbox to N cores. **Secondary control:** degrades (warn + continue) when `taskset` is absent. Under gVisor, verified host-side via the argv record (ADR 028). |
| `memory_mb` | int (MiB) | `RLIMIT_AS` — bwrap: in-sandbox `prlimit --as`; gVisor: OCI `process.rlimits` | A payload exceeding the address-space ceiling is killed by the allocator. |
| `pids` | int | `RLIMIT_NPROC` — bwrap: in-sandbox `prlimit --nproc` (per-sandbox via the userns); gVisor: OCI `process.rlimits` | A fork bomb hits the cap ("Cannot fork"). |
| `disk_mb` | int (MiB) | `/tmp` tmpfs size — bwrap: `--size`; gVisor: tmpfs `size=` option | Writes past the cap return ENOSPC. **Secondary control:** degrades (warn + continue) when the writable layer can't be size-capped (`diskQuotaSupported`). |
| `timeout_sec` | int (s) | host-side `context.WithTimeout` + process-group `SIGKILL` (backend-agnostic) | The payload (and its whole process group) is killed at the deadline; `sandbox_status.status` becomes `"timeout"`, `exit_code` `137`. |

---

## Environment variables

The **application** reads no environment variables. (Inside the sandbox, the environment is
cleared — `bwrap --clearenv` / gVisor `process.env` — and `PATH=/usr/bin:/bin` is set by default.
A caller-supplied `run.env` overrides `PATH` and adds further variables — ADR 005; the credential
invariant is untouched, `run.env` carries no secret.)

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

`run.workdir` defaults to `""` — **no writable host mount**. A run only ever gains a writable host
surface when the caller explicitly names a directory, and even then it is the single `/work`
mount; the rootfs and system dirs stay read-only and the network stays unshared regardless. A
malformed `run.workdir` fails the run loudly rather than silently running without the mount.

`FileRead{paths}` defaults to none and `run.env` to `{}` — **no read-only host mounts, bare PATH**.
A run gains read-only host paths only when the caller lists them, and they are mounted **read-only**
(never writable — `/work` stays the only writable surface). FileRead opens no egress and the
network stays unshared regardless. A malformed `FileRead` path (relative or nonexistent) fails the
run loudly before any side effect rather than silently running without the mount.

`profile.limits` defaults to "no limit" per field — an unset cap is simply not applied (limits
*narrow* the sandbox; they never widen it). The `cpu_count` and `disk_mb` caps are **secondary**
anti-DoS controls and degrade gracefully (a stderr `WARNING` + a `sandbox_status.limits.degraded`
entry, never a hard failure) on hosts that can't enforce them — a documented, ADR-003-justified
exception (mirroring agent-builder ADR 027), not a weakening of the load-bearing controls
(no-network, proxy-only egress, memory/pids rlimits, wall-clock kill).
