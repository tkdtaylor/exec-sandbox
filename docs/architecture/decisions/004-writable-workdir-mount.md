# ADR 004: writable working-directory mount — bind the host worktree at /work, rw, cwd=/work

**Date:** 2026-06-18
**Status:** Accepted
**Task:** 003 (add a writable working-directory mount to `run()`)
**Related:** ADR 001 (foundational stack: no-network, proxy-only egress, unprivileged bwrap),
ADR 002 (gVisor Tier-2 backend), ADR 003 (profile.limits enforcement). Ports the reference
implementation in the consumer repo **agent-builder**
(`containment/execution-box/run.sh` `--mount …,/work,rw` + `--workdir /work`;
`internal/sandbox/podman/run.go` `validateWorktree`).

## Context

Today `run()` runs the payload in an isolated sandbox with a **fully read-only** view of the
host: `/usr`, `/etc` (ro), `/payload.sh` (ro), `/proxy.sock`, and a throwaway tmpfs `/tmp`.
There is **no way to give the payload a host directory to operate on**. A run can read its
script, reach allowlisted hosts through the proxy, and scribble in `/tmp` — but nothing it
produces survives, and it can never see a checked-out repo.

The downstream consumer (**agent-builder**) needs exactly that: it gates an agent's work on a
**checked-out repo worktree** — build, test, lint, then *commit*. That is a write workload
against a host directory. agent-builder's own `run()` adapter currently **drops** its
`Request.Worktree` because exec-sandbox has nowhere to put it, so the block backend can only run
self-contained payloads, not agent-builder's real "gate-on-a-repo" job. agent-builder's task 062
(the block-as-default adapter) is implemented and tested but **held unmerged pending this mount**.

The v1 contract already names a host-path capability — `FileRead{paths}` — but it is (a)
**read-only** by name and intent, and (b) documented-but-unimplemented. The consumer needs
**write** access (it produces commits). A read-only `FileRead` mount cannot satisfy a commit
workload. So this is a distinct mechanism: a single **writable working directory**, not a list
of read-only file paths.

A complete, working reference exists in agent-builder and is the thing to port:

- `run.sh:521` — `--mount "type=bind,source=$worktree,target=/work,rw,relabel=private"` mounts
  the host worktree at `/work`, **read-write**.
- `run.sh:507` — `--workdir /work` runs the payload with `/work` as its current directory.
- `run.sh:473-474` — the worktree path must **exist** and is canonicalized to an absolute dir
  (`[ -d "$worktree" ] || die …; worktree="$(cd "$worktree" && pwd)"`).
- `internal/sandbox/podman/run.go` `validateWorktree()` — trims, `filepath.Abs`, `os.Stat`,
  `IsDir`; a blank/missing/non-dir path is a hard `ErrInvalidWorktree`, never a silent fall-back.

agent-builder targets rootless **Podman**, so it expresses the mount as Podman `--mount`/
`--workdir` flags. exec-sandbox does not use Podman — its backends are **bubblewrap** (Tier-1)
and **gVisor `runsc`** (Tier-2). The *mechanism* must be adapted to each backend's own mount
vocabulary, but the *semantics* (one host dir, writable, at `/work`, cwd `/work`, everything
else still read-only and the network still unshared) port verbatim.

## Decision

Add an optional host working-directory input to the request — `run.workdir` (a host path
string). When **non-empty**, the named host directory is bind-mounted **read-write** at `/work`
inside the sandbox and the payload's working directory is set to `/work`. When **empty/absent**,
behavior is exactly as today — no `/work` mount, cwd unchanged — so the change is fully
backward compatible.

| Concern | Bubblewrap (Tier-1) | gVisor `runsc` (Tier-2) |
|---------|---------------------|--------------------------|
| Mount | `--bind <workdir> /work` (writable; **not** `--ro-bind`) | OCI `mounts` entry `{destination:/work, type:bind, source:<workdir>, options:[rbind]}` — no `ro` ⇒ read-write |
| Cwd | `--chdir /work` | OCI `process.cwd = "/work"` |
| Everything else | unchanged: `--unshare-all`, `--die-with-parent`, `--clearenv`, `/usr` `/etc` etc. stay `--ro-bind` | unchanged: empty `network` namespace, `--network=none`, read-only rootfs, system dirs `ro,rbind` |

`run.workdir` is **validated before spawn** and resolved the same way the reference does: the
path is trimmed, made absolute (`filepath.Abs`), and `os.Stat`'d; it must be an **existing
directory**. A blank path means "no workdir mount". A non-blank path that does not exist, or
resolves to a non-directory, is a **hard error** (`{error: "invalid run.workdir: …"}`) — the run
does not start. This is the no-silent-fall-back stance ADR 001/003 already commit to: a
load-bearing input that is malformed fails loud, it is never quietly ignored.

### Security posture — the worktree is the writable untrusted boundary

This mount is deliberately the **one** writable host surface, and it is the *only* deviation
from "the sandbox can write nothing on the host":

- **Only `/work` is writable.** The rootfs and every system dir (`/usr`, `/etc`, `/bin`, `/lib`,
  `/lib64`, `/sbin`) stay read-only on both backends; `/payload.sh` stays read-only; `/tmp` is
  still a throwaway tmpfs (size-capped when `disk_mb` is set — ADR 003). Writing untrusted output
  is confined to the directory the *caller chose to expose*. The caller is trusted to hand in a
  scratch worktree (agent-builder hands in a disposable git worktree), not `/` or `$HOME`.
- **The network stays fully unshared.** `--unshare-all` (bwrap) and the empty `network`
  namespace + `--network=none` (gVisor) are untouched. A writable workdir does **not** open any
  egress path: the only way out remains the bind-mounted `/proxy.sock`. The two invariants are
  orthogonal and both hold.
- **No credential exposure.** The workdir carries no secret; credential injection still happens
  only at the proxy edge (ADR 001). A payload could *write* a secret it already exfiltrated into
  `/work`, but it had no way to obtain one — the credential never enters the sandbox in proxy
  mode, mount or no mount.

The threat this accepts is the obvious one: a malicious payload can corrupt or delete the
contents of the worktree it was given. That is inherent to "let the code build and commit in this
directory" and is bounded to that directory. The caller mitigates it by exposing a disposable
worktree, which is exactly agent-builder's model (gate on a throwaway checkout, inspect the
result, discard).

### Composition — a mutation step, not a new signature for the base builders

The workdir is applied as a small, independent mutation, mirroring how ADR 003 applied limits:

- bubblewrap: `bwrapArgv` gains a `workdir` argument and, when it is non-empty, appends
  `--bind <workdir> /work --chdir /work` after the read-only system mounts.
- gVisor: a new `applyWorkdirToOCISpec(spec, workdir)` appends the writable `/work` bind mount
  and sets `process.cwd`, in place — the exact shape of `applyLimitsToOCISpec`. `gvisorOCISpec`
  (the base-spec builder) is unchanged, so the no-limits/no-workdir base spec is byte-for-byte
  identical to today and the existing `TestGvisorSpec*` assertions hold.

The `Backend.Argv` seam gains a `workdir` parameter alongside the existing `lim Limits`, since
the workdir is a per-run input the same way limits are. This is the composability boundary the
`tier` seam exists for: each backend translates the same `(scriptPath, proxySock, workdir, lim)`
into its own mount vocabulary.

## Options considered

### Option A — a dedicated writable `run.workdir` mounted at `/work`, cwd `/work` (chosen)

- **Pros:** ports the proven agent-builder reference 1:1 (same `/work`, same rw, same cwd, same
  validation); satisfies the *write* workload the consumer actually has; one well-named field;
  backward compatible (empty ⇒ today's behavior); the single writable surface is explicit and
  auditable; orthogonal to the network invariant, which is untouched.
- **Cons:** introduces a writable host surface (mitigated: one caller-chosen dir, everything else
  ro, net still unshared); a second host-path concept alongside the contract's `FileRead{paths}`
  (reconciled below).

### Option B — implement `FileRead{paths}` and make it writable

- **Pros:** reuses the existing contract capability; no new field.
- **Cons:** `FileRead` is **read-only by name and intent** — overloading it to mean "writable
  working dir" is a contract lie that would confuse every other consumer and reviewer. The
  consumer needs a *working directory* (one dir, cwd set), not a *list of readable files*. These
  are different shapes with different security postures. Rejected: do not redefine a named
  read-only capability to mean its opposite.

### Option C — make the whole rootfs writable / use a writable overlay

- **Pros:** maximal flexibility for the payload.
- **Cons:** discards the read-only-system-dirs invariant that is the core of the isolation model;
  lets untrusted code tamper with `/usr`/`/etc` for the life of the run. Far more surface than the
  job needs. Rejected.

## Consequences

**What becomes true.** `run()` accepts a host working directory, bind-mounts it writable at
`/work` on **both** tiers with cwd `/work`, and a payload can read **and write** the host
worktree — output (built artifacts, a git commit) persists to the host after the run. An
absent `run.workdir` leaves behavior byte-for-byte unchanged. A bad path fails loud before any
payload runs. This is the **prerequisite** that lets agent-builder forward its `Request.Worktree`,
make exec-sandbox its default backend, and pass the live Phase-0 capstone (its held task 062).

**Reconciliation with `FileRead{paths}`.** `run.workdir` is the **writable working-directory**
mechanism; the v1 `FileRead{paths}` capability remains the (still-unimplemented) **read-only**
host-path mechanism and is unchanged by this ADR. They are complementary, not competing: one
mounts a single dir read-write at a fixed mountpoint and sets cwd; the other would mount a list
of paths read-only. `configuration.md`/`data-model.md` state this distinction so a future
`FileRead` implementation does not collide with `run.workdir`.

**Trade-offs, stated.** A writable host surface now exists; it is bounded to the single
caller-supplied directory, mounted at a fixed `/work`, with everything else still read-only and
the network still fully unshared. A payload can corrupt the worktree it was handed — inherent to
the build-and-commit job, bounded to that dir, mitigated by exposing a disposable worktree.

**Reopening condition.** If a consumer needs *multiple* host directories, or a mix of read-only
and read-write host paths, generalize `run.workdir` into a `run.mounts: [{source, target, rw}]`
list behind the same per-backend translation — and implement `FileRead{paths}` as the read-only
case of it. Until a second concrete use case exists, a single writable `/work` is the right size
(defer premature decisions).

**Spec updates land with the code in task 003.** `docs/CONTRACT.md`, `docs/spec/data-model.md`,
`docs/spec/interfaces.md`, and `docs/spec/configuration.md` gain `run.workdir` (rewritten in
place, not appended); `docs/spec/behaviors.md` gains the writable-workdir behavior (B-010); and
`docs/spec/fitness-functions.md` gains a row (F-006) asserting that only the workdir is writable
while system dirs stay read-only and the netns stays unshared.
