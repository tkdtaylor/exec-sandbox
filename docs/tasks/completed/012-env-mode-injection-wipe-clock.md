# Task 012: env-mode credential injection + wipe clock

**Status:** ⬜ backlog
**Branch:** `task/012-env-mode-injection-wipe-clock`
**Spec:** [`docs/tasks/test-specs/012-env-mode-injection-wipe-clock-test-spec.md`](../test-specs/012-env-mode-injection-wipe-clock-test-spec.md)
**ADR:** ADR 012 — **REQUIRED** (env-mode is a deliberate, documented exception to the proxy-mode "credential never enters the sandbox" rule; the wipe-clock point and the delivery mechanism must be recorded).

## Readiness

**Ready to execute after ADR 012 settles three points** (all resolvable from the existing code + the
vault contract — no cross-block *negotiation* needed, unlike task 011, but the vault env-mode
response field names must be confirmed against the vault contract):
1. **The wipe-clock point** — when the env credential is wiped (recommended: post-spawn / teardown).
2. **The env delivery mechanism** — must keep the value **out of the spawn argv** (so
   `/proc/<pid>/cmdline` can't leak it): `os/exec` `cmd.Env` or an env-file bind, **not** `--setenv`
   on the argv.
3. **The vault env-mode response field names** — the credential value + target env-var name fields
   (confirm against `data-model.md` §vault.inject and the vault block contract).

## Problem

Only **proxy-mode** injection is wired. In `Run()`'s inject loop a proxy-mode response loads the
credential onto the `EgressProxy` (`run.go:97-106`); an **env-mode** response is recorded as
`{handle_prefix, delivery:"env"}` (`run.go:107-110`) but the credential value is **never
delivered** to the sandbox — the `else` branch is accounting only. `configuration.md` confirms env
is *"recorded but not loaded onto the proxy in v0."* The README lists *"env-mode injection + wipe
clock"* as deferred v1 work. Some tools read a token from the environment rather than via an
injected header; env-mode is how exec-sandbox serves them — but the delivered value must be wiped
per a defined clock so it does not outlive its need.

## Scope

(Implementation begins after ADR 012 settles the wipe clock + delivery mechanism + vault field names.)

- **Implement env-mode delivery** in the inject loop's `else` branch (`run.go:107-110`): take the
  credential value + target env-var name from the env-mode `vault.inject` response and deliver it
  into the sandbox **environment** for the spawned payload, via a mechanism that keeps it **off the
  argv** (`cmd.Env` / env-file bind — **not** `--setenv`, which lands in `/proc/<pid>/cmdline`).
  The payload can then read it (e.g. `$API_TOKEN`).
- **Implement the wipe clock** (per ADR 012): the env credential is delivered at spawn and the
  host-side copy is wiped at the defined point (recommended: retain no host copy of the value past
  spawn; nothing survives teardown — mirrors the proxy `Wipe()`). Keep the host-side credential
  holder in one place so the wipe is a single, testable operation (a field cleared / buffer zeroed).
- **PRESERVE the proxy-mode invariant.** Proxy-mode (`delivery:"proxy"`) credentials still **never**
  enter the sandbox env/args/stdout (F-002, the `SPEC.md` top-level invariant, `CLAUDE.md`). This
  task does **not** weaken it. Env-mode and proxy-mode handles in the same run are handled
  independently (proxy → proxy edge; env → sandbox env + wipe).
- **Accounting unchanged in shape:** one `{handle_prefix, delivery:"env"}` per delivered env-mode
  handle (8-char `prefix(handle, 8)`, never the full handle, never the value). An env-mode inject
  failure emits `inject_failed`, skips the handle (no partial/empty env var), and the run continues
  — same as proxy-mode.
- **Keep the value out of result/audit/stdout** (beyond the deliberate sandbox-env delivery): the
  value never appears in the returned `result`, `sandbox_status`, or any audit event.
- **Spec + config update in the same commit:** `docs/spec/behaviors.md` B-003 (document env-mode
  delivery + the wipe clock alongside proxy-mode; make the env-vs-proxy distinction explicit —
  proxy never enters the sandbox, env deliberately does and is wiped per the clock);
  `docs/spec/data-model.md` (env-mode `vault.inject` response is now **delivered**, not just
  recorded; document the wipe clock; preserve the proxy-mode credential data-invariant and state the
  env-mode exception); `docs/spec/configuration.md` (`wiring.injection_mode` "`env`" row no longer
  says "recorded but not loaded in v0"). Rewrite in place, no future tense.

Out of scope: env-mode under **gVisor** if the delivery mechanism differs materially — if `cmd.Env`
covers both tiers (the child is `runsc`/`bwrap` either way) it is in scope; if gVisor needs a
distinct env path, note it as ADR 012's reopening condition and scope it to bwrap first. Also out of
scope: a TTL-based mid-run rotation if ADR 012 picks post-spawn wipe (the simpler clock);
multi-value env bundles per handle.

## Verification plan

- **Highest level achievable: L5/L6.** This host has `bwrap`, so env-mode delivery is observable
  end-to-end: a payload prints `$API_TOKEN` and the sentinel value appears in stdout; a mixed
  env+proxy run proves proxy stays out while env comes in. The wipe-clock and argv-absence are
  unit/integration-observable.
- **Harness command:** `go test -count=1 ./...`.
- **Runtime observation (L5/L6):** (1) a sandboxed payload reads the env-mode credential from its
  environment (sentinel in stdout); (2) the sentinel never appears in the spawn argv (no
  `/proc/cmdline` leak), the result, or audit; (3) after the wipe point exec-sandbox retains no
  host-side copy of the value; (4) a mixed run delivers env-mode in and keeps proxy-mode out
  (F-002 intact); (5) an env-mode failure emits `inject_failed`, skips the handle, delivers no env
  var, run continues.
- **ADR 012 written during implementation:** settles the wipe-clock point, the off-argv delivery
  mechanism, the vault env-mode response field names, the env-vs-proxy boundary (the documented
  exception to "credential never enters the sandbox"), and the gVisor scope boundary.

## Definition of done

- Env-mode delivers the credential into the sandbox environment (off the argv); a payload reads it.
- The wipe clock removes the host-side copy at the defined point; nothing survives teardown.
- The env-mode value never appears in the spawn argv, the result/sandbox_status, or any audit event.
- `secrets_injected` accounting correct (`{handle_prefix, delivery:"env"}`, 8-char prefix, no value).
- Env-mode failure skips the handle (no partial env var) and the run continues; `inject_failed`
  emitted.
- **Proxy-mode unchanged** — proxy-mode credentials still never enter the sandbox (F-002 intact);
  mixed runs handled independently.
- `docs/spec/behaviors.md` B-003 / `data-model.md` / `configuration.md` updated in place ("recorded
  but not loaded" removed) — in the feat commit, no future tense.
- ADR 012 written; `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE before promotion to ✅.
