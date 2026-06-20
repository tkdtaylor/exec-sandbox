# Prior art — hyperlight, firecracker & rvm

**Last updated:** 2026-06-20
**Status:** reference analysis (not spec; not a commitment to build anything)

This is a competitive / prior-art comparison of three external projects against exec-sandbox,
plus a map of which of their ideas are already owned by sibling building blocks in the
secure-agent ecosystem. It exists so that "should we adopt X?" and "should we build Y?" can be
answered from a written baseline instead of re-researched each time.

The three projects:

- [hyperlight-dev/hyperlight](https://github.com/hyperlight-dev/hyperlight) — Microsoft/Azure
  embeddable micro-VM manager (CNCF Sandbox).
- [firecracker-microvm/firecracker](https://github.com/firecracker-microvm/firecracker) — AWS
  KVM-based microVM monitor; powers Lambda and Fargate. **Already this project's named Tier-3.**
- [ruvnet/rvm](https://github.com/ruvnet/rvm) — a bare-metal Rust hypervisor/kernel for
  "agentic" workloads.

## Bottom line

**None of the three does what exec-sandbox does, and none should replace it. Firecracker and
hyperlight are isolation *primitives* that plug in *behind* the `tier` seam; rvm is the wrong
layer entirely.**

- **firecracker** is a KVM-based microVM monitor — a *lower-level isolation primitive*, same
  class as hyperlight (hardware virtualization, stronger than gVisor's userspace kernel). It is
  **already this project's accepted-but-unimplemented Tier-3** (ADR-001 D7). Of the three it is
  the **best-aligned VMM tier candidate**: it runs a full guest kernel and *arbitrary Linux
  binaries*, which matches exec-sandbox's "arbitrary agent-generated payload" workload format
  directly — no guest-format adapter needed (hyperlight needs one). It is *not* a substitute for
  the `run()` contract above the seam.
- **hyperlight** is also a *lower-level isolation primitive* — an embeddable VMM library, not a
  turnkey runner. Future candidate behind the `tier` seam (Tier-4, ADR 006), *after* Firecracker,
  because it runs only `no_std` ELF/Wasm and is pre-1.0.
- **rvm** is a research-stage bare-metal kernel at the wrong layer (ring -1 / EL2), QEMU-only,
  AI-generated, with **no networking and no credential model at all** — i.e. it is missing the
  entire half of the stack that is exec-sandbox's reason to exist.

The decisive fact for all three: exec-sandbox's core invariant — **no network except a
credential-injecting egress proxy on a Unix socket** — has no equivalent in any of them.
Firecracker even *has* a network device model (virtio-net), but it is opt-in: exec-sandbox
would simply never attach a NIC, making no-network hold by construction and the proxy reachable
over vsock or a bind-mounted socket.

## Side-by-side

| Dimension | exec-sandbox (this repo) | hyperlight | firecracker | rvm |
|---|---|---|---|---|
| Layer | Userspace Go CLI on a Linux host | Embeddable VMM *library* | Standalone KVM VMM (one process per microVM) | Bare-metal hypervisor/kernel (EL2) |
| Language | Go, stdlib-only | Rust + C FFI | Rust | Rust, `no_std` |
| Isolation primitive | bwrap / gVisor / (Firecracker) behind `tier` seam | Hardware micro-VM, **no guest OS** | Hardware microVM via KVM, **full guest kernel** + jailer | Capability "coherence domains" + Stage-2 MMU |
| Workload format | Arbitrary agent-generated payload | `no_std` ELF (Rust/C); Wasm/JS via sibling repos | **Arbitrary Linux binaries** (full guest kernel + rootfs) | Wasm agents; native partitions |
| **Network egress** | **No-net + single credential-injecting proxy socket** | Host owns all I/O; `allow_domain()` allowlist in *experimental* sandbox layer | virtio-net **opt-in** via host TAP; omit NIC → no-net by construction; vsock for proxy | **None — no stack, no egress, no proxy** |
| **Credentials** | **`vault.inject(handle, identity, mode)`; value never enters sandbox** | No named API — keep host-side via host functions | None native — host owns all I/O; secret stays host-side at the vsock/proxy edge | **None** (capabilities ≠ secrets) |
| Audit | Emits spawn/inject/exit events to audit-trail | None native | None native | Kernel-native witness hash-chain |
| Snapshot/restore | Host-side reset baseline (task 008 / ADR 009) | `snapshot()`/`restore()` in-process | **Native VMM snapshot/restore, ~5–30 ms restore** (Lambda SnapStart) | n/a |
| Contract | One-shot `run(payload, profile, tier, secret_refs) → {stdout, stderr, exit_code, sandbox_status}` | In-process bidirectional typed RPC (call guest fn by name) | REST API over a Unix socket (configure → `InstanceStart`) | In-kernel Rust object API |
| Maturity | early | CNCF Sandbox, Microsoft-backed, pre-1.0, ~4.5k★ | **Production at AWS (Lambda/Fargate), post-1.0, ~35k★** | Experimental, ~110★, QEMU-only, self-reported/AI-generated |
| License | Apache-2.0 | Apache-2.0 | Apache-2.0 | Apache-2.0 OR MIT |
| Threat focus | Network exfiltration / credential leak | Host compromise from untrusted guest | Host compromise from untrusted guest (multi-tenant FaaS) | Memory isolation, capability forgery, side channels, DMA |

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

## firecracker — detail

- **Core:** an open-source VMM written in Rust that uses KVM to create minimal **microVMs** for
  "secure, multi-tenant container and function-based services." Each microVM is a separate
  Firecracker process with a stripped-down device model (no BIOS, no PCI). Originally built at
  AWS; it powers **Lambda and Fargate** in production.
- **Isolation:** hardware virtualization via KVM — the strongest of the candidates here, on par
  with hyperlight and stronger than gVisor's userspace-kernel interception. Each workload gets
  its **own dedicated guest kernel**, fully separated from host and neighbours. The **jailer**
  adds a cgroup/namespace barrier and drops privileges before the VMM starts (defence in depth
  around the VMM process itself).
- **Workload format — the key fit:** unlike hyperlight (which runs only `no_std` ELF/Wasm),
  Firecracker boots a **full guest kernel + rootfs and runs arbitrary Linux binaries**. That maps
  directly onto exec-sandbox's "arbitrary agent-generated payload" model — **no guest-format
  adapter required**, which is the single biggest reason it is a cleaner tier fit than hyperlight.
- **Network / secrets:** Firecracker *does* have a network device model (virtio-net backed by a
  host TAP), but it is **opt-in** per microVM. exec-sandbox would simply **not attach a NIC** —
  the no-network invariant then holds *by construction*, and the egress proxy is reached over a
  **vsock** device (or a bind-mounted socket) rather than the bwrap bind-mount. As with the
  other VMMs there is no native secret API: the host owns all I/O, so a proxy-mode credential
  stays host-side at the vsock/proxy edge — structurally compatible with our invariant.
- **Snapshot/restore:** native, VMM-level **full and diff snapshots**; restore in ~5–30 ms
  (the mechanism behind Lambda SnapStart). This is exactly the **kernel/VMM-snapshot variant**
  deferred as an open question in ADR 009 (task 008 ships only the host-side reset baseline). A
  documented caveat — *guest network connectivity is not guaranteed across resume* — is a
  **non-issue here precisely because exec-sandbox runs the guest with no network.**
- **Performance:** ~125 ms cold boot; very low per-microVM memory overhead. Slower cold start
  than hyperlight's ~1–2 ms, but snapshot/restore closes most of that gap for warm reuse.
- **API:** a REST API served on a Unix socket — configure boot source, drives, machine config
  and (optionally) network, then `InstanceStart`. A natural seam to drive from Go behind
  `backendFor`, the same way `gvisor.go` drives `runsc`.

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

1. **Snapshot/restore for clean-slate reuse (hyperlight + firecracker).** Reset sandbox state to
   pristine between invocations instead of full teardown+rebuild per run. Aligns with the "every
   run is ephemeral" non-goal while improving per-call hygiene/throughput at scale. Task 008 ships
   only the **host-side reset baseline**; both hyperlight (`snapshot()`/`restore()`) and
   **Firecracker (native VMM snapshot, ~5–30 ms restore)** are the VMM-level variant deferred as
   an open question in ADR 009 — to be revisited when a VMM tier actually lands.
2. **Per-run resource bounding *enforcement* (rvm: timeout/quota/DMA budgets; armor: latency
   budgets).** Execution timeout, CPU/memory quotas (cgroups), output caps. policy-engine
   *decides* obligations and explicitly does **not** enforce workload budgets; armor and
   memory-guard disclaim it too — so enforcement is exec-sandbox's to own.
3. **Per-HTTP-verb allowlist *enforcement* in the proxy (hyperlight `allow_domain` w/ verb).**
   Pairs with a policy-engine decision; the proxy currently enforces domain-only. Split work:
   decision in policy-engine, enforcement here.
4. **VMM isolation tiers behind the `tier` seam — Firecracker (Tier-3) then hyperlight (Tier-4).**
   - **Firecracker is already the accepted Tier-3** (ADR-001 D7) and is the **best-aligned VMM
     candidate to wire first**: KVM hardware isolation, a full guest kernel running *arbitrary
     Linux binaries* (no guest-format adapter), production-proven at AWS, with native
     snapshot/restore. No new ADR needed — implementing it is a future task against the existing
     Tier-3 slot. The work is the `backendFor` adapter (REST-over-Unix-socket, no-NIC + vsock
     proxy) and preserving the no-network/credential invariants, not a design decision.
   - **hyperlight is the Tier-4 watching-brief** (ADR 006), *after* Firecracker — hardware
     isolation without a guest OS, ~1–2 ms cold start, but it runs only `no_std` ELF/Wasm (not
     arbitrary payloads) and is pre-1.0 with an unstable API. Watching-brief, not a port.
5. **Graduated failure containment (rvm F1→F4).** Blast-radius control for crashing untrusted
   code (restart → reconstruct → teardown). Lower priority; partially implicit in ephemeral runs.

## Sources

- hyperlight: repo README, `docs/security.md`, `docs/technical-requirements-document.md`;
  Microsoft intro blog (Nov 2024), 0.0009s micro-VM blog (Feb 2025), Nanvix POSIX blog
  (Jan 2026); sibling repos `hyperlight-wasm`, `hyperlight-sandbox`, `hyperagent`.
- firecracker: repo README + `docs/snapshotting/snapshot-support.md`; GitHub metadata (~35k★,
  Apache-2.0); microVM-isolation surveys comparing Firecracker / gVisor / Kata (2026).
- rvm: repo README; ruvnet "RVM Hypervisor Core" deep-research gist; GitHub API metadata.
- Ecosystem ownership: sibling `docs/spec/` and `CLAUDE.md` for vault, audit-trail,
  policy-engine, armor, memory-guard, agent-mesh, agent-integration.
