# Test Spec 001: gVisor (runsc) Tier-2 backend

**Linked task:** [`docs/tasks/backlog/001-gvisor-tier2-backend.md`](../backlog/001-gvisor-tier2-backend.md)
**Written:** 2026-06-18

## Requirements coverage

| Req ID | Test cases | Covered? |
|--------|-----------|----------|
| REQ-001 (tier-dispatch seam) | TC-001, TC-002 | ⏳ |
| REQ-002 (gvisor path: no-network + proxy-only egress) | TC-003, TC-004 | ⏳ |
| REQ-003 (contract unchanged) | TC-005 | ⏳ |
| REQ-004 (clean skip when runsc absent) | TC-006 | ⏳ |
| REQ-005 (bubblewrap path unchanged) | TC-001, TC-007 | ⏳ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Edge cases and error paths are covered
- [x] Every REQ-ID from the task has at least one test case
- [x] Success criteria are unambiguous

---

## Test cases

### TC-001: Default/empty/`bubblewrap` tier dispatches to the existing bwrap path

- **Requirement:** REQ-001, REQ-005
- **Input:** a `RunRequest` with `tier` set to `""`, `"bubblewrap"`, in three sub-cases.
- **Expected output:** the tier-dispatch seam selects the bubblewrap backend (`bwrapArgv`) unchanged. Observable via the existing integration assertions — `TestSandboxReachesAllowlistedHostViaProxy` and `TestProxyBlocksNonAllowlistedHost` continue to pass byte-for-byte. (These tests must NOT be edited — per the no-modify constraint on `run_test.go`; the dispatch seam must keep them green.)
- **Edge cases:** empty string must behave identically to `"bubblewrap"` (default-to-Tier-1).

### TC-002: `tier == "gvisor"` selects the runsc backend, not bwrap

- **Requirement:** REQ-001
- **Input:** a `RunRequest` with `tier: "gvisor"`.
- **Expected output:** the dispatch seam routes to the gVisor backend builder (runsc OCI invocation), not `bwrapArgv`. Asserted at the seam (e.g. a `backendFor(tier)` selector returns the gvisor backend) without requiring runsc to be installed — the selection is testable in isolation from execution.
- **Edge cases:** an unknown tier value (e.g. `"firecracker"`, still unimplemented) returns a clear "tier not implemented" error rather than silently falling back to bubblewrap.

### TC-003: gVisor path enforces no-network (no network namespace / `--network none`)

- **Requirement:** REQ-002
- **Input:** the runsc OCI config / runtime spec the gvisor backend produces for a payload.
- **Expected output:** the produced OCI runtime spec declares **no network namespace** (no shared host networking; network is `none`/empty), mirroring the bubblewrap `--unshare-all` invariant. Asserted against the generated spec/argv, runnable without runsc present.
- **Edge cases:** the spec must not contain any flag or namespace entry that would grant the sandbox host or bridged networking.

### TC-004: gVisor path exposes only the bind-mounted proxy socket for egress

- **Requirement:** REQ-002
- **Input:** the runsc OCI mounts the gvisor backend produces.
- **Expected output:** the proxy Unix socket is the only egress affordance bind-mounted into the container (analogous to `/proxy.sock` in the bwrap path). No additional network mounts/devices. Asserted against the generated mount list.
- **Edge cases:** the proxy socket path inside the container matches what the payload expects (`/proxy.sock`).

### TC-005: The run() contract is unchanged across both tiers

- **Requirement:** REQ-003
- **Input:** any successful run under either tier.
- **Expected output:** the result shape is identical — `{stdout, stderr, exit_code, sandbox_status:{sandbox_id, tier, duration_ms, secrets_injected, status}}`. `sandbox_status.tier` echoes the requested tier (`"gvisor"` for the gvisor path). No new top-level fields; no removed fields. Audit `spawn`/`exit` events still emitted with the same shape (tier carried in the spawn context).
- **Edge cases:** `secrets_injected` and the proxy credential-injection flow behave identically regardless of tier (vault.inject is tier-independent).

### TC-006: gVisor integration test skips cleanly when runsc is absent

- **Requirement:** REQ-004
- **Input:** the test environment has no `runsc` on `PATH`.
- **Expected output:** the gVisor integration test calls a `requireRunsc(t)` helper (mirroring `requireBwrap`) that `t.Skip(...)`s with a clear message. `go test ./...` exits 0 with the gvisor test reported as skipped, not failed. New test code lives in a NEW test file (e.g. `gvisor_test.go`), since `run_test.go` must not be modified.
- **Edge cases:** the skip must fire before any runsc invocation, so the suite is green on machines without gVisor (including the current dev box).

### TC-007: Existing bubblewrap behavior is byte-for-byte preserved

- **Requirement:** REQ-005
- **Input:** the existing `run_test.go` suite, unmodified.
- **Expected output:** `go build ./... && go test ./...` is green; all three existing tests (`TestSandboxReachesAllowlistedHostViaProxy`, `TestProxyBlocksNonAllowlistedHost`, `TestNetAllowlistParsing`) pass unchanged.
- **Edge cases:** none — this is the regression guard.

---

## Post-implementation verification

- [ ] All test cases above pass (TC-006 as a skip when runsc absent)
- [ ] No regressions in existing tests (TC-007)
- [ ] L5: `go test ./...` green on a box without runsc (gvisor test skipped)
- [ ] L6 (deferred / opportunistic): a real `runsc` run confirms no-network + proxy-only egress end-to-end

---

## Test framework notes

- Standard Go `testing`. Mirror the `requireBwrap`/`t.Skip` pattern for `requireRunsc`.
- The tier-dispatch seam and the gVisor spec/argv builder should be **unit-testable without
  runsc installed** (TC-002/003/004 inspect the produced spec, they do not execute it). Only the
  end-to-end execution test (TC-006) requires runsc and skips when absent.
- New tests go in a new file (`gvisor_test.go` or similar) — `run_test.go` is not to be edited.
- No mocking of vault/audit is required for these cases (they're tier-independent and already
  exercised by the existing suite with empty sockets).
