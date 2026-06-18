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
- **Response:** writes `req.run.payload` to a `payload.sh` (mode 0600) in a fresh temp dir, selects the isolation backend by `run.tier` (see B-008), then runs the payload under that backend with a minimal read-only root (`/usr`, `/etc`, conditionally `/bin /lib /lib64 /sbin`), `/proc`, `/dev`, a `/tmp` tmpfs, `PATH=/usr/bin:/bin`, the payload bind-mounted read-only as `/payload.sh`, and the egress proxy socket bind-mounted as `/proxy.sock` — with **no network namespace regardless of tier**. When `run.workdir` is set, the named host directory is additionally bind-mounted **read-write** at `/work` and becomes the payload's cwd (see B-010); otherwise there is no `/work` mount. For the bubblewrap tier this is `bwrap --unshare-all --die-with-parent --clearenv`; for the gvisor tier it is `runsc run` over a generated OCI bundle whose spec declares an empty network namespace (see B-008). Captures stdout, stderr, and exit code. Returns a JSON result `{stdout, stderr, exit_code, sandbox_status}` on stdout.
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
- **Response:** the result's `sandbox_status` carries `{sandbox_id, tier, duration_ms, secrets_injected, status, limits}`. `status` is `"clean"` normally, or `"timeout"` when the payload was killed by the `timeout_sec` wall-clock deadline (see B-009). `tier` echoes `req.run.tier`. `secrets_injected` lists `{handle_prefix, delivery}` per successfully injected handle (handles are truncated to an 8-char prefix — never the full handle, never the credential). `limits` is the applied-caps record `{cpu_count, memory_mb, pids, disk_mb, timeout_sec, degraded[]}` (zeros mean "no limit requested"; `degraded` lists any secondary cap the host could not enforce — see B-009).
- **Side effects:** none beyond the returned JSON.
- **Failure modes:** none specific; this is assembled unconditionally.
- **References:** `run.go` `Run` return / `limitsReport`; [data-model.md](data-model.md).

### B-008: Select the isolation backend by tier

- **Trigger:** a run reaches the spawn step with `req.run.tier`.
- **Response:** `backendFor(tier)` selects the isolation backend. `""` and `"bubblewrap"` select the Tier-1 bubblewrap backend (`bwrapArgv`, unchanged); `"gvisor"` selects the Tier-2 gVisor backend. The gVisor backend writes an OCI bundle (`config.json` + a rootfs dir) to its own temp dir: the spec declares a `network` namespace with **no path** (a fresh empty netns — loopback only, no host/bridged networking, the OCI equivalent of `--unshare-all`), a read-only root with the host system dirs bind-mounted read-only, the payload read-only at `/payload.sh`, and the proxy socket as the only egress bind-mount at `/proxy.sock`. It then runs `runsc --network=none --host-uds=open --ignore-cgroups run --bundle <dir> <id>` (and `--rootless` when exec-sandbox runs unprivileged). `--host-uds=open` lets the payload connect to the existing proxy socket but never create host sockets. The bundle is removed after the process exits.
- **Side effects:** for the gVisor tier, an OCI bundle temp dir is created and removed (backend cleanup func). The chosen backend's runtime binary (`bwrap` or `runsc`) is exec'd.
- **Failure modes:** any tier other than `""`/`bubblewrap`/`gvisor` (e.g. `firecracker`) returns `{error: "tier not implemented: <tier>"}` — there is **no silent fall-back** to bubblewrap, and no payload runs. A bundle-write error returns `{error: <err>}`.
- **References:** ADR-001 D7, ADR-002; `run.go` `backendFor` / `Backend` / `bubblewrapBackend`; `gvisor.go` `gvisorBackend` / `gvisorOCISpec`; tests `TestBackendForRoutesByTier`, `TestBackendForUnknownTierErrors`, `TestGvisorSpecHasNoSharedNetwork`, `TestGvisorSpecMountsOnlyProxySocketForEgress`, `TestGvisorRunReachesAllowlistedHostAndBlocksOthers`.

### B-009: Enforce profile.limits (cpu / memory / pids / disk / wall-clock)

- **Trigger:** a run carries a `run.profile.limits` object with one or more of `cpu_count`, `memory_mb`, `pids`, `disk_mb`, `timeout_sec` (parsed by `parseLimits`; a missing/zero/non-positive field means "no limit").
- **Response:** each cap is enforced on the selected backend (ADR 003):
  - `timeout_sec` — backend-agnostic, in `Run()`: the child runs in its own process group (`Setpgid`) under a `context.WithTimeout`; on the deadline the whole group is `SIGKILL`ed, `sandbox_status.status` becomes `"timeout"`, and `exit_code` is `137`.
  - `memory_mb` → `RLIMIT_AS`: under bubblewrap via an in-sandbox `prlimit --as`; under gVisor via OCI `process.rlimits` (sentry-enforced). A payload that exceeds it is killed by the allocator.
  - `pids` → `RLIMIT_NPROC`: under bubblewrap via in-sandbox `prlimit --nproc` (per-sandbox because the bwrap user namespace gives a fresh process count); under gVisor via OCI `process.rlimits`. A fork bomb hits the cap ("Cannot fork").
  - `disk_mb` → writable-layer (`/tmp` tmpfs) size cap: bubblewrap `--size <bytes> --tmpfs /tmp`; gVisor `/tmp` tmpfs `size=` mount option. A write past the cap returns ENOSPC.
  - `cpu_count` → `taskset -c 0-(N-1)` affinity prefix on the spawn argv (inherited into the sandbox). Visible in-box under bubblewrap (`nproc`); under gVisor the in-box cpu view is virtualized, so cpu_count is verified host-side by the argv record (ADR 003 / agent-builder ADR 028).
- **Side effects:** the spawn argv and/or OCI `config.json` carry the caps; `sandbox_status.limits` records the applied values; the `exit` audit context carries `status`.
- **Failure modes:** `cpu_count` and `disk_mb` are **secondary** anti-DoS controls — when the host lacks the affordance (`taskset` absent; the writable layer reports it can't be size-capped via the `diskQuotaSupported` check), exec-sandbox **omits that one cap, prints a `WARNING` to stderr naming the control, records it in `sandbox_status.limits.degraded`, and continues** (the run is not failed — agent-builder ADR 027). `memory_mb`/`pids` are load-bearing: an inability to apply them (e.g. `prlimit` absent) is returned as `{error: …}`, not silently dropped.
- **References:** ADR 003; agent-builder ADR 027 (degrade) / ADR 028 (runtime-aware verification); `limits.go`; `run.go` `Run` (timeout/kill) / `bubblewrapBackend` / `bwrapArgv`; `gvisor.go` `gvisorBackend` / `applyLimitsToOCISpec`; tests `TestParseLimits`, `TestTimeoutTerminatesPayload`, `TestMemoryLimitKillsPayload_Bwrap`, `TestPidsLimitRejectsForkBomb_Bwrap`, `TestDiskLimitBlocksWrites_Bwrap`, `TestCPUAffinity_Bwrap`, `TestDiskQuotaDegradesGracefully_Bwrap`, `TestGvisorEnforcesLimits`, `TestGvisorOCISpecCarriesLimits`.

### B-010: Mount a writable host working directory at /work

- **Trigger:** a run carries a non-empty `run.workdir` (a host path).
- **Response:** the path is validated before spawn (`validateWorkdir`: trimmed, made absolute, must be an existing **directory**) and then bind-mounted **read-write** at `/work` inside the sandbox, with the payload's cwd set to `/work`. Under bubblewrap this is `--bind <workdir> /work --chdir /work` (writable — **not** `--ro-bind`); under gVisor it is an OCI `/work` bind mount whose `options` omit `ro` plus `process.cwd = "/work"` (`applyWorkdirToOCISpec`). A file seeded in the host dir is readable by the payload at `/work`, and a file the payload writes under `/work` persists to the host directory after the run. `/work` is the **only** writable host surface: the rootfs and system dirs (`/usr`, `/etc`, …) stay read-only and the network namespace stays unshared (the no-network invariant is untouched — the only egress is still `/proxy.sock`).
- **Side effects:** the spawn argv (bwrap) and/or OCI `config.json` (gVisor) carry the writable `/work` mount and the cwd; the payload's writes land in the caller-supplied host directory.
- **Failure modes:** a non-empty `run.workdir` that does not exist, or resolves to a non-directory, returns `{error: "invalid run.workdir: …"}` **before** the proxy starts or any payload runs — there is no silent fall-back to a no-mount run. An empty/absent `run.workdir` produces **no** `/work` mount and leaves cwd unchanged (backward compatible).
- **References:** ADR 004; agent-builder `containment/execution-box/run.sh` (`/work,rw` + `--workdir /work`) / `internal/sandbox/podman/run.go` `validateWorktree`; `run.go` `validateWorkdir` / `bwrapArgv`; `gvisor.go` `applyWorkdirToOCISpec`; tests `TestValidateWorkdir`, `TestWorkdirSeededFileReadable_Bwrap`, `TestWorkdirWritePersists_Bwrap`, `TestWorkdirIsCwd_Bwrap`, `TestNoWorkdirNoMount_Bwrap`, `TestBadWorkdirFailsLoud`, `TestWorkdirEndToEnd_Gvisor`, `TestWorkdirOCISpec`, `TestOnlyWorkdirWritable_Bwrap`.

### B-011: Mount read-only host paths (FileRead) and provision the payload's env/PATH

- **Trigger:** a run carries one or more `{"type":"FileRead","paths":[…]}` capabilities in `run.profile.capabilities`, and/or a non-empty `run.env`.
- **Response:** the FileRead paths are collected (`fileReadPaths`; multiple entries union their lists) and validated before spawn (`validateFileReads`: each must be **absolute** and **exist**) — then each is bind-mounted **read-only** at the **same** host path inside the sandbox. Under bubblewrap this is `--ro-bind <path> <path>` (read-only — **not** the writable `--bind`); under gVisor it is an OCI mount `{destination:<path>, type:bind, source:<path>, options:[ro,rbind]}` (`applyFileReadToOCISpec`). `run.env` is exported into the sandbox: a `PATH` entry replaces the bare default `PATH=/usr/bin:/bin`, every other entry is exported `k=v` (bwrap `--setenv`; gVisor `process.env` via `applyEnvToOCISpec`), in a deterministic order (PATH first, then sorted keys). A payload can **read and execute** a FileRead-mounted tool, and — with the tool's dir on `run.env["PATH"]` — resolve it by name (`command -v <tool>`). A FileRead mount is **read-only**: a write to it fails (EROFS/permission), distinct from the writable `/work` (B-010). FileRead adds read-only host paths and PATH/env entries only — it opens **no** egress and **no** writable surface; the network namespace stays unshared (the only egress is still `/proxy.sock`).
- **Side effects:** the spawn argv (bwrap) and/or OCI `config.json` (gVisor) carry the read-only FileRead mounts and the provisioned env.
- **Failure modes:** a FileRead path that is **relative** or does **not exist** returns `{error: "invalid FileRead path: …"}` **before** the proxy starts or any payload runs — there is no silent fall-back to a no-mount run (same ordering as `validateWorkdir`). An empty/absent FileRead and an empty/absent `run.env` produce **no** extra mounts and the bare default PATH (backward compatible).
- **References:** ADR 005; agent-builder `containment/execution-box/run.sh` (`--mount …,ro` gate-tools dir + `gate_tool_path` PATH; `resolve_gate_tools`); `run.go` `fileReadPaths` / `validateFileReads` / `bwrapArgv` / `envSetenvPairs`; `gvisor.go` `applyFileReadToOCISpec` / `applyEnvToOCISpec` / `envList`; tests `TestFileReadParsing`, `TestValidateFileReads`, `TestFileReadMountReadableExecutable_Bwrap`, `TestFileReadOnPathResolves_Bwrap`, `TestFileReadMountIsReadOnly_Bwrap`, `TestNoFileReadNoEnv_Bwrap`, `TestBadFileReadFailsLoud`, `TestFileReadArgv_Bwrap`, `TestFileReadEndToEnd_Gvisor`, `TestFileReadOCISpec`, `TestFileReadRegressionBaseUnchanged`.

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
- **Every requested `profile.limits` cap is enforced or its degradation is recorded** (B-009): a cap is applied on the active backend, or — for the secondary `cpu_count`/`disk_mb` controls only — it appears in `sandbox_status.limits.degraded` with a stderr `WARNING`. No requested cap is ever silently ignored.
- **The only writable host surface is `run.workdir` at `/work`** (B-010), and only when the caller sets it. The rootfs, system dirs, `/payload.sh`, and any `FileRead{paths}` mounts (B-011) are always read-only; the writable surface never widens beyond the single caller-supplied directory, and no host mount (writable or read-only) ever opens an egress path (the network stays unshared).
- **Every successful run returns the full result shape** (B-005); there is no partial-result path on the success side.
