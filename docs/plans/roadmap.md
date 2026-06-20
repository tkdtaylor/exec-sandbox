# Roadmap — exec-sandbox

A tiered, risk-selected OS execution-isolation block: run untrusted agent-generated code with no
network, egress only through a credential-injecting host-side proxy, composed with vault (credential
injection), policy-engine (risk → tier), and audit-trail (event emission).

Authoritative design: the project's internal design notes.
As-built foundational stack: [ADR 001](../architecture/decisions/001-foundational-stack.md).

## v0 — Tier-1 isolation + egress proxy + vault.inject — ✅ shipped

Working today (`main.go`/`run.go`/`proxy.go`): the `run(payload, profile, tier, secret_refs)`
contract over stdin/stdout JSON; **bubblewrap `--unshare-all`** Tier-1 isolation (no network
namespace); host-side Unix-socket egress proxy with a domain allowlist; `vault.inject` at spawn
(proxy and env modes); `spawn`/`inject`/`exit` audit emission. Tier-1 only; the `tier` field accepts
`bubblewrap|gvisor|firecracker` but only `bubblewrap` is wired.

## v1 — Tiered runtimes behind the OCI seam + contract hardening

The v0→v1 work, each item a self-contained task. The tier seam (now `backendFor(tier)`) is the
dispatch point so higher tiers slot in **without changing the `run()` contract**.

### Shipped

| Work | Status |
|------|--------|
| **Tier 2: gVisor (`runsc`) backend** behind the `tier` seam — `tier=="gvisor"` runs via runsc OCI, same contract, same no-network + proxy-only egress + audit. | ✅ task 001, ADR-002 |
| Enforce `profile.limits` (cpu/mem/pids/disk/timeout) + output caps. | ✅ tasks 002/006, ADR-003/007 |
| Writable `/work` mount + toolchain mount/PATH + env provisioning. | ✅ tasks 003/004, ADR-004/005 |
| Per-host verb allowlist on the egress proxy. | ✅ task 007, ADR-008 |
| Snapshot/restore pristine-baseline reset boundary. | ✅ task 008, ADR-009 |

### Planned — filed in `docs/tasks/backlog/` (test-spec-first, ready to pick up)

| ID | Work | Status |
|----|------|--------|
| 009 | **Wire the fitness functions** — `fitness-<id>` + `fitness:` umbrella over the 9 block rules; author the 3 missing checks (F-001 bwrap argv, F-002 cred-leak, F-004 prefix bound); flip those `proposed → active`. | 📋 ready |
| 010 | Terminal audit event on early proxy-start failure (resolves behaviors.md B-007 TODO). | 📋 ready |
| 011 | Signed `sandbox_identity.attestation` (v0 uses random bytes). | ⚠️ blocked — needs ADR + vault-consumer-contract check (trust root: ephemeral ed25519 vs host-provided key) |
| 012 | Env-mode credential injection + wipe clock (v0 wires proxy-mode only). | 📋 ready — confirm vault env-mode field names |

### Planned — Tier 3 Firecracker epic (ADR-010, dependency-ordered 013 → 018)

| ID | Work | Status |
|----|------|--------|
| 013 | Firecracker tier dispatch + config-generator skeleton (no-NIC by construction; no VMM launch). | 📋 ready — epic root |
| 014 | No-NIC + vsock-bridge egress enforcement (the egress crux; credential never enters the guest). | 📋 ready after 013 — target L6 |
| 015 | Guest boot: kernel image + rootfs + jailer launch. | ⚠️ blocked — ADR-010 Q1 (kernel/rootfs sourcing) + Q3 (jailer privilege model); needs ADR amendment |
| 016 | `profile.limits` → microVM machine-config mapping. | 📋 ready after 013/015 |
| 017 | `/work` + FileRead mount semantics in the microVM. | ⚠️ Q2 (mount mechanism) — small in-task decision |
| 018 | Teardown + spec/diagram sync + no-`network-interface` fitness function. | 📋 ready after 013–017 |

### Future — deferred, not yet filed (need a scoping decision/ADR before a task file)

| Work | Why deferred |
|------|--------------|
| Full seccomp-BPF profile (default-deny syscall baseline). | Needs a profile-design decision (allowlist scope, per-tier baseline). |
| Egress hardening: two-layer DNS-proxy + `nftables` (OpenSandbox **OSEP-0001** reference). | Evaluated as *reference design, not adopted dependency* (the internal design hub task 019); revisit is a go/no-go, not yet a task. |
| SOCKS5 proxy (alongside the HTTP egress proxy). | Product decision — do we need it at all — before scoping. |
| Warm-pool / snapshot reuse (ADR-009 Q2/Q3). | Snapshot/restore is built and proven leak-free, but the reuse trigger + pool sizing + eviction are undesigned. |
| Tier 4: Hyperlight (micro-isolate). | Watching brief only — see ADR-006. |
| Secrets refresh/rotation during a run (v0 injects once at spawn). | Lower priority; revisit after env-mode (012) lands. |

## Notes for the orchestrator

This repo is built out one task at a time by **agent-builder**: it reads this roadmap +
`docs/tasks/backlog/NNN-*.md`, builds the next ready task in a sandbox, runs the verification gate,
and opens a PR. The working v0 source is not rewritten — v1 work extends it behind the tier seam.
