# Task 001: gVisor (runsc) Tier-2 backend

**Project:** exec-sandbox
**Created:** 2026-06-18
**Status:** ready

## Goal

Add a gVisor (`runsc`) Tier-2 isolation backend behind the existing `tier` seam, so that
`tier == "gvisor"` runs the payload via the runsc OCI runtime instead of bubblewrap — preserving
the `run()` contract, the no-network + proxy-only-egress invariant, and audit emission, and
leaving the bubblewrap path unchanged.

## Context

- Tech stack: Go 1.26, stdlib-only, single `main` package at repo root.
- Related ADRs: [`001-foundational-stack.md`](../../architecture/decisions/001-foundational-stack.md) — D7 (tier seam: `bubblewrap | gvisor | firecracker`, v0 wires bubblewrap only).
- Spec: [`docs/spec/behaviors.md`](../../spec/behaviors.md) B-001/B-002/B-003/B-004 (the run flow, egress, injection, audit); [`docs/spec/interfaces.md`](../../spec/interfaces.md) — the `bwrapArgv` tier seam point and the `Run()` contract; [`docs/spec/SPEC.md`](../../spec/SPEC.md) top-level invariants.
- Dependencies: none (additive behind the seam). The current dispatch in `Run()` calls `bwrapArgv` unconditionally; this task introduces a `tier`-keyed selector.
- New decision: wiring a second isolation backend behind the seam is significant enough to warrant **ADR-002** (refines ADR-001 D7). Write it before/with the implementation.

## Requirements

| Req ID | Description | Priority |
|--------|-------------|----------|
| REQ-001 | A tier-dispatch seam selects the isolation backend by `run.tier` (bubblewrap vs gvisor); empty/default → bubblewrap | must have |
| REQ-002 | The gvisor path enforces no-network (no network namespace / `network: none`) and exposes only the bind-mounted proxy socket for egress | must have |
| REQ-003 | The `run()` contract is unchanged: same request fields, same `{stdout, stderr, exit_code, sandbox_status}` result, same audit events; `sandbox_status.tier` echoes the requested tier | must have |
| REQ-004 | The gvisor integration test skips cleanly when `runsc` is absent (mirroring the `requireBwrap` skip in `run_test.go`) | must have |
| REQ-005 | The bubblewrap path (and `tier` empty/default) is unchanged; existing `run_test.go` stays green and is not edited | must have |

## Readiness gate

- [x] Test spec `001-gvisor-tier2-backend-test-spec.md` exists in `docs/tasks/test-specs/`
- [x] All acceptance criteria below have a linked REQ ID
- [ ] Any blocking tasks are complete (none)

## Acceptance criteria

- [ ] [REQ-001] A `tier`-keyed selector (e.g. `backendFor(tier)`) chooses bubblewrap vs gvisor; `""` and `"bubblewrap"` both select bubblewrap; an unimplemented tier returns a clear "tier not implemented" error rather than silently defaulting.
- [ ] [REQ-002] The gvisor backend produces a runsc OCI spec/invocation with no network namespace (`network: none`) and only the proxy socket bind-mounted as egress.
- [ ] [REQ-003] The result shape and audit-event shapes are byte-identical across tiers; `sandbox_status.tier` reflects the requested tier.
- [ ] [REQ-004] A new test (in a new file, e.g. `gvisor_test.go`) uses a `requireRunsc(t)` helper that `t.Skip`s when `runsc` is absent; `go test ./...` is green on a box without runsc.
- [ ] [REQ-005] `run_test.go` is untouched; `go build ./... && go test ./...` is green.

## Verification plan

- **Highest level achievable:** L5 — `go test ./...` exercises the tier-dispatch seam and the gvisor spec/argv builder in isolation (without runsc), and confirms the bubblewrap path is unchanged. The end-to-end gvisor execution test skips cleanly when runsc is absent (the case on the current dev box), so L6 is deferred/opportunistic.
- **Level 5 — Validation harness command:**
  ```
  go build ./... && go test ./...
  ```
  Expected final assertion: `ok  	github.com/tkdtaylor/exec-sandbox` with the gvisor execution test reported as `--- SKIP` when runsc is absent, and all existing tests passing.
- **Level 6 — Operator observation (deferred; requires runsc):**
  - Binary path: `echo '{"run":{"payload":"…","tier":"gvisor",…},"wiring":{…}}' | ./bin/exec-sandbox run`
  - Targeted behaviour to observe: an allowlisted request succeeds through the proxy and a non-allowlisted host is blocked, with `sandbox_status.tier == "gvisor"`, while a direct (non-proxy) network attempt fails — confirming no-network + proxy-only egress under runsc.
- **Cross-module state risk:** none new — the gvisor backend reuses the same `EgressProxy`, vault.inject loop, and audit emission. The only new state is the backend selector keyed on `tier`.
- **Runtime-visible surface:** CLI/result JSON (`sandbox_status.tier`) and the sandbox's network behaviour. The executor must run `go test ./...` and quote the output; an L6 runsc run is opportunistic.

## Out of scope

- Firecracker/Kata (Tier 3) — still returns "tier not implemented".
- Full seccomp profiles, resource cgroup limits, env-mode injection + wipe clock, SOCKS5/TLS proxy — all separate deferred items.
- Any change to `main.go`, `run.go` egress/injection logic beyond introducing the dispatch seam, `proxy.go`, `run_test.go`, or `go.mod`. (Introducing the seam may touch `Run()`'s single `bwrapArgv` call site — keep that change minimal and behavior-preserving for the bubblewrap path.)
- Installing runsc in CI — the skip pattern is the contract here.

## Notes

- The seam point today is `Run()`'s direct call to `bwrapArgv(scriptPath, proxySock)` and the
  subsequent `exec.Command`. Generalize that to a backend abstraction selected by `tier`, with
  bubblewrap as one implementation and gvisor as the second. Keep the no-network + proxy-only
  invariant identical across both.
- runsc consumes an OCI bundle (config.json + rootfs). The gvisor backend builds that bundle
  with `network: none` (or no network namespace) and the proxy socket bind-mounted in.
- Write **ADR-002** recording the gVisor backend as a refinement of ADR-001 D7.
- This task is the first real build for agent-builder against this repo.
