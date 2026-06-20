# Task 005: Apache-2.0 relicense follow-up — SPDX headers + push

**Status:** ✅ complete

## Context

Relicensed PolyForm Noncommercial → Apache-2.0 in commit `c78f2a6`.

Done in that commit:

- `LICENSE` (Apache-2.0 text), `NOTICE`
- README adoption sections
- `CONTRIBUTING.md` (DCO)
- `.github/FUNDING.yml` + `.github/dco.yml`
- PolyForm references fixed in `README.md`, `CLAUDE.md`, and ADR-001

## Remaining

a. ✅ **SPDX headers** — added `// SPDX-License-Identifier: Apache-2.0` as the first line of every
   first-party Go source file (`*.go`, including `_test.go`). 10 tracked files; `.claude/worktrees/`
   and `vendor/` skipped; no generated files. `go build ./...` and `gofmt -l` clean. Own commit.

b. ✅ **Push** — relicense pushed; commit `c78f2a6` (Apache-2.0 `LICENSE`/`NOTICE` + adoption
   docs) is on `origin/main` (`github.com/tkdtaylor/exec-sandbox`).

## Acceptance

- ✅ SPDX header (`// SPDX-License-Identifier: Apache-2.0`) on every first-party `.go` file.
- ✅ Relicense pushed to the remote (`c78f2a6` on `origin/main`).

## Notes

- ADR-001's license section was updated in place; a dedicated relicense ADR is **optional**.
