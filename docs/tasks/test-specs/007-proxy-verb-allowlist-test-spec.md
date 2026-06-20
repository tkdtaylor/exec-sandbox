# Test Spec 007: per-HTTP-verb allowlist enforcement in the egress proxy

**Linked task:** [`docs/tasks/backlog/007-proxy-verb-allowlist.md`](../backlog/007-proxy-verb-allowlist.md)
**ADR:** ADR 008 (to be written during implementation)
**Written:** 2026-06-19

## Context for the test author

The proxy enforces a **domain-only** allowlist today (`proxy.go:82`: non-allowlisted host →
`403`). policy-engine's `NetAllowlist` is domain-only at v0 — the *decision* of which verbs are
permitted is policy-engine's to grow. This task is the proxy-side **enforcement mechanism** plus
the allowlist data shape that can carry verbs: an allowlisted host may permit only a subset of
HTTP methods (e.g. `GET` to `api.example.com` but not `POST`). A non-allowlisted verb is rejected
with `403`, the same status the proxy returns for a non-allowlisted host.

The data shape must be **backward compatible**: a host listed with no verb constraint behaves
exactly as today (all verbs allowed).

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-007-01 | The egress allowlist carries an optional per-host **verb set**; the wire shape extends `NetConnect` so each entry can name allowed methods (e.g. `{"type":"NetConnect","allowlist":["api.example.com:443"],"methods":["GET","HEAD"]}` or a per-host form — exact shape settled in ADR 008). A host with **no** verb constraint ⇒ all verbs allowed (backward compatible) | TC-007-01 (unit, parser) | ✅ |
| REQ-007-02 | The proxy permits a request whose method is in the host's allowed verb set, forwarding to the origin exactly as today (credential injection unchanged) | TC-007-02 (unit, handler), TC-007-05 (integration) | ✅ |
| REQ-007-03 | The proxy rejects a request whose method is **not** in the host's allowed verb set with HTTP `403` (same status as a blocked host), and does **not** forward to the origin (no upstream call, no credential injected) | TC-007-03 (unit, handler), TC-007-06 (integration) | ✅ |
| REQ-007-04 | A non-allowlisted **host** is still rejected with `403` regardless of method (host check precedes verb check); the verb mechanism never widens host access | TC-007-04 (unit, handler) | ✅ |
| REQ-007-05 | Verb matching is case-insensitive per RFC-correct method comparison normalization (canonical upper-case) so `get`/`GET` are treated alike; the allowed-set is compared against the request's normalized method | TC-007-07 (unit) | ✅ |
| REQ-007-06 | The verb allowlist **preserves the no-network + proxy-only-egress invariant**: it only *narrows* what the proxy forwards; it adds no new route, no `--share-net`, and the proxy remains the sole egress. A blocked verb produces **no** outbound connection | TC-007-06 (integration — origin observes no request) | ✅ |
| REQ-007-07 | Backward compatible: an allowlist entry with no `methods` (the only shape that exists today) allows every method exactly as before; existing proxy tests stay green | TC-007-08 (regression) | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Edge cases and error paths are covered
- [x] Every REQ-ID has at least one test case
- [x] Success criteria are unambiguous
- [x] Confirmed: the *decision* of allowed verbs belongs to policy-engine; this task is enforcement + carrier shape only

---

## Test cases

### TC-007-01: NetConnect parsing yields a per-host verb set; absent ⇒ all-verbs

- **Requirement:** REQ-007-01
- **Type:** unit (no sandbox)
- **Input:** profiles with (a) a NetConnect entry naming `methods: ["GET","HEAD"]` for
  `api.example.com:443`; (b) a NetConnect entry with **no** methods; (c) two NetConnect entries
  for different hosts with different method sets.
- **Expected:** (a) the parser records `api.example.com → {GET, HEAD}`; (b) records the host with
  the "all verbs" sentinel (nil/empty set meaning unconstrained); (c) each host keeps its own
  set. The bare-domain allowlist set (existing behavior) is still produced for host-level checks.
- **Edge cases:** an empty `methods: []` is treated as "all verbs" (unconstrained), not "deny
  all" — explicit verb denial is the absence of a verb from a non-empty set, not an empty set.

### TC-007-02: an allowed verb to an allowed host is forwarded

- **Requirement:** REQ-007-02
- **Type:** unit (proxy handler against a stub origin)
- **Input:** proxy configured with `api.example.com → {GET}` and a route to a stub origin;
  issue a `GET` to `api.example.com`.
- **Expected:** the stub origin receives the request; the proxy returns the origin's status; a
  configured credential is injected exactly as today.

### TC-007-03: a disallowed verb to an allowed host is 403'd and not forwarded

- **Requirement:** REQ-007-03
- **Type:** unit (proxy handler against a stub origin that records hits)
- **Input:** proxy configured with `api.example.com → {GET}`; issue a `POST` to
  `api.example.com`.
- **Expected:** the proxy returns `403`; the stub origin records **zero** requests; no credential
  is injected (the upstream call never happens).
- **Edge cases:** the 403 body distinguishes the verb rejection (e.g. `blocked-by-method`) from
  the host rejection (`blocked-by-allowlist`) so callers can tell them apart, while the status
  code is identical (403).

### TC-007-04: a non-allowlisted host is 403'd regardless of method (host check first)

- **Requirement:** REQ-007-04
- **Type:** unit (proxy handler)
- **Input:** proxy configured with only `api.example.com → {GET}`; issue a `GET` to
  `other.example.com`.
- **Expected:** `403` (`blocked-by-allowlist`); the host check runs before any verb check, so an
  unlisted host is blocked even for an otherwise-allowed method.

### TC-007-05: end-to-end — allowed verb reaches the allowlisted host via the proxy (sandbox)

- **Requirement:** REQ-007-02
- **Type:** integration (bwrap; reuse the existing proxy-reach test harness)
- **Input:** a sandboxed payload that `curl`s an allowlisted host with an allowed method over
  `/proxy.sock`.
- **Expected:** the request reaches the origin (status 200 via the stub), exactly as the existing
  `TestSandboxReachesAllowlistedHostViaProxy` — proving the verb mechanism does not break the
  allowed path.

### TC-007-06: end-to-end — disallowed verb is blocked, origin sees nothing (sandbox)

- **Requirement:** REQ-007-03, REQ-007-06
- **Type:** integration (bwrap)
- **Input:** a sandboxed payload that `curl -X POST`s an allowlisted host whose verb set is
  `{GET}`, over `/proxy.sock`; the origin stub records every hit.
- **Expected:** the payload receives `403`; the origin stub records **no** POST — proving the
  block happens at the proxy with no outbound connection, the invariant intact (the proxy is
  still the only egress and it forwards nothing it must not).

### TC-007-07: verb matching is case-insensitive / normalized

- **Requirement:** REQ-007-05
- **Type:** unit (proxy handler)
- **Input:** proxy configured with `api.example.com → {GET}`; issue requests with method `get`
  and `GET`.
- **Expected:** both are permitted (normalized to canonical `GET`). A method not in the set
  (any case) is rejected.

### TC-007-08: no methods constraint ⇒ all verbs allowed (regression)

- **Requirement:** REQ-007-07
- **Type:** unit + integration (bwrap)
- **Input:** an allowlist entry with no `methods` (today's only shape); issue `GET`, `POST`,
  `DELETE`.
- **Expected:** all are forwarded exactly as before this task; `proxy_test.go`'s existing
  `TestProxyBlocksNonAllowlistedHost` / `TestSandboxReachesAllowlistedHostViaProxy` and the
  `TestNetAllowlistParsing` test stay green (host-level behavior unchanged).

---

## Post-implementation verification

- [x] Unit TCs pass (TC-007-01..04, TC-007-07, TC-007-08 unit half)
- [x] bwrap integration TCs pass on a box with bwrap (TC-007-05, TC-007-06, TC-007-08 integ half)
- [x] L5/L6: a disallowed verb observed returning 403 with the origin stub recording no hit
- [x] No regressions in existing proxy tests (TC-007-08)

## Test framework notes

- Standard Go `testing`. Reuse the stub-origin + `httptest` / Unix-socket harness already in
  `run_test.go` for the proxy-reach tests. Add a stub origin that records method + count so
  TC-007-03/06 can assert zero upstream hits on a blocked verb.
- New tests live in `proxy_verb_test.go`; `proxy.go` gains the per-host verb set; the
  `NewEgressProxy` constructor signature change (or a setter) is the only mechanical touch to
  existing callers — pass "no constraint" to preserve current behavior.
