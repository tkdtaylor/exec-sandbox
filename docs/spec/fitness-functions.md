# Fitness functions

**Project:** exec-sandbox
**Last updated:** 2026-06-18

## What this file is

Fitness functions are **executable architectural invariants** — automated checks that verify
the code still obeys the rules this project commits to. This file is the **declarative spec**
for those checks; the implementation lives in the runner the rules point to.

## Why this is separate from the rest of the spec

| Mechanism | What it guards | When it runs |
|-----------|---------------|--------------|
| `spec-coverage-check` hook | Active task's TC markers must have test references before commit | Pre-commit |
| `architect` drift-audit mode | Spec docs and diagrams still describe what the code does | On demand, periodically |
| **Fitness functions (this file)** | **Architectural invariants the code must always satisfy** | **Continuously — `make fitness` (when wired), also at Stop in `strict` profile** |

## How to run

> **Status:** no `make fitness` target exists yet. The Makefile currently has `build`, `test`,
> `fmt`, `clean`. The rules below are **proposed** — wiring them (adding `fitness` and
> `fitness-<rule>` targets) is itself a task. Until then, the invariants are enforced by code
> structure and by the integration tests, not by a dedicated fitness runner.

```bash
make fitness          # (proposed) run all fitness functions
make fitness-<rule>   # (proposed) run one rule by name
```

## Rules

> All rows are `proposed` until the user confirms and a `make fitness-<rule>` target is wired.
> Each points to where the invariant is (or is not yet) enforced today.

| ID | Rule | Category | Asserts | Threshold | Check command | Severity | Status | Where enforced today |
|----|------|----------|---------|-----------|---------------|----------|--------|----------------------|
| F-001 | No shared network in any backend | security | No backend grants the sandbox a network namespace: the bwrap argv always carries `--unshare-all` and omits `--share-net`; the gVisor OCI spec declares an empty `network` namespace (no path) and `runsc` runs with `--network=none` | 0 violations | `make fitness-no-share-net` *(not yet wired)* | block | proposed | `run.go` `bwrapArgv` hard-codes `--unshare-all`; `gvisor.go` `gvisorOCISpec` declares an empty netns and the argv adds `--network=none`. `TestGvisorSpecHasNoSharedNetwork` asserts the gVisor side; the bwrap *absence* assertion is not yet wired |
| F-002 | Proxy-mode credential never appears in sandbox env/args/stdout | security | A loaded credential value is never placed into the bwrap argv, the sandbox env, the payload, or the returned `stdout` | 0 leaks | `make fitness-cred-not-in-sandbox` *(not yet wired)* | block | proposed | By construction: credentials live only in `EgressProxy.creds` (`proxy.go`) and are injected at the proxy edge; no automated leak check exists yet |
| F-003 | Stdlib-only (no third-party Go dependencies) | structural | `go.mod` declares no `require` block / external modules | 0 deps | `make fitness-no-deps` *(not yet wired)* | warn | proposed | `go.mod` currently has only the module + `go` directive; any new dep must pass dep-scan |
| F-004 | `secrets_injected` exposes only an 8-char handle prefix | security | The result never carries a full secret handle or credential | prefix ≤ 8 chars | `make fitness-handle-prefix` *(not yet wired)* | block | proposed | `run.go` `prefix(handle, 8)`; no test asserts the bound |

Categories: `structural`, `hygiene`, `performance`, `complexity`, `security`, `coverage`.

Severity: `block` (fails the runner) / `warn` (surfaces but does not fail).

## Rules considered but rejected

| Proposed rule | Why rejected |
|---------------|--------------|
| *(none yet)* | — |

## Source-of-truth links

- F-001 ← [SPEC.md](SPEC.md) top-level invariant "No network in the sandbox"; ADR-001 D3; [behaviors.md](behaviors.md) B-001/B-002.
- F-002 ← [SPEC.md](SPEC.md) invariant "credential value never enters the sandbox"; ADR-001 D5; [behaviors.md](behaviors.md) B-003.
- F-003 ← ADR-001 D1 (stdlib-only).
- F-004 ← [data-model.md](data-model.md) data invariants.

## Notes

- F-001 and F-002 are the two seed rules the adoption flow called out: they encode the
  load-bearing security model and should be the first to get real `make fitness-*` targets.
- Rules here are the *project's* commitments, not generic best practices.
- Fitness functions should fail fast and have low false-positive rates.
