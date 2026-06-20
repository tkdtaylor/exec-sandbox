# Task 007: per-HTTP-verb allowlist enforcement in the egress proxy

**Status:** ⬜ backlog
**Branch:** `task/007-proxy-verb-allowlist`
**Spec:** [`docs/tasks/test-specs/007-proxy-verb-allowlist-test-spec.md`](../test-specs/007-proxy-verb-allowlist-test-spec.md)
**ADR:** ADR 008 (to be written during implementation — see Verification plan)

## Problem

The roadmap (`docs/architecture/prior-art.md`, "Net-new candidates" #3, inspired by hyperlight's
`allow_domain` with verb) calls for **per-HTTP-verb allowlist enforcement** in the proxy. Today
the egress proxy enforces a **domain-only** allowlist (`proxy.go:80-85`: a non-allowlisted host
gets `403`); it forwards **any** method to an allowlisted host. A real policy may want to allow
`GET` to a host but deny `POST`/`DELETE` — read-only egress to an API without write access.

The work splits along the project's decide-vs-enforce boundary (recorded in the prior-art
ownership map): the **decision** of which verbs are permitted belongs to **policy-engine** (whose
`NetAllowlist` is domain-only at v0 and is the natural place to grow per-verb policy). **This task
is exec-sandbox's half: the proxy-side enforcement mechanism, plus the allowlist data shape that
can carry verbs.** A non-allowlisted verb is rejected with `403` — the same status as a
non-allowlisted host.

## Scope

- **Extend the allowlist data shape to carry verbs.** Add an optional per-host allowed-method set
  to the `NetConnect` capability wire shape (e.g. `{"type":"NetConnect","allowlist":[…],
  "methods":["GET","HEAD"]}`, or a richer per-host form — settle the exact JSON shape in ADR 008
  and document it). A host listed with **no** verb constraint ⇒ **all verbs allowed** (backward
  compatible — today's only shape). An empty `methods: []` is treated as "unconstrained", **not**
  "deny all" (deny is the absence of a verb from a non-empty set).
- **Parse it** in `run.go` alongside `netAllowlist` (`run.go:389`): produce, in addition to the
  bare-host allowlist set the proxy already gets, a `host → allowed-method-set` map (nil/empty set
  = unconstrained). Carry it into `NewEgressProxy`.
- **Enforce it in the proxy handler** (`proxy.go:80`, `handle`): after the existing host check
  (host check stays **first** so an unlisted host is still blocked regardless of method), check
  the request's normalized (canonical upper-case) method against the host's allowed set. If the
  host is unconstrained, allow. If the method is not in the set, return `403` with a body that
  distinguishes it (`blocked-by-method`) from the host block (`blocked-by-allowlist`) — same
  status code, different reason — and **do not** forward upstream and **do not** inject a
  credential (no outbound connection happens).
- **Preserve the invariant.** The verb check only ever **narrows** what the proxy forwards. It
  adds no route, no `--share-net`, no second socket; the proxy remains the sole egress. A blocked
  verb produces **no** outbound connection. Credential injection on the allowed path is unchanged.
- **Spec + contract update in the same commit:** `docs/CONTRACT.md`, `docs/spec/data-model.md`
  (the `NetConnect` capability gains the optional `methods` field; `EgressProxy` in-memory state
  gains the per-host verb set), `docs/spec/configuration.md` (the `NetConnect.allowlist` row gains
  the verb-constraint note), `docs/spec/behaviors.md` (the proxy egress flow gains the verb check
  after the host check), `docs/spec/fitness-functions.md` (add F-008/F-009 — pick the next free
  number relative to task 006 — asserting a non-allowlisted verb is `403`'d with no upstream
  connection). Rewrite in place. Note in the spec that the **decision** is policy-engine's; this
  block enforces.

Out of scope: the policy-engine-side *decision* of which verbs to allow (a sibling-block task —
this task only carries + enforces the shape); path/prefix-level allowlisting (verbs only — note
it as ADR 008's reopening condition); method-specific credential injection (the credential is
host-keyed as today).

## Verification plan

- **Highest level achievable: L5/L6.** This host has `bwrap`, so the end-to-end proxy path
  (allowed verb reaches the origin; disallowed verb is `403`'d with the origin observing no hit)
  is observable under a real sandbox, reusing the existing `TestSandboxReachesAllowlistedHostViaProxy`
  harness.
- **Harness command:** `go test -count=1 ./...`
- **Runtime observation (L6):** a sandboxed `curl` with an allowed method reaches the allowlisted
  origin (200); a `curl -X POST` to a host whose verb set is `{GET}` gets `403` and the origin
  stub records **zero** POSTs — proving the block is at the proxy with no outbound connection. The
  host-block path (`blocked-by-allowlist`) and verb-block path (`blocked-by-method`) are
  distinguished in unit tests against a hit-recording stub origin.
- **Fitness (L3):** new fitness row asserting "a non-allowlisted verb to an allowlisted host is
  403'd and never forwarded upstream"; check command is the verb test set.
- **ADR 008 written during implementation:** settles the JSON shape (per-host `methods` array vs a
  structured per-host object), the empty-set = unconstrained semantics, the case-normalization
  rule, the distinct 403 reason body, and records that the verb *decision* is policy-engine's
  while enforcement is here (the decide/enforce split from the prior-art ownership map).

## Definition of done

- The `NetConnect` shape carries an optional per-host verb set; absent/empty ⇒ all verbs allowed.
- The proxy forwards an allowed verb to an allowed host (credential injection unchanged) and
  `403`s a disallowed verb **without** any upstream connection or credential injection.
- The host check still precedes the verb check; a non-allowlisted host is `403`'d regardless of
  method; verb matching is case-insensitive.
- The no-network + proxy-only-egress invariant is intact — the verb check only narrows egress.
- Backward compatible: an entry with no `methods` allows all verbs exactly as before; existing
  proxy tests green.
- Spec + CONTRACT updated in place; fitness row added; **ADR 008** written.
- spec-verifier APPROVE before promotion to ✅.
