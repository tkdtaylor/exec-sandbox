# Test Spec 022: Reconcile README and roadmap drift with shipped Tier 3 and v2 attestation

**Linked task:** [`docs/tasks/backlog/022-reconcile-readme-and-roadmap-drift.md`](../backlog/022-reconcile-readme-and-roadmap-drift.md)
**Written:** 2026-10-31
**Status:** ready for implementation

## Requirements coverage

| Req ID | Description | Test cases |
|--------|-------------|------------|
| REQ-022-01 | `README.md` accurately describes all 3 tiers (`bubblewrap`, `gvisor`, `firecracker`) and v2 host-key attestation (`keygen`, `verify-attestation`) as shipped. | TC-022-01 |
| REQ-022-02 | `docs/plans/roadmap.md` marks tasks 009 through 021 as completed (`✅`). | TC-022-02 |
| REQ-022-03 | Full test and fitness suite passes. | TC-022-03 |

## Test cases

### TC-022-01: `README.md` reflects shipped Tier 3 and v2 attestation

- **Requirement:** REQ-022-01
- **Level:** L2 (`doc_drift_022_test.go`)
- **Step:** Read `README.md` and assert it mentions `firecracker`, `keygen`, and `verify-attestation` as shipped features and contains no stale claim that Firecracker is "planned" or "not yet wired".

### TC-022-02: `docs/plans/roadmap.md` reflects completed tasks 009 through 021

- **Requirement:** REQ-022-02
- **Level:** L2 (`doc_drift_022_test.go`)
- **Step:** Read `docs/plans/roadmap.md` and assert tasks 009 through 021 are recorded as completed (`✅`) with no stale "Planned" status for shipped tasks.

### TC-022-03: Full test and fitness suite passes

- **Requirement:** REQ-022-03
- **Level:** L3
- **Step:** Run `make check` and confirm all unit tests and fitness checks pass.
