# exec-sandbox — OS execution isolation with tiered, risk-selected runtimes

Answers one question: *when agent-generated code runs, is its execution boundary isolated
from the host and from other sandboxes?* exec-sandbox runs the code in a sandbox with **no
network**, and its **only** path out is a host-side egress proxy with a domain allowlist.
`vault` plugs credential injection into that proxy — in proxy mode the secret never enters
the sandbox at all.

- **Real Tier-1 isolation** — `bwrap --unshare-all` (no network namespace; a direct connect returns curl `000`)
- **exec-sandbox owns the network boundary** — `--network none` + egress proxy (Unix socket) + allowlist
- **vault.inject at spawn** — pull-triggered push; presents `{handle, sandbox_identity}`, receives `{credential, binding}` (proxy) and injects it into allowlisted egress
- **Audit emission** — spawn / inject / exit events to `audit-trail`

> Prior-art verdict (from the project's internal design notes): **BUILD an open tiered orchestration harness** — adopt OCI runtimes (gVisor/Firecracker/Kata) as pluggable backends; derive Tier 1 from `@anthropic-ai/sandbox-runtime` (Apache-2.0). The value-add is the harness: policy→tier selection, vault credential injection, audit emission. **Language: Go** (bubblewrap/OCI/containerd ecosystem). **License: PolyForm Noncommercial 1.0.0.**

## Contract (interface-contracts.md §2, v1)

```
run(payload, profile, tier, secret_refs) -> { stdout, stderr, exit_code, sandbox_status }
profile = { capabilities:[ NetConnect{allowlist}, FileRead{paths}, … ], limits:{cpu,mem,disk,timeout} }
tier    = bubblewrap | gvisor | firecracker        # v0 implements bubblewrap
```

`secret_refs` carries opaque handles; exec-sandbox calls `vault.inject(handle,
sandbox_identity, mode)` itself at the boundary. Validated by the tracer-bullet (A1/A2/A3).

## Build & run

```sh
go build ./... && go test ./...      # the integration tests need bubblewrap (skip if absent)
echo '{"run":{"payload":"…","profile":{…},"tier":"bubblewrap","secret_refs":["…"]},
       "wiring":{"vault_socket":"…","audit_socket":"…","origin_map":{…},"injection_mode":"proxy"}}' \
  | exec-sandbox run
```

## Status

🚧 **v0 implementation, v1 contract.** Working Tier-1 bubblewrap isolation + Unix-socket
egress proxy + allowlist + vault.inject (proxy mode) + audit emission (ported from the
tracer-bullet). **Deferred (v1):** Tier 2/3 (gVisor / Firecracker /
Kata) behind the OCI seam, full seccomp profile, env-mode injection + wipe clock, SOCKS5
proxy, resource cgroup limits, sandbox_identity attestation signatures. See
[docs/CONTRACT.md](docs/CONTRACT.md).

## Adapter seam & standards

OCI Runtime Spec + Linux seccomp-BPF (+ WASI for a future WASM tier). Pluggable backends:
bubblewrap (Tier 1, default), gVisor/runsc (Tier 2), Firecracker/Kata (Tier 3).
