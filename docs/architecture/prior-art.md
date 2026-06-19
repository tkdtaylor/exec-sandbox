# Prior art — hyperlight & rvm

**Last updated:** 2026-06-19
**Status:** reference analysis (not spec; not a commitment to build anything)

This is a competitive / prior-art comparison of two external projects against exec-sandbox,
plus a map of which of their ideas are already owned by sibling building blocks in the
secure-agent ecosystem. It exists so that "should we adopt X?" and "should we build Y?" can be
answered from a written baseline instead of re-researched each time.

The two projects:

- [hyperlight-dev/hyperlight](https://github.com/hyperlight-dev/hyperlight) — Microsoft/Azure
  embeddable micro-VM manager (CNCF Sandbox).
- [ruvnet/rvm](https://github.com/ruvnet/rvm) — a bare-metal Rust hypervisor/kernel for
  "agentic" workloads.

## Bottom line

**Neither does what exec-sandbox does, and neither should replace it.**

- **hyperlight** is a *lower-level isolation primitive* — an embeddable VMM library, not a
  turnkey runner. It is a legitimate **future candidate behind the `tier` seam** (alongside the
  not-yet-implemented Firecracker tier), not a substitute for the `run()` contract above the
  seam.
- **rvm** is a research-stage bare-metal kernel at the wrong layer (ring -1 / EL2), QEMU-only,
  AI-generated, with **no networking and no credential model at all** — i.e. it is missing the
  entire half of the stack that is exec-sandbox's reason to exist.

The decisive fact for both: exec-sandbox's core invariant — **no network except a
credential-injecting egress proxy on a Unix socket** — has no equivalent in either project.

## Side-by-side

| Dimension | exec-sandbox (this repo) | hyperlight | rvm |
|---|---|---|---|
| Layer | Userspace Go CLI on a Linux host | Embeddable VMM *library* | Bare-metal hypervisor/kernel (EL2) |
| Language | Go, stdlib-only | Rust + C FFI | Rust, `no_std` |
| Isolation primitive | bwrap / gVisor / (Firecracker) behind `tier` seam | Hardware micro-VM, **no guest OS** | Capability "coherence domains" + Stage-2 MMU |
| Workload format | Arbitrary agent-generated payload | `no_std` ELF (Rust/C); Wasm/JS via sibling repos | Wasm agents; native partitions |
| **Network egress** | **No-net + single credential-injecting proxy socket** | Host owns all I/O; `allow_domain()` allowlist in *experimental* sandbox layer | **None — no stack, no egress, no proxy** |
| **Credentials** | **`vault.inject(handle, identity, mode)`; value never enters sandbox** | No named API — keep host-side via host functions | **None** (capabilities ≠ secrets) |
| Audit | Emits spawn/inject/exit events to audit-trail | None native | Kernel-native witness hash-chain |
| Contract | One-shot `run(payload, profile, tier, secret_refs) → {stdout, stderr, exit_code, sandbox_status}` | In-process bidirectional typed RPC (call guest fn by name) | In-kernel Rust object API |
| Maturity | early | CNCF Sandbox, Microsoft-backed, pre-1.0, ~4.5k★ | Experimental, ~110★, QEMU-only, self-reported/AI-generated |
| License | Apache-2.0 | Apache-2.0 | Apache-2.0 OR MIT |
| Threat focus | Network exfiltration / credential leak | Host compromise from untrusted guest | Memory isolation, capability forgery, side channels, DMA |

## hyperlight — detail

- **Core:** "lightweight VMM designed to be embedded within applications" enabling "safe
  execution of untrusted code within micro virtual machines with very low latency." Aimed at
  functions-at-scale / FaaS. It is a building block embedded in *your* host process, not a
  standalone runner.
- **Isolation:** hardware-virtualized micro-VMs with **no guest kernel/OS** — guests are
  `no_std` ELF binaries. Hypervisors: KVM, `/dev/mshv` (MSHV), WHP (Hyper-V). No virtual
  devices exposed to the guest; out-of-bounds access is trapped by the hypervisor.
- **Network / secrets:** base hyperlight has *no* network and *no* secret API. The only
  boundary crossing is registered host functions over shared memory, validated against
  FlatBuffer schemas (default-deny capabilities). A domain+verb allowlist (`allow_domain()`)
  and a capability filesystem (`/input` ro, `/output` rw) exist only in the **experimental**
  `hyperlight-sandbox` sibling. Credentialed I/O is done by the host on the guest's behalf,
  so the secret stays host-side — a structural analog to our proxy invariant, but with no
  named injection contract.
- **Performance:** ~1–2 ms cold start per VM (~100× faster than traditional VMs); guest calls
  in microseconds.
- **API:** create `UninitializedSandbox` from a guest ELF → register host functions →
  `snapshot()`/`restore()` for clean-slate reuse between calls → `call("Fn", args)`.

## rvm — detail

- **Core:** a bare-metal Rust hypervisor/kernel for AI-agent workloads. Its novel idea is
  **"coherence domains"** — graph-structured partitions whose isolation/scheduling/placement
  are driven by how agents communicate (in-kernel mincut). "RVM" is a tagline, not an acronym.
- **Isolation:** capability coherence domains + Stage-2 (hypervisor) page tables; per-partition
  IOMMU + DMA budgets; seL4-style unforgeable, monotonically-attenuated capability tokens.
  A partition is explicitly "NOT a VM."
- **Network / secrets:** **none.** Its "communication" is purely local intra-hypervisor IPC
  (CommEdges) feeding the scheduler — a *placement* construct, not a security boundary against
  external egress. No secrets/credential handling; capability tokens gate resource access,
  not external credentials.
- **Threat model:** articulate for its maturity — six kernel invariants (proof-gated mutations,
  capability-gated access, witness-native audit, etc.), 3-tier proof checks, hash-chained
  signed witness records with deterministic replay. But all self-reported, QEMU-only, no
  third-party audit, and it targets future/minimal hardware profiles (including a hypothetical
  "Cognitum silicon").
- **Caveat:** AI-generated/maintained; benchmark and formal-verification claims are in-simulator
  targets, not corroborated.

## Sibling building blocks (ecosystem record)

The secure-agent ecosystem (sibling repos under the same parent folder) already owns most of
the concerns hyperlight/rvm surface. Recorded here so the boundary is explicit and the
ownership map below is traceable. Scope summaries are from each block's `docs/spec/` and
`CLAUDE.md`.

| Block | Layer | Owns | Relation to exec-sandbox |
|---|---|---|---|
| **vault** (Rust) | Secret store / credential broker | Encrypted-at-rest credentials, single-use opaque handles, `resolve()` + `inject(handle, sandbox_identity, mode)`, raise-only injection floor. Plaintext never reaches agent core or sandbox. | exec-sandbox calls `vault.inject` at the injection edge; receives `{credential, binding}`. |
| **audit-trail** (Go) | Tamper-evident forensic log | SHA-256 hash-chained append-only records, `Emit`/`Verify`, log resumption/reconstruction; Ed25519 signed checkpoints + Rekor anchoring (v1+). | exec-sandbox *emits* spawn/inject/exit events; owns none of the log machinery. |
| **policy-engine** (Go) | Out-of-process authz control plane | `allow`/`deny`/`require_approval` decisions, `NetAllowlist`, obligations: `tier_select`, `vault_injection_floor`, `audit_emit`. Decides; does not enforce. | Selects exec-sandbox's tier + injection floor; exec-sandbox enforces. Per-verb policy is v0-baseline only. |
| **armor** (Python) | Semantic guard for LLM agents | Prompt-injection / jailbreak / exfiltration / obfuscation detection at the prompt/output boundary; latency budgets, soft-fail. Does **not** sandbox or enforce network policy. | Complementary, orthogonal layer — detects the attack; exec-sandbox contains the runtime. |
| **memory-guard** (Go) | Agent memory I/O gate | `validate_write`/`validate_read`/`verify_delete`, PII redaction, context-poisoning rejection, authenticated deletion. | Separate layer; no execution isolation. Out of exec-sandbox scope by design. |
| **agent-mesh** (Go, v0) | Secure inter-agent messaging | Ed25519-signed, replay-protected envelopes (SPIFFE-style identity). No egress/credentials/audit/isolation. | Peer block one layer up; relevant only in multi-agent orchestration. |
| **agent-integration** (Python, MIT) | Cross-block integration harness | Launches real vault/policy-engine/audit-trail/exec-sandbox binaries over real sockets; proves the v1 contract composition (esp. the vault→exec-sandbox credential handoff). | exec-sandbox is the *subject* of the integration matrix, not a peer. |

## Ownership map — where each idea already lives

The features these projects suggest mostly fall to sibling blocks, not here:

| Idea from hyperlight/rvm | Rightful owner | Already covered? |
|---|---|---|
| Tamper-evident / hash-chained / replayable audit (rvm witness log) | **audit-trail** | ✅ SHA-256 hash chain, `Verify`, Ed25519 signed checkpoints + Rekor anchoring, log resumption. exec-sandbox only *emits*. |
| Credential injection / keep-secret-host-side (hyperlight host fns) | **vault** | ✅ `vault.inject(handle, identity, mode)`, raise-only floor, value never enters sandbox. |
| Allowlist *decision*, capability/tier selection | **policy-engine** | ✅ Owns `NetAllowlist`, `tier_select`, `vault_injection_floor` obligations. Per-verb is v0-baseline only — see below. |
| Semantic exfil/injection detection | **armor** | ✅ Prompt/output guard; complementary layer. |
| Memory I/O gating; MMU-level memory protection | **memory-guard** | Out of exec-sandbox scope by design. |

## Net-new candidates that are genuinely exec-sandbox's

These are the only ideas that map to exec-sandbox's own responsibility (OS-level execution +
egress *enforcement*). Listed here as candidates for evaluation — **not** a commitment, and
deliberately kept out of `docs/spec/` (which is present-tense only). Roadmap items, once chosen,
become task files under `docs/tasks/`.

1. **Snapshot/restore for clean-slate reuse (hyperlight).** Reset sandbox state to pristine
   between invocations instead of full teardown+rebuild per run. Aligns with the "every run is
   ephemeral" non-goal while improving per-call hygiene/throughput at scale.
2. **Per-run resource bounding *enforcement* (rvm: timeout/quota/DMA budgets; armor: latency
   budgets).** Execution timeout, CPU/memory quotas (cgroups), output caps. policy-engine
   *decides* obligations and explicitly does **not** enforce workload budgets; armor and
   memory-guard disclaim it too — so enforcement is exec-sandbox's to own.
3. **Per-HTTP-verb allowlist *enforcement* in the proxy (hyperlight `allow_domain` w/ verb).**
   Pairs with a policy-engine decision; the proxy currently enforces domain-only. Split work:
   decision in policy-engine, enforcement here.
4. **hyperlight as a future isolation tier (Tier-4) behind the `tier` seam.** Hardware
   isolation without a guest OS, ~1–2 ms cold start. Watching-brief / ADR, not a port —
   it runs `no_std` ELF/Wasm (not arbitrary payloads) and is pre-1.0 with an unstable API.
5. **Graduated failure containment (rvm F1→F4).** Blast-radius control for crashing untrusted
   code (restart → reconstruct → teardown). Lower priority; partially implicit in ephemeral runs.

## Sources

- hyperlight: repo README, `docs/security.md`, `docs/technical-requirements-document.md`;
  Microsoft intro blog (Nov 2024), 0.0009s micro-VM blog (Feb 2025), Nanvix POSIX blog
  (Jan 2026); sibling repos `hyperlight-wasm`, `hyperlight-sandbox`, `hyperagent`.
- rvm: repo README; ruvnet "RVM Hypervisor Core" deep-research gist; GitHub API metadata.
- Ecosystem ownership: sibling `docs/spec/` and `CLAUDE.md` for vault, audit-trail,
  policy-engine, armor, memory-guard, agent-mesh, agent-integration.
