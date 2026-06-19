# Task 008: snapshot/restore for clean-slate sandbox reuse

**Status:** ⬜ backlog
**Branch:** `task/008-snapshot-restore`
**Spec:** [`docs/tasks/test-specs/008-snapshot-restore-test-spec.md`](../test-specs/008-snapshot-restore-test-spec.md)
**ADR:** ADR 009 (to be written during implementation — this task carries open design questions)

## Problem

The roadmap (`docs/architecture/prior-art.md`, "Net-new candidates" #1, inspired by hyperlight's
`snapshot()`/`restore()`) calls for **resetting a sandbox to a pristine baseline between
invocations** instead of a full teardown + rebuild per run, to improve per-call hygiene and
throughput at scale. hyperlight's `UninitializedSandbox → snapshot() → restore() → call()` loop
gives it ~1–2 ms reuse instead of a fresh build per call.

This is a **performance / hygiene** task, **not** a security gap. The current ephemeral model
(fresh temp dir, fresh proxy, wiped at teardown) is already correct; this task seeks the same
correctness more cheaply. Therefore the **hard bar** is the project's own non-goal — "no
persistent state; every run is ephemeral; no state leaks between runs" — restated as a property
of reuse: **a reused sandbox must be byte-for-byte indistinguishable from a freshly built one.**
No file, env var, leftover process, or credential from a prior run may survive a reset.

**Scope is deliberately conservative.** This task does not commit to a long-lived sandbox process
or a kernel-level memory snapshot. It pins down the **reset/baseline contract and its leak-proof
property** behind the unchanged one-shot `run()` contract, leaving the throughput-maximizing
variants as recorded open questions for ADR 009.

## Scope

- **Factor a `snapshot`/`restore` boundary out of `Run()`.** Today `Run()` inlines: make temp
  dir → write `payload.sh` → start fresh proxy (empty creds) → spawn → teardown
  (`os.RemoveAll` + `proxy.Wipe()`). Introduce a small internal sandbox-state value that captures
  the **pristine baseline** (`snapshot`: the prepared workdir state, the `payload.sh` contents,
  the fresh proxy with an empty credential map) and a `restore` that returns that state to the
  baseline (remove anything written under the writable surface, reset env, clear the credential
  map). The default one-shot path is `snapshot → run → teardown`, observationally identical to
  today.
- **Leak-proofing is the load-bearing deliverable.** `restore` must make the writable surface and
  the proxy credential state equal to a freshly-built baseline. Prove it with a direct
  `restored == fresh` diff and a "credential map empty after restore" check (the credential half
  of the ephemeral non-goal — `restore` subsumes `Wipe()`).
- **Preserve the invariant.** A restored sandbox still has **no network namespace** (`--unshare-all`
  / path-less OCI netns) and its only egress is a **fresh** `/proxy.sock`. `restore` must never
  re-bind a stale credential or reuse a stale socket from a prior run. The argv/OCI spec for a
  restored sandbox are identical to a fresh one's.
- **The `run()` contract is unchanged.** snapshot/restore is an **internal** reuse mechanism — no
  externally-visible result-schema change beyond, at most, an optional internal telemetry marker
  (settle in ADR 009). A one-shot `run()` with no reuse behaves byte-for-byte as today.
- **Spec + contract update in the same commit** (only for what actually lands): if this increment
  is internal-only with no externally-visible behavior change, the spec update is a note in
  `docs/spec/behaviors.md` describing the factored snapshot/restore reset path and a
  `docs/spec/fitness-functions.md` row asserting "a restored sandbox is indistinguishable from a
  fresh one (no file/env/credential leak); netns stays unshared". `docs/architecture/diagrams.md`
  gets the reset loop if a runtime flow changed. Do **not** add future-tense spec text for the
  deferred long-lived-process variant — that stays in this task / ADR 009.

### Open design questions (resolve in ADR 009, do not pre-decide)

1. **What is the "baseline"?** Host-side only (temp dir + `payload.sh` + proxy), or also a
   kernel-level snapshot of the bwrap/gVisor root? The conservative first increment is host-side
   only; record whether a kernel snapshot is even reachable with stdlib + bwrap/runsc.
2. **Long-lived sandbox process?** The real throughput win (hyperlight's ~1–2 ms) needs a
   persistent sandbox kept warm across calls — but the current `run()` is strictly one-shot and
   the contract must not change. Decide whether this task delivers only the factored reset (no
   persistent process) and defers the warm-pool variant, or attempts a guarded long-lived mode.
3. **Where does reuse get triggered?** If reuse is added, by what input — and does it risk the
   ephemeral guarantee enough to require a stronger leak proof? Default: no external trigger in
   this increment; the mechanism exists and is proven leak-free, reuse wiring is a follow-on.
4. **Per-tier reach.** Does the factored reset apply uniformly to bwrap and gVisor, or only the
   host-side surface common to both? Record the boundary.

## Verification plan

- **Highest level achievable: L5/L6 (host-side reset increment).** This host has `bwrap`, so the
  leak-proof property is observable: a restored sandbox does **not** expose run 1's `/work/leak.txt`
  or run 1's credential, and is indistinguishable from a fresh build. The deeper kernel-snapshot /
  warm-pool variant, if deferred per ADR 009, is not claimed.
- **Harness command:** `go test -count=1 ./...`
- **Runtime observation (L6):** run 1 writes `/work/leak.txt`; after `restore`, run 2's payload
  cannot read it; the proxy credential map is empty after restore; a one-shot run with no reuse is
  byte-for-byte unchanged. The `restored == fresh` diff and the empty-credential-map check are the
  load-bearing assertions.
- **Fitness (L3):** new fitness row asserting "a restored sandbox is indistinguishable from a
  fresh one — no file/env/credential leak; netns stays unshared"; check command is the snapshot
  test set.
- **ADR 009 written during implementation:** records the resolved scope of the first increment
  (host-side reset vs kernel snapshot vs warm pool), each of the four open questions and its
  disposition, why the conservative cut still satisfies the ephemeral non-goal, and the reopening
  condition for the warm-pool throughput variant.

## Definition of done

- A `snapshot`/`restore` boundary is factored out of `Run()`; the default one-shot path is
  observationally identical to today.
- `restore` makes the writable surface and the proxy credential state equal to a fresh baseline,
  proven by a `restored == fresh` diff and an empty-credential-map check.
- A second run on a restored sandbox cannot read the first run's files or credential (bwrap, L6).
- A restored sandbox keeps the no-network + proxy-only-egress invariant — fresh socket, no stale
  credential, netns unshared, argv/spec identical to a fresh build.
- The `run()` contract and result schema are unchanged for a one-shot run; existing tests green.
- Spec/behaviors + fitness row updated for what actually landed (no future-tense for deferred
  variants); **ADR 009** written with the open-question dispositions.
- spec-verifier APPROVE before promotion to ✅.
