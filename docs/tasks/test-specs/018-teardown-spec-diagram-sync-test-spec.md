# Test Spec 018: Teardown + spec/diagram sync + no-NIC & constraints-≥-jailer fitness functions

**Linked task:** [`docs/tasks/backlog/018-teardown-spec-diagram-sync.md`](../backlog/018-teardown-spec-diagram-sync.md)
**ADR:** ADR 010 D5 (host-side baseline stays; terminate the microVM + reclaim jailer chroot/cgroup at teardown), D6 (VMM-native snapshot OUT of scope). No new ADR required — this completes the Tier-3 wiring and syncs the source-of-truth docs.
**Written:** 2026-06-20

## Context for the test author

This is the **closing** task of the Firecracker epic. It does three things:

1. **Teardown.** At the end of a firecracker run, terminate the microVM and reclaim its jailer
   chroot/cgroup and the per-run bundle dir, so **no guest outlives the run** (ADR 010 D5). The
   host-side snapshot baseline (ADR 009, `snapshot.go`) is host-side and tier-independent and stays
   unchanged — it covers the host work dir, `payload.sh`, and the proxy credential map; it does NOT
   reach inside the guest. The firecracker-specific teardown is the VMM-process termination + jailer
   resource reclaim, layered onto the existing `defer cleanup()` / `defer baseline.teardown()` path
   in `Run()`.
2. **Spec + diagram sync.** Rewrite `docs/spec/SPEC.md` Non-goals (Tier-3 is now wired, not "not yet
   implemented") and update `docs/architecture/diagrams.md` to show the Firecracker tier behind the
   seam with its vsock-bridged egress — in the same commit as the closing code, per CLAUDE.md's
   spec-and-diagram-with-code rule.
3. **No-NIC + cred-not-in-guest + constraints-≥-jailer fitness functions.** Add the microVM analogue
   of F-001: a `fitness-no-nic` check (and a `fitness-cred-not-in-guest` analogue of F-002) asserting
   the generated Firecracker config contains no `network-interface` key and the credential never
   crosses the vsock into the guest; **plus** `fitness-constraints-ge-jailer` (ADR-010 Amendment 1
   A1.Q3) asserting the bwrap-wrapped firecracker launch's effective constraints are **≥ jailer**
   (non-host uid, all namespaces unshared, cgroup limits applied, chroot/`pivot_root` in effect; no
   jailer binary required). **Coordinate with task 009** (which wires F-001/F-002/F-004 into
   `make fitness`): the new firecracker rules join the same umbrella and reuse the no-NIC + leak-scan
   helpers (tasks 013/014) + the constraints inspection exercised in task 015 (TC-015-05).

**VMM-native snapshot stays OUT of scope** (ADR 010 D6) — this task does the one-shot teardown
only; native Firecracker snapshot/restore is a separate future decision.

Ground truth to mirror:
- `Run()` already defers `cleanup()` (the backend bundle cleanup) and `baseline.teardown()`
  (host work dir RemoveAll + `proxy.Wipe()`) — `run.go:83,116,131-133`. The firecracker teardown
  hooks into the backend's `cleanup` func (the `func()` returned from `Argv`, like gVisor's bundle
  RemoveAll at `gvisor.go:26`).
- `snapshot.go`'s baseline is explicitly host-side-only and does not reach inside a tier's kernel
  root (`snapshot.go:16-18`) — unchanged.
- The fitness wiring shape (per-rule `fitness-<id>` target + `fitness:` umbrella of block rules,
  positive + negative cases) is task 009's pattern — follow it for the two new firecracker rules.
- F-001 is "no shared network in any backend" (`fitness-functions.md` F-001) — the no-NIC rule is
  the Firecracker enforcement point of that same invariant; record it as such, not as a new
  invariant.

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-018-01 | At teardown the firecracker microVM is terminated and its jailer chroot/cgroup + per-run bundle dir are reclaimed — no guest process, chroot, or cgroup outlives the run; teardown runs even on an error/timeout exit path | TC-018-01, TC-018-02, TC-018-03 | ✅ |
| REQ-018-02 | The host-side snapshot baseline (ADR 009) is UNCHANGED — `snapshot.go` is not modified; the host work dir + payload.sh + proxy credential map reset is tier-independent and still applies | TC-018-04 | ✅ |
| REQ-018-03 | `docs/spec/SPEC.md` Non-goals is rewritten in place: Tier-3 Firecracker is wired (no-NIC + vsock-bridged egress, host-side baseline), not "not yet implemented"; no future tense; VMM-native snapshot remains an explicit non-goal | TC-018-05 | ✅ |
| REQ-018-04 | `docs/architecture/diagrams.md` is updated (with a date bump) to show the Firecracker tier behind the seam and its vsock-bridged proxy egress | TC-018-06 | ✅ |
| REQ-018-05 | A `fitness-no-nic` rule (microVM analogue of F-001) asserts the generated Firecracker config carries no `network-interface` key; it passes on current code and fails on a constructed NIC config; it joins the `make fitness` umbrella (coordinated with task 009) | TC-018-07 (positive), TC-018-08 (negative), TC-018-09 (umbrella) | ✅ |
| REQ-018-06 | A `fitness-cred-not-in-guest` rule (microVM analogue of F-002) asserts the credential value never crosses the vsock into the guest (guest env/args/stdout); passes on current code, fails on a constructed guest-leak; joins the umbrella. `docs/spec/fitness-functions.md` gains the firecracker rows (or extends F-001/F-002's "Where enforced today" with the microVM point) in the same commit | TC-018-10 (positive), TC-018-11 (negative), TC-018-12 (spec) | ✅ |
| REQ-018-07 | A `fitness-constraints-ge-jailer` rule (microVM, ADR-010 Amendment 1 A1.Q3) asserts the Tier-3 launch's effective constraints are **≥ jailer** — non-host uid, all namespaces unshared (none shared with host), cgroup limits applied, chroot/`pivot_root` in effect, no jailer binary; passes on the bwrap-wrapped firecracker launch, fails on a constructed launch that weakens a constraint; joins the umbrella; `docs/spec/fitness-functions.md` gains a row for it (present tense, `active`, real `make fitness-constraints-ge-jailer`) in the same commit | TC-018-13 (positive), TC-018-14 (negative), TC-018-15 (umbrella + spec) | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] The teardown-on-error-path case is specified (no guest leaks even on timeout/error exit)
- [x] The three new fitness rules (no-NIC, cred-not-in-guest, constraints-≥-jailer) have positive AND negative cases (provably not no-ops)
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: VMM-native snapshot is OUT of scope (D6) — one-shot teardown only
- [x] Confirmed: coordinate the new fitness rules with task 009's umbrella (do not fork the runner)
- [x] Target verification level: L5 (validation harness: after a run, no firecracker process / jailer cgroup / bundle dir survives — observed via process table + filesystem) — guest-side checks need `/dev/kvm`; the config-level no-NIC fitness check runs everywhere

---

## Test cases

### TC-018-01: the microVM process is terminated at teardown

- **Requirement:** REQ-018-01
- **Type:** integration (Go test) — target L5, requires `/dev/kvm` + firecracker + jailer
- **Input:** a firecracker run; capture the firecracker/jailer PID(s); after `Run()` returns,
  inspect the process table.
- **Expected:** no firecracker or jailer process from this run survives — the VMM is terminated and
  reaped. Skip-guard when prerequisites absent.

### TC-018-02: the jailer chroot/cgroup + bundle dir are reclaimed

- **Requirement:** REQ-018-01
- **Type:** integration (Go test) — target L5
- **Input:** a firecracker run; note the jailer chroot path, cgroup path, and per-run bundle dir;
  after `Run()` returns, stat them.
- **Expected:** the bundle dir is removed (the backend `cleanup` func ran), and the jailer
  chroot/cgroup is reclaimed — no per-run jailer resource leaks. Mirrors the gVisor bundle removal
  (`gvisor.go:26`) plus the jailer-specific reclaim.

### TC-018-03: teardown runs on the error/timeout exit path too

- **Requirement:** REQ-018-01
- **Type:** integration (Go test) — target L5
- **Input:** a firecracker run with `timeout_sec = 1` and a `sleep 30` payload (forces the
  timeout-kill path), and separately a run that errors during launch.
- **Expected:** in both cases no firecracker/jailer process and no bundle dir survives — teardown is
  on a `defer`, so it fires regardless of how the run exits (clean, non-zero, timeout, launch
  error). The microVM never outlives the run even on the failure paths.

### TC-018-04: snapshot.go (host-side baseline) is unchanged

- **Requirement:** REQ-018-02
- **Type:** inspection + unit (Go test)
- **Input:** diff `snapshot.go` against the pre-task baseline; run the F-010 snapshot/restore tests.
- **Expected:** `snapshot.go` is byte-for-byte unchanged; the F-010 tests still pass. The
  firecracker teardown is additive (in the backend `cleanup`), not a modification of the
  tier-independent host-side baseline.

### TC-018-05: SPEC.md Non-goals rewritten — Tier-3 is wired

- **Requirement:** REQ-018-03
- **Type:** inspection (spec)
- **Input:** read `docs/spec/SPEC.md` Non-goals + the project-summary tier sentence after the feat
  commit.
- **Expected:** the "Tier 3 not yet implemented — `firecracker` … returns `tier not implemented`"
  bullet is rewritten in place to state Tier-3 Firecracker is wired (no-NIC + vsock-bridged egress,
  host-side baseline reset, jailer-launched), present tense, no future tense. VMM-native
  snapshot/restore remains listed as an explicit non-goal (D6).

### TC-018-06: diagrams.md shows the Firecracker tier + vsock egress

- **Requirement:** REQ-018-04
- **Type:** inspection (diagram)
- **Input:** read `docs/architecture/diagrams.md` after the feat commit.
- **Expected:** the tier-seam diagram includes the Firecracker (Tier-3) backend with its
  vsock-bridged proxy egress (no NIC); the date bump at the top of the file is updated. The egress
  flow shows the guest `/proxy.sock` shim → vsock → host `EgressProxy` (the only path out).

### TC-018-07: fitness-no-nic passes on current code (positive)

- **Requirement:** REQ-018-05
- **Type:** unit (Go test, the check `fitness-no-nic` runs)
- **Input:** build the firecracker config via `firecrackerConfig(...)` (the task-013 generator),
  serialize, scan.
- **Expected:** no `network-interface`/`network-interfaces` key — the check passes on current code.
  Reuses the no-NIC helper authored in task 013.

### TC-018-08: fitness-no-nic fails on a constructed NIC config (negative)

- **Requirement:** REQ-018-05
- **Type:** unit (Go test, negative)
- **Input:** feed the no-NIC check a config mutated to include a `network-interfaces` entry.
- **Expected:** the check returns a non-nil error / fails — proving it is not a no-op (catches a
  NIC). Mirrors task 009's F-001 negative case.

### TC-018-09: fitness-no-nic joins the make fitness umbrella

- **Requirement:** REQ-018-05
- **Type:** harness (make) + inspection
- **Input:** inspect the `fitness:` umbrella rule list; run `make fitness`.
- **Expected:** `fitness-no-nic` is a prerequisite of the umbrella (coordinated with task 009's
  block-rule set); `make fitness` runs it and stays green on current code. The umbrella's rule list
  is updated in the one inspectable place task 009 established.

### TC-018-10: fitness-cred-not-in-guest passes on current code (positive)

- **Requirement:** REQ-018-06
- **Type:** unit/integration (Go test)
- **Input:** drive the firecracker egress path (or the surface-build path) with a proxy-mode
  credential loaded; scan the guest-surface set (guest env/args/stdout) + the host surfaces.
- **Expected:** the sentinel credential value appears in none of the guest or host surfaces — the
  credential is injected host-side after the vsock hop (reuses the task-014 microVM leak-scan
  helper). The no-`/dev/kvm` half (surface-build) runs everywhere.

### TC-018-11: fitness-cred-not-in-guest fails on a constructed guest leak (negative)

- **Requirement:** REQ-018-06
- **Type:** unit (Go test, negative)
- **Input:** feed the leak-scan a guest-surface set that DOES contain the sentinel value.
- **Expected:** the check returns a non-nil error / fails — proving it catches a credential that
  crossed into the guest. Mirrors task 009's F-002 negative case.

### TC-018-12: fitness-functions.md gains the microVM enforcement points

- **Requirement:** REQ-018-06
- **Type:** inspection (spec)
- **Input:** read `docs/spec/fitness-functions.md` after the feat commit.
- **Expected:** F-001's and F-002's `Where enforced today` notes (or new firecracker rows)
  record the Firecracker enforcement point — no `network-interface` in the generated config
  (F-001 / `fitness-no-nic`) and credential-never-in-guest over vsock (F-002 /
  `fitness-cred-not-in-guest`). Present tense; the rules are `active`; check commands name the real
  `make fitness-no-nic` / `make fitness-cred-not-in-guest` targets.

### TC-018-13: fitness-constraints-ge-jailer passes on the bwrap-wrapped firecracker launch (positive)

- **Requirement:** REQ-018-07
- **Type:** unit/integration (Go test)
- **Input:** build the Tier-3 launch (the `firecrackerBackend.Argv` from task 015 — direct
  `firecracker` under `bwrap --unshare-all` + `limits.go`, no jailer) and inspect its effective
  constraints (argv flags + the surface the task-015 constraints inspection checks).
- **Expected:** the launch satisfies **all** of: non-host uid, all namespaces unshared (mnt/pid/ipc/
  **net**/user — none shared with the host), cgroup limits applied, chroot/`pivot_root` in effect, and
  **no `jailer`** in the argv. The check passes on current code. Reuses the constraints inspection
  exercised in task 015 (TC-015-05). The config-level half runs everywhere; the live-process half
  skip-guards on `/dev/kvm`.

### TC-018-14: fitness-constraints-ge-jailer fails on a weakened launch (negative)

- **Requirement:** REQ-018-07
- **Type:** unit (Go test, negative)
- **Input:** feed the check a launch mutated to weaken a single constraint — e.g. a namespace shared
  with the host (drop `--unshare-net` or `--unshare-pid`), the host uid retained, or a cgroup limit
  omitted.
- **Expected:** the check returns a non-nil error / fails for each weakening — proving it is not a
  no-op and genuinely enforces **≥ jailer**. Mirrors task 009's F-001/F-002 negative-case shape.

### TC-018-15: fitness-constraints-ge-jailer joins the umbrella + the spec records it

- **Requirement:** REQ-018-07
- **Type:** harness (make) + inspection (spec)
- **Input:** inspect the `fitness:` umbrella rule list and run `make fitness`; read
  `docs/spec/fitness-functions.md` after the feat commit.
- **Expected:** `fitness-constraints-ge-jailer` is a prerequisite of the umbrella (coordinated with
  task 009's block-rule set); `make fitness` runs it and stays green on current code.
  `fitness-functions.md` gains a row for it — present tense, `active`, check command names the real
  `make fitness-constraints-ge-jailer` target, source linked to ADR-010 Amendment 1 A1.Q3.

---

## Post-implementation verification

- [ ] TC-018-01..03: no microVM/jailer/bundle survives a run — including timeout + launch-error paths (L5)
- [ ] TC-018-04: `snapshot.go` unchanged; F-010 tests green
- [ ] TC-018-05: SPEC.md Non-goals rewritten — Tier-3 wired, VMM snapshot still a non-goal
- [ ] TC-018-06: diagrams.md shows Firecracker + vsock egress; date bumped
- [ ] TC-018-07..09: `fitness-no-nic` passes positive, fails negative, joins the umbrella
- [ ] TC-018-10..12: `fitness-cred-not-in-guest` passes positive, fails negative; fitness spec updated
- [ ] TC-018-13..15: `fitness-constraints-ge-jailer` passes positive, fails negative, joins the umbrella; fitness spec gains its row

## Test framework notes

- Standard Go `testing` + `make`. The teardown process/cgroup tests (TC-018-01/02/03) need
  `/dev/kvm` + firecracker + jailer and MUST skip-guard when absent. The `fitness-no-nic` config
  check (TC-018-07/08/09) runs everywhere (pure config). The leak-scan's surface-build half runs
  everywhere; the live half skip-guards.
- Reuse the no-NIC helper (task 013), the microVM leak-scan helper (task 014), and the constraints
  inspection exercised in task 015 (TC-015-05) — do NOT re-author them. Reuse task 009's `fitness:`
  umbrella — add the three firecracker rules (no-NIC, cred-not-in-guest, constraints-≥-jailer) to its
  rule list, do not fork the runner.
- **Depends on tasks 013, 014, 015, 016, 017 landing first (the full Tier-3 wiring), and coordinates
  with task 009 (the fitness umbrella).** Mark the coverage row `❌ planned (not started)`.
