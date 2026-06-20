# Task 009: wire the fitness functions (Make targets + 3 new block-rule checks)

**Status:** ⬜ backlog
**Branch:** `task/009-wire-fitness-functions`
**Spec:** [`docs/tasks/test-specs/009-wire-fitness-functions-test-spec.md`](../test-specs/009-wire-fitness-functions-test-spec.md)
**ADR:** none required — this wires already-declared invariants (F-001…F-010 in `docs/spec/fitness-functions.md`) into runnable targets; no new design decision. (If the F-002 leak-scan needs a non-obvious harness shape, record the rationale in the spec note, not a fresh ADR.)

## Problem

`docs/spec/fitness-functions.md` declares F-001…F-010 as the project's **executable architectural
invariants**, but the `Makefile` has **zero** `fitness-*` targets — only `build`, `test`, `fmt`,
`clean`. The block-severity security invariants therefore rest on integration/unit tests plus
"by construction" notes, not on a dedicated, named, invokable fitness runner. The spec's own
`How to run` section admits this: *"no `make fitness` target exists yet … wiring them … is itself
a task."* This is that task.

Three block-severity checks do not exist as automated assertions and must be **authored**:

- **F-001 (no shared network), bwrap side.** `TestGvisorSpecHasNoSharedNetwork` covers only gVisor.
  The bwrap presence/absence assertion (argv carries `--unshare-all`, omits `--share-net`) is only
  exercised incidentally inside other tests (`output_caps_test.go:214`, `workdir_test.go:322`).
  There is no dedicated F-001 check that builds a baseline `bwrapArgv` and asserts the invariant in
  isolation — create one (and have `fitness-no-share-net` cover **both** backends as F-001 declares).
- **F-002 (credential never in sandbox env/args/stdout).** No automated leak check exists — create
  a scan that asserts a loaded proxy-mode credential value appears in none of: the spawn argv, the
  `--setenv` env pairs, the payload, or the returned `stdout`.
- **F-004 (`secrets_injected` ≤ 8-char handle prefix).** `run.go` uses `prefix(handle, 8)`
  (`run.go:541`) but nothing asserts the bound — create the assertion.

F-005…F-010 already have `go test -run …` check commands declared in the spec table; this task
**wraps** those existing commands in `fitness-<id>` targets (no re-authoring).

## Scope

- **Add a `fitness-<id>` Make target per block rule** plus the warn-rule target:

  | Target | Rule | Severity | Recipe (in the umbrella?) |
  |--------|------|----------|----------------------------|
  | `fitness-no-share-net` | F-001 | block | NEW check (bwrap + gVisor no-share-net) — yes |
  | `fitness-cred-not-in-sandbox` | F-002 | block | NEW leak-scan check — yes |
  | `fitness-no-deps` | F-003 | **warn** | stdlib-only check — **NO (excluded from umbrella)** |
  | `fitness-handle-prefix` | F-004 | block | NEW ≤8-char prefix-bound check — yes |
  | `fitness-limits` | F-005 | block | wraps `go test -run 'Limit\|Timeout\|CPUAffinity\|DiskQuota' ./...` — yes |
  | `fitness-only-workdir` | F-006 | block | wraps `go test -run 'Workdir\|OnlyWorkdir' ./...` — yes |
  | `fitness-fileread-ro` | F-007 | block | wraps `go test -run 'FileRead' ./...` — yes |
  | `fitness-output-cap` | F-008 | block | wraps `go test -run 'OutputCap\|CapWriter\|MaxOutputBytes\|OutputTruncated\|NoOutputCap' ./...` — yes |
  | `fitness-verb-allowlist` | F-009 | block | wraps `go test -run 'Verb\|NetVerb\|BlockedByMethod\|DisallowedVerb\|AllowedVerb\|HostCheckPrecedes' ./...` — yes |
  | `fitness-snapshot-restore` | F-010 | block | wraps `go test -run 'Snapshot\|Restore\|Baseline\|Leak\|OneShot\|SecondRun' ./...` — yes |

  (Confirm the exact `go test -run` patterns against the current `fitness-functions.md` table at
  implementation time — copy them verbatim from the spec's `Check command` column.)
- **Add a `fitness:` umbrella target** whose prerequisites are **exactly** the 9 block-severity
  targets (F-001, F-002, F-004, F-005, F-006, F-007, F-008, F-009, F-010). **F-003 is `warn` and
  MUST be excluded** — a warn rule must never fail the umbrella. Keep the umbrella's rule list in
  one inspectable place.
- **Author the 3 new block checks** (F-001 bwrap-side, F-002, F-004) as Go test functions (e.g. in
  `fitness_test.go`), each with a positive case (passes on current code) and a **negative case**
  (the assertion helper rejects a constructed invariant-violating surface — so the check is provably
  not a no-op). The `fitness-<id>` target runs the matching `go test -run` pattern.
- **Wrap F-005..F-010's existing commands** verbatim — do not re-author those checks.
- **Spec update in the same commit (this is the implementation task — the spec flip lands with the
  feat work):** in `docs/spec/fitness-functions.md`, flip **F-001, F-002, F-004** from
  `Status: proposed` → `active`; update their `Check command` columns to the now-real
  `make fitness-*` targets (drop the `*(not yet wired)*` markers); update the `Where enforced today`
  notes (F-001: bwrap absence assertion now wired; F-002: leak check now exists; F-004: bound now
  asserted); and rewrite the `How to run` blockquote so it no longer says "no `make fitness` target
  exists yet" — describe the wired `make fitness` / `make fitness-<rule>` targets. Rewrite in place,
  no future tense.

Out of scope: changing any invariant's *semantics* (this only makes existing invariants runnable);
adding F-003 to the umbrella; CI wiring of `make fitness` (a follow-on — note it as a reopening
condition); the `strict`-profile Stop-hook integration mentioned in the spec (separate concern).

## Verification plan

- **Highest level achievable: L3 (fitness) + L2 (unit), with L5/L6 reachable.** This host has
  `bwrap`, so `make fitness` exercises the F-006..F-010 integration tests live; the 3 new checks are
  unit-testable (L2) with negative cases proving they bite. The load-bearing evidence is **L3**:
  `make fitness` green on current `main` with all 9 block rules running.
- **Harness command:** `make fitness` (umbrella) and `make fitness-no-share-net
  fitness-cred-not-in-sandbox fitness-handle-prefix` (the 3 new targets) and `go test -count=1 ./...`.
- **Runtime observation (L3/L5):** paste the closing line of `make fitness` showing the umbrella
  exited `0` with the 9 block rules run; paste the `ok github.com/tkdtaylor/exec-sandbox` line for
  the new-check test run. Demonstrate each new check's negative case fails (the assertion helper
  returns non-nil on a bad argv / a leaked credential / an over-length prefix). Confirm
  `make fitness` does **not** invoke `fitness-no-deps` (inspect the umbrella rule list).
- **No ADR.** If the F-002 leak-scan's surface set turns out to need a design call (e.g. scanning
  stdout from a live run vs. just the build-time surfaces), record the rationale in the
  `fitness-functions.md` F-002 note, not a new ADR.

## Definition of done

- A `fitness-<id>` target exists for each of the 9 block rules + `fitness-no-deps` (F-003); each is
  invokable and passes on current `main`.
- The `fitness:` umbrella runs **exactly** the 9 block rules and **excludes** F-003.
- The 3 new checks (F-001 bwrap, F-002, F-004) pass on current code and **fail** on their negative
  cases (proven not to be no-ops).
- F-005..F-010 targets wrap the spec's already-declared `go test -run` commands verbatim; failure
  propagates through the target and the umbrella.
- `docs/spec/fitness-functions.md`: F-001/F-002/F-004 flipped to `active`; check-command and
  `Where enforced today` columns updated; the `How to run` "not wired" caveat removed — all in the
  feat commit, rewritten in place, no future tense.
- `make fitness` green on `main`; `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE before promotion to ✅.
