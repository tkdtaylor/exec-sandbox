# ADR 008: per-HTTP-verb allowlist enforcement in the egress proxy

**Date:** 2026-06-19
**Status:** Accepted
**Task:** 007 (per-HTTP-verb allowlist enforcement in the egress proxy)
**Related:** ADR 001 (foundational stack: no-network, proxy-only egress, domain allowlist),
ADR 002 (gVisor Tier-2 backend behind the `tier` seam), ADR 005 (FileRead read-only mounts +
env provisioning — the capability-parsing pattern this extends), ADR 007 (per-run output caps —
the prior "narrows only, never widens" host-side control).

## Context

The roadmap (`docs/architecture/prior-art.md`, "Net-new candidates" #3, inspired by hyperlight's
`allow_domain` with verb) calls for **per-HTTP-verb allowlist enforcement** in the egress proxy.
Today the proxy enforces a **domain-only** allowlist (`proxy.go` `handle`: a non-allowlisted host
gets `403 blocked-by-allowlist`); it then forwards **any** method to an allowlisted host. A real
policy may want read-only egress — allow `GET`/`HEAD` to an API host but deny `POST`/`DELETE` so
the payload can read but not mutate.

The work splits along the project's **decide-vs-enforce** boundary (recorded in the prior-art
ownership map): the **decision** of which verbs are permitted belongs to **policy-engine** (whose
`NetAllowlist` is domain-only at v0 and is the natural place to grow per-verb policy). **This task
is exec-sandbox's half: the proxy-side enforcement mechanism, plus the allowlist data shape that
can carry verbs.** exec-sandbox does not *decide* the verb policy — it carries the shape the
decider emits and *enforces* it at the only egress.

## Decision

### 1. Wire shape: a per-host `methods` array on the `NetConnect` capability

The `NetConnect` capability gains an optional `methods` array that applies to **every host in that
capability's `allowlist`**:

```json
{ "type": "NetConnect", "allowlist": ["api.example.com:443"], "methods": ["GET", "HEAD"] }
```

A profile that needs **different** verb sets per host emits **multiple** `NetConnect`
capabilities — one per host group — exactly as it already may for different allowlists:

```json
"capabilities": [
  { "type": "NetConnect", "allowlist": ["read.example.com:443"], "methods": ["GET"] },
  { "type": "NetConnect", "allowlist": ["write.example.com:443"], "methods": ["GET", "POST"] }
]
```

**Rejected alternative — a structured per-host object** (e.g.
`"allowlist": [{"host": "api.example.com:443", "methods": ["GET"]}]`): it is more expressive in a
single capability, but it **breaks the existing wire shape** — today `allowlist` is a flat
`[]string` and every existing profile, test, and the `netAllowlist` parser depends on that. The
flat-`methods`-array-per-capability form is **strictly additive**: an existing capability with no
`methods` key parses and behaves byte-for-byte as before, and the multiple-capabilities idiom
already exists for differing allowlists. Per-host granularity is preserved by splitting into more
capabilities, so no expressiveness is lost — only the JSON shape stays backward compatible. If a
future need arises for path/prefix-level constraints (out of scope here), that is the **reopening
condition** for revisiting the structured-object form, since paths cannot be folded into a flat
per-capability array.

### 2. Empty / absent `methods` ⇒ **unconstrained** (all verbs), never deny-all

The host → allowed-method-set map uses a **nil/empty set as the "unconstrained" sentinel**:

- **No `methods` key** (today's only shape) ⇒ the host carries no verb constraint ⇒ **all verbs
  allowed**. This is what makes the change backward compatible.
- **`"methods": []`** (explicitly empty) ⇒ also **unconstrained**, *not* deny-all. Deny is the
  **absence of a verb from a non-empty set**, never an empty set.

Rationale: an empty set meaning "deny everything" would be a silent footgun — a profile that
accidentally emits `"methods": []` (or a serializer that drops the contents) would brick all egress
to that host with no diagnostic. The default-closed stance lives at the **host** layer (an
unlisted host is already blocked); the verb layer only ever **narrows within** an
already-allowlisted host, and the safe narrowing-default is "no narrowing" (all verbs). A policy
that wants to deny a verb must name the verbs it *does* allow in a non-empty set.

### 3. Case-normalization: canonical upper-case comparison

HTTP methods are case-sensitive on the wire but conventionally upper-case (RFC 9110 registers them
upper-case). The proxy normalizes both sides to **upper-case** before comparison: the parsed
`methods` entries are upper-cased at parse time, and the request's `r.Method` is upper-cased at
check time. So `get`, `Get`, and `GET` all match an allowed `GET`. This avoids a trivial bypass
(or a trivial accidental deny) from case alone and matches Go's `http` conventions.

### 4. The distinct 403 reason body: `blocked-by-method` vs `blocked-by-allowlist`

A blocked verb returns **HTTP 403** — the *same status code* the proxy already returns for a
blocked host — but with a **different body** so a caller (or an audit consumer) can tell the two
apart:

- `blocked-by-allowlist` — the **host** is not on the allowlist (existing behavior, unchanged).
- `blocked-by-method` — the host *is* allowed but the **method** is not in its non-empty verb set.

Same status, different reason. The host check stays **first**: an unlisted host is
`blocked-by-allowlist` regardless of method, so the verb mechanism can never widen host access — it
only narrows *within* a host already past the host gate.

### 5. The verb check **narrows only** — no new egress, no outbound connection on a block

The verb check sits **between** the host/route resolution and the `client.Do` that opens the
outbound connection. On a blocked verb the handler returns the 403 **before** building the upstream
request, so:

- **No outbound connection happens** — the origin observes zero requests for a blocked verb.
- **No credential is injected** — credential injection happens only on the forward path, which the
  block short-circuits.
- **No new route, no `--share-net`, no second socket** — the proxy remains the sole egress; the
  verb check can only *remove* requests from what the proxy forwards, never add any.

This is the same "narrows only" property ADR 007's output cap has: a host-side control that can
reduce what crosses the boundary but never widen it. The no-network + proxy-only-egress invariant
is untouched.

### 6. Fitness rule: **new F-009** (F-008 is taken by task 006's output cap)

F-009 asserts: "a request whose method is not in an allowlisted host's non-empty verb set is
`403`'d (`blocked-by-method`) and is **never forwarded upstream** — the origin observes no
connection and no credential is injected; an unconstrained host (no/empty `methods`) forwards every
verb as before, and the host check still precedes the verb check." Check command: the verb test set
(`go test -run 'Verb|VerbAllowlist|BlockedByMethod' ./...`). This is a distinct invariant from
F-001 (no shared network) and the host-allowlist enforcement — it guards the *intra-host verb
narrowing* and the *no-outbound-on-block* property specifically.

## Consequences

- `NetConnect` gains an optional `methods: []string` field, parsed into a `host → method-set` map
  (`map[string]map[string]bool`); nil/empty set ⇒ unconstrained. The bare-host allowlist set
  (existing `netAllowlist`) is **still** produced unchanged for the host-level check.
- `NewEgressProxy` gains a `verbAllowlist map[string]map[string]bool` parameter (nil = every host
  unconstrained); existing callers pass the parsed map (or `nil` to preserve current behavior).
- `EgressProxy.handle` checks the canonical-upper method against the host's set **after** the host
  check and **before** building/forwarding the upstream request; a miss is `403 blocked-by-method`
  with no outbound connection and no credential injection.
- Spec/contract updated in the same feat commit: `docs/CONTRACT.md`, `docs/spec/data-model.md`
  (`NetConnect` gains optional `methods`; `EgressProxy` in-memory state gains the per-host verb
  set), `docs/spec/configuration.md` (the `NetConnect.allowlist` row gains the verb-constraint
  note), `docs/spec/behaviors.md` (B-002 gains the verb check after the host check),
  `docs/spec/fitness-functions.md` (F-009 added). The spec notes the verb **decision** is
  policy-engine's; this block **enforces**.
- Out of scope (unchanged): the policy-engine-side *decision* of which verbs to allow (a
  sibling-block task — this task only carries + enforces the shape); **path/prefix-level**
  allowlisting (verbs only — the reopening condition for the structured-per-host-object form);
  method-specific credential injection (the credential stays host-keyed as today).
