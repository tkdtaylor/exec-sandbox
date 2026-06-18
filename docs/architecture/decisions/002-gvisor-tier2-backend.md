# ADR 002 — gVisor (runsc) Tier-2 backend behind the tier seam

**Status:** Accepted
**Date:** 2026-06-18

## Context

ADR-001 D7 established the `tier` seam (`bubblewrap | gvisor | firecracker`) as the project's
deliberate composability boundary: a new isolation backend should plug in behind the seam without
changing the `run()` contract or the no-network + proxy-only-egress invariant. v0 wired only
bubblewrap, and `Run()` called `bwrapArgv(...)` unconditionally.

This ADR records the second backend: **gVisor**, via the `runsc` OCI runtime, selected when
`req.run.tier == "gvisor"`. It refines (does not supersede) ADR-001 D7 — the seam itself, the
contract, and the security invariants are unchanged; only a second realization is added behind the
seam.

## Decisions

### D7.1 — Tier dispatch is an explicit selector, not a silent default

`Run()` no longer calls `bwrapArgv` directly. A `backendFor(tier)` selector maps the requested
tier to a backend implementation:

- `""` and `"bubblewrap"` → the bubblewrap backend (byte-for-byte the previous behavior).
- `"gvisor"` → the runsc backend.
- any other value (e.g. `"firecracker"`) → a clear `tier not implemented: <tier>` error. There is
  **no silent fall-back** to bubblewrap; an unrecognized tier fails fast and loudly, consistent with
  the project's "fail fast, crash loudly" principle.

A backend exposes a single responsibility: given the on-host `scriptPath` and `proxySock`, prepare
whatever it needs and return the `os/exec` argv to spawn. The bubblewrap backend returns the bwrap
argv; the gVisor backend writes an OCI bundle to a temp dir and returns the `runsc run` argv. The
orchestration in `Run()` (allowlist parse, identity mint, audit emit, vault.inject loop, proxy
start, stdout/stderr/exit capture, result assembly) is tier-independent and unchanged.

### D7.2 — gVisor enforces the same invariant via an OCI spec: empty network namespace + proxy-only egress

runsc consumes an OCI bundle (`config.json` + a `root.path` rootfs). The gVisor backend builds this
bundle so it enforces exactly the bubblewrap invariant:

- **No network.** The OCI `linux.namespaces` list includes a `network` namespace **with no `path`**,
  which directs the runtime to create a *fresh, empty* network namespace (loopback only — no host,
  bridged, or shared networking). This is the OCI-spec equivalent of `bwrap --unshare-all` for the
  network dimension. The spec contains no host-network sharing and no bridged/CNI configuration.
- **Proxy socket is the only egress.** The proxy Unix socket is bind-mounted into the container at
  `/proxy.sock` (matching the path payloads already expect under bwrap). No other socket, device, or
  network mount is added.
- **Minimal read-only root.** Host system dirs (`/usr`, `/etc`, conditionally `/bin /lib /lib64
  /sbin`) are bind-mounted read-only, mirroring the bwrap root. The payload is bind-mounted
  read-only at `/payload.sh` and run with `/usr/bin/sh /payload.sh` and `PATH=/usr/bin:/bin`, exactly
  as under bwrap.

The bundle lives in the same ephemeral temp dir as the rest of the run and is removed at teardown
(`defer os.RemoveAll`). `runsc run` is invoked with a rootless-friendly flag set
(`--network=none --ignore-cgroups`) so the no-network guarantee is also asserted at the runtime-flag
layer, and so the run does not require cgroup write access. `--network=none` is belt-and-suspenders
with the empty-namespace spec; both express the same invariant.

### D7.3 — The `run()` contract and audit shape are unchanged across tiers

The result remains `{stdout, stderr, exit_code, sandbox_status:{sandbox_id, tier, duration_ms,
secrets_injected, status}}`; `sandbox_status.tier` echoes the requested tier. The spawn/exit audit
events carry the same shape (tier in the spawn context). The `EgressProxy`, `vault.inject` loop, and
audit emission are reused verbatim — the only new state is the tier-keyed backend selection.

## Consequences

- The no-network + proxy-only-egress invariant (ADR-001 D3/D4) now has two enforcement points: the
  bwrap argv and the gVisor OCI spec. Any future backend must enforce it the same way.
- The seam is now a real dispatch point (`backendFor`), so the spec's "tier seam" reference moves
  from "`bwrapArgv` is dispatched unconditionally" to "`backendFor(tier)` selects the backend."
- An unrecognized tier is now a hard error rather than an implicit bubblewrap run. Callers that
  relied on an unknown tier silently working (there were none — only bubblewrap was wired) would
  break; this is the intended fail-fast behavior.
- The gVisor path adds a build-time dependency on `runsc` being on `PATH` *at run time for the
  gvisor tier only*. Its absence skips the integration test (mirroring `requireBwrap`) and yields a
  spawn error (`exit_code 127`) for an actual gvisor run, never a silent bubblewrap fall-back.

## Refines

ADR-001 D7 (tier seam). Does not supersede any decision; D3/D4/D5/D6 invariants are preserved.
