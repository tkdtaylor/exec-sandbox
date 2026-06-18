# Interfaces

**Project:** exec-sandbox
**Last updated:** 2026-06-18

The system's contact surface — everything that calls into the system, everything it calls out
to, and the public boundaries within it.

Not in this file: what the interfaces *do* ([behaviors.md](behaviors.md)), what data flows
through them ([data-model.md](data-model.md)), how they're configured
([configuration.md](configuration.md)).

---

## Inbound interfaces

### CLI

```
exec-sandbox run     # reads a JSON RunRequest on stdin, writes a JSON result on stdout
```

| Subcommand / flag | Type | Default | Effect |
|-------------------|------|---------|--------|
| `run` | subcommand (positional, required) | — | The only subcommand. Reads a `RunRequest` from stdin, executes it, writes the result to stdout. |
| stdin | JSON `RunRequest` | — | The request body (see [data-model.md](data-model.md)). |
| stdout | JSON result | — | `{stdout, stderr, exit_code, sandbox_status}` or `{error}`. |

There are no flags in v0 — all input is the stdin JSON.

**Exit codes (of the `exec-sandbox` process itself, distinct from the payload's `exit_code` field):**
- `0` — request handled and result written (the payload's own exit code is reported inside the result JSON, not as the process exit code)
- `1` — could not read stdin, or could not parse the `RunRequest` JSON
- `2` — usage error (missing or unknown subcommand)

> Note: a payload that exits non-zero still yields process exit `0` — the payload's exit code is carried in `result.exit_code`. The process only exits non-zero on input/usage errors.

### HTTP (internal, sandbox-facing)

The egress proxy listens on a Unix socket (`/proxy.sock` inside the sandbox) and speaks HTTP.
This is not a public API — it is the sandbox's only egress path. The payload reaches it via
`--unix-socket /proxy.sock`. Behavior:

| Condition | Response |
|-----------|----------|
| Host in allowlist + has `origin_map` route | Forwarded to the origin (credential injected if loaded); upstream status/body returned |
| Host not in allowlist | `403 blocked-by-allowlist` |
| Host allowlisted but no `origin_map` route | `502 no-route` |
| Upstream/dial error | `502` with the error string |

---

## Outbound interfaces

| Dependency | What we call | Transport | Failure mode |
|------------|-------------|-----------|--------------|
| vault | `vault.inject(handle, sandbox_identity, mode)` | Unix-socket JSON-line (`ipcCall`, 10s dial timeout) | On error/`error` field → `inject_failed` audit event, handle skipped, run continues. Empty `vault_socket` → call skipped. |
| audit-trail | `emit(event)` (`op: "emit"`) | Unix-socket JSON-line (`ipcCall`, 10s dial timeout) | Best-effort: empty `audit_socket` → no-op; transport error swallowed. |
| Allowlisted origin | Forwarded HTTP request | `net/http` client over TCP | `502` returned to the sandbox on dial/transport error. |
| bubblewrap (`bwrap`) | Subprocess exec of the Tier-1 sandbox argv (`tier` empty/`bubblewrap`) | `os/exec`, `bwrap` resolved on `PATH` | If `bwrap` is absent or fails to start, `result.exit_code = 127` and stderr carries the error. |
| gVisor (`runsc`) | Subprocess exec of `runsc run` over a generated OCI bundle (`tier == gvisor`) | `os/exec`, `runsc` resolved on `PATH` | If `runsc` is absent or fails to start, `result.exit_code = 127` and stderr carries the error. No fall-back to bubblewrap. |

---

## Internal public surface

The codebase is a single `main` package with no exported cross-package API. The load-bearing
internal functions (stable within the package, the seams future work plugs into):

### `Run(req RunRequest) map[string]any` (`run.go`)

The orchestration entry point. Stable contract: given a `RunRequest`, returns the result map
(`{stdout, stderr, exit_code, sandbox_status}` or `{error}`). This realizes the v1 `run()`
contract.

### `backendFor(tier string) (Backend, error)` (`run.go`)

The **tier seam point**. Maps `req.run.tier` to an isolation `Backend`: `""` and `"bubblewrap"`
→ the bubblewrap backend; `"gvisor"` → the runsc backend; any other tier → a
`tier not implemented: <tier>` error (no silent fall-back). `Run()` calls `backendFor` and then
`backend.Argv(scriptPath, proxySock, workdir, fileReads, env, lim)` to obtain the spawn argv. A new
isolation backend is added by implementing `Backend` and registering it here, preserving the
no-network + proxy-only-egress invariant and the captured stdout/stderr/exit contract.

### `Backend` interface (`run.go`)

```go
type Backend interface {
    Argv(scriptPath, proxySock, workdir string, fileReads []string, env map[string]string, lim Limits) (argv []string, cleanup func(), degrades []degrade, err error)
}
```

Given the on-host payload script, proxy socket, optional writable working directory (`workdir`;
`""` ⇒ no `/work` mount), the validated read-only `fileReads` host paths (`nil` ⇒ no extra mounts),
the provisioned `env` (`nil`/empty ⇒ bare `PATH=/usr/bin:/bin`), and the parsed resource `Limits`, a
backend returns the `os/exec` argv to spawn, an optional `cleanup` func run after the process exits
(nil if nothing to clean up), the list of `degrades` (secondary caps the host could not enforce —
ADR 003), and an error if it could not prepare the run. Two implementations exist:
`bubblewrapBackend` (returns the `bwrapArgv` slice; no cleanup) and `gvisorBackend` (writes an OCI
bundle to a temp dir, returns the `runsc run` argv, and a cleanup that removes the bundle).

### `bwrapArgv(scriptPath, proxySock, workdir string, fileReads []string, env map[string]string, diskBytes int, finalCmd []string) []string` (`run.go`)

Builds the Tier-1 bubblewrap argv (`--unshare-all`, minimal read-only root, `/payload.sh` and
`/proxy.sock` bind-mounted; `diskBytes > 0` size-caps the `/tmp` tmpfs; `finalCmd` is the in-sandbox
command). When `workdir` is non-empty it appends `--bind <workdir> /work --chdir /work` — the host
dir mounted **read-write** (not `--ro-bind`) as the payload's cwd (ADR 004). For each `fileReads`
path it appends `--ro-bind <path> <path>` — mounted **read-only** at the same path (ADR 005). The
provisioned `env` is emitted as `--setenv` pairs (PATH defaulted to `/usr/bin:/bin` when absent),
via `envSetenvPairs`. Used by `bubblewrapBackend`.

### `fileReadPaths(profile map[string]any) []string` / `validateFileReads(paths []string) error` (`run.go`)

`fileReadPaths` collects the host paths from every `FileRead` capability in `profile.capabilities`
(mirroring `netAllowlist`'s `NetConnect` scan); multiple entries union their lists.
`validateFileReads` checks each path is **absolute** and **exists** before any side effect — a
relative or nonexistent path is a hard error (the run does not start; no silent skip — ADR 005).

### `gvisorOCISpec(scriptPath, proxySock string) map[string]any` (`gvisor.go`)

Builds the base OCI runtime spec (`config.json` contents) for the gVisor backend. A pure function of
the on-host paths (unit-testable without runsc). Declares a `network` namespace with no path (a
fresh empty netns — no host/bridged networking), a read-only root with the host system dirs
bind-mounted read-only, and the proxy socket as the only egress bind-mount at `/proxy.sock`. The
`runsc run` invocation adds `--network=none` (belt-and-suspenders no-network), `--host-uds=open`
(lets the payload connect to the existing proxy socket but not create host sockets), and
`--ignore-cgroups`. `applyFileReadToOCISpec`, `applyEnvToOCISpec`, `applyWorkdirToOCISpec`, and
`applyLimitsToOCISpec` mutate this base in place.

### `applyWorkdirToOCISpec(spec map[string]any, workdir string)` (`gvisor.go`)

Mutates the OCI spec in place to mount `workdir` **read-write** at `/work` (mount `options` omit
`ro`) and set `process.cwd = "/work"` (ADR 004). A no-op when `workdir` is empty, so the base spec —
read-only rootfs/system dirs, cwd `"/"` — is unchanged and backward-compatible. The `/work` mount is
the only writable host-path bind; the empty network namespace is untouched. Mirrors the shape of
`applyLimitsToOCISpec`.

### `applyFileReadToOCISpec(spec map[string]any, paths []string)` / `applyEnvToOCISpec(spec map[string]any, env map[string]string)` (`gvisor.go`)

`applyFileReadToOCISpec` appends one **read-only** bind mount per FileRead path
(`{destination:<p>, type:bind, source:<p>, options:[ro,rbind]}`) at the same path — mirroring the
read-only system-dir mounts, never the writable `/work` bind (ADR 005). `applyEnvToOCISpec` sets
`process.env` from the provisioned env (PATH replaces the bare default; deterministic order via
`envList`). Both are no-ops when their input is empty, so the base spec is byte-for-byte unchanged.
The empty network namespace is untouched.

### `EgressProxy` (`proxy.go`)

```go
func NewEgressProxy(allowlist []string, originMap map[string][2]string) *EgressProxy
func (p *EgressProxy) SetCredential(host string, c Credential)
func (p *EgressProxy) Wipe()
func (p *EgressProxy) Start(socketPath string) error
func (p *EgressProxy) Stop()
```

- **Consumers:** `Run()`.
- **Required behavior:** enforces the allowlist on every request; injects a loaded credential only into allowlisted requests; the sandbox must never be able to read the credential; credentials are wiped on teardown.

### `ipcCall(socket string, req map[string]any) (map[string]any, error)` (`run.go`)

The single Unix-socket JSON-line IPC primitive used by both `vaultInject` and `emit`. Writes a
newline-terminated JSON request, reads a newline-terminated JSON response. An empty socket path
returns an empty map (no-op).

---

## Extension points

- **Isolation backends** plug in behind the `tier` seam (`backendFor(tier)` in `Run()`), modeled on
  the OCI Runtime Spec. Bubblewrap (Tier 1) and gVisor/runsc (Tier 2) are wired; Firecracker
  (Tier 3) returns `tier not implemented`. (ADR-001 D7, ADR-002)
- **Otherwise: extension is by source modification** — there is no plugin registry. The IPC
  contracts (vault/audit) are the integration points with the rest of the ecosystem.
