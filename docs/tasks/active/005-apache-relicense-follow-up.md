# Task 005: Apache-2.0 relicense follow-up — SPDX headers + push

**Status:** 🟡 follow-up open

## Context

Relicensed PolyForm Noncommercial → Apache-2.0 in commit `c78f2a6`.

Done in that commit:

- `LICENSE` (Apache-2.0 text), `NOTICE`
- README adoption sections
- `CONTRIBUTING.md` (DCO)
- `.github/FUNDING.yml` + `.github/dco.yml`
- PolyForm references fixed in `README.md`, `CLAUDE.md`, and ADR-001

## Remaining

a. **SPDX headers** — add `// SPDX-License-Identifier: Apache-2.0` as the **first line** of every
   first-party Go source file (`*.go`). Skip generated/vendored files. Including `_test.go` files is
   optional but fine. Make this its **own commit**.

b. **Push** — push the relicense once public/private visibility is confirmed.

## Acceptance

- SPDX header (`// SPDX-License-Identifier: Apache-2.0`) on every first-party `.go` file.
- Relicense pushed to the remote.

## Notes

- ADR-001's license section was updated in place; a dedicated relicense ADR is **optional**.
