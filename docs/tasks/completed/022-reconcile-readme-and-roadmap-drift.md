# Task 022: Reconcile README and roadmap drift with shipped Tier 3 and v2 attestation

**Project:** exec-sandbox
**Created:** 2026-10-31
**Status:** completed

## Goal

Update `README.md` and `docs/plans/roadmap.md` so they accurately reflect the shipped and verified state of tasks 001 through 021 (Firecracker microVM Tier 3, vsock egress bridge, Tier 1 seccomp-BPF profile, and ADR 017 host-signed v2 attestation) instead of listing them as planned or unbuilt.

## Context

`docs/spec/SPEC.md` and `docs/tasks/test-specs/coverage-tracker.md` record all 21 tasks as `✅` verified, and the compiled `vmlinux-6.1.176`, `base.ext4`, and `tier1.bpf` artifacts are present in the repository. However, `README.md` and `docs/plans/roadmap.md` still describe Firecracker Tier 3 and tasks 009 through 021 as planned future work.

## Requirements

| Req ID | Description | Priority |
|--------|-------------|----------|
| REQ-022-01 | `README.md` status and tier descriptions state that all three tiers (`bubblewrap`, `gvisor`, `firecracker`), the vsock egress bridge, the seccomp-BPF filter, and host-key v2 attestation (`keygen`, `verify-attestation`, `wiring.attestation_key`) are shipped. | must have |
| REQ-022-02 | `docs/plans/roadmap.md` marks tasks 001 through 021 as completed and moves only genuinely unscheduled items (HTTPS CONNECT / dynamic DNS in `EgressProxy`, warm microVM pools, Firecracker `env`-mode credential delivery) to the deferred section. | must have |
| REQ-022-03 | All existing unit and fitness checks (`make check`) pass without regression. | must have |

## Readiness gate

- [x] Test spec `docs/tasks/test-specs/022-reconcile-readme-and-roadmap-drift-test-spec.md` exists
- [x] Tasks 001 through 021 are completed and verified

## Implementation outline

1. Run `scripts/start-task.sh 022 reconcile-readme-and-roadmap-drift`.
2. Update `README.md` to reflect shipped Tier 3 (`firecracker`), seccomp-BPF on Tier 1, and ADR 017 v2 host-key attestation (`keygen` and `verify-attestation` CLI subcommands), and document current operational caveats (`origin_map` HTTP upstream, Firecracker `proxy`-only credential mode).
3. Update `docs/plans/roadmap.md` so tasks 001 through 021 reflect their completed `✅` state.
4. Add a doc-consistency test in `doc_drift_022_test.go` verifying `README.md` and `docs/plans/roadmap.md` no longer claim Firecracker or tasks 009-021 are unbuilt.
5. Run `make check` and confirm all tests and fitness checks pass.

## Acceptance criteria

- [x] [REQ-022-01] TC-022-01: `README.md` documents all three tiers (`bubblewrap`, `gvisor`, `firecracker`) and v2 host-key attestation (`keygen`, `verify-attestation`) as shipped.
- [x] [REQ-022-02] TC-022-02: `docs/plans/roadmap.md` marks tasks 009 through 021 as completed.
- [x] [REQ-022-03] TC-022-03: `go test -race ./...` and `make check` pass.

## Verification plan

- **Highest level achievable:** L2/L3 automated doc-drift test + `make check`.
- **Command:** `go test -race -v -run TestTC022 ./... && make check`
