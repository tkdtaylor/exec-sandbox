# Task 018: Teardown + spec/diagram sync + no-NIC fitness function

**Status:** ⬜ backlog
**Branch:** `task/018-teardown-spec-diagram-sync`
**Spec:** [`docs/tasks/test-specs/018-teardown-spec-diagram-sync-test-spec.md`](../test-specs/018-teardown-spec-diagram-sync-test-spec.md)
**ADR:** ADR 010 D5 (host-side baseline stays; terminate the microVM + reclaim jailer chroot/cgroup at teardown), D6 (VMM-native snapshot OUT of scope). No new ADR — this closes the Tier-3 wiring and syncs the source-of-truth docs.

## Readiness

**READY once tasks 013, 014, 015, 016, 017 land** (the full Tier-3 wiring), and **coordinates with
task 009** (the `make fitness` umbrella). No open-question block of its own. This is the **closing**
task of the epic.

**Dependency position:** 013 → 014 → 015 → {016, 017} → **018**. The terminal node; depends on the
whole epic plus task 009's fitness umbrella.

## Problem

After tasks 013–017 the Firecracker tier boots, runs the payload, bridges egress, maps limits, and
mounts `/work`/FileRead — but three things close it out:

1. **Teardown (D5).** No firecracker-specific teardown exists: a run could leave a firecracker/jailer
   process, a jailer chroot/cgroup, or a bundle dir behind. The host-side snapshot baseline (ADR 009,
   `snapshot.go`) is host-side and tier-independent (`snapshot.go:16-18`) and stays **unchanged** — it
   resets the host work dir + `payload.sh` + the proxy credential map, not the guest. The
   firecracker teardown (VMM termination + jailer chroot/cgroup reclaim + bundle removal) layers onto
   the existing `defer cleanup()` / `defer baseline.teardown()` path (`run.go:83,116,131-133`), via
   the backend `cleanup` func.
2. **Spec + diagram sync.** SPEC.md still says "Tier 3 not yet implemented"; `diagrams.md` does not
   show the Firecracker tier. CLAUDE.md requires the spec + diagram to update in the same commit as
   the closing code.
3. **No-NIC fitness function (the microVM F-001).** ADR 010's Consequences call for a fitness function
   asserting the generated config has no `network-interface` key — the third enforcement point of the
   no-network invariant alongside `bwrapArgv` and `gvisorOCISpec`. Plus the F-002 analogue
   (credential-never-in-guest).

**VMM-native snapshot stays OUT of scope (D6)** — one-shot teardown only.

## Coordinate with task 009

Task 009 wires F-001/F-002/F-004 into per-rule `fitness-<id>` targets + a `fitness:` umbrella of the
block rules (positive + negative cases, one inspectable rule list). This task **adds two firecracker
rules to that same umbrella** — `fitness-no-nic` (microVM F-001) and `fitness-cred-not-in-guest`
(microVM F-002) — reusing the no-NIC helper (task 013) and the microVM leak-scan helper (task 014).
**Do not fork the runner**; extend task 009's umbrella rule list. If 018 lands before 009, stage the
two rules so 009's umbrella picks them up (note the ordering in the PR).

## Scope

- **Firecracker teardown (D5):** at run end, terminate the microVM and reclaim its jailer
  chroot/cgroup + per-run bundle dir, via the backend `cleanup` func on the existing `defer` path.
  Teardown must fire on **every** exit path (clean, non-zero, timeout, launch error) — no guest,
  chroot, or cgroup outlives the run. `snapshot.go` is **not modified**.
- **Spec sync:** rewrite SPEC.md Non-goals + the project-summary tier sentence in place — Tier-3
  Firecracker is wired (no-NIC + vsock-bridged egress, host-side baseline, jailer-launched), present
  tense, no future tense; VMM-native snapshot stays an explicit non-goal.
- **Diagram sync:** update `docs/architecture/diagrams.md` (date bump) to show the Firecracker tier
  behind the seam with its vsock-bridged egress (guest `/proxy.sock` shim → vsock → host
  `EgressProxy`).
- **`fitness-no-nic`** (microVM F-001): asserts the generated config carries no `network-interface`
  key; passes on current code, fails on a constructed NIC config; joins task 009's umbrella.
- **`fitness-cred-not-in-guest`** (microVM F-002): asserts the credential never crosses the vsock
  into the guest (guest env/args/stdout); passes on current code, fails on a constructed guest leak;
  joins the umbrella.
- **`docs/spec/fitness-functions.md`** gains the two firecracker enforcement points (extend F-001/
  F-002 `Where enforced today`, or two new rows) in the same commit — present tense, `active`, real
  `make fitness-no-nic` / `make fitness-cred-not-in-guest` check commands.

Out of scope: VMM-native snapshot/restore (D6); warm-pool reuse (D6); re-authoring task 009's
existing F-001..F-010 rules (reuse the umbrella). Do NOT modify `snapshot.go`.

## Verification plan

- **Highest level achievable: L5 (per ADR-010 decomposition).** A validation harness confirms no
  firecracker process / jailer cgroup / bundle dir survives a run (process table + filesystem), on
  every exit path. Requires `/dev/kvm` + firecracker + jailer; the `fitness-no-nic` config check and
  the leak-scan surface-build half run everywhere (L2/L3).
- **Harness command:** `make fitness` (umbrella incl. the two new rules) and
  `make fitness-no-nic fitness-cred-not-in-guest`;
  `go test -count=1 -run 'FirecrackerTeardown|NoNIC|CredNotInGuest|Jailer' ./...`;
  the teardown TCs under `/dev/kvm`; `go test -count=1 ./...`; `gofmt -l .`.
- **Runtime observation (L5/L3):** paste the post-run process-table + filesystem check showing no
  firecracker/jailer process, no jailer cgroup, no bundle dir survives — including the timeout and
  launch-error paths (TC-018-01/02/03); the `make fitness` closing line showing `fitness-no-nic` +
  `fitness-cred-not-in-guest` ran green in the umbrella (TC-018-09); the negative cases failing
  (TC-018-08/11); the diff showing `snapshot.go` unchanged + F-010 tests green (TC-018-04).
- **No ADR.** ADR 010 D5/D6 already specify the teardown scope and the snapshot deferral.

## Definition of done

- At teardown the microVM is terminated and its jailer chroot/cgroup + bundle dir reclaimed on every
  exit path (clean, non-zero, timeout, launch error) — no guest outlives the run.
- `snapshot.go` is byte-for-byte unchanged; the F-010 snapshot/restore tests still pass.
- SPEC.md Non-goals + project summary rewritten in place — Tier-3 wired, VMM snapshot still a
  non-goal; no future tense.
- `diagrams.md` shows the Firecracker tier + vsock egress; date bumped.
- `fitness-no-nic` passes positive, fails on a constructed NIC config, and joins the `make fitness`
  umbrella; `fitness-cred-not-in-guest` passes positive, fails on a constructed guest leak, and joins
  the umbrella.
- `fitness-functions.md` records the two Firecracker enforcement points (present tense, `active`,
  real check commands) in the same commit.
- `make fitness` green on the merged epic; `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE + recorded L5 evidence before promotion to ✅.
