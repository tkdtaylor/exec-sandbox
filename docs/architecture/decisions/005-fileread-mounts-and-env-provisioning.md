# ADR 005: `FileRead{paths}` read-only host mounts + payload PATH/env provisioning

**Date:** 2026-06-18
**Status:** Accepted
**Task:** 004 (implement `FileRead{paths}` read-only host mounts + payload PATH/env provisioning)
**Related:** ADR 001 (foundational stack: no-network, proxy-only egress, unprivileged bwrap),
ADR 002 (gVisor Tier-2 backend), ADR 003 (profile.limits enforcement), ADR 004 (writable
`/work` working-directory mount). Ports the reference implementation in the consumer repo
**agent-builder** (`containment/execution-box/run.sh` `--mount …,ro` gate-tools dir +
`gate_tool_path` PATH; `resolve_gate_tools` validation).

## Context

After ADR 004 a run gets its **repo** at `/work` (writable). It still has **no way to get a
toolchain into the sandbox**. The sandbox runs `--clearenv` with a bare `PATH=/usr/bin:/bin`
(`run.go` bwrap `--setenv PATH`, gVisor `process.env`), and only `/usr`/`/etc`/`/bin`/`/lib*`/
`/sbin` are bind-mounted read-only. Real build toolchains do **not** live there: Go is under
`/usr/local/go/bin`, and a consumer's curated scanners/linters live in a dedicated tool dir. A
payload therefore cannot `command -v go`, run `gofmt`/`golangci-lint`, or invoke a scanner — the
exact tools a **gate** is made of.

The v1 contract already names the capability for this — `FileRead{paths}` — and flags it
*documented-but-unimplemented* (`docs/spec/configuration.md`, `docs/spec/data-model.md`). This
task implements it: a `FileRead{paths}` entry bind-mounts each listed host path **read-only** into
the sandbox. ADR 004 already settled that `FileRead` is the **read-only** host-path mechanism,
complementary to the writable `run.workdir` — this ADR makes it real.

A read-only mount alone is useless if the toolchain dir is not on `PATH` (the sandbox is
`--clearenv` with a bare PATH). So this task also adds a caller env-provisioning input. The proven
reference is agent-builder's Podman launcher: it mounts a curated gate-tools dir **read-only**
(`run.sh` `--mount type=bind,…,ro`), validates and canonicalizes it (`resolve_gate_tools`), and
prepends it to the payload's `PATH` (`gate_tool_path`). Porting this to exec-sandbox's bubblewrap
and gVisor backends brings the block to parity so it can **replace** that launcher.

## Decision

### 1. `FileRead{paths}` — read-only, same-path host mounts on both tiers

Parse `FileRead` entries from `run.profile.capabilities` (mirroring `netAllowlist`): each
`{"type":"FileRead","paths":["/abs/host/path", …]}` entry contributes its paths; multiple entries
**union** their lists; a missing/empty `paths` contributes nothing. Each path is **validated
before any side effect** (alongside `validateWorkdir`, before proxy/vault): it must be
**absolute** and **exist** on the host. A relative or nonexistent path is a hard `{error}` — the
run does not start (no silent skip). Each validated path is bind-mounted **read-only** at the
**same** host path inside the sandbox.

| Concern | Bubblewrap (Tier-1) | gVisor `runsc` (Tier-2) |
|---------|---------------------|--------------------------|
| Mount (per path) | `--ro-bind <path> <path>` (**read-only**, **not** `--bind`) | OCI `mounts` entry `{destination:<path>, type:bind, source:<path>, options:[ro,rbind]}` |
| Placement | alongside the existing `--ro-bind /usr /usr` system mounts | alongside the `ro,rbind` system-dir mounts, via `applyFileReadToOCISpec` (in-place) |

`gvisorOCISpec` (the base builder) is **unchanged**: FileRead mounts are added by a separate
`applyFileReadToOCISpec(spec, paths)` mutation, so the no-FileRead base spec is byte-for-byte
identical and the existing `TestGvisorSpec*` tests stay green. This mirrors how ADR 003/004 added
limits and the workdir as in-place mutations.

**Same-path, not remapped.** FileRead mounts each host path at the identical path inside the
sandbox (`/opt/tools` → `/opt/tools`). agent-builder's launcher remaps to a fixed `/opt/...`
target, but same-path is simpler, needs no target field, and is sufficient: a payload references a
tool by the path the caller already knows. Per-path remapping (a `{source, target}` shape) is the
reopening condition, not v1.

### 2. PATH/env provisioning — `run.env map[string]string`

Add `run.env` (`map[string]string`) to the request. Each entry is exported into the sandbox as an
environment variable; in particular a `PATH` entry replaces the bare default. When `run.env` is
empty/absent, behavior is exactly as today — `PATH=/usr/bin:/bin`, no other env — so the change is
fully backward compatible.

| Concern | Bubblewrap (Tier-1) | gVisor `runsc` (Tier-2) |
|---------|---------------------|--------------------------|
| PATH | `run.env["PATH"]` replaces the default `--setenv PATH …`; absent ⇒ `--setenv PATH /usr/bin:/bin` | `run.env["PATH"]` replaces the default `process.env` `PATH=…`; absent ⇒ `PATH=/usr/bin:/bin` |
| Other vars | one `--setenv <k> <v>` per non-PATH entry | one `process.env` `k=v` per non-PATH entry |
| Ordering | env entries emitted in a deterministic (sorted-key) order so the argv/spec are reproducible | same |

The credential invariant is **untouched**: `run.env` is a caller-supplied, non-secret provisioning
input (PATH, locale, tool flags). Proxy-mode credentials still enter only at the proxy edge and
never via env (ADR 001). A caller MUST NOT place a secret value in `run.env` — the same as any
other plaintext field of the request.

#### Why `run.env` (map) over `run.path` (list)

Two shapes were on the table (the task left the choice to this ADR):

- **`run.path []string`** — a list of dirs prepended to the default PATH. Minimal, single-purpose.
- **`run.env map[string]string`** — a general env map, at minimum honouring `PATH`. (chosen)

`run.env` is chosen because:

1. **The reference needs more than PATH eventually.** agent-builder's gate-tools model is about
   PATH today, but real toolchains read env (`GOFLAGS`, `GOCACHE`, `LANG`, proxy-tool config). A
   list-of-dirs shape solves exactly one variable and would have to be widened to a map the moment
   a second variable is needed — the "second concrete use case" is already visible, so a map is not
   premature here, it is the right size.
2. **One field, not two.** `run.path` would coexist with an eventual `run.env`, giving two ways to
   influence the environment. `run.env` subsumes the dir-list case (`run.env["PATH"]`) with one
   well-understood shape (`map[string]string`, the same shape `os/exec`'s `Env` reduces to).
3. **Explicit over implicit.** A `run.path` list implies a *prepend-to-default* merge policy the
   caller cannot see; `run.env["PATH"]` is the literal PATH the payload gets. The caller composes
   the full PATH string (as agent-builder already does for `gate_tool_path`), so there is no hidden
   merge semantics to reason about. Empty `run.env` ⇒ the documented bare default.

The cost is that the caller writes the whole PATH (`<tools>:/usr/bin:/bin`) instead of just the
prefix. That is a single line the caller already composes in the reference, and it keeps the merge
policy out of the block.

### Security posture — FileRead is read-only, opens no egress, adds no writable surface

- **FileRead mounts are READ-ONLY** on both tiers (`--ro-bind` / `options:[ro,rbind]`). A payload
  can read and execute a FileRead-mounted tool but **cannot write** it (EROFS/permission),
  distinct from `/work`, which remains the **only** writable host surface (ADR 004).
- **The network stays fully unshared.** `--unshare-all` (bwrap) and the empty `network` namespace +
  `--network=none` (gVisor) are untouched. FileRead adds read-only host paths and PATH/env entries
  — it opens **no** egress. The only path out remains the bind-mounted `/proxy.sock`.
- **No credential exposure.** `run.env` carries no secret (caller responsibility); credential
  injection still happens only at the proxy edge.

The threat this accepts is that the caller chooses which host paths to expose read-only. A
malicious *caller* could mount a sensitive host dir, but the caller is the trusted party here
(it already chooses the workdir and the allowlist); the *payload* gains only read access to
caller-chosen paths and cannot widen that set.

### Composition — a mutation step plus a widened `Backend.Argv` seam

FileRead and env are applied as small, independent mutations, mirroring ADR 003/004:

- bubblewrap: `bwrapArgv` gains `fileReads []string` and `env map[string]string`; it appends
  `--ro-bind <p> <p>` per FileRead path alongside the system mounts and emits `--setenv` entries
  from the env map (PATH defaulted when absent).
- gVisor: a new `applyFileReadToOCISpec(spec, paths)` appends the read-only mounts in place, and an
  `applyEnvToOCISpec(spec, env)` sets `process.env`; `gvisorOCISpec` is unchanged.
- The `Backend.Argv` seam gains `fileReads []string` and `env map[string]string` alongside the
  existing `workdir`/`lim`, since they are per-run inputs the same way. This is the composability
  boundary the `tier` seam exists for: each backend translates the same
  `(scriptPath, proxySock, workdir, fileReads, env, lim)` into its own mount/env vocabulary.

## Options considered

### Provisioning model

#### Option A — caller-specified host-path mounts + caller-composed env (chosen)

- **Pros:** ports the proven agent-builder reference 1:1 (curated ro tool dir on PATH); zero new
  runtime dependencies; the lean bwrap model stays lean (no image build, no rootfs management);
  hermeticity/version-pinning is the **caller's** responsibility — it mounts a curated, portable
  toolchain dir, exactly as agent-builder curates its gate-tools dir; gVisor receives the identical
  mounts via OCI; backward compatible (empty ⇒ today's behavior).
- **Cons:** the block does not *guarantee* a hermetic toolchain — it trusts the caller to mount a
  consistent one. Accepted: the caller already owns the worktree and allowlist; owning the
  toolchain dir is the same trust boundary, and it keeps the block a thin containment substrate.

#### Option B — bake a toolchain into a rootfs/image

- **Pros:** hermetic, version-pinned by the block; reproducible toolchain across runs.
- **Cons:** discards the lean bwrap model (bwrap binds host dirs; it has no image concept); forces
  building and maintaining a rootfs/image per toolchain version; couples the block to a specific
  toolchain set instead of letting each consumer bring its own; far more surface than the job needs.
  The consumer (agent-builder) already curates its own gate-tools dir and wants to *bring* it, not
  receive a block-baked one. Rejected — host-path mounts fit the lean model and the consumer's
  existing curation.

### Env shape — `run.env` vs `run.path` (settled above; `run.env` chosen).

## Consequences

**What becomes true.** `run()` parses `FileRead{paths}`, validates each path (absolute + exists,
else hard `{error}` before any side effect), and bind-mounts each **read-only** at the same path on
**both** tiers. A caller `run.env` provisions the payload's PATH (and any other env) on both tiers,
so a tool from a FileRead-mounted dir on PATH resolves via `command -v` and executes — while a
*write* to the FileRead mount fails (read-only), distinct from the writable `/work`. An absent
`FileRead`/`run.env` leaves behavior byte-for-byte unchanged (no extra mounts, bare PATH). This
brings exec-sandbox to parity with agent-builder's gate-tools launcher: a curated toolchain dir
mounted read-only and placed on PATH makes its tools invocable by name **inside the block**, the
last piece needed to run a real build/test/lint gate in the sandbox.

**Reconciliation with `run.workdir` and `FileRead`.** `run.workdir` is the **writable**
single-dir mechanism (ADR 004); `FileRead{paths}` is now the **read-only** multi-path mechanism.
They compose: a run can mount a writable repo at `/work` *and* a read-only toolchain dir via
`FileRead`, with `run.env["PATH"]` putting the tools on PATH. The two host-path concepts no longer
collide — one is rw at a fixed mountpoint, the other is ro at same-path.

**Trade-offs, stated.** Read-only host surface is now caller-extensible (a list of ro paths);
bounded to caller-chosen paths, no writable access, no egress, net still unshared. The block does
not guarantee toolchain hermeticity — that is the caller's responsibility (it curates the mounted
dir), consistent with the block being a thin containment substrate.

**Reopening condition.** If a consumer needs per-path remapping (mount a host dir at a *different*
in-sandbox path, as agent-builder's launcher does), generalize `FileRead{paths}` into a
`FileRead{mounts:[{source,target}]}` shape behind the same per-backend translation. If a consumer
needs the block to *own* a hermetic toolchain (not trust the caller's dir), revisit Option B as a
separate "toolchain image" feature. Until a second concrete use case exists, same-path read-only
mounts + caller-composed `run.env` are the right size (defer premature decisions).

**Spec updates land with the code in task 004.** `docs/CONTRACT.md`, `docs/spec/data-model.md`,
`docs/spec/interfaces.md`, and `docs/spec/configuration.md` mark `FileRead{paths}` **implemented**
(read-only) and document `run.env` (rewritten in place, not appended); `docs/spec/behaviors.md`
gains the FileRead + env-provisioning behavior; and `docs/spec/fitness-functions.md` gains a row
(F-007) asserting FileRead host mounts are read-only, only `/work` is writable, and the netns stays
unshared.
