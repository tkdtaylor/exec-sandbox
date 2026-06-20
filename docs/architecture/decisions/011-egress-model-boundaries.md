# ADR 011 — Egress model boundaries: SOCKS5 and OSEP-0001 two-layer filter both rejected

**Status:** Accepted
**Date:** 2026-06-20
**Related:** ADR 001 D4 (network boundary owned by exec-sandbox: host-side egress proxy +
allowlist — this ADR supersedes that decision's *future-work prediction*, see Consequences),
ADR 008 (per-HTTP-verb allowlist — the enforcement layer that evaporates below HTTP),
ADR 010 D2 (Firecracker no-NIC + vsock egress; the rejected TAP+nftables shape this ADR's
Decision 2 declines to un-reject) and ADR 010 A1.Q3 (the unprivileged / no-`CAP_NET_ADMIN`
invariant Decision 2 preserves). Source analysis: a 2026-06-20 roadmap review with a
primary-source research pass (sources cited inline below).

## Context

A roadmap review researched the two deferred egress items carried in ADR-001 D4's future-work
note and in the README "Deferred (v1)" list — a **SOCKS5 proxy** alongside the HTTP egress
proxy, and a **two-layer network-namespace egress filter** modelled on Alibaba OpenSandbox's
**OSEP-0001** (DNS-proxy + `nftables`). Both were carried as "evaluate before scoping a task."
The review reached a decision on each, backed by primary sources. This ADR records the outcomes
so they are not re-litigated. Both decisions are **made** — this is a record, not a re-derivation.

## Decisions

### Decision 1 — SOCKS5 proxy: REJECTED

A SOCKS5 egress proxy alongside (or in place of) the HTTP egress proxy is **rejected**.

- **It is fundamentally incompatible with the block's headline feature — credential injection.**
  exec-sandbox's reason to exist is credential-injecting egress: vault hands the proxy
  `{credential, binding:{host,header,scheme}}` and the proxy **splices the secret into the
  structured HTTP request as a header**, so the secret never enters the sandbox (ADR-001 D5).
  That splice is only possible at the HTTP layer, where requests are structured into a method,
  a host, a path, and headers. A SOCKS5 tunnel carries an **opaque TCP byte stream** — the proxy
  sees bytes, not a request, and has nowhere to inject a credential. SOCKS5 could therefore only
  ever serve **uncredentialed allowlisted TCP**, a narrow slice that excludes the one feature the
  block is built around.
- **Arbitrary-TCP egress is the exfiltration surface the no-network invariant exists to deny.**
  For untrusted agent-generated code, a raw TCP tunnel is exactly the channel the no-network
  model forbids. ADR-008's per-host HTTP-verb allowlist **evaporates below the HTTP layer** —
  there is no method to allow or deny in an opaque stream — and egress logging degrades from
  "`GET host/path`" to "opened TCP to `host:port`," losing the per-request audit granularity the
  block promises.
- **Real-world evidence: SOCKS5 imports a known allowlist-bypass bug class.** E2B exposes SOCKS5
  for raw TCP (E2B internet-access docs), and that surface produced the **2026 Claude Code SOCKS5
  hostname null-byte allowlist-bypass** (versions ≤ 2.1.89): null-byte / CRLF / percent
  canonicalization smuggled a disallowed host past the matcher because **SOCKS5 hostname parsing
  sits below the layer where the allowlist canonicalizes the host**. Adopting SOCKS5 means
  adopting that whole bug class — exactly the wrong trade for a security box.
- **The genuine gap is HTTPS via `CONNECT`, which is NOT this decision.** The real, smaller
  primitive that the deferred SOCKS5 item was groping toward is **HTTPS via the HTTP `CONNECT`
  method**: the host allowlist *survives* (the client sends the bare host in the `CONNECT` line,
  which the existing allowlist canonicalizes the same way it does any host), and credential
  injection is simply **N/A** once the client TLS-terminates end-to-end (there is no plaintext
  header to splice — which is correct, not a regression). `CONNECT` is the correct smaller
  primitive and the real tracked gap, but it is **out of scope for this ADR** and tracked
  separately on the roadmap. A bare uncredentialed SOCKS5/TCP tunnel is revisited **only** if a
  concrete, named non-HTTP uncredentialed workload appears — filed as its own task, with the
  null-byte / CRLF / percent-canonicalization negative-test battery as an **upfront requirement**,
  not an afterthought.

### Decision 2 — OSEP-0001 two-layer egress filter (DNS-proxy + nftables): REJECTED for adoption; retained as cited prior art

The OpenSandbox **OSEP-0001** two-layer egress filter (Layer-1 DNS-proxy via iptables `REDIRECT`,
Layer-2 `nftables` on resolved IPs) is **rejected for adoption**, and **retained as cited prior
art** for the different problem it solves.

- **It filters a route exec-sandbox does not have.** OSEP-0001 constrains an **existing** egress
  route and presupposes a network namespace **with** a route — it shares the sandbox's netns and
  requires `CAP_NET_ADMIN` to program the firewall. exec-sandbox has **no route by construction**:
  `bwrap --unshare-all` (Tier-1), `runsc --network=none` + empty netns (Tier-2), and ADR-010 D2's
  no-NIC + vsock bridge (Tier-3). There is no IP stack to `REDIRECT`, no NIC to filter — OSEP-0001
  would filter a route that does not exist.
- **Deny-by-construction dominates filter-an-existing-route, and fails closed.** `nftables`
  filtering **fails open**: a missing or misordered rule is a silent egress hole, and OSEP-0001
  even degrades to "dns-only" when `CAP_NET_ADMIN` is absent — a mode in which direct-IP
  connections bypass the filter entirely. The no-network model **fails closed**: no NIC, no route,
  nothing to misconfigure. For untrusted code, fail-closed is the required posture; this is the
  same reasoning ADR-010 D2 used to reject the TAP+nftables shape for the microVM tier.
- **Adopting it would regress two standing invariants.** It would **un-reject ADR-010 D2** (which
  explicitly rejected the TAP+nftables egress shape) and would regress the **unprivileged
  invariant** ADR-010 A1.Q3 fought to preserve (no `CAP_NET_ADMIN`, no root). No tier on the
  roadmap has a guest route that needs `nftables`. Only a deliberate **future** tier that attaches
  a real NIC (e.g. Kata-with-CNI) would flip this calculus — and no such tier is designed.
- **Keep it as cited prior art.** OSEP-0001 (OpenSandbox egress component README) is a credible
  design for the **different** problem of namespace-with-route sandboxes. It stays referenced in
  the README as a reference architecture — not an adopted dependency — and this rejection is the
  authoritative record of *why* it is not adopted.

## Alternatives considered

- **Adopt SOCKS5 for uncredentialed TCP egress.** *Rejected (Decision 1).* It cannot carry the
  credential-injection feature, it re-opens the arbitrary-TCP exfiltration surface, and it imports
  the SOCKS5 hostname-canonicalization bypass bug class (the 2026 Claude Code ≤2.1.89 bypass). The
  smaller correct primitive — HTTPS via `CONNECT` — preserves the host allowlist and is tracked
  separately.
- **Adopt OSEP-0001's two-layer DNS-proxy + nftables filter.** *Rejected (Decision 2).* It filters
  an existing route; exec-sandbox has none. It fails open and needs `CAP_NET_ADMIN`; the
  no-network model fails closed and stays unprivileged. Adopting it would un-reject ADR-010 D2 and
  regress ADR-010 A1.Q3.
- **Do nothing / leave both items in the deferred list.** *Rejected.* Leaving them as open
  "evaluate before scoping" items means the same two questions get re-researched each time they
  surface, and ADR-001 D4's prediction continues to imply they are coming. Recording the
  rejections (with sources) settles them and lets the roadmap reflect the real, smaller gap
  (`CONNECT`).

## Consequences

- **This ADR supersedes ADR-001 D4's future-work prediction** that "v1 adds TLS-terminating +
  SOCKS5 and the two-layer network-namespace egress filter." SOCKS5 and the two-layer filter are
  rejected. **HTTPS via `CONNECT` remains a tracked gap** (host allowlist preserved; credential
  injection N/A once the client TLS-terminates) — tracked on the roadmap, not in this ADR. ADR-001
  D4's *historical* decision body (the v0 single-layer HTTP proxy, the domain allowlist, the
  origin-map forwarding) stands unchanged; only its forward-looking note is superseded.
- **The README and CONTRACT.md drop the SOCKS5 / two-layer-filter future promise.** The contract
  statement becomes present-tense and accurate: the proxy speaks HTTP over the Unix socket, does
  not TLS-terminate or tunnel arbitrary TCP; HTTPS via `CONNECT` is the known gap; SOCKS5 was
  evaluated and rejected. OSEP-0001 stays in the README as cited reference architecture, now
  annotated as rejected-for-adoption.
- **What gets harder:** an HTTPS-origin workload that needs the client to TLS-terminate end-to-end
  is not yet served — that is the `CONNECT` gap, deferred until such a workload is named. A
  non-HTTP uncredentialed workload is likewise not served, and the bar to add one is deliberately
  high (a named workload + an upfront canonicalization-bypass negative-test battery).
- **What stays simple:** the egress surface remains a single host-side HTTP proxy with a domain +
  per-host-verb allowlist and host-side credential injection. No SOCKS5 parser, no `nftables`
  ruleset, no `CAP_NET_ADMIN`, no route to fail open. The no-network-by-construction, fail-closed
  posture is reaffirmed across all three tiers.

## Sources

- E2B internet-access documentation (SOCKS5 raw-TCP egress surface).
- The 2026 Claude Code SOCKS5 hostname null-byte allowlist-bypass write-up (versions ≤ 2.1.89;
  null-byte / CRLF / percent canonicalization smuggled past the host matcher).
- OpenSandbox **OSEP-0001** egress component README (two-layer DNS-proxy + `nftables`
  default-deny model; degrades to DNS-only without `CAP_NET_ADMIN`).
- Internal: ADR-010 D2 (rejected TAP+nftables for the microVM tier; fail-closed-by-omission) and
  ADR-010 A1.Q3 (unprivileged, no-`CAP_NET_ADMIN` invariant); ADR-008 (per-HTTP-verb allowlist,
  which is meaningless below the HTTP layer).

## Supersedes

ADR-001 D4 — **in part**. Supersedes only its forward-looking prediction that v1 adds a
TLS-terminating + SOCKS5 proxy and the two-layer network-namespace egress filter. The historical
v0 decision body of ADR-001 D4 (single-layer HTTP proxy, domain allowlist, origin-map forwarding,
no-network boundary) is preserved and reaffirmed.
