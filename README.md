# exec-sandbox — OS execution isolation with tiered, risk-selected runtimes

Answers one question: *when agent-generated code runs, is its execution boundary isolated from the host and from other sandboxes?* exec-sandbox runs the code in a sandbox with **no network**, and its **only** path out is a host-side egress proxy with a domain allowlist. [vault](https://github.com/tkdtaylor/vault) plugs credential injection into that proxy — in proxy mode the secret never enters the sandbox at all.

- **Tiered isolation** — Tier-1 `bwrap --unshare-all` (no network namespace; a direct connect returns curl `000`) and Tier-2 gVisor/`runsc` over a generated OCI bundle, selected per run by `tier` (ADR 002)
- **exec-sandbox owns the network boundary** — `--network none` + egress proxy (Unix socket) + domain allowlist, narrowed by an optional **per-host HTTP-verb allowlist** (ADR 008)
- **vault.inject at spawn** — pull-triggered push; presents `{handle, sandbox_identity}`, receives `{credential, binding}` (proxy) and injects it into allowlisted egress
- **Per-run resource limits** — cpu / memory / pids / disk / wall-clock plus host-side stdout/stderr output caps enforced above the tier seam (ADR 003, ADR 007)
- **Controlled host I/O** — optional writable `/work` dir, read-only `FileRead` mounts, and `env`/`PATH` provisioning (ADR 004, ADR 005)
- **Leak-proof reset** — snapshot/restore returns the sandbox to a pristine baseline between runs (ADR 009)
- **Audit emission** — spawn / inject / exit events to [audit-trail](https://github.com/tkdtaylor/audit-trail)

> Prior-art verdict (from ecosystem prior-art scoping): **BUILD an open tiered orchestration harness** — adopt OCI runtimes (gVisor/Firecracker/Kata) as pluggable backends; derive Tier 1 from `@anthropic-ai/sandbox-runtime` (Apache-2.0). The value-add is the harness: policy→tier selection, vault credential injection, audit emission. **Language: Go** (bubblewrap/OCI/containerd ecosystem). **License: Apache-2.0.**

## Scope

**What exec-sandbox does:** OS-level execution isolation for agent-generated code — tiered namespaces/seccomp → gVisor → Firecracker, selected per run, owning the network egress boundary.

**What it does *not* do (and which sibling owns it instead):**
- Inspect LLM content — prompts, outputs, tool-calls → **[armor](https://github.com/tkdtaylor/armor)**
- Store or own secret values — it *receives* them at the boundary at spawn → **vault**
- Decide whether an action is permitted → **[policy-engine](https://github.com/tkdtaylor/policy-engine)**
- Ship a standalone WASM / pre-compiled-tool sandbox — that is a *possible future tier here* (nested inside OS isolation, since WASM is not a standalone trust boundary), **not** a separate block; typed WASM tool *invocation* is an MCP-WASM interop concern, not ours to rebuild. See [ADR 012](docs/architecture/decisions/012-wasm-tool-isolation-scope.md).

`exec-sandbox` is one block in a composable secure-agent ecosystem — each block is standalone and independently usable, and composes with its siblings over published contracts rather than absorbing their responsibilities (no central "god object").

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
make build && go test ./...          # the integration tests need bubblewrap (skip if absent)
echo '{"run":{"payload":"…","profile":{…},"tier":"bubblewrap","secret_refs":["…"]},
       "wiring":{"vault_socket":"…","audit_socket":"…","origin_map":{…},"injection_mode":"proxy"}}' \
  | ./bin/exec-sandbox run
```

## Documentation

- [docs/architecture/overview.md](docs/architecture/overview.md) — system design and design principles
- [docs/architecture/diagrams.md](docs/architecture/diagrams.md) — C4 diagrams and runtime flows
- [docs/spec/SPEC.md](docs/spec/SPEC.md) — authoritative spec
- [docs/plans/roadmap.md](docs/plans/roadmap.md) — roadmap and current status

## Status

🚧 **v0 implementation, v1 contract.** Working Tier-1 bubblewrap **and** Tier-2 gVisor/`runsc` isolation behind the OCI seam + Unix-socket egress proxy + domain allowlist + per-host verb allowlist + per-run resource limits (cpu/mem/pids/disk/timeout/output) + writable `/work`, read-only `FileRead` mounts, env provisioning + snapshot/restore reset + vault.inject (proxy mode) + audit emission (ported from the tracer-bullet).

See the [roadmap](docs/plans/roadmap.md) for filed/deferred work and known gaps. See also [docs/CONTRACT.md](docs/CONTRACT.md).

## Adapter seam & standards

OCI Runtime Spec + Linux seccomp-BPF (+ WASI for a future WASM tier). Pluggable backends:
bubblewrap (Tier 1, default), gVisor/runsc (Tier 2), Firecracker/Kata (Tier 3).

**Egress hardening (v1) — reference architecture:** the current egress is a single-layer
Unix-socket HTTP proxy with a domain allowlist plus an optional per-host verb allowlist (the
Tier-1 floor). For the v1 network-namespace
egress filter, the reference is Alibaba **OpenSandbox OSEP-0001**'s two-layer default-deny
model — Layer 1 DNS proxy (iptables `REDIRECT`), Layer 2 `nftables` filter, with graceful
degradation to DNS-only when `CAP_NET_ADMIN` is unavailable (Apache-2.0). Evaluated as prior
art: a **reference design, not an adopted dependency** (rejected for
adoption — see ADR-011) — exec-sandbox stays a modular block (the OpenSandbox platform lacks
pluggable policy-engine / external vault / separated audit-trail). See
`exec-sandbox.md` §1.

## License

exec-sandbox is licensed under the **Apache License 2.0** — free to use, modify, and distribute, including in commercial and proprietary products. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

> **Security notice:** exec-sandbox is a security tool provided **as-is, without warranty**. It does not guarantee the security of any system. See the disclaimer in [NOTICE](NOTICE).

## Enterprise Support

Need hardened deployments, integration help, or a support SLA? **Commercial support and consulting are available.**

📧 Contact **[tools@taylorguard.me](mailto:tools@taylorguard.me)**

## Sponsorship

exec-sandbox is independent, open-source security tooling. If it saves you time or risk, consider sponsoring continued development:

- 💜 [GitHub Sponsors](https://github.com/sponsors/tkdtaylor)

## Contributing

Contributions are welcome and become part of the project under Apache-2.0. See [CONTRIBUTING.md](CONTRIBUTING.md). We use the **Developer Certificate of Origin (DCO)** — sign off your commits with `git commit -s`. No CLA required.
