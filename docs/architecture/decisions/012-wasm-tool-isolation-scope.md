# ADR 012 — WASM / pre-compiled-tool isolation is a future tier here, not a separate block

**Status:** Accepted
**Date:** 2026-06-20
**Related:** ADR 001 D7 (tier seam: `bubblewrap | gvisor | firecracker`), ADR 002 (gVisor Tier-2),
ADR 006 (hyperlight Tier-4 watching brief), ADR 010 (Firecracker Tier-3), ADR 011 (egress-model
boundaries). Decision record: ecosystem `tool-sandbox-decision.md` scoping brief.

## Context

A long-standing forward reference in the ecosystem scoping called for a separate **"tool-sandbox"**
block: WASM/WASI capability-scoped isolation for *pre-compiled* agent tools, listed as out-of-scope
for exec-sandbox. Two questions were open: is that a real separate block, a feature/tier here, or
should it be scoped out — and the bias was toward a separate, composable project (the all-in-one
platform shape, e.g. OpenSandbox/Wassette, being the anti-pattern we differentiate against).

Three threads were researched and adversarially verified (2026-06-20):

1. **Invocation contract.** Used as typed WIT components (structured params/results, capability
   imports declared in-band), the WASM tool interface is **genuinely not isomorphic** to the OCI
   `run()` contract (argv/env → exit code) — it is a schema-checked function-call API closer to an
   MCP tool definition. But that value is **interop, not isolation**.
2. **Security-boundary strength.** WASM is **not a sufficient standalone trust boundary** for
   untrusted code. Wasmtime — the most mature runtime — has shipped multiple critical
   (CVSS 9.0+) host-memory escapes (CVE-2023-26489 9.9; two 9.0 escapes on 2026-04-09), all
   codegen miscompilations the WASM spec cannot prevent; Spectre can defeat its in-process
   isolation (Swivel, USENIX'21); and serious operators nest accordingly — Cloudflare adds "a
   layer 2 sandbox [using] Linux namespaces and seccomp," Fermyon runs WASM inside Firecracker.
3. **Prior art.** Microsoft **Wassette** (MIT, Wasmtime-based) already runs WASM components as MCP
   tools with deny-by-default per-component capability YAML — nearly the exact model a "tool-sandbox"
   would build — and "MCP tool = WASM component + declared capabilities" is an emerging pattern.

## Decisions

### D1 — No separate "tool-sandbox" block. The isolation concern is a future tier behind this seam.

For *untrusted* pre-compiled tools, WASM must run **inside** OS isolation (finding 2) — so the WASM
isolation lane is a codegen-level inner layer that nests under exec-sandbox's existing `tier` seam,
not an independent peer. If/when a real consumer needs it, it plugs in as one more `backendFor(tier)`
arm (a "Tier-0" lightweight, poolable WASM runtime) under the no-network + proxy-only-egress
invariant — the `run()` contract is unchanged. This adds a recognized concern to the seam; it does
**not** implement, schedule, or prioritize the work.

### D2 — The typed-invocation concern is interop, not ours to rebuild.

The typed-WIT capability-scoped tool-invocation interface (finding 1) lives at the agent's
tool-calling / MCP layer, not in this OS-isolation block. Folding it in here would force the
OCI-isomorphic mode and discard its value. If built, it should **adopt** Wassette's capability schema
and the MCP-WASM-component pattern rather than invent a new format.

### D3 — Build neither now. Revisit on a concrete trigger.

No ecosystem consumer needs the untrusted-pre-compiled-tool lane today, and Wassette is a fast-moving
near-match. Revisit only when a concrete consumer needs that lane at scale **and** existing tools
(Wassette/Extism) prove inadequate behind an adapter seam.

## Consequences

- `docs/spec/` stays present-tense and continues to describe only the wired tiers; no spec change
  follows from this ADR.
- The README `## Scope` section records "no standalone WASM tool-sandbox block" so the boundary is
  visible to adopters.
- This supersedes the earlier "separate tool-sandbox block" framing wherever it appears in
  exec-sandbox docs.
