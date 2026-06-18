# exec-sandbox v1 contract

Mirrors `interface-contracts.md §2`, validated by the
tracer-bullet (A1/A2/A3).

## run(payload, profile, tier, secret_refs) -> result
```
payload = shell script run as /payload.sh
profile = { capabilities:[ {type:NetConnect, allowlist:["host:443"]},
                            {type:FileRead, paths:[…]}, … ],
            limits:{ cpu_count, memory_mb, pids, disk_mb, timeout_sec } }   # enforced — see below
tier    = bubblewrap | gvisor | firecracker        # bubblewrap + gvisor wired; firecracker → "tier not implemented"
secret_refs = [ handle ]                            # opaque; exec-sandbox calls vault.inject
workdir = host path                                 # optional; "" → no mount (see below)

result = { stdout, stderr, exit_code,
           sandbox_status:{ sandbox_id, tier, duration_ms, secrets_injected:[…],
                            status,                 # "clean" | "timeout"
                            limits:{ cpu_count, memory_mb, pids, disk_mb, timeout_sec, degraded:[…] } } }
```

## Writable working directory (`run.workdir`)
Optional host path (ADR 004). When non-empty, the named host directory is bind-mounted
**read-write** at `/work` inside the sandbox and the payload's cwd is set to `/work` — the one
writable host surface (everything else stays read-only and the network stays unshared). bwrap
applies it as `--bind <workdir> /work --chdir /work`; gVisor as a writable OCI `/work` bind mount
(`options` without `ro`) + `process.cwd = "/work"`. The path is validated before spawn — it must be
an existing directory — and a bad path is a hard `{error}` (no silent fall-back). When empty/absent
there is no `/work` mount and behavior is exactly as before (backward compatible). This is the
**writable** host-path mechanism; the read-only `FileRead{paths}` capability (still unimplemented)
is its complement, not a substitute — see `configuration.md`.

## Resource limits (`profile.limits`)
Enforced on every wired tier (ADR 003). `cpu_count` → `taskset` CPU affinity; `memory_mb` →
`RLIMIT_AS`; `pids` → `RLIMIT_NPROC`; `disk_mb` → writable-layer (`/tmp` tmpfs) size cap;
`timeout_sec` → host-side wall-clock process-group kill (`status: "timeout"`). bwrap applies the
rlimits in-sandbox via `prlimit` and the disk cap via `--size`; gVisor applies them via OCI
`process.rlimits` + the tmpfs `size=` option. `cpu_count`/`disk_mb` are secondary controls that
degrade gracefully (stderr `WARNING` + `sandbox_status.limits.degraded`) on hosts that can't
enforce them; the load-bearing controls never silently weaken.

## vault.inject (called by exec-sandbox at spawn)
Pull-triggered push: present `{handle, sandbox_identity}`. In proxy mode vault returns
`{credential, binding:{host,header,scheme}}`; exec-sandbox loads it into the egress proxy
(never the sandbox). See vault's contract.

## Network boundary
`bwrap --unshare-all` → no network namespace. Bind-mounted `/proxy.sock` is the only egress.
The proxy enforces the domain allowlist (from `profile.NetConnect`) and injects credentials
into allowlisted requests. v0 = HTTP over the Unix socket; v1 adds TLS-terminating + SOCKS5.
