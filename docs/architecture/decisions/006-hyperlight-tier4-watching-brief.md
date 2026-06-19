# ADR 006 — hyperlight as a recognized future Tier-4 backend behind the tier seam (watching brief)

**Status:** Accepted
**Date:** 2026-06-19
**Related:** ADR 001 D7 (tier seam: `bubblewrap | gvisor | firecracker`), ADR 002 (gVisor Tier-2
backend). Source analysis: `docs/architecture/prior-art.md` (hyperlight detail + net-new candidate #4).

## Context

ADR-001 D7 established the `tier` seam as the project's deliberate composability boundary: a new
isolation backend plugs in behind `backendFor(tier)` (ADR-002 D7.1) without changing the `run()`
contract or the no-network + proxy-only-egress invariant. Today the seam wires Tier-1 bubblewrap and
Tier-2 gVisor (`runsc`); Tier-3 Firecracker is accepted by the `tier` field but unimplemented
(`tier not implemented`).

The prior-art analysis (`docs/architecture/prior-art.md`) surveyed two external projects —
**hyperlight** (Microsoft/Azure, CNCF Sandbox) and **rvm** (ruvnet) — against exec-sandbox. Its net-new
candidate #4 flags hyperlight as a future isolation tier. **hyperlight** is an embeddable micro-VM VMM
library that gives hardware-virtualized isolation with **no guest OS** and a ~1–2 ms cold start
(~100× faster than traditional VMs). It is a lower-level isolation *primitive*, not a turnkey runner —
which is exactly the shape the `tier` seam exists to absorb.

This ADR records hyperlight as a recognized future tier candidate so that "should we adopt it?" is
answered from a written baseline. **It is a watching brief, not a commitment to implement.** No code,
no task, no spec change follows from accepting this ADR. `docs/spec/` remains present-tense and
continues to describe only the wired tiers (bubblewrap, gVisor) and the accepted-but-unimplemented
Firecracker tier.

## Decisions

### D1 — hyperlight is recognized as the Tier-4 candidate, after Firecracker (Tier-3)

When/if a *hardware-isolation* backend is needed beyond the namespace (Tier-1) and userspace-kernel
(Tier-2) tiers, hyperlight is the recorded candidate to evaluate, sequenced as **Tier-4** behind the
already-accepted Firecracker Tier-3. It plugs in behind the existing `tier` seam as one more
`backendFor(tier)` arm; the `run()` contract and the security invariants are unchanged by its
existence as a candidate.

This decision adds a name to the seam's candidate set. It does **not** implement, schedule, or
prioritize the work. Adoption is a future, separately-justified decision.

### D2 — Adopting hyperlight would require a guest-format adapter (and likely a Wasm path)

hyperlight runs `no_std` ELF/Wasm guests, **not** arbitrary agent-generated payloads. Tier-1 and
Tier-2 run an arbitrary payload as `/usr/bin/sh /payload.sh`; hyperlight cannot. Adopting it would
therefore require either a guest-format adapter or, more likely, a Wasm execution path
(`hyperlight-wasm`) — which **changes what "payload" means for that tier**. That is a contract-shaped
question for the Tier-4 payload semantics, to be resolved in the adoption ADR, not assumed here. Any
divergence from "arbitrary payload" must be explicit and bounded to that tier.

### D3 — The no-network + credential-injecting-proxy invariant stays exec-sandbox's to enforce

hyperlight provides **no** equivalent to exec-sandbox's load-bearing invariant: base hyperlight has no
network and no secret API, and its domain allowlist + capability filesystem live in the *experimental*
`hyperlight-sandbox` sibling, not the core. As with bubblewrap (ADR-001 D3/D4) and gVisor (ADR-002
D7.2), a hyperlight Tier-4 backend would still have to enforce the no-network + proxy-only-egress
boundary and the `vault.inject` credential edge *itself*, at the exec-sandbox layer. The isolation
primitive does not relieve exec-sandbox of owning the egress and credential boundary; it only changes
the isolation mechanism beneath it.

### D4 — Maturity gate: pre-1.0 with an explicitly unstable API

hyperlight is pre-1.0 (CNCF Sandbox, Microsoft-backed) with an explicitly unstable API. A watching
brief is the correct posture: track it, do not depend on it. Reassessment is warranted when it reaches
a stable API and/or when the egress/filesystem capabilities graduate out of the experimental sibling
into a supported surface.

## Alternatives considered

- **Adopt hyperlight as a replacement for exec-sandbox.** *Rejected.* hyperlight is an embeddable
  isolation *primitive*, not a runner: it has no `run()` contract, no egress boundary, and no
  credential-injection model — the entire half of the stack that is exec-sandbox's reason to exist
  (`docs/architecture/prior-art.md` "Bottom line"). It belongs *behind* the seam, not above it.
- **Adopt rvm as a tier.** *Rejected.* rvm is a research-grade bare-metal hypervisor/kernel at the
  wrong layer (ring -1 / EL2), QEMU-only and self-reported/AI-generated, with **no networking and no
  credential model at all**. It is not a candidate isolation backend for a userspace Go CLI and
  provides nothing the seam can consume.
- **Do nothing / record no candidate.** *Rejected for the watching brief, not the work.* Leaving the
  Tier-4 slot unnamed means the same prior-art question gets re-researched each time it surfaces.
  Recording the candidate (with its caveats) costs nothing and preserves the analysis; it does not
  commit to building anything.

## Consequences

- The `tier` seam's candidate set now has a recorded Tier-4 name (hyperlight) with its constraints
  written down. Future "should we adopt hyperlight?" discussions start from this ADR plus
  `docs/architecture/prior-art.md`, not a blank page.
- **Nothing in the code or spec changes.** No `backendFor` arm, no `tier` field value, no task file.
  `firecracker` remains the only accepted-but-unimplemented tier; an unrecognized tier still fails
  fast with `tier not implemented`.
- The adoption decision is deferred and gated: it requires (a) a stable hyperlight API, (b) a separate
  ADR resolving the Tier-4 payload semantics (guest-format adapter and/or `hyperlight-wasm`), and
  (c) explicit re-confirmation that the no-network + proxy-only-egress + `vault.inject` invariants are
  enforced at the exec-sandbox layer for that tier.
- SPEC.md's tier-seam description (SPEC.md, "Two isolation tiers are wired … Tier-3 Firecracker …
  accepted … not yet implemented") stays present-tense and unchanged. This forward-looking decision
  lives here, in the ADR log, by design — adding roadmap language to the spec is explicitly out of
  scope.

## Refines

ADR-001 D7 (tier seam) — adds a recognized future candidate to the seam's backend set. Does not
supersede any decision; D3/D4/D5 invariants are preserved and reaffirmed as exec-sandbox's to enforce
regardless of isolation primitive.
