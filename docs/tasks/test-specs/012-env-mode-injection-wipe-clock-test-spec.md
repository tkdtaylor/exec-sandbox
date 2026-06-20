# Test Spec 012: env-mode credential injection + wipe clock

**Linked task:** [`docs/tasks/backlog/012-env-mode-injection-wipe-clock.md`](../backlog/012-env-mode-injection-wipe-clock.md)
**ADR:** ADR 012 (**required** — env-mode deliberately delivers a credential value *into* the sandbox, a documented exception to the proxy-mode "never enters the sandbox" rule; the wipe-clock point and the env-vs-proxy boundary must be recorded).
**Written:** 2026-06-20

## Context for the test author

Only **proxy-mode** injection is wired today. In `Run()`'s inject loop, a proxy-mode response loads
the credential onto the `EgressProxy` and records `{handle_prefix, delivery:"proxy"}`
(`run.go:97-106`); an **env-mode** response is recorded as `{handle_prefix, delivery:"env"}`
(`run.go:107-110`) but the credential value is **never delivered** to the sandbox — the `else`
branch only does accounting. `configuration.md` confirms: *"`env` is recorded but not loaded onto
the proxy in v0."* The README lists *"env-mode injection + wipe clock"* as deferred v1 work
(`README.md:47`).

This task implements **env-mode delivery** (deliver the credential to the sandbox via the
environment) **plus a wipe clock** (the credential is wiped from the env after a defined point).

**CRITICAL invariant — env-mode is DISTINCT from proxy-mode:**
- **Proxy-mode** (`delivery:"proxy"`) keeps the credential value **out** of the sandbox entirely —
  it lives only on the host-side proxy at the injection edge. **This rule still holds unchanged for
  proxy-mode** (F-002; the top-level invariant in `SPEC.md`; `CLAUDE.md` "Never let a proxy-mode
  credential value reach the sandbox"). This task must **not** weaken it.
- **Env-mode** (`delivery:"env"`) **deliberately delivers** the credential value into the sandbox
  via an environment variable — that is its entire purpose (some tools read a token from env, not an
  injected header). But it **must be wiped** per the defined clock so the value does not outlive its
  need.

**Wipe-clock — define the point (settle in ADR 012):** the credential env var is delivered into the
sandbox at spawn and must be removed at a defined moment. Options to settle:
- **post-spawn wipe** — the host-side env material (and any host copy of the value) is wiped from
  exec-sandbox's memory immediately after the child is spawned with it (the child has it; the host
  no longer retains it), and is gone by teardown.
- **TTL** — the value is available to the payload for a bounded window.

The recommended v1 clock is **post-spawn / teardown wipe**: deliver to the child's env at spawn,
retain no host-side copy of the value past spawn, and ensure nothing in the result/audit/host
outlives the run (mirrors the proxy `Wipe()` at teardown). ADR 012 fixes the exact point; the tests
assert whatever ADR 012 settles.

Ground truth:
- bwrap exports env via `--setenv k v` pairs built by `envSetenvPairs` from `run.env`
  (`run.go:330-332`, `355-373`); gVisor via `applyEnvToOCISpec`/`envList`. The env-mode credential
  must reach the sandbox by the **same** env mechanism but **must not** be a plain `run.env` entry
  the caller controls (the value comes from `vault.inject`, not the request). It must also never
  appear in the spawn argv where another process could read it via `/proc/<pid>/cmdline` — settle
  the delivery mechanism (env file / `os/exec` `cmd.Env` vs `--setenv`) in ADR 012 with the
  argv-visibility constraint in mind.
- `vault.inject` env-mode response shape: `{delivery:"env", …}` (`data-model.md` §vault.inject) —
  the credential value + the target env var name come from vault; confirm the exact field names
  against the vault contract during implementation.
- `secrets_injected` accounting: `{handle_prefix, delivery}` (`run.go:105-110`).

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-012-01 | An env-mode `vault.inject` response delivers the credential value into the sandbox process environment under the vault-specified env var name; a payload can read it from its environment | TC-012-01 | ✅ |
| REQ-012-02 | The wipe clock removes the credential at the defined point (per ADR 012 — post-spawn/teardown): exec-sandbox retains no host-side copy of the env credential value past the wipe point, and it is gone by teardown | TC-012-02 | ✅ |
| REQ-012-03 | The env-mode credential value never appears in the spawn **argv** (where `/proc/<pid>/cmdline` would expose it) nor in the returned `stdout`/`sandbox_status` nor in any audit event — only `{handle_prefix, delivery:"env"}` is recorded | TC-012-03 | ✅ |
| REQ-012-04 | `secrets_injected` accounting is correct for env-mode: one `{handle_prefix, delivery:"env"}` entry per delivered env-mode handle (8-char prefix, never the full handle, never the value) | TC-012-04 | ✅ |
| REQ-012-05 | **Proxy-mode behavior is unchanged:** a proxy-mode credential still never enters the sandbox env/args/stdout (the F-002 invariant holds); env-mode and proxy-mode handles in the **same** run are handled independently (proxy → proxy, env → env) | TC-012-05 | ✅ |
| REQ-012-06 | An env-mode inject failure (error/`error` field) emits `inject_failed` and skips the handle exactly as proxy-mode does (the run is not aborted); no partial/empty env var is delivered for a failed handle | TC-012-06 | ✅ |
| REQ-012-07 | Spec + config updated: `docs/spec/behaviors.md` B-003, `data-model.md` (vault.inject env-mode response now delivered; the wipe clock), `configuration.md` (`wiring.injection_mode` "`env`" row no longer says "recorded but not loaded") reflect the implemented env-mode + wipe clock | TC-012-07 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] The wipe-clock point is asserted
- [x] Proxy-mode unchanged (F-002) is asserted
- [x] The env-mode value's absence from argv (/proc/cmdline) is asserted
- [x] Every REQ-ID has at least one test case
- [ ] **BLOCKER:** ADR 012 written settling (a) the wipe-clock point, (b) the env delivery mechanism
      that keeps the value out of the argv, (c) the vault env-mode response field names — ticked
      before implementation

---

## Test cases

### TC-012-01: env-mode delivers the credential to the sandbox process env

- **Requirement:** REQ-012-01
- **Type:** integration (bwrap) against a stub vault socket returning an env-mode response
- **Input:** a run with one `secret_ref`, `injection_mode:"env"`, a stub vault returning
  `{delivery:"env", credential:"SENTINEL-ENV-VALUE", <env-var-name field>:"API_TOKEN"}` (confirm
  field names against the vault contract); a payload that prints its env (e.g.
  `printf '%s' "$API_TOKEN"`).
- **Expected:** the payload's stdout contains `SENTINEL-ENV-VALUE` — the credential reached the
  sandbox under `API_TOKEN`. (This is the deliberate, env-mode-only delivery.)

### TC-012-02: the wipe clock removes the credential at the defined point

- **Requirement:** REQ-012-02
- **Type:** unit/integration
- **Input:** the same env-mode run; inspect exec-sandbox's host-side state after the wipe point
  (per ADR 012 — e.g. after spawn / at teardown): any host-side buffer/struct holding the env
  credential value, and the proxy/teardown state.
- **Expected:** after the wipe point, exec-sandbox retains **no** host-side copy of
  `SENTINEL-ENV-VALUE` (the field/buffer is cleared or never persisted past spawn); by teardown the
  value is gone everywhere on the host side. (If the clock is post-spawn, the value is unreadable
  from exec-sandbox's memory model after the child is launched; if TTL, the value is gone after the
  window — assert whichever ADR 012 fixes.)

### TC-012-03: the env-mode value never appears in argv, stdout result, or audit

- **Requirement:** REQ-012-03
- **Type:** unit/integration
- **Input:** the env-mode run with a payload that does **not** print the token; capture the spawn
  argv, `result["stdout"]`, `result["sandbox_status"]`, and the emitted audit events.
- **Expected:** `SENTINEL-ENV-VALUE` appears in **none** of: the spawn argv (so `/proc/<pid>/cmdline`
  cannot leak it — the value is delivered via `cmd.Env`/an env file, **not** `--setenv` on the
  argv), the returned `stdout`/`sandbox_status`, or any audit event. Only
  `{handle_prefix, delivery:"env"}` is recorded.

### TC-012-04: secrets_injected accounting is correct for env-mode

- **Requirement:** REQ-012-04
- **Type:** unit
- **Input:** the env-mode run with handle `vault://handle/env-abcdefghij` (> 8 chars).
- **Expected:** `sandbox_status.secrets_injected` has exactly one entry
  `{handle_prefix:"vault://", delivery:"env"}` (8-char prefix via `prefix(handle, 8)`); no
  `credential`/`value` key; never the full handle.

### TC-012-05: proxy-mode unchanged; mixed run handled independently

- **Requirement:** REQ-012-05
- **Type:** integration (bwrap) against a stub vault returning per-handle modes
- **Input:** a run with two `secret_refs`: handle A → env-mode (`SENTINEL-ENV`), handle B →
  proxy-mode (`SENTINEL-PROXY`, bound to an allowlisted host). Payload prints its env.
- **Expected:** `SENTINEL-ENV` is readable in the payload env (env-mode delivered); `SENTINEL-PROXY`
  appears in **none** of the sandbox env/argv/stdout (proxy-mode F-002 invariant intact — it lives
  only on the proxy and is injected into the outbound request). `secrets_injected` has
  `{…, delivery:"env"}` for A and `{…, delivery:"proxy"}` for B.

### TC-012-06: env-mode inject failure skips the handle, no partial env var

- **Requirement:** REQ-012-06
- **Type:** unit/integration
- **Input:** an env-mode run whose stub vault returns an `error` field (or the IPC errors) for the
  handle.
- **Expected:** an `inject_failed` audit event (decision `deny`) is emitted, the handle is skipped,
  the run continues (and completes the payload), and **no** env var (empty or partial) is delivered
  for the failed handle — the sandbox sees no `API_TOKEN` at all. `secrets_injected` has no entry
  for the failed handle.

### TC-012-07: spec + config updated

- **Requirement:** REQ-012-07
- **Type:** inspection (spec)
- **Input:** read `docs/spec/behaviors.md` B-003, `data-model.md` (vault.inject env-mode response +
  `sandbox_identity`/credential invariants), and `configuration.md` after the feat commit.
- **Expected:** B-003 documents env-mode delivery + the wipe clock alongside proxy-mode, making the
  env/proxy distinction explicit (proxy never enters the sandbox; env deliberately does and is
  wiped per the clock). `data-model.md`'s env-mode response line no longer says "recorded but not
  loaded onto the proxy" — it documents delivery + wipe; the data-invariant about proxy-mode
  credentials is preserved and the env-mode exception is stated. `configuration.md`'s
  `wiring.injection_mode` row no longer says env is "recorded but not loaded in v0." No future tense.

---

## Post-implementation verification

- [ ] TC-012-01: env-mode delivers the credential to the sandbox env (bwrap)
- [ ] TC-012-02: wipe clock removes the host-side copy at the defined point
- [ ] TC-012-03: value never in argv (/proc/cmdline) / result / audit
- [ ] TC-012-04: secrets_injected accounting correct
- [ ] TC-012-05: proxy-mode unchanged; mixed run independent (F-002 intact)
- [ ] TC-012-06: env-mode failure skips handle, no partial env var
- [ ] TC-012-07: B-003 / data-model / configuration updated
- [ ] ADR 012 written settling wipe clock + delivery mechanism + vault field names

## Test framework notes

- Reuse the recording/stub vault socket and the bwrap proxy-reach harness. The env-mode stub vault
  returns `{delivery:"env", credential:…, <name field>:…}`; confirm the field names against the
  vault block contract before fixing them in the test.
- TC-012-03's argv-absence assertion is load-bearing: env-mode must use `cmd.Env` or an env-file
  bind, **not** `--setenv` on the argv, so the value never lands in `/proc/<pid>/cmdline`. The test
  joins the spawn argv and asserts the sentinel is absent.
- TC-012-02 must assert against whatever wipe point ADR 012 fixes; keep the host-side credential
  holder in one place so the wipe is testable (a struct field set to "" / a buffer zeroed), mirroring
  `EgressProxy.Wipe()`.
- Keep proxy-mode tests green (F-002 / B-003 regression) — the env-mode `else` branch is the only
  behavioral change to the inject loop.
