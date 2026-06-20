# ADR 009: snapshot/restore — a leak-proof clean-slate reset boundary in `Run()`

**Date:** 2026-06-19
**Status:** Accepted
**Task:** 008 (snapshot/restore for clean-slate sandbox reuse)
**Related:** ADR 001 (foundational stack: no-network, proxy-only egress), ADR 002 (gVisor Tier-2
backend behind the `tier` seam), ADR 004 (writable `/work` mount), ADR 005 (FileRead read-only
mounts + env provisioning), ADR 006 (hyperlight Tier-4 watching brief — the prior art this borrows
from).

## Context

The roadmap (`docs/architecture/prior-art.md`, "Net-new candidates" #1, inspired by hyperlight's
`UninitializedSandbox → snapshot() → restore() → call()` loop) calls for **resetting a sandbox to a
pristine baseline between invocations** instead of a full teardown + rebuild per run, to improve
per-call hygiene and throughput at scale.

This is a **performance / hygiene** task, **not** a security gap. The current ephemeral model
(fresh temp dir, fresh proxy, wiped at teardown) is already correct. The hard bar is the project's
own non-goal — "no persistent state; every run is ephemeral; no state leaks between runs" — restated
as a property of reuse: **a reused sandbox must be byte-for-byte indistinguishable from a freshly
built one.** No file, env var, leftover process, or credential from a prior run may survive a reset.

Today `Run()` *inlines* the per-run baseline lifecycle: `os.MkdirTemp` → write `payload.sh` →
`NewEgressProxy` (empty creds) → `proxy.Start` → spawn → `os.RemoveAll(work)` + `proxy.Wipe()`. There
is no named "baseline" value and no named "reset" operation — so there is nothing a leak-proof
property can be asserted *against*. This task pins down that contract.

## Decision

Factor a **`sandboxBaseline`** value and a **`restore`** operation out of `Run()`'s inlined
setup/teardown, **without** changing the one-shot `run()` contract. `sandboxBaseline` captures the
pristine per-run state — the host work dir, the `payload.sh` contents, and a fresh `EgressProxy` with
an empty credential map. `restore` returns that state to baseline: it removes everything written
under the writable surface (the work dir, re-seeded only with `payload.sh`) and clears the proxy
credential map (subsuming `Wipe()`). `snapshot` is the capture of the pristine baseline taken
**before** the payload runs; `restore` is the reset back to it.

The default one-shot path is `snapshot → run → teardown` and is **observationally identical to
today** — the snapshot is the baseline `Run()` already built, and one-shot teardown is the existing
`os.RemoveAll` + `Wipe()`. No externally-visible result-schema change; no argv/OCI-spec change. The
reuse *mechanism* now exists and is proven leak-free; *wiring* a second invocation onto a restored
baseline is a deliberate follow-on (see Q3).

### Leak-proofing is the load-bearing deliverable

`restore` makes the writable surface **and** the proxy credential state equal to a freshly-built
baseline. The property is proven two ways, both written as loud equality checks so a regression is
obvious:

1. **`restored == fresh` diff** — after mutating the writable surface (scratch files, env marker) and
   restoring, the writable-surface file set and the credential map are byte-for-byte equal to a
   newly-built baseline. (TC-008-02, TC-008-03.)
2. **credential map empty after restore** — `restore` subsumes `Wipe()`; the proxy carries no stale
   credential. (TC-008-05, TC-008-06.)

Under bwrap this is observable end-to-end: run 1 writes `/work/leak.txt`; after `restore`, run 2's
payload cannot read it (TC-008-04).

### The invariant is preserved by construction

A restored baseline rebuilds the spawn argv/OCI spec from the same `bwrapArgv` / `gvisorOCISpec`
path a fresh run uses — `--unshare-all` (or the path-less OCI netns), no `--share-net`, and the
**fresh** per-run `/proxy.sock`. `restore` never re-binds a stale credential and never reuses a stale
socket. The no-network + proxy-only-egress invariant is untouched; the writable surface never widens.

## Open-question dispositions

### Q1 — What is the "baseline"? Host-side only vs a kernel-level snapshot.

**Disposition: host-side only this increment.** The baseline is the host-side per-run state — the
work dir, `payload.sh`, and the fresh proxy with an empty credential map. **No** kernel-level memory
snapshot of the bwrap/gVisor root.

Reachability finding: a hyperlight-style in-process memory snapshot/restore is **not reachable with
stdlib + bwrap/runsc**. hyperlight snapshots a micro-VM's guest memory because it owns the VMM;
bwrap and runsc are spawned as opaque subprocesses we do not control the address space of, and the
Go stdlib exposes no `CRIU`/checkpoint primitive. A kernel snapshot would require either CRIU
(a third-party dependency — barred by the stdlib-only commitment, ADR 001 D1) or a VMM tier
(Firecracker/Kata, still unwired). So the host-side reset is not merely the conservative choice —
it is the *only* choice within the current stack. The deeper variant is gated on a future VMM tier.

### Q2 — Long-lived sandbox process (warm pool)?

**Disposition: deliver only the factored reset; defer the warm-pool variant.** The real throughput
win (hyperlight's ~1–2 ms reuse) needs a sandbox process kept *warm* across calls. The current
`run()` is strictly one-shot and the contract must not change. A persistent process is a different
risk surface (a live process between calls is exactly the "leftover process" the ephemeral non-goal
forbids) and a different lifecycle. This increment ships the leak-proof reset boundary one-shot
`run()` uses; the warm-pool variant is deferred with the reopening condition below.

### Q3 — Where does reuse get triggered?

**Disposition: no external trigger this increment.** The mechanism exists and is proven leak-free;
no `RunRequest` field or CLI flag selects reuse yet. Adding a reuse trigger is a follow-on that must
ship *with* the leak proof already in place (which this task provides). Keeping the trigger out now
means the one-shot contract is byte-for-byte unchanged and there is no new input to abuse.

### Q4 — Per-tier reach.

**Disposition: the factored reset applies to the host-side surface common to both tiers.** The
baseline `restore` resets — the host work dir, `payload.sh`, the host-side proxy credential map — is
**tier-independent**: it is the same host-side state under bwrap and gVisor (the gVisor OCI bundle is
itself rebuilt from the same host-side baseline). The reset does **not** reach inside a tier's kernel
root (that is Q1's deferred kernel snapshot). The boundary is: *restore covers the host-side baseline
that both tiers are built from; it does not snapshot or restore tier-internal kernel state.*

## Why the conservative cut still satisfies the ephemeral non-goal

The non-goal is "every run is ephemeral; no state leaks between runs." The host-side reset satisfies
it directly and is *proven* to: after `restore`, the writable surface and the credential map equal a
fresh baseline (the `restored == fresh` diff), and a second real run cannot read the first run's
files or credential. Because no long-lived process is introduced (Q2) and no reuse is triggered (Q3),
the one-shot path remains byte-for-byte the already-correct ephemeral model — the factoring adds a
*named, tested* reset boundary without weakening anything. The increment makes the ephemeral
guarantee **more** legible (it is now an asserted property, not an emergent one), not less.

## Reopening condition for the warm-pool throughput variant (Q2/Q3)

Revisit a long-lived warm-pool sandbox when **all** of:

1. A measured per-call latency budget shows the fresh build/teardown (currently dominated by the
   bwrap/runsc spawn, not the host-side baseline) is the bottleneck for a real workload, **and**
2. A VMM tier (Firecracker/Kata) or an in-process snapshot primitive lands, making a *kernel* snapshot
   reachable (Q1) — a host-side-only warm pool keeps a live process without the kernel-snapshot win,
   the worst of both, **and**
3. The leak proof is extended to cover a *reused* kernel root (not just the host-side surface this
   task proves) with the same `restored == fresh` rigor.

Until all three hold, the one-shot fresh-build model is correct and cheap enough; this ADR's
host-side reset is the durable boundary the warm-pool variant would build behind.

## Fitness rule: new **F-010**, not an extension of F-006/F-007

F-006/F-007 assert the *writable-surface and no-network invariants of a single run*. F-010 asserts a
different property: that a **restored** sandbox is indistinguishable from a **fresh** one — no
file/env/credential leak across a reset, netns still unshared. Its defining check is the
cross-reset `restored == fresh` diff + empty-credential-map, which neither F-006 nor F-007 covers.
So **F-010** is added: "a restored sandbox is indistinguishable from a freshly-built one (no
file/env/credential leak); the no-network namespace stays unshared and the only egress is a fresh
`/proxy.sock`." Check command: the snapshot test set
(`go test -run 'Snapshot|Restore|Baseline|Leak' ./...`).

## Consequences

- A `sandboxBaseline` value + `snapshot`/`restore` are factored out of `Run()`; the one-shot path
  is observationally identical to today (`snapshot → run → teardown`).
- `restore` subsumes `proxy.Wipe()` — the credential half of the reset — and re-seeds the writable
  work dir with only `payload.sh`, equal to a fresh baseline.
- No `run()` contract or result-schema change. No bwrap argv / OCI-spec change. Existing tests stay
  green and unmodified.
- Spec/contract updated in the same feat commit: `docs/spec/behaviors.md` (new B-012 describing the
  factored reset path), `docs/spec/fitness-functions.md` (F-010 added),
  `docs/architecture/diagrams.md` (reset loop note on the sequence flow).
- Out of scope (deferred per Q1–Q3): kernel-level memory snapshot, a long-lived warm-pool process, a
  reuse trigger in the `RunRequest`/CLI. These are recorded above with an explicit reopening
  condition — no future-tense spec text carries them.
