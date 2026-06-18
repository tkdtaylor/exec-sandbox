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

The v0→v1 work, each item a self-contained task. The tier seam (currently `bwrapArgv`) becomes a
dispatch point so higher tiers slot in **without changing the `run()` contract**.

| # | Work | Status |
|---|------|--------|
| 1 | **Tier 2: gVisor (`runsc`) backend** behind the `tier` seam — `tier=="gvisor"` runs via runsc OCI, same contract, same no-network + proxy-only egress + audit. | ✅ shipped (task 001, ADR-002) |
| — | Tier 3: Firecracker / Kata (hardware isolation) for highest-risk actions. | future |
| — | Enforce `profile.limits` (cpu/mem/disk/timeout) — accepted but not enforced in v0. | future |
| — | Signed `sandbox_identity.attestation` (v0 uses random bytes). | future |
| — | Secrets refresh/rotation during a run (v0 injects once at spawn). | future |
| — | Egress hardening: two-layer DNS-proxy + nftables design (OpenSandbox OSEP-0001 reference). | future |

## Notes for the orchestrator

This repo is built out one task at a time by **agent-builder**: it reads this roadmap +
`docs/tasks/backlog/NNN-*.md`, builds the next ready task in a sandbox, runs the verification gate,
and opens a PR. The working v0 source is not rewritten — v1 work extends it behind the tier seam.
