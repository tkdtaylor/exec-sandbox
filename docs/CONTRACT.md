# exec-sandbox v1 contract

Mirrors `interface-contracts.md §2`, validated by the
tracer-bullet (A1/A2/A3).

## run(payload, profile, tier, secret_refs) -> result
```
payload = shell script run as /payload.sh
profile = { capabilities:[ {type:NetConnect, allowlist:["host:443"], methods:["GET",…]},  # methods optional; absent/[] ⇒ all verbs
                            {type:FileRead, paths:[…]}, … ],
            limits:{ cpu_count, memory_mb, pids, disk_mb, timeout_sec, max_output_bytes } }   # enforced — see below
tier    = bubblewrap | gvisor | firecracker        # bubblewrap + gvisor wired; firecracker → "tier not implemented"
secret_refs = [ handle ]                            # opaque; exec-sandbox calls vault.inject
workdir = host path                                 # optional; "" → no mount (see below)
env     = { KEY: value }                            # optional; PATH replaces the bare default; {} → unchanged (see below)

result = { stdout, stderr, exit_code,
           sandbox_status:{ sandbox_id, tier, duration_ms, secrets_injected:[…],
                            status,                 # "clean" | "timeout"
                            limits:{ cpu_count, memory_mb, pids, disk_mb, timeout_sec, max_output_bytes,
                                     degraded:[…], output_truncated:[…] } } }   # output_truncated: streams the cap dropped bytes from
```

## Writable working directory (`run.workdir`)
Optional host path (ADR 004). When non-empty, the named host directory is bind-mounted
**read-write** at `/work` inside the sandbox and the payload's cwd is set to `/work` — the one
writable host surface (everything else stays read-only and the network stays unshared). bwrap
applies it as `--bind <workdir> /work --chdir /work`; gVisor as a writable OCI `/work` bind mount
(`options` without `ro`) + `process.cwd = "/work"`. The path is validated before spawn — it must be
an existing directory — and a bad path is a hard `{error}` (no silent fall-back). When empty/absent
there is no `/work` mount and behavior is exactly as before (backward compatible). This is the
**writable** host-path mechanism; the read-only `FileRead{paths}` capability is its complement, not
a substitute — see below and `configuration.md`.

## Read-only host mounts (`FileRead{paths}`) + env provisioning (`run.env`)
**Implemented** (ADR 005). Each `{type:FileRead, paths:[…]}` capability bind-mounts its host paths
**read-only** at the **same** path inside the sandbox (bwrap `--ro-bind <p> <p>`; gVisor OCI mount
`options:[ro,rbind]`); multiple `FileRead` entries union their paths. Each path is validated before
spawn — it must be **absolute** and **exist** — and a relative/nonexistent path is a hard `{error}`
(no silent skip), the same ordering as `run.workdir`. `run.env` (`map[string]string`) is exported
into the sandbox: `PATH` replaces the bare default `PATH=/usr/bin:/bin`, every other entry is
exported `k=v`. Combined, `run.env["PATH"]` puts a FileRead-mounted toolchain dir on PATH so a
payload can `command -v <tool>` and run it. FileRead is **read-only** (a write fails — only `/work`
is writable), opens **no** egress, and leaves the network unshared. Empty `FileRead`/`run.env` ⇒ no
extra mounts, bare PATH (backward compatible). `run.env` carries no secret — proxy-mode credentials
never enter the sandbox.

## Resource limits (`profile.limits`)
Enforced on every wired tier (ADR 003). `cpu_count` → `taskset` CPU affinity; `memory_mb` →
`RLIMIT_AS`; `pids` → `RLIMIT_NPROC`; `disk_mb` → writable-layer (`/tmp` tmpfs) size cap;
`timeout_sec` → host-side wall-clock process-group kill (`status: "timeout"`). bwrap applies the
rlimits in-sandbox via `prlimit` and the disk cap via `--size`; gVisor applies them via OCI
`process.rlimits` + the tmpfs `size=` option. `cpu_count`/`disk_mb` are secondary controls that
degrade gracefully (stderr `WARNING` + `sandbox_status.limits.degraded`) on hosts that can't
enforce them; the load-bearing controls never silently weaken. `max_output_bytes` (ADR 007) is
enforced **host-side, above the `tier` seam**: `Run()` captures each of stdout/stderr through a
capping writer that retains at most `max_output_bytes` bytes per stream and **drops** the overflow
without erroring the payload's pipe (its exit is unchanged). stdout/stderr are capped
**independently** at the same ceiling; the truncated length + the `sandbox_status.limits.output_truncated`
record are **identical** under bubblewrap and gVisor (the backend argv/OCI spec are unchanged by the
cap). Missing/zero/non-positive ⇒ no cap (full output, `output_truncated: []`).

## vault.inject (called by exec-sandbox at spawn)
Pull-triggered push: present `{handle, sandbox_identity}`. In proxy mode vault returns
`{credential, binding:{host,header,scheme}}`; exec-sandbox loads it into the egress proxy
(never the sandbox). See vault's contract.

## Network boundary
`bwrap --unshare-all` → no network namespace. Bind-mounted `/proxy.sock` is the only egress.
The proxy enforces the domain allowlist (from `profile.NetConnect`) — host check **first**
(unlisted host → `403 blocked-by-allowlist`) — and, when an entry carries an optional `methods`
set, a **per-host HTTP-verb allowlist** (ADR 008): a method not in a host's non-empty set →
`403 blocked-by-method` (same status, distinct body) with **no** outbound connection and **no**
credential injection. Absent/empty `methods` ⇒ all verbs (backward compatible). Verb matching is
case-insensitive (canonical upper-case). The verb check only **narrows** egress — no new route, no
`--share-net`, no second socket. The verb *decision* is policy-engine's; exec-sandbox **enforces**.
The proxy injects credentials into allowlisted, verb-permitted requests. The proxy speaks HTTP
over the Unix socket; it does not TLS-terminate or tunnel arbitrary TCP. HTTPS via `CONNECT` is a
known gap; SOCKS5 was evaluated and rejected (ADR-011).
