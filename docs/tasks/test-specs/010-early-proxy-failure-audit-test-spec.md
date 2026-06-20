# Test Spec 010: terminal audit event on early proxy-start failure (B-007)

**Linked task:** [`docs/tasks/backlog/010-early-proxy-failure-audit.md`](../backlog/010-early-proxy-failure-audit.md)
**ADR:** ADR 010 (lightweight — records the resolution of the B-007 TODO: a started run emits a terminal audit event even on early failure, for audit completeness). Settle the event `action`/`decision`/`context` shape there against the existing audit IPC contract.
**Written:** 2026-06-20

## Context for the test author

`docs/spec/behaviors.md` **B-007** ("Fail fast if the egress proxy cannot start") carries an open
TODO: *"confirm whether an `exit`/teardown audit event should also fire on early proxy-start
failure — currently it does not."* Today, when `proxy.Start(proxySock)` fails, `Run()` returns
`{error: "proxy start failed: <err>"}` (`run.go:113-115`) **after** the `spawn` audit event already
fired (`run.go:68`) and after any credential injection. **No terminal (`exit`) event is emitted on
this path** — the audit trail sees a `spawn` with no matching terminal event, an audit-completeness
gap (every started run should have a terminal event).

**Resolution this task implements (recommended, to be confirmed in ADR 010):** a run that has
emitted `spawn` MUST emit a terminal audit event even on early proxy-start failure. The natural
shape mirrors the existing `exit` event but carries the failure status.

Ground truth — the existing audit emission path:
- `spawn`: `emit(auditSocket, {actor:"exec-sandbox", action:"spawn", target:sandboxID,
  decision:"allow", context:{tier, request_id}})` (`run.go:68-71`).
- `exit` (success path): `emit(auditSocket, {actor:"exec-sandbox", action:"exit", target:sandboxID,
  decision:"allow", context:{exit_code, duration_ms, status, request_id}})` (`run.go:190-194`).
- `inject_failed`: `{action:"inject_failed", decision:"deny", context:{request_id}}` (`run.go:91-94`).
- `emit` is best-effort: an empty `audit_socket` is a no-op; an IPC error is swallowed
  (`run.go:392-397`). The audit event shape is documented in `data-model.md` §"audit event".
- The early-failure return path is `run.go:113-115`; at that point `defer baseline.teardown()` is
  already registered (`run.go:83`) but the `proxy.Stop/Wipe` defer (`run.go:116`) is **not yet**
  registered (the failure is on the line that would register it).

**Contract constraint:** the new event's `action`/`decision`/`context` must fit the existing audit
IPC contract (`data-model.md` §"audit event" — `{op:"emit", event:{actor, action, target, decision,
context}}`). Pick ONE of: reuse `action:"exit"` with a failure-flavored `status`/`context`, or a
distinct `action` (e.g. `"teardown"` / `"exit"` with `status:"proxy_start_failed"`). ADR 010
records the choice; the test asserts whatever ADR 010 settles. Recommended: `action:"exit"`,
`decision:"deny"`, `context:{status:"proxy_start_failed", error:<msg>, request_id}` — keeps the
"every spawn has a matching exit" invariant clean for audit consumers.

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-010-01 | When `proxy.Start` fails after `spawn` has been emitted, `Run()` emits a **terminal** audit event (matching the resolved ADR-010 shape) to the audit socket, with `target == sandbox_id` and a failure-indicating status/decision | TC-010-01 | ✅ |
| REQ-010-02 | The terminal event's shape conforms to the existing audit IPC contract (`{op:"emit", event:{actor:"exec-sandbox", action, target, decision, context}}`) and carries the `request_id` from `wiring`; its `context` distinguishes the early-failure cause from a clean exit | TC-010-02 | ✅ |
| REQ-010-03 | `Run()` still returns `{error: "proxy start failed: <err>"}` and runs **no** payload on this path (the fail-fast contract is unchanged; the audit event is additive) | TC-010-03 | ✅ |
| REQ-010-04 | On the **success** path the audit sequence is unchanged: `spawn` then `exit` (`decision:"allow"`), with no spurious extra terminal event — the new emission fires **only** on the early-failure path | TC-010-04 | ✅ |
| REQ-010-05 | Emission stays best-effort: an empty `audit_socket` makes the new terminal emission a no-op and the run still returns the `{error}` result (an audit IPC failure never changes the returned error) | TC-010-05 | ✅ |
| REQ-010-06 | `docs/spec/behaviors.md` B-007 is updated to document the resolved behavior (the terminal event now fires; the TODO is removed); `data-model.md` audit-event section reflects the new terminal-on-early-failure event if its shape adds an `action`/`status` value | TC-010-06 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] The success path is asserted unchanged (no spurious extra event)
- [x] Best-effort emission (empty socket) is covered
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: the event shape is settled in ADR 010 against the existing audit IPC contract

---

## Test cases

### TC-010-01: proxy-start failure emits a terminal audit event after spawn

- **Requirement:** REQ-010-01
- **Type:** unit/integration (Go test against a stub audit socket that records emitted events)
- **Input:** a `RunRequest` wired to a stub audit Unix socket (records every `emit`), with a
  `proxySock` path forced to fail `net.Listen("unix", …)` — e.g. point the per-run proxy socket at
  a path whose parent is non-writable or already bound/occupied so `proxy.Start` errors. (The
  baseline builds a proxy socket path under the work dir; the test must steer `Start` to fail —
  settle the injection seam during implementation, e.g. a pre-bound socket at the expected path.)
- **Expected:** the stub records, in order, a `spawn` event then a **terminal** event with
  `target == sandbox_id` and the resolved failure status (per ADR 010). No `exit`/allow success
  event is recorded.

### TC-010-02: terminal event conforms to the audit IPC contract and carries request_id

- **Requirement:** REQ-010-02
- **Type:** unit (assert the recorded event's shape)
- **Input:** the terminal event captured in TC-010-01, with `wiring.request_id = "req-xyz"`.
- **Expected:** the event is `{op:"emit", event:{actor:"exec-sandbox", action:<resolved>,
  target:<sandbox_id>, decision:<resolved, failure-flavored>, context:{… request_id:"req-xyz" …}}}`.
  The `context` distinguishes the early-failure cause from a clean exit (e.g.
  `status:"proxy_start_failed"` or equivalent per ADR 010) and does **not** carry a success
  `exit_code:0` that would let a consumer mistake it for a clean run.

### TC-010-03: the fail-fast contract is unchanged — error returned, no payload run

- **Requirement:** REQ-010-03
- **Type:** unit
- **Input:** the same forced-proxy-failure run as TC-010-01.
- **Expected:** `Run()` returns `{error: "proxy start failed: <err>"}` (string prefix unchanged);
  no backend is selected/spawned (no `bwrap`/`runsc` exec on this path), and no success result shape
  (`{stdout, …, sandbox_status}`) is returned. The audit event is purely additive.

### TC-010-04: success path audit sequence unchanged (no spurious terminal event)

- **Requirement:** REQ-010-04
- **Type:** integration (bwrap) — reuse the existing proxy-reach harness with a stub audit socket
- **Input:** a normal successful run (proxy starts, payload runs) wired to a recording stub audit
  socket.
- **Expected:** the recorded sequence is exactly `spawn` (allow) → `exit` (allow), with the usual
  `exit` context `{exit_code, duration_ms, status, request_id}`. The new early-failure emission does
  **not** fire — there is no extra terminal event on the success path.

### TC-010-05: empty audit_socket — emission is a no-op, error still returned

- **Requirement:** REQ-010-05
- **Type:** unit
- **Input:** the forced-proxy-failure run with `wiring.audit_socket = ""`.
- **Expected:** `Run()` returns the same `{error: "proxy start failed: <err>"}`; no panic, no
  IPC attempt (the `emit` no-op path at `run.go:392-393`). Best-effort emission never alters the
  returned result.

### TC-010-06: B-007 spec updated, TODO removed

- **Requirement:** REQ-010-06
- **Type:** inspection (spec)
- **Input:** read `docs/spec/behaviors.md` B-007 and `data-model.md` audit-event section after the
  feat commit.
- **Expected:** B-007's "Side effects" / "Failure modes" now state that a terminal audit event
  **is** emitted on early proxy-start failure; the `(TODO: confirm whether …)` line is **removed**.
  If the resolved event introduces a new `action`/`status` value, `data-model.md`'s audit-event
  list documents it. The behavioral-invariant bullet about B-007 (line ~135) still holds (proxy
  failure aborts before any payload runs) and is consistent with the added emission.

---

## Post-implementation verification

- [ ] TC-010-01..03, TC-010-05: forced-proxy-failure unit tests pass; terminal event recorded,
      conforms to contract, error unchanged, empty-socket no-op
- [ ] TC-010-04: success path emits only spawn→exit (bwrap; integration)
- [ ] TC-010-06: B-007 TODO removed; data-model audit section consistent
- [ ] ADR 010 written settling the event shape

## Test framework notes

- Reuse the recording stub audit socket pattern from the existing audit tests (a Unix listener that
  appends each received `{op:"emit", event:…}` line). If none exists yet, add a small helper that
  spins a `net.Listen("unix", …)` goroutine and collects decoded events — the audit IPC is the same
  JSON-line `ipcCall` both `emit` and `vaultInject` use.
- Forcing `proxy.Start` to fail: the cleanest seam is to pre-bind the per-run proxy socket path (so
  `net.Listen` returns `address already in use`) or to make its parent dir non-writable. Settle the
  exact mechanism during implementation; whichever is chosen, the test must reach the
  `run.go:113-115` early-return branch.
- The new emission must be placed so the `spawn`-already-emitted invariant holds (the event fires
  only after `spawn`, only on the `Start` failure branch) and so it does not double-fire on the
  success path.
