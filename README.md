# exec-sandbox

[![License: Apache 2.0](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/tkdtaylor/exec-sandbox)](go.mod)
[![Last commit](https://img.shields.io/github/last-commit/tkdtaylor/exec-sandbox)](https://github.com/tkdtaylor/exec-sandbox/commits)

**OS-level execution isolation for untrusted agent-generated code.** The payload runs in a sandbox with no network; its only egress is a host-side proxy with a domain allowlist and credential injection. Tiered isolation backends (bubblewrap for Tier 1, gVisor for Tier 2, and Firecracker microVMs for Tier 3) are shipped, all enforcing the same security boundary.

It's built for **operators who need to isolate code execution**, as one piece of the [Secure Agent Ecosystem](https://github.com/tkdtaylor/agent-builder#the-building-blocks) alongside vault, policy-engine, and audit-trail. Apache-2.0 licensed.

> **Status.** All three isolation tiers are shipped behind a stable contract: Tier 1 (bubblewrap with `seccomp/tier1.bpf` syscall filter), Tier 2 (gVisor `runsc`), and Tier 3 (Firecracker microVM with host vsock egress bridge). Resource limits, writable `/work` directory, read-only mounts (`FileRead`), vault credential injection (`proxy` and `env` modes), audit emission, per-host verb allowlists, and ADR 017 v2 host-key attestation (`keygen`, `verify-attestation`, `wiring.attestation_key`) are shipped and verified. Current operational caveats: `origin_map` supports HTTP upstream only (HTTPS CONNECT is deferred), and Firecracker supports `proxy`-only credential mode (in-guest env mode is deferred). See the [roadmap](docs/plans/roadmap.md) for future items.

## Contents

- [Quick start](#quick-start)
- [How it works](#how-it-works)
- [Isolation tiers](#isolation-tiers)
- [Develop locally](#develop-locally)
- [Tech stack](#tech-stack)
- [Sponsorship](#sponsorship)
- [Enterprise support](#enterprise-support)
- [License](#license)

## Quick start

```bash
git clone https://github.com/tkdtaylor/exec-sandbox && cd exec-sandbox
go test ./...                    # tests (sandbox tests skip without bubblewrap)
```

The primary interface, `exec-sandbox run`, reads a JSON `RunRequest` on stdin (containing the payload, isolation tier, resource limits, credential handles, and network allowlist) and writes a result JSON on stdout. Subcommands `keygen` and `verify-attestation` generate host ed25519 signing keypairs and verify attestation payloads against a trust root. To see it integrated into a working system, see [agent-builder](https://github.com/tkdtaylor/agent-builder), which uses it as the default execution backend.

## How it works

You hand it untrusted code. A RunRequest specifies the payload, which tier to use, and the network allowlist (domain plus per-host HTTP-verb restrictions). exec-sandbox spawns the sandbox, runs the payload, captures output and exit code, injects credentials at the egress proxy boundary (never into the sandbox or guest in proxy mode), emits spawn and exit events to audit-trail, and returns the result. The sandbox has no network namespace; its only path out is the proxy socket (bind-mounted in Tiers 1 and 2, bridged over vsock in Tier 3).

```
Payload → Tier 1 (bwrap) / Tier 2 (gVisor) / Tier 3 (Firecracker)
          ↓
          No network, /proxy.sock egress only (vsock bridge for Tier 3)
          ↓
          vault.inject(handle) at boundary (credential never in sandbox)
          ↓
          stdout/stderr captured, limits enforced
          ↓
          Result to audit-trail, returned to caller
```

The contract (`run(payload, profile, tier, secret_refs) → {stdout, stderr, exit_code, sandbox_status}`) is stable across all tiers. Add a new backend without touching the caller. Detailed architecture: [overview.md](docs/architecture/overview.md), [diagrams.md](docs/architecture/diagrams.md), and the [spec](docs/spec/SPEC.md).

## Isolation tiers

| Tier | Backend | Status | Isolation | Overhead |
|---|---|---|---|---|
| 1 | bubblewrap + seccomp | Shipped | Namespaces + capability drop + seccomp-BPF filter | ~5ms |
| 2 | gVisor/`runsc` | Shipped | Userspace kernel + syscall interception | ~100ms |
| 3 | Firecracker microVM | Shipped | Hardware-assisted isolation (KVM) + vsock bridge | ~200ms spawn |

All three tiers enforce the same security model: no network namespace (no-NIC in Tier 3 microVM), domain allowlist on egress, per-host HTTP-verb restrictions, vault-injected credentials (never in sandbox or guest in proxy mode), resource limits (cpu/mem/pids/disk/timeout), and output capping. Tier selection happens per run; the contract remains stable across all tiers.

## Develop locally

```bash
make build          # go build -o bin/exec-sandbox ./...
make test           # go test ./...
make fmt            # go fmt ./...
go test -run FitnessNoShareNet ./...      # run a specific fitness rule
make fitness        # run all architectural fitness checks
```

Contributing runs through a test-spec-first, one-task-one-branch workflow. Read [AGENTS.md](AGENTS.md) (the canonical, harness-neutral briefing) and [CONTRIBUTING.md](CONTRIBUTING.md) before starting; tasks and their specs live under [docs/tasks/](docs/tasks/).

## Tech stack

Go 1.26: single-binary CLI, standard library only, no third-party dependencies. External runtime dependencies: bubblewrap (Tier 1), gVisor/runsc (Tier 2), Firecracker (Tier 3). Integrates with vault and audit-trail over Unix-socket JSON-lines IPC.

## Sponsorship

exec-sandbox is independent, open-source security tooling. If it saves you time or risk, [sponsoring its development](https://github.com/sponsors/tkdtaylor) is the most direct way to keep it maintained.

## Enterprise support

Commercial support, integration help, and SLAs are available. Apache-2.0 means you can build on exec-sandbox freely; paid support is a partner if you want one, never a requirement. Contact [tools@taylorguard.me](mailto:tools@taylorguard.me).

## License

[Apache License 2.0](LICENSE): consistent with the Secure Agent Ecosystem. See [NOTICE](NOTICE) for attribution and disclaimers, and [CONTRIBUTING.md](CONTRIBUTING.md) for the inbound=outbound / DCO contribution terms.
