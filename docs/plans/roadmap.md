# Roadmap: exec-sandbox

A tiered, risk-selected OS execution-isolation block: run untrusted agent-generated code with no network, egress only through a credential-injecting host-side proxy, composed with vault (credential injection), policy-engine (risk → tier), and audit-trail (event emission).

Authoritative contract: [docs/CONTRACT.md](../CONTRACT.md). As-built foundational stack: [ADR 001](../architecture/decisions/001-foundational-stack.md).

## v0: Tier-1 isolation + egress proxy + vault.inject (✅ shipped)

The foundational implementation (`main.go`, `run.go`, `proxy.go`): the `run(payload, profile, tier, secret_refs)` contract over stdin/stdout JSON; bubblewrap `--unshare-all` Tier-1 isolation (no network namespace); host-side Unix-socket egress proxy with a domain allowlist; `vault.inject` at spawn (proxy and env modes); and `spawn`/`inject`/`exit` audit emission.

## v1: Tiered runtimes behind the OCI seam + contract hardening (✅ shipped)

The v0→v1 work, extending isolation behind the `backendFor(tier)` dispatch point without changing the `run()` contract. All three isolation tiers, host-side limits, reset boundaries, seccomp filtering, and host-key attestation are shipped and verified.

### Shipped tasks

| ID | Work | Status |
|----|------|--------|
| 001 | Tier 2: gVisor (`runsc`) backend behind the `tier` seam (OCI bundle, no-network, proxy-only egress). | ✅ task 001, ADR 002 |
| 002 | Enforce `profile.limits` (cpu, mem, pids, disk, timeout) on bubblewrap and gVisor. | ✅ task 002, ADR 003 |
| 003 | Writable `/work` working-directory mount on bubblewrap and gVisor. | ✅ task 003, ADR 004 |
| 004 | `FileRead` read-only host mounts and payload PATH/env provisioning. | ✅ task 004, ADR 005 |
| 006 | Per-run stdout and stderr output caps (`profile.limits.max_output_bytes`). | ✅ task 006, ADR 007 |
| 007 | Per-host HTTP-verb allowlist enforcement in the egress proxy. | ✅ task 007, ADR 008 |
| 008 | Snapshot and restore pristine-baseline reset boundary. | ✅ task 008, ADR 009 |
| 009 | Wire fitness functions: `fitness-<id>` targets and `fitness:` umbrella (F-001 through F-010). | ✅ task 009 |
| 010 | Terminal audit event on early proxy-start failure. | ✅ task 010, ADR 013 |
| 011 | Signed `sandbox_identity` attestation (ephemeral ed25519 self-attestation). | ✅ task 011, ADR 014 |
| 012 | Env-mode credential injection and wipe clock. | ✅ task 012, ADR 015 |
| 013 | Firecracker Tier-3 dispatch and config-generator skeleton (no-NIC by construction). | ✅ task 013, ADR 010 |
| 014 | Firecracker no-NIC and vsock-bridge egress enforcement. | ✅ task 014, ADR 010 |
| 015 | Firecracker guest boot, kernel and rootfs, direct launch without jailer. | ✅ task 015, ADR 010 Amendments 1 and 2 |
| 016 | Firecracker `profile.limits` machine-config mapping. | ✅ task 016, ADR 010 |
| 017 | Firecracker `/work` and `FileRead` mount semantics. | ✅ task 017, ADR 010 |
| 018 | Firecracker teardown, fitness functions, and spec sync. | ✅ task 018, ADR 010 |
| 019 | Tier-1 seccomp-BPF default-deny syscall profile (`seccomp/tier1.bpf`). | ✅ task 019, ADR 016 |
| 020 | Attestation trust-root design for host-signed tokens. | ✅ task 020, ADR 017 |
| 021 | Host-signed `sandbox_identity` attestation (`keygen`, `verify-attestation`, `wiring.attestation_key`). | ✅ task 021, ADR 017 |
| 022 | Reconcile README and roadmap drift with shipped Tier 3 and v2 attestation. | ✅ task 022 |

### Future and deferred (unscheduled)

| Work | Disposition |
|------|-------------|
| HTTPS via `CONNECT` and dynamic DNS in `EgressProxy`. | **TRACKED GAP**: genuine egress gap (host allowlist preserved via bare host in `CONNECT` line; credential injection N/A once client TLS-terminates). Deferred until an HTTPS-origin workload requires it (ADR 011). |
| Warm microVM pools and snapshot reuse. | **DEFER**: host-side snapshot and restore is built and proven leak-free, but microVM pool sizing, reuse triggers, and eviction are undesigned (ADR 009, ADR 010). |
| Firecracker `env`-mode credential delivery. | **DEFER**: Firecracker currently supports `proxy`-mode credential delivery over vsock; delivering secrets into guest env requires guest-side injection mechanics. |
| Egress hardening: two-layer DNS-proxy + `nftables` (OpenSandbox **OSEP-0001** reference). | **DROP (ADR 011)**: no-network-by-construction dominates filter-an-existing-route and fails closed; OSEP-0001 filters a route exec-sandbox does not have. Retained as cited prior art only. |
| SOCKS5 proxy (alongside HTTP egress proxy). | **DROP (ADR 011)**: incompatible with credential injection (opaque TCP has nowhere to splice secret) and imports SOCKS5 hostname-canonicalization bypass bugs. HTTPS via `CONNECT` is the real gap, tracked separately above. |
| Tier 4: Hyperlight (micro-isolate). | **KEEP-WATCHING**: promotion trigger is Hyperlight **1.0** or `agent-framework-hyperlight` **CodeAct GA** (ADR 006, 2026-06-20 reassessment). |
| Secrets refresh and rotation during a run. | **DEFER**: lower priority; revisit when long-running workflows demand mid-flight secret rotation. |

## Notes for the orchestrator

This repo is built out one task at a time by **agent-builder**: it reads this roadmap and `docs/tasks/backlog/NNN-*.md`, builds the next ready task in a sandbox, runs the verification gate, and opens a PR. The working v0 source is not rewritten; v1 work extends it behind the tier seam.
