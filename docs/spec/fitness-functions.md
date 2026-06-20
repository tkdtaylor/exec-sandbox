# Fitness functions

**Project:** exec-sandbox
**Last updated:** 2026-06-19 (ADR 009: F-010 — restored sandbox indistinguishable from a fresh one)

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
| F-005 | Every `profile.limits` cap is enforced on every wired tier | security | A requested cap is applied on the active backend (memory/pids via rlimits, disk via tmpfs size, cpu via taskset affinity, timeout via host-side kill) or — for the secondary `cpu_count`/`disk_mb` controls only — recorded in `sandbox_status.limits.degraded` with a stderr WARNING. No requested cap is silently ignored: a payload exceeding memory/pids/disk is killed, and an over-running payload is terminated with `status: "timeout"` | 0 unenforced caps | `go test -run 'Limit\|Timeout\|CPUAffinity\|DiskQuota' ./...` (9 tests) | block | **active** | `limits.go` + `run.go` (`Run` timeout/kill, `bubblewrapBackend`) + `gvisor.go` (`applyLimitsToOCISpec`); asserted by `TestParseLimits`, `TestTimeoutTerminatesPayload`, `TestMemoryLimitKillsPayload_Bwrap`, `TestPidsLimitRejectsForkBomb_Bwrap`, `TestDiskLimitBlocksWrites_Bwrap`, `TestCPUAffinity_Bwrap`, `TestDiskQuotaDegradesGracefully_Bwrap`, `TestGvisorEnforcesLimits`, `TestGvisorOCISpecCarriesLimits` |
| F-006 | Only `run.workdir` is writable; system dirs stay ro; netns stays unshared | security | When `run.workdir` is set, exactly one writable host bind exists — `/work` (`--bind`/non-`ro` options) — while the rootfs and system dirs (`/usr`, `/etc`, …) and `/payload.sh` remain read-only, and the network namespace stays unshared (`--unshare-all`, no `--share-net`; the OCI `network` namespace has no path). A bad `run.workdir` fails the run rather than running unmounted | 0 extra writable surfaces, 0 net openings | `go test -run 'Workdir\|OnlyWorkdir' ./...` (9 tests) | block | **active** | `run.go` `validateWorkdir`/`bwrapArgv` + `gvisor.go` `applyWorkdirToOCISpec`; asserted by `TestOnlyWorkdirWritable_Bwrap` (writable `/work` + read-only `/usr` + `--unshare-all` kept, no `--share-net`, netns path-less), `TestWorkdirOCISpec`, `TestBadWorkdirFailsLoud`, and the read/write/cwd tests |
| F-007 | FileRead host mounts are read-only; only `/work` is writable; netns stays unshared | security | Every `FileRead{paths}` mount is **read-only** on both tiers — bwrap uses `--ro-bind <p> <p>` (never the writable `--bind`); the OCI mount carries `options:[ro,rbind]` (contains `"ro"`). A write to a FileRead mount fails (EROFS/permission) while `/work` stays writable. A relative or nonexistent FileRead path fails the run before any side effect (no silent skip). The network namespace stays unshared (`--unshare-all`, no `--share-net`; OCI `network` namespace path-less) — FileRead opens no egress and no writable surface | 0 writable FileRead mounts, 0 net openings | `go test -run 'FileRead' ./...` (11 tests) | block | **active** | `run.go` `fileReadPaths`/`validateFileReads`/`bwrapArgv` + `gvisor.go` `applyFileReadToOCISpec`/`applyEnvToOCISpec`; asserted by `TestFileReadMountIsReadOnly_Bwrap` (host `evil.txt` never created, `/work` write persists), `TestFileReadArgv_Bwrap` (`--ro-bind` not `--bind`, `--unshare-all` kept, no `--share-net`), `TestFileReadOCISpec` (`"ro"` option, netns path-less), `TestBadFileReadFailsLoud` (relative/nonexistent → `{error}` before spawn), and the read/execute/`command -v` tests on bwrap + gVisor |
| F-008 | Per-run output cap is host-side and tier-independent | security | `profile.limits.max_output_bytes` caps captured stdout/stderr **above** the `tier` seam: each stream is retained up to the ceiling and overflow is **dropped** without erroring the payload's pipe (its exit is unchanged), stdout/stderr capped **independently**, and the capped streams are recorded in `sandbox_status.limits.output_truncated` (deterministic order). The cap is identical under bubblewrap and gVisor — the bwrap argv and the OCI spec are **byte-for-byte unchanged** by the cap (it never reaches a backend). Writing exactly the cap does not flag; cap+1 does. No `max_output_bytes` ⇒ full output, `output_truncated: []` (prior behavior) | 0 unbounded captures when capped, 0 backend-wiring changes, 0 payload-exit perturbations | `go test -run 'OutputCap\|CapWriter\|MaxOutputBytes\|OutputTruncated\|NoOutputCap' ./...` (7 tests) | block | **active** | `limits.go` `capWriter`/`newCapWriter`/`parseLimits` + `run.go` `Run` (host capture path) / `outputTruncated` / `limitsReport`; asserted by `TestCapWriter` (exact-cap no-flag, cap+1 flag, chunked, uncapped), `TestParseLimits_MaxOutputBytes`, `TestOutputTruncatedRecord` (deterministic order), `TestOutputCapTruncates_Bwrap` (1 MiB → 1024, exit 0, `output_truncated:[stdout]`), `TestOutputCapTruncates_Gvisor` (identical under runsc), `TestOutputCapDoesNotTouchBwrapArgv` (argv/OCI spec unchanged, `--unshare-all` kept, no `--share-net`), `TestNoOutputCapFullOutput_Bwrap` (full output, `[]`) |
| F-009 | Per-host verb allowlist narrows egress; a blocked verb makes no outbound connection | security | A request whose method is **not** in an allowlisted host's **non-empty** verb set (`profile.capabilities[NetConnect].methods`, ADR 008) is `403`'d (`blocked-by-method`) and is **never forwarded upstream** — the origin observes **zero** requests and **no** credential is injected. The **host check precedes** the verb check (an unlisted host is `403 blocked-by-allowlist` regardless of method). An **unconstrained** host (no/empty `methods`) forwards **every** verb exactly as before (backward compatible). Verb matching is **case-insensitive** (canonical upper-case). The check only **narrows** egress — it adds no route, no `--share-net`, no second socket; the proxy stays the sole egress | 0 forwarded-on-block, 0 credential injections on block, 0 outbound connections on block, 0 net openings | `go test -run 'Verb\|NetVerb\|BlockedByMethod\|DisallowedVerb\|AllowedVerb\|HostCheckPrecedes' ./...` (8 tests) | block | **active** | `run.go` `netVerbAllowlist` + `proxy.go` `NewEgressProxy`/`handle` (verb check after host check, before upstream); asserted by `TestNetVerbAllowlistParsing` (per-host set; absent/empty ⇒ unconstrained, not deny-all), `TestProxyForwardsAllowedVerb`, `TestProxyBlocksDisallowedVerb` (403 `blocked-by-method`, **origin hits == 0**), `TestProxyHostCheckPrecedesVerbCheck` (unlisted host → `blocked-by-allowlist` even for an allowed verb), `TestProxyVerbMatchingCaseInsensitive`, `TestProxyUnconstrainedHostAllowsAllVerbs`, `TestSandboxAllowedVerbReachesHost` (bwrap, 200 + 1 hit), `TestSandboxDisallowedVerbBlockedOriginSeesNothing` (bwrap, 403 + **0 hits**) |
| F-010 | A restored sandbox is indistinguishable from a fresh one (no file/env/credential leak; netns stays unshared) | security | After `restore`, the writable surface **and** the proxy credential map are byte-for-byte equal to a freshly-built baseline (ADR 009): any file/dir the payload wrote under the writable surface is gone, the credential map is empty (restore subsumes `Wipe()`), and `payload.sh` is re-seeded equal to fresh. A second real run on a restored writable surface cannot read the first run's files (bwrap). The restored sandbox rebuilds the **same** spawn argv/OCI spec as a fresh one — `--unshare-all` / path-less OCI netns, no `--share-net`, the **same fresh** `/proxy.sock` (no stale socket, no re-bound stale credential). The one-shot path is observationally identical to before. The reset is host-side only and tier-independent | 0 file/env/credential leaks across restore, 0 net openings, 0 one-shot-contract changes | `go test -run 'Snapshot\|Restore\|Baseline\|Leak\|OneShot\|SecondRun' ./...` (7 tests) | block | **active** | `snapshot.go` `sandboxBaseline`/`snapshotBaseline`/`restore`/`teardown`/`writableSurface`/`credentialHosts` + `run.go` `Run` + `proxy.go` `Wipe`; asserted by `TestSnapshotCapturesPristineBaseline` (pristine surface + empty creds, idempotent), `TestRestoreReturnsToBaseline` (scratch gone, creds empty, double-restore no-op), `TestNoStateLeaksAcrossRestore` (**restored == fresh** diff: surface + payload.sh + creds), `TestRestoredSandboxKeepsNoNetworkInvariant` (`--unshare-all` kept, no `--share-net`, same fresh socket, empty creds), `TestNoCredentialLeaksAcrossRestore` (credential map empty after restore), `TestSecondRunCannotSeeFirstRunFiles_Bwrap` (run-2 sees CLEAN, not the run-1 leak file), `TestOneShotRunUnchanged_Bwrap` (full result schema, 200 via proxy) |

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
- F-005 ← ADR 003 (profile.limits enforcement); [behaviors.md](behaviors.md) B-009; [configuration.md](configuration.md) `run.profile.limits`; adapts agent-builder ADR 027 (degrade) / ADR 028 (runtime-aware verification).
- F-006 ← ADR 004 (writable working-directory mount); [behaviors.md](behaviors.md) B-010 + the "only writable host surface" invariant; [configuration.md](configuration.md) `run.workdir`.
- F-007 ← ADR 005 (FileRead read-only mounts + env provisioning); [behaviors.md](behaviors.md) B-011 + the "only writable host surface" invariant; [configuration.md](configuration.md) `run.profile.capabilities[FileRead].paths` + `run.env`.
- F-008 ← ADR 007 (per-run output cap enforced above the tier seam, overflow dropped, `output_truncated` observability); [behaviors.md](behaviors.md) B-009 (`max_output_bytes`) + the per-cap-enforced invariant; [configuration.md](configuration.md) `run.profile.limits.max_output_bytes`; [data-model.md](data-model.md) `sandbox_status.limits.output_truncated`.
- F-009 ← ADR 008 (per-host verb allowlist; decide=policy-engine / enforce=here; `blocked-by-method` distinct from `blocked-by-allowlist`; narrows-only); [behaviors.md](behaviors.md) B-002 (verb check after host check) + the no-network/proxy-only-egress invariant; [configuration.md](configuration.md) `run.profile.capabilities[NetConnect].methods`; [data-model.md](data-model.md) `EgressProxy.verbAllowlist`.
- F-010 ← ADR 009 (snapshot/restore host-side leak-proof reset boundary; host-side-only + tier-independent; reopening condition for the warm-pool variant); [behaviors.md](behaviors.md) B-012 + the "a restored sandbox is indistinguishable from a freshly-built one" invariant.

## Notes

- F-001 and F-002 are the two seed rules the adoption flow called out: they encode the
  load-bearing security model and should be the first to get real `make fitness-*` targets.
- Rules here are the *project's* commitments, not generic best practices.
- Fitness functions should fail fast and have low false-positive rates.
