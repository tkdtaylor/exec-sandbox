# Test Spec 009: wire the fitness functions (Make targets + 3 new checks)

**Linked task:** [`docs/tasks/backlog/009-wire-fitness-functions.md`](../backlog/009-wire-fitness-functions.md)
**ADR:** none required — wiring existing declared invariants into runnable targets (no new design decision). If F-001/F-002/F-004's check *mechanism* turns out to need a non-obvious harness shape, note it in the task readiness section rather than minting an ADR.
**Written:** 2026-06-20

## Context for the test author

`docs/spec/fitness-functions.md` declares F-001…F-010 as the project's executable architectural
invariants, but the `Makefile` (`build`, `test`, `fmt`, `clean`) has **zero** `fitness-*` targets.
Today the block-severity invariants rest on (a) integration/unit tests and (b) "by construction"
notes — not on a dedicated, named, invokable fitness runner. Three checks do not exist as
automated assertions at all:

- **F-001** (no shared network) — `TestGvisorSpecHasNoSharedNetwork` (`gvisor_test.go:75`) asserts
  only the **gVisor** side. The **bwrap** presence/absence assertion (argv carries `--unshare-all`,
  omits `--share-net`) is exercised incidentally inside other tests (`output_caps_test.go:214`,
  `workdir_test.go:322`, etc.) but there is **no dedicated F-001 check** that builds a baseline
  `bwrapArgv` and asserts the invariant in isolation. This task creates that check.
- **F-002** (proxy-mode credential never in sandbox env/args/stdout) — no automated leak check
  exists; "by construction" only. This task creates one.
- **F-004** (`secrets_injected` exposes only an ≤8-char handle prefix) — `run.go` uses
  `prefix(handle, 8)` (`run.go:541`), but nothing asserts the bound. This task creates the
  assertion.

F-005…F-010 already have `go test -run …` check commands declared in the spec table (the columns
exist); this task **wraps those existing commands in `fitness-<id>` Make targets** — it does not
re-author them.

Severity gates the umbrella: F-001, F-002, F-004, F-005, F-006, F-007, F-008, F-009, F-010 are
`block` (9 rules). **F-003 is `warn`** (stdlib-only) — it gets its own `fitness-no-deps` target but
is **EXCLUDED** from the `fitness:` umbrella (the umbrella runs the 9 block rules only; a `warn`
rule must never fail the umbrella).

Ground truth to mirror:
- The existing bwrap no-share-net assertion idiom: build argv via `bwrapArgv(...)`, join, assert
  `strings.Contains(joined,"--unshare-all")` true and `strings.Contains(joined,"--share-net")`
  false (see `output_caps_test.go:214-218`).
- `prefix(s string, n int) string` returns `s` when `len(s) < n`, else `s[:n]` (`run.go:541-546`).
- `secrets_injected` entries are `{handle_prefix, delivery}` built with `prefix(handle, 8)`
  (`run.go:106`, `109`).
- Credentials live only in `EgressProxy.creds` and are injected at `handle()` via
  `out.Header.Set(...)` (`proxy.go:126`); `bwrap --clearenv` plus the `--setenv` env-pairs is the
  full set of env that reaches the sandbox (`run.go:327-332`).

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-009-01 | A `fitness-<id>` Make target exists for each of the 9 block-severity rules (F-001, F-002, F-004, F-005, F-006, F-007, F-008, F-009, F-010) and for the `warn` rule F-003 (`fitness-no-deps`); each target is invokable and runs its declared check command | TC-009-01, TC-009-02 | ✅ |
| REQ-009-02 | A `fitness:` umbrella target runs **exactly** the 9 block-severity rules and **excludes** F-003; the umbrella passes on current `main` and fails if any single block rule fails | TC-009-03, TC-009-04 | ✅ |
| REQ-009-03 | A **new** F-001 check (bwrap side) asserts the baseline `bwrapArgv` carries `--unshare-all` and omits `--share-net`; it passes on current code and fails when the invariant is violated (negative case) | TC-009-05 (positive), TC-009-06 (negative) | ✅ |
| REQ-009-04 | A **new** F-002 check asserts a loaded proxy-mode credential value never appears in the spawn argv, the sandbox env (`--setenv` pairs / `--clearenv`), the payload, or the returned `stdout`; it passes on current code and fails when a credential value is leaked into any of those surfaces (negative case) | TC-009-07 (positive), TC-009-08 (negative) | ✅ |
| REQ-009-05 | A **new** F-004 check asserts every `secrets_injected[].handle_prefix` is ≤ 8 chars and never equals the full handle (when the handle is > 8 chars) nor carries a credential; it passes on current code and fails when the prefix bound is exceeded (negative case) | TC-009-09 (positive), TC-009-10 (negative) | ✅ |
| REQ-009-06 | The F-005…F-010 `fitness-<id>` targets wrap the **already-declared** `go test -run …` commands from the spec table verbatim (no re-authoring of those checks); each target exits non-zero iff its underlying test set fails | TC-009-11 | ✅ |
| REQ-009-07 | The spec status for F-001, F-002, F-004 is flipped `proposed → active` in `docs/spec/fitness-functions.md` in the same commit as the wiring; the "no `make fitness` target exists yet" caveat (the spec's `How to run` blockquote) is removed/updated to reflect the now-wired targets | TC-009-12 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Negative (invariant-violating) cases are specified for each of the 3 new checks
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: F-003 is `warn` and excluded from the `fitness:` umbrella
- [x] Confirmed: F-005..F-010 targets wrap existing `go test -run` commands, not re-author them

---

## Test cases

### TC-009-01: every block-rule `fitness-<id>` target exists and is invokable

- **Requirement:** REQ-009-01
- **Type:** harness (make)
- **Input:** for each of `fitness-no-share-net` (F-001), `fitness-cred-not-in-sandbox` (F-002),
  `fitness-handle-prefix` (F-004), and the F-005..F-010 targets (names per the spec's check column,
  e.g. `fitness-limits`, `fitness-only-workdir`, `fitness-fileread-ro`, `fitness-output-cap`,
  `fitness-verb-allowlist`, `fitness-snapshot-restore`): run `make -n <target>` (dry run) and then
  `make <target>`.
- **Expected:** `make -n <target>` resolves (no "No rule to make target"); `make <target>` exits
  `0` on current `main`. Settle the exact target names in the task file's target table; the spec's
  `Check command` column for F-001/F-002/F-004 already names `fitness-no-share-net` /
  `fitness-cred-not-in-sandbox` / `fitness-handle-prefix`.

### TC-009-02: the `fitness-no-deps` (F-003, warn) target exists and is invokable

- **Requirement:** REQ-009-01
- **Type:** harness (make)
- **Input:** `make fitness-no-deps`.
- **Expected:** exits `0` on current `main` (`go.mod` has no `require` block). It is a standalone
  target — **not** invoked by the `fitness:` umbrella (see TC-009-04).

### TC-009-03: the `fitness:` umbrella runs all 9 block rules and passes on current main

- **Requirement:** REQ-009-02
- **Type:** harness (make)
- **Input:** `make fitness` on a clean checkout of current `main` (a host with `bwrap`; the
  F-006..F-010 integration tests need it — note any that skip without it).
- **Expected:** exits `0`; the output shows all 9 block rules ran (F-001, F-002, F-004, F-005,
  F-006, F-007, F-008, F-009, F-010). Capture the final closing line for the L3 evidence.

### TC-009-04: the umbrella excludes F-003 (warn) and does not fail on a warn-rule failure

- **Requirement:** REQ-009-02
- **Type:** harness (make) + inspection
- **Input:** inspect the `fitness:` target's prerequisite/recipe list; confirm it does **not**
  include `fitness-no-deps`. (Optionally, simulate an F-003 failure by adding a throwaway `require`
  line to a scratch `go.mod` copy and confirm `make fitness` still passes while
  `make fitness-no-deps` fails.)
- **Expected:** `fitness-no-deps` is absent from the umbrella's rule set; a `warn`-rule failure
  never fails the umbrella. The umbrella's rule set is **exactly** the 9 block IDs.

### TC-009-05: F-001 new check passes on current code (positive)

- **Requirement:** REQ-009-03
- **Type:** unit (Go test, the check the `fitness-no-share-net` target runs)
- **Input:** build the baseline argv via `bwrapArgv(scriptPath, proxySock, "", nil, nil, 0,
  finalCmd)` (the no-workdir/no-fileread/no-env/no-disk shape).
- **Expected:** the joined argv **contains** `--unshare-all` and does **not** contain
  `--share-net`. (Mirror the idiom at `output_caps_test.go:214-218`.) Optionally also assert the
  gVisor side by calling the existing `TestGvisorSpecHasNoSharedNetwork` path, so
  `fitness-no-share-net` covers **both** backends as F-001 declares.

### TC-009-06: F-001 new check fails when the invariant is violated (negative)

- **Requirement:** REQ-009-03
- **Type:** unit (Go test, negative)
- **Input:** a deliberately mutated argv (in-test) that either drops `--unshare-all` or appends
  `--share-net` — exercised through a small helper that the F-001 check function consumes, so the
  *assertion logic* is provably failing on a bad argv (not the real `bwrapArgv`, which is correct).
- **Expected:** the F-001 assertion function returns a non-nil error / the test using it would
  fail. This proves the check is not a no-op (it actually rejects a network-sharing argv).

### TC-009-07: F-002 new check passes on current code (positive)

- **Requirement:** REQ-009-04
- **Type:** unit/integration (Go test, the check the `fitness-cred-not-in-sandbox` target runs)
- **Input:** drive a run (or the argv+env build path) with a proxy-mode credential loaded onto the
  proxy via `SetCredential(host, Credential{Value:"SENTINEL-SECRET-abc123", …})`, a payload that
  echoes its env (`env` / `set`), and a sentinel value distinct from anything else in the request.
- **Expected:** the sentinel credential value appears in **none** of: the spawn argv
  (`bwrapArgv` slice), the `--setenv` env pairs, the payload script bytes, or the returned
  `result["stdout"]`. The credential lives only in `EgressProxy.creds` (`proxy.go`).
- **Notes:** if run end-to-end this needs `bwrap` (skip-guard like the other integration tests);
  the argv/env-surface half can run without `bwrap` (build argv, assert sentinel absent). At least
  the no-bwrap half must run everywhere so the check is meaningful on a bwrap-less CI.

### TC-009-08: F-002 new check fails when a credential value is leaked (negative)

- **Requirement:** REQ-009-04
- **Type:** unit (Go test, negative)
- **Input:** feed the F-002 leak-scan assertion a constructed surface set (argv / env / stdout)
  that **does** contain the sentinel credential value (simulating a regression that put the
  credential into `--setenv` or echoed it to stdout).
- **Expected:** the F-002 assertion returns a non-nil error / a test using it fails — proving the
  scan actually catches a leak and is not vacuous.

### TC-009-09: F-004 new check passes on current code (positive)

- **Requirement:** REQ-009-05
- **Type:** unit (Go test, the check the `fitness-handle-prefix` target runs)
- **Input:** a `secrets_injected` slice as produced by the inject loop for handles longer than 8
  chars (e.g. `vault://handle/abcdefghijklmnop`), built via `prefix(handle, 8)`.
- **Expected:** every `handle_prefix` has `len ≤ 8`, does **not** equal the full handle (for handles
  > 8 chars), and the entry carries no `credential`/`value` key — only `{handle_prefix, delivery}`.

### TC-009-10: F-004 new check fails when the prefix bound is exceeded (negative)

- **Requirement:** REQ-009-05
- **Type:** unit (Go test, negative)
- **Input:** a `secrets_injected` slice with an entry whose `handle_prefix` is the **full** handle
  (length > 8) — simulating a regression that dropped the `prefix(handle, 8)` truncation.
- **Expected:** the F-004 assertion returns a non-nil error / a test using it fails — proving the
  bound check actually rejects an over-length prefix.

### TC-009-11: F-005..F-010 targets wrap the existing test commands and propagate failure

- **Requirement:** REQ-009-06
- **Type:** harness (make) + inspection
- **Input:** inspect each F-005..F-010 `fitness-<id>` recipe; confirm it invokes the **same**
  `go test -run '<pattern>' ./...` command already declared in that rule's `Check command` column
  in `fitness-functions.md` (verbatim pattern). Confirm `make <target>` exits non-zero if that
  underlying test set is made to fail (e.g. point it at a deliberately-failing scratch test, or
  trust Go's exit semantics: `go test` exits non-zero on failure ⇒ `make` does too).
- **Expected:** each F-005..F-010 target's recipe is the spec's declared command (no re-authored
  check); failure of the underlying tests fails the target (and thus the umbrella).

### TC-009-12: spec status flipped to active for F-001/F-002/F-004; "not wired" caveat removed

- **Requirement:** REQ-009-07
- **Type:** inspection (spec)
- **Input:** read `docs/spec/fitness-functions.md` after the feat commit.
- **Expected:** F-001, F-002, F-004 rows show `Status: active` (matching F-005..F-010); their
  `Check command` columns reference the now-real `make fitness-*` targets (no `*(not yet wired)*`
  marker). The `How to run` blockquote no longer says "no `make fitness` target exists yet"; it
  describes the wired `make fitness` / `make fitness-<rule>` targets. The `Where enforced today`
  note for F-001 no longer says the bwrap absence assertion "is not yet wired"; for F-002 no longer
  "no automated leak check exists yet"; for F-004 no longer "no test asserts the bound."

---

## Post-implementation verification

- [ ] TC-009-01..02: all `fitness-<id>` + `fitness-no-deps` targets resolve and pass on `main`
- [ ] TC-009-03: `make fitness` passes; closing line captured for L3 evidence
- [ ] TC-009-04: umbrella excludes F-003; warn-rule failure does not fail the umbrella
- [ ] TC-009-05..10: the 3 new checks pass on current code AND fail on their negative cases
- [ ] TC-009-11: F-005..F-010 targets wrap the declared commands; failure propagates
- [ ] TC-009-12: spec rows flipped to active; "not wired" caveats removed

## Test framework notes

- Standard Go `testing` for the new F-001/F-002/F-004 check functions (live in a `fitness_test.go`
  or alongside the relevant module test file). The negative cases call the **assertion helper** on
  a constructed bad surface so the failing path is exercised without breaking the real code.
- Make targets are thin wrappers: each `fitness-<id>` recipe is a single `go test -run '<pattern>'
  ./...` (block rules) or `go list -m all`/`grep` style check (F-003 no-deps). The `fitness:`
  umbrella is a `.PHONY` target whose prerequisites are exactly the 9 block-rule targets.
- Keep the umbrella's rule list in **one place** so "exactly 9 block rules, F-003 excluded" is
  inspectable at a glance (TC-009-04).
