# ADR 015: env-mode credential injection + wipe clock

**Status:** Accepted
**Date:** 2026-06-20
**Deciders:** exec-sandbox maintainers
**Supersedes:** —
**Reopening condition:** (1) **gVisor distinct-path revisit** — env-mode delivery is implemented for
**both** Tier-1 (bwrap) and Tier-2 (gVisor) because both keep the value off the spawn argv (bwrap via
`--args FD`, gVisor via the OCI `config.json process.env`, a 0600 file). If a future tier (e.g.
Firecracker) cannot deliver env off-argv with an equivalent mechanism, scope it to its own task and
re-open this ADR with the new tier's env path. (2) **TTL / mid-run rotation** — this ADR fixes a
**post-spawn / teardown** wipe clock. If a consumer later needs a bounded mid-run availability window
(TTL) or rotation, that is a different clock and a new task. (3) **Multi-value env bundles** — one
env var per handle is the contract here; a `{var_name → value}` bundle per handle re-opens this ADR.

---

## Context

Only **proxy-mode** injection is wired. In `Run()`'s inject loop a `delivery:"proxy"` response loads
the credential onto the `EgressProxy` (`run.go:104`); a `delivery:"env"` response is recorded as
`{handle_prefix, delivery:"env"}` (`run.go:114`) but the credential value is **never delivered** to
the sandbox — the `else` branch is accounting only. `configuration.md` confirmed env was *"recorded
but not loaded onto the proxy in v0."* The README listed *"env-mode injection + wipe clock"* as
deferred v1 work.

Some tools read a token from the **environment** (`$API_TOKEN`) rather than via an injected request
header. Env-mode is how exec-sandbox serves them. But unlike proxy-mode — where the credential value
never enters the sandbox at all (F-002) — env-mode **deliberately delivers** the value into the
sandbox process environment. That makes env-mode a documented exception to the F-002
"credential never enters the sandbox" rule, and the delivered value must be wiped per a defined clock
so it does not outlive its need.

Three points had to be settled before implementation (all resolved against the vault contract in
`interface-contracts.md` and the existing code):

1. **Vault env-mode response field names.**
2. **The env delivery mechanism** (must keep the value off the spawn argv / `/proc/<pid>/cmdline`).
3. **The wipe-clock point.**

## Decision

### 1. Vault env-mode response field names (confirmed against the vault contract)

`inject(handle, sandbox_identity, mode)` in env-mode returns:

```
{ ok, delivery:"env", credential, var_name, wiped_at }
```

- `credential` — the secret value to deliver.
- `var_name` — the target environment-variable name inside the sandbox (e.g. `API_TOKEN`).
- `wiped_at` — vault's own wipe-clock timestamp (vault-side bookkeeping; exec-sandbox does not
  re-deliver or persist it).

exec-sandbox reads `credential` + `var_name` from the response; the value never appears in any
exec-sandbox-produced surface (result, `sandbox_status`, audit) beyond the deliberate sandbox-env
delivery.

### 2. Env delivery mechanism — off the argv, keeping `--clearenv`

The value **must not** land on the spawn argv, where any process could read it via
`/proc/<pid>/cmdline`. Two candidate mechanisms were evaluated:

- **`os/exec` `cmd.Env`** (the initially-suggested mechanism). **Rejected for bwrap.** bwrap is
  invoked with `--clearenv`, which unsets the inherited child environment *before* the payload runs;
  a value placed in `cmd.Env` is therefore cleared and never reaches the payload (verified
  empirically). The only way `cmd.Env` would reach the payload is to **drop `--clearenv`**, which
  would leak the entire host process environment into the sandbox — a security regression strictly
  worse than the problem being solved. `cmd.Env` is not used.
- **bwrap `--args FD`** (chosen for Tier-1). bwrap reads NUL-separated arguments from a file
  descriptor. exec-sandbox writes the `--setenv <var_name> <value>` pairs (and `--clearenv` /
  `--setenv PATH …`) into a pipe whose read end is passed as an `ExtraFile`; the real argv carries
  only `bwrap --args 3 …`. The credential value lives in the pipe, **never** in `/proc/<pid>/cmdline`.
  `--setenv` on the *literal* argv (the rejected naive approach) is exactly what this avoids.
- **OCI `config.json process.env`** (Tier-2 / gVisor). The OCI spec is written to `config.json`
  (mode 0600) and consumed by `runsc` by directory; the env never appears on the `runsc` argv. This
  is off-argv by construction, so env-mode is **in scope for gVisor too** — the delivery mechanism
  differs (config file vs `--args FD`) but the off-argv invariant holds for both.

The env-mode credential is threaded to the backend as a separate `{var_name → value}` map, distinct
from `run.env` (the caller-controlled env from ADR 005). The caller cannot name an env-mode var; its
name and value come only from `vault.inject`.

### 3. Wipe-clock point — post-spawn / teardown

The host-side credential holder is a single `EnvCredentials` struct (mirroring `EgressProxy.creds` +
`Wipe()`). The value is delivered to the child at spawn and the host-side copy is **wiped
immediately after the child process returns** (post-spawn), and the deferred teardown wipes again so
nothing survives the run on the host side. There is no TTL and no mid-run rotation. This mirrors the
proxy `Wipe()` discipline: the host retains no credential past the run.

### env-vs-proxy boundary (the documented exception)

- **proxy-mode** (`delivery:"proxy"`): the value **never** enters the sandbox env/args/stdout — it
  lives only on the host-side proxy at the injection edge (F-002, unchanged by this ADR).
- **env-mode** (`delivery:"env"`): the value **deliberately** enters the sandbox via an environment
  variable — that is its entire purpose — and is wiped from the host post-spawn/teardown.

A mixed run (some handles env, some proxy) handles each independently: proxy handles → proxy edge;
env handles → sandbox env + wipe. Env-mode delivery does **not** weaken F-002 for proxy-mode
credentials; the F-002 leak-scan (argv / `--setenv` pairs / stdout) still passes for proxy creds.

### Accounting + failure (unchanged in shape)

One `{handle_prefix: prefix(handle, 8), delivery: "env"}` entry per delivered env handle — never the
full handle, never the value. An env-mode inject failure emits `inject_failed` (decision `deny`),
skips the handle (no partial/empty env var delivered), and the run continues — identical to
proxy-mode.

## Consequences

- The `Backend.Argv` signature gains an `envCreds` parameter and an `extraFiles` return so the bwrap
  backend can pass the `--args` pipe FD up to `Run()` (which sets `cmd.ExtraFiles`).
- A new `EnvCredentials` host-side holder is the single wipe point (testable: field cleared).
- `docs/spec/behaviors.md` B-003, `docs/spec/data-model.md`, and `docs/spec/configuration.md` are
  updated in the feat commit: env-mode is now delivered (not merely recorded), the wipe clock is
  documented, and the proxy-mode data-invariant is preserved with the env-mode exception stated.
- The no-network + proxy-only-egress invariant is untouched: env-mode adds no route out; it only
  populates an in-sandbox environment variable.
</content>
</invoke>
