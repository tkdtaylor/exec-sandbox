# Behaviors

**Project:** exec-sandbox
**Last updated:** 2026-06-18

What the system does, observably. Each behavior describes a triggering condition, the system's response, and any externally-visible side effects.

Not in this file:
- *How* it does it (source code), *why* it does it (ADRs), *what data* it operates on ([data-model.md](data-model.md)), *what the entry points are* ([interfaces.md](interfaces.md)).

---

## Format

> **B-NNN: title** — Trigger / Response / Side effects / Failure modes / (optional) References.

Behaviors are numbered `B-001`, … sequentially. Numbers are stable references — never reuse a number.

---

## Core behaviors

### B-001: Run a payload in a no-network sandbox

- **Trigger:** `exec-sandbox run` receives a JSON `RunRequest` on stdin with a `run.payload`.
- **Response:** writes `req.run.payload` to a `payload.sh` (mode 0600) in a fresh temp dir, selects the isolation backend by `run.tier` (see B-008), then runs the payload under that backend with a minimal read-only root (`/usr`, `/etc`, conditionally `/bin /lib /lib64 /sbin`), `/proc`, `/dev`, a `/tmp` tmpfs, `PATH=/usr/bin:/bin`, the payload bind-mounted read-only as `/payload.sh`, and the egress proxy socket bind-mounted as `/proxy.sock` — with **no network namespace regardless of tier**. For the bubblewrap tier this is `bwrap --unshare-all --die-with-parent --clearenv`; for the gvisor tier it is `runsc run` over a generated OCI bundle whose spec declares an empty network namespace (see B-008). Captures stdout, stderr, and exit code. Returns a JSON result `{stdout, stderr, exit_code, sandbox_status}` on stdout.
- **Side effects:** the temp dir and its contents (payload, proxy socket; for gvisor also the OCI bundle in its own temp dir) are created and removed (`defer os.RemoveAll` / the backend cleanup func); spawn and exit audit events are emitted (see B-004).
- **Failure modes:** if the selected runtime (`bwrap` or `runsc`) fails to start (e.g. binary absent or non-exec error), `exit_code` is set to `127` and the error string is appended to stderr. If the runtime exits non-zero, that exit code is propagated. An unrecognized tier returns `{error: "tier not implemented: <tier>"}` without running anything (see B-008).
- **References:** ADR-001 D2/D3, ADR-002; `run.go` `Run` / `backendFor` / `bwrapArgv`; `gvisor.go`.

### B-002: Enforce the egress allowlist and route through the proxy

- **Trigger:** the sandboxed payload makes an HTTP request through `/proxy.sock`.
- **Response:** the egress proxy strips the port from the request Host and checks it against the allowlist derived from `profile.capabilities[NetConnect].allowlist`. An allowlisted host is resolved to a real origin via the `origin_map` (`host -> {ip, port}`) and the request is forwarded; the upstream status and body are returned. The sandbox itself has no network namespace, so the proxy socket is the only way out.
- **Side effects:** an outbound HTTP request to the real origin (with a credential header injected if one was loaded for that host — see B-003).
- **Failure modes:** a non-allowlisted host returns `403 blocked-by-allowlist`. An allowlisted host with no `origin_map` entry returns `502 no-route`. An upstream/dial error returns `502` with the error string.
- **References:** ADR-001 D3/D4; `proxy.go` `handle`; tests `TestSandboxReachesAllowlistedHostViaProxy`, `TestProxyBlocksNonAllowlistedHost`.

### B-003: Inject vault credentials at spawn (proxy mode, secret never in sandbox)

- **Trigger:** `req.run.secret_refs` contains one or more opaque handles.
- **Response:** for each handle, exec-sandbox mints a `sandbox_identity` (`{sandbox_id, attestation}`) and calls `vault.inject(handle, sandbox_identity, mode)` over the `vault_socket`. On a proxy-mode response (`delivery == "proxy"`), it loads the returned credential onto the egress proxy for the binding's host (`Header: Scheme Value`, defaulting to `Authorization: Bearer`). The credential is **never** placed into the sandbox env, args, or payload. The proxy injects it into allowlisted outbound requests to that host. An `env`-mode response is recorded in `secrets_injected` but is not loaded onto the proxy.
- **Side effects:** a `vault.inject` IPC call per handle; `secrets_injected` entries (`{handle_prefix, delivery}`) recorded in `sandbox_status`; the proxy's credential map is populated and then wiped at teardown.
- **Failure modes:** if the inject call errors or returns an `error` field, exec-sandbox emits an `inject_failed` audit event (decision `deny`) for that handle and continues with the remaining handles (the run is not aborted).
- **References:** ADR-001 D5; `run.go` `Run` inject loop / `vaultInject`; `proxy.go` `SetCredential` / `Wipe`.

### B-004: Emit spawn / inject_failed / exit audit events

- **Trigger:** the lifecycle of a run — at spawn, on a failed injection, and at exit.
- **Response:** emits JSON-lines audit events to the `audit_socket`. `spawn` (`decision: allow`, context `{tier, request_id}`) before the inject loop; `inject_failed` (`decision: deny`) per failed handle; `exit` (`decision: allow`, context `{exit_code, duration_ms, request_id}`) after the payload finishes.
- **Side effects:** one `emit` IPC call per event.
- **Failure modes:** emission is best-effort. An empty `audit_socket` makes `emit` a no-op; an IPC error is swallowed (the run is not aborted).
- **References:** ADR-001 D6; `run.go` `emit`.

### B-005: Report sandbox status in the result

- **Trigger:** a run completes (any exit code).
- **Response:** the result's `sandbox_status` carries `{sandbox_id, tier, duration_ms, secrets_injected, status}`. `status` is the literal `"clean"` in v0. `tier` echoes `req.run.tier`. `secrets_injected` lists `{handle_prefix, delivery}` per successfully injected handle (handles are truncated to an 8-char prefix — never the full handle, never the credential).
- **Side effects:** none beyond the returned JSON.
- **Failure modes:** none specific; this is assembled unconditionally.
- **References:** `run.go` `Run` return; [data-model.md](data-model.md).

### B-008: Select the isolation backend by tier

- **Trigger:** a run reaches the spawn step with `req.run.tier`.
- **Response:** `backendFor(tier)` selects the isolation backend. `""` and `"bubblewrap"` select the Tier-1 bubblewrap backend (`bwrapArgv`, unchanged); `"gvisor"` selects the Tier-2 gVisor backend. The gVisor backend writes an OCI bundle (`config.json` + a rootfs dir) to its own temp dir: the spec declares a `network` namespace with **no path** (a fresh empty netns — loopback only, no host/bridged networking, the OCI equivalent of `--unshare-all`), a read-only root with the host system dirs bind-mounted read-only, the payload read-only at `/payload.sh`, and the proxy socket as the only egress bind-mount at `/proxy.sock`. It then runs `runsc --network=none --host-uds=open --ignore-cgroups run --bundle <dir> <id>` (and `--rootless` when exec-sandbox runs unprivileged). `--host-uds=open` lets the payload connect to the existing proxy socket but never create host sockets. The bundle is removed after the process exits.
- **Side effects:** for the gVisor tier, an OCI bundle temp dir is created and removed (backend cleanup func). The chosen backend's runtime binary (`bwrap` or `runsc`) is exec'd.
- **Failure modes:** any tier other than `""`/`bubblewrap`/`gvisor` (e.g. `firecracker`) returns `{error: "tier not implemented: <tier>"}` — there is **no silent fall-back** to bubblewrap, and no payload runs. A bundle-write error returns `{error: <err>}`.
- **References:** ADR-001 D7, ADR-002; `run.go` `backendFor` / `Backend` / `bubblewrapBackend`; `gvisor.go` `gvisorBackend` / `gvisorOCISpec`; tests `TestBackendForRoutesByTier`, `TestBackendForUnknownTierErrors`, `TestGvisorSpecHasNoSharedNetwork`, `TestGvisorSpecMountsOnlyProxySocketForEgress`, `TestGvisorRunReachesAllowlistedHostAndBlocksOthers`.

---

## Edge cases and error behaviors

### B-006: Reject invalid invocation or unparseable request

- **Trigger:** the binary is invoked without the `run` subcommand, or stdin is not a valid `RunRequest` JSON, or stdin cannot be read.
- **Response:** missing/unknown subcommand prints `usage: exec-sandbox run …` to stderr and exits `2`. A stdin read error or a JSON parse error prints the error to stderr and exits `1`.
- **Side effects:** none (no sandbox is started, no audit emitted).
- **Failure modes:** this *is* the failure path.
- **References:** `main.go`.

### B-007: Fail fast if the egress proxy cannot start

- **Trigger:** the egress proxy fails to bind its Unix socket.
- **Response:** `Run` returns `{error: "proxy start failed: <err>"}` and does not run the payload.
- **Side effects:** the spawn audit event and any credential injection have already happened; no exit event is emitted on this path.
- **Failure modes:** this is the failure path. (TODO: confirm whether an `exit`/teardown audit event should also fire on early proxy-start failure — currently it does not; see `run.go` `Run`.)
- **References:** `run.go` `Run`; `proxy.go` `Start`.

---

## Behavioral invariants

- **The sandbox never has network access** regardless of profile, tier, or secret_refs. The only egress is the bind-mounted proxy socket.
- **A proxy-mode credential value is never observable from inside the sandbox** — not in env, args, payload, or stdout. It exists only on the host-side proxy.
- **Audit and vault calls are best-effort and non-fatal** except proxy-start failure (B-007), which aborts the run before any payload executes.
- **Every successful run returns the full result shape** (B-005); there is no partial-result path on the success side.
