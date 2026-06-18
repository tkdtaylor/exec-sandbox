# Task 004: implement `FileRead{paths}` (read-only host mounts) + payload PATH/env provisioning

**Status:** ⬜ backlog
**Branch:** `task/004-toolchain-mount-and-path`
**Spec:** [`docs/tasks/test-specs/004-toolchain-mount-and-path-test-spec.md`](../test-specs/004-toolchain-mount-and-path-test-spec.md)
**ADR:** ADR 005 (to be written during implementation — see Verification plan)

## Problem

A run can now operate on a writable repo at `/work` (task 003), but it still has **no way to get a
toolchain into the sandbox**. The sandbox runs `--clearenv` with a bare `PATH=/usr/bin:/bin`
(`run.go:279`, gVisor `process.env` `gvisor.go:181`), and only `/usr`/`/etc`/`/bin`/`/lib*`/`/sbin`
are bind-mounted read-only. Real build toolchains do **not** live there: Go is under
`/usr/local/go/bin`, and a consumer's curated scanners/linters live in a dedicated tool dir. So a
payload cannot `command -v go`, run `gofmt`/`golangci-lint`, or invoke a scanner — the exact tools a
**gate** is made of.

This is the next rung after task 003. Task 003 gave the payload its **repo** at `/work` (writable);
this task gives it its **TOOLS** (read-only). Together they let a consumer run a real
build/test/lint gate **inside the block**.

The block's own v1 contract already names the capability that does this — `FileRead{paths}` — and
flags it **documented-but-unimplemented** (`docs/spec/configuration.md:42`,
`docs/spec/data-model.md:69-71`, where "Other capability types … are part of the v1 contract but not
consumed yet"). This task implements it: a `FileRead{paths}` entry bind-mounts each listed host path
**read-only** into the sandbox, and a companion env-provisioning input lets the caller put a mounted
toolchain dir on `PATH`.

### Why it matters — block/launcher parity

exec-sandbox is meant to be the containment substrate for a coding agent. **agent-builder** (its
first consumer) currently **cannot run its gate in the block**, because `go`/`gofmt`/
`golangci-lint` and its scanners are missing inside the sandbox. agent-builder's own Podman launcher
already solves this and is the reference to port: it mounts a curated gate-tools dir **read-only**
and prepends it to PATH —
`containment/execution-box/run.sh:56-57` in the agent-builder repo:

```
gate_tool_mount="/opt/agent-builder/gate-tools"
gate_tool_path="$gate_tool_mount:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
```

— mounted `ro` (`run.sh` `--mount …,ro` around the gate-tools target) and exported as the payload's
`PATH`. The toolchain dir is validated to exist and canonicalized (`run.sh:206-215`
`resolve_gate_tools`). Porting this to exec-sandbox (bwrap + gVisor) brings the block to parity so it
can **replace** that launcher.

## Scope

- **Parse `FileRead` from `run.profile.capabilities`.** Add a parser mirroring `netAllowlist`
  (`run.go:328`): read `{"type":"FileRead","paths":["/abs/host/path", …]}` entries and collect their
  paths. Multiple `FileRead` entries union their path lists.
- **Validate each FileRead path before spawn** (alongside `validateWorkdir`, before any
  proxy/vault side effect): each path must be **absolute** and **exist** on the host. A relative
  path or a nonexistent path is a hard `{error}` (no silent skip) — the no-silent-fall-back
  invariant ADR 001/003/004 already commit to. Empty/absent `FileRead` ⇒ no extra mounts.
- **bubblewrap backend** (`bwrapArgv`, `run.go:264-291`): for each FileRead path, append
  `--ro-bind <path> <path>` (mounted at the **same** host path inside the sandbox). Read-only is
  load-bearing — `--ro-bind`, **not** the writable `--bind` used for `/work`. Place these alongside
  the existing `--ro-bind /usr /usr` system mounts.
- **gVisor backend** (`applyWorkdirToOCISpec`'s sibling, `gvisor.go`): add an
  `applyFileReadToOCISpec(spec, paths)` (in-place mutation, the shape of `applyWorkdirToOCISpec` /
  `applyLimitsToOCISpec`) that appends a read-only OCI mount per path —
  `{destination:<path>, type:bind, source:<path>, options:["ro","rbind"]}` — mirroring the `ro`
  system-dir mounts (`gvisor.go:166-172`). `gvisorOCISpec` (the base builder) stays unchanged so the
  no-FileRead base spec is byte-for-byte identical and existing `TestGvisorSpec*` tests stay green.
- **PATH/env provisioning.** The sandbox is `--clearenv` with a bare PATH, so a mounted toolchain
  dir is useless unless it is on PATH. Add a caller env input on `RunRequest.Run` — decide the exact
  shape and document it (recommended: `run.env` `map[string]string`, at minimum honouring `PATH`;
  a simpler `run.path []string` that is prepended to the default PATH is an acceptable alternative —
  pick one in ADR 005 and justify it). Merge it into:
  - bwrap: replace/extend the `--setenv PATH …` (`run.go:279`) and emit a `--setenv <k> <v>` per env
    entry.
  - gVisor: set `process.env` entries (`gvisor.go:181`) — `PATH=…` plus any extra `k=v`.
  - Empty/absent env ⇒ the current bare `PATH=/usr/bin:/bin`, behavior unchanged.
  The payload must be able to `command -v <tool>` and resolve a tool that was FileRead-mounted from a
  host toolchain dir that the caller put on PATH.
- **Security posture (state in task + ADR 005):** FileRead mounts are **READ-ONLY**; the network
  namespace stays fully unshared (`--unshare-all` / empty OCI netns); `/work` remains the **only**
  writable host surface. FileRead adds read-only host paths and PATH entries — it opens **no** egress
  and **no** writable surface.
- **Spec + contract update in the same commit:** `docs/CONTRACT.md`, `docs/spec/data-model.md`,
  `docs/spec/configuration.md`, `docs/spec/interfaces.md`, `docs/spec/behaviors.md` — rewrite in
  place to mark `FileRead{paths}` **implemented** (read-only) and document the new `run.env`/`run.path`
  input; reconcile with the `run.workdir` distinction already recorded. Add a fitness row (F-007)
  asserting FileRead mounts are read-only and the netns stays unshared.

Out of scope: writable FileRead (that is `run.workdir`); per-path remapping to a different in-sandbox
mountpoint (FileRead mounts at the same path — the agent-builder launcher remaps, but same-path is
simpler and sufficient; note the remap option in ADR 005's reopening condition); baking a toolchain
into a rootfs/image (ADR 005 rejects this — see Options).

## Verification plan

- **Highest level achievable: L5/L6.** This host has **both** `bwrap` and `runsc`, so the
  read-back, PATH-resolution, read-only-enforcement, and no-network assertions run end-to-end under
  real sandboxes on both tiers (the level task 002/003 reached).
- **Harness command:** `go test -count=1 ./...`
- **Runtime observation (L6):** a host dir holding a marker executable, mounted via `FileRead` and
  placed on PATH via `run.env`/`run.path`, is resolvable by the payload (`command -v <tool>` →
  the mounted path) and **executable**; a **write** to the FileRead mount fails (EROFS/permission),
  distinct from `/work` which is writable; an absent `FileRead`/env leaves the run byte-for-byte
  unchanged (no extra mounts, bare PATH); a nonexistent / relative FileRead path returns `{error}`
  before any side effect; the network namespace remains unshared (the existing proxy is still the
  only egress). Observed under **both** bwrap and runsc.
- **Fitness (L3):** new F-007 row asserts "FileRead host mounts are read-only; only `/work` is
  writable; the netns stays unshared"; check command is the FileRead test set.
- **ADR 005 written during implementation** capturing the provisioning-model decision: `FileRead{paths}`
  mounts **caller-specified** host paths **read-only**; hermeticity/version-pinning is the **caller's**
  responsibility (the caller mounts a curated, portable toolchain dir). Record the alternative
  considered (baking a toolchain into a rootfs/image) and why host-path mounts fit the lean bwrap
  model; note that gVisor (OCI) receives the same mounts; settle the `run.env` vs `run.path` shape.

## Definition of done

- `run()` parses `FileRead{paths}`, validates each path (absolute + exists, else hard `{error}`), and
  bind-mounts each **read-only** at the same path on **both** tiers; a payload reads/executes a
  FileRead-mounted tool but **cannot write** it.
- A caller env input (`run.env`/`run.path`) provisions the payload's PATH on both tiers; a tool from
  a FileRead-mounted toolchain dir resolves via `command -v`. Empty env ⇒ bare PATH, unchanged.
- Absent FileRead/env is backward compatible (no extra mounts, prior behavior byte-for-byte); a bad
  FileRead path fails loud before spawn.
- The netns stays unshared and `/work` stays the only writable surface, with FileRead mounted.
- Spec + CONTRACT mark `FileRead{paths}` implemented (read-only) and document the env input
  (rewritten in place); F-007 fitness row added; **ADR 005** written.
- spec-verifier APPROVE before promotion to ✅.
