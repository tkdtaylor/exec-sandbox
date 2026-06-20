# ADR 013: Terminal audit event on early proxy-start failure (resolve B-007 TODO)

**Status:** Accepted  
**Date:** 2026-06-20  
**Deciders:** exec-sandbox maintainers  
**Supersedes:** —  
**Reopening condition:** if terminal events for the other early-error returns (`validateWorkdir` /
`validateFileReads` / `backendFor` / baseline-prepare) are also desired, open a separate task —
those fail **before** `spawn` is emitted and require a different invariant ("every run" vs "every
spawn"), which is a separate audit-completeness question outside this decision's scope.

---

## Context

`run.go` emits a `spawn` audit event (`action:"spawn"`, `decision:"allow"`) early in `Run()`,
before the inject loop and before the proxy starts (`run.go:68-71`). If `proxy.Start` subsequently
fails (`run.go:113-115`), the function returns `{error: "proxy start failed: <err>"}` without
emitting any terminal audit event. The audit trail is left with a `spawn` and no matching terminal
event — an audit-completeness gap. A run that has emitted `spawn` (and possibly injected
credentials) has no terminal record.

`docs/spec/behaviors.md` B-007 carried an open TODO acknowledging this gap: *"confirm whether an
`exit`/teardown audit event should also fire on early proxy-start failure — currently it does not."*

Other early-error returns (`validateWorkdir`, `validateFileReads`, `backendFor`, `snapshotBaseline`)
occur **before** `spawn` is emitted (`run.go:53-64`, `79-82`), so they do not create a
spawn-without-exit gap. This ADR is scoped to the post-`spawn`, pre-backend gap only.

---

## Decision

**Emit a terminal audit event on the `proxy.Start` failure branch** — after `spawn` has fired,
before returning the `{error}`.

**Event shape:**

```json
{
  "actor":    "exec-sandbox",
  "action":   "exit",
  "target":   "<sandbox_id>",
  "decision": "deny",
  "context": {
    "status":     "proxy_start_failed",
    "error":      "<err.Error()>",
    "request_id": "<wiring.request_id>"
  }
}
```

**Rationale for `action:"exit"`:** reuses the existing `exit` action so the "every `spawn` has a
matching terminal event" invariant holds for audit consumers with no new action vocabulary to parse.
`decision:"deny"` + `status:"proxy_start_failed"` in the context distinguish it unambiguously from
a clean exit (`decision:"allow"`, `status:"clean"` or `"timeout"`). The `error` field carries the
underlying error message for operational debugging.

**Rationale against a new action (e.g. `"teardown"`):** a new action would require all
audit-trail consumers to be updated. Reusing `exit` is backward-compatible: any consumer that
already tracks `spawn → exit` will now close the pair on the failure path too; the
`decision:"deny"` + failure `status` are sufficient for a consumer that wants to distinguish the
failure case.

**Credential invariant:** the event carries **no** credential value, no vault handle, and no
`sandbox_identity.attestation` — only the `sandbox_id` (already public from the `spawn` event),
the error message (from `proxy.Start` return, not from vault), and the `request_id`. The credential
invariant is untouched.

**Fail-fast contract preserved:** `Run()` still returns `{error: "proxy start failed: <err>"}` (the
string prefix is unchanged) and runs **no** payload — no backend selected, no `bwrap`/`runsc`
executed. The audit event is purely additive.

**Best-effort emission unchanged:** an empty `audit_socket` makes `emit` a no-op (`run.go:392-393`);
an IPC error is swallowed. Neither changes the returned `{error}`.

**Implementation site:** a single `emit(...)` call is added immediately before the
`return map[string]any{"error": "proxy start failed: " + err.Error()}` line at `run.go:114`.

---

## Consequences

### Positive

- Every `spawn` event now has a matching terminal event — the audit-completeness gap that B-007's
  TODO named is closed.
- No new action vocabulary; audit consumers that already handle `spawn → exit` pairs get the
  failure pair for free.
- The failure context (`status:"proxy_start_failed"`, `error`, `decision:"deny"`) lets an audit
  consumer distinguish the early-failure event from a clean exit without extra schema.

### Negative / trade-offs

- The `exit` context schema now has two shapes (success and early-failure). They are distinguished
  by `decision` and `status`, but a consumer that assumed all `exit` events carried `exit_code` and
  `duration_ms` must be updated. The `data-model.md` audit-event section is updated in the same
  commit to document both shapes.

### Out of scope

- Terminal events for the **other** early-error returns (`validateWorkdir`, `validateFileReads`,
  `backendFor`, `snapshotBaseline`) — these fail before `spawn` fires, so there is no
  spawn-without-exit gap; a separate audit-completeness decision would be needed if consistency
  for those paths is also desired.
- Changing the `inject_failed` event shape or the success `exit` event.
- Changing the fail-fast return value (`{error: "proxy start failed: <err>"}`).
