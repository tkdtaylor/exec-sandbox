# Task 010: terminal audit event on early proxy-start failure (resolve B-007 TODO)

**Status:** ⬜ backlog
**Branch:** `task/010-early-proxy-failure-audit`
**Spec:** [`docs/tasks/test-specs/010-early-proxy-failure-audit-test-spec.md`](../test-specs/010-early-proxy-failure-audit-test-spec.md)
**ADR:** ADR 010 (lightweight — records the B-007 resolution and the chosen terminal-event shape).

## Problem

`docs/spec/behaviors.md` **B-007** carries an open TODO: when the egress proxy fails to start
(before the backend runs), should an `exit`/teardown audit event fire? Today it does **not**.
`Run()` emits `spawn` first (`run.go:68`), then on `proxy.Start` failure returns
`{error: "proxy start failed: <err>"}` (`run.go:113-115`) **without** any terminal audit event.
The audit trail is left with a `spawn` and no matching terminal event — an **audit-completeness
gap**: a run that started (emitted `spawn`, possibly injected credentials) has no terminal record.

## Decision (to confirm in ADR 010)

**Yes — emit a terminal audit event on early proxy-start failure.** A run that has emitted `spawn`
should always emit a terminal event, so every `spawn` has a matching terminal event for audit
consumers. **Recommended shape:** `action:"exit"`, `decision:"deny"`,
`context:{status:"proxy_start_failed", error:<msg>, request_id}` — reuses the existing `exit`
action so "every spawn has an exit" stays clean, while `decision:"deny"` + the failure `status`
distinguish it from a clean exit. ADR 010 settles the exact `action`/`decision`/`context` against
the existing audit IPC contract (`data-model.md` §"audit event"); the test asserts whatever ADR 010
fixes. **Read `run.go` + `proxy.go` + the audit emission path (`emit`/`ipcCall`) first** so the
event shape matches the contract exactly and the credential invariant is untouched (no credential
or handle in the event).

## Scope

- **Emit a terminal audit event on the `proxy.Start` failure branch** (`run.go:113-115`), after
  `spawn` has fired, before returning the `{error}`. Use the existing `emit(auditSocket, {…})`
  helper. The event carries `target == sandbox_id` and the resolved failure status/decision.
- **Preserve fail-fast.** `Run()` still returns `{error: "proxy start failed: <err>"}` (string
  prefix unchanged) and runs **no** payload — no backend selected, no `bwrap`/`runsc` exec. The
  audit event is purely additive.
- **Best-effort emission unchanged.** An empty `audit_socket` makes the new emission a no-op
  (`run.go:392-393`); an IPC error is swallowed; neither changes the returned `{error}`.
- **No spurious event on the success path.** The new emission fires **only** on the early-failure
  branch; the success path's `spawn → exit` sequence is unchanged.
- **Spec update in the same commit:** `docs/spec/behaviors.md` B-007 — state that a terminal audit
  event **is** emitted on early proxy-start failure, and **remove the TODO** (rewrite the
  "Side effects" / "Failure modes" lines in place). If the resolved event adds a new
  `action`/`status` value, document it in `docs/spec/data-model.md`'s audit-event section. Keep the
  behavioral-invariant bullet (B-007 aborts before any payload runs) consistent. No future tense.

Out of scope: emitting a terminal event for the **other** early-error returns
(`validateWorkdir`/`validateFileReads`/`backendFor`/baseline-prepare) — those fail **before**
`spawn` is emitted (`run.go:53-64`, `79-82`) or after but as a deliberately separate concern; this
task is specifically the post-`spawn`, pre-backend proxy-start gap B-007 names. If consistency for
those paths is wanted, note it as ADR 010's reopening condition. Also out of scope: changing the
`inject_failed` event or the success `exit` event.

## Verification plan

- **Highest level achievable: L5/L6.** This host has `bwrap`, so the success-path sequence
  (`spawn → exit`, no spurious extra event) is observable end-to-end against a recording stub audit
  socket; the early-failure branch is unit-observable by forcing `proxy.Start` to fail and reading
  the recorded events.
- **Harness command:** `go test -count=1 ./...`.
- **Runtime observation (L5/L6):** against a recording stub audit socket — (1) a forced
  proxy-start failure records `spawn` then the terminal failure event (correct `target`, failure
  status, `request_id`), and `Run()` returns `{error: "proxy start failed: …"}` with no payload
  run; (2) a normal run records exactly `spawn → exit` (allow) with no extra terminal event;
  (3) with `audit_socket:""` the failure path returns the same `{error}` and emits nothing.
- **ADR 010 written during implementation:** records the decision (terminal-on-early-failure for
  audit completeness), the chosen event shape against the audit IPC contract, and the scope
  boundary (this gap only; pre-`spawn` early errors deferred).

## Definition of done

- A terminal audit event is emitted on the `proxy.Start` failure branch, after `spawn`, conforming
  to the existing audit IPC contract, with `target == sandbox_id`, the resolved failure
  status/decision, and the `request_id`.
- The fail-fast contract is unchanged: `{error: "proxy start failed: <err>"}` returned, no payload
  run; best-effort emission (empty socket → no-op) preserved.
- No spurious terminal event on the success path (`spawn → exit` unchanged).
- `docs/spec/behaviors.md` B-007 updated, **TODO removed**; `data-model.md` audit section updated
  if a new action/status value was introduced — all in the feat commit, rewritten in place.
- ADR 010 written; `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE before promotion to ✅.
