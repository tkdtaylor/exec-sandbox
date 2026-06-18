# Task 003: add a writable working-directory mount to `run()`

**Status:** ✅ verified
**Branch:** `task/003-writable-workdir-mount`
**Spec:** [`docs/tasks/test-specs/003-writable-workdir-mount-test-spec.md`](../test-specs/003-writable-workdir-mount-test-spec.md)
**ADR:** [`docs/architecture/decisions/004-writable-workdir-mount.md`](../../architecture/decisions/004-writable-workdir-mount.md)

## Problem

`run()` runs the payload with a **fully read-only** view of the host (`/usr`, `/etc` ro,
`/payload.sh` ro, `/proxy.sock`, a throwaway `/tmp` tmpfs). There is **no way to expose a host
directory** for the payload to operate on. The consumer (**agent-builder**) needs to build, test,
lint, and *commit* against a checked-out repo worktree — a **write** workload against a host dir,
impossible until `run()` can mount one. The contract's `FileRead{paths}` capability is read-only
by name/intent and unimplemented; the consumer needs **write** access, so this is a distinct
writable-workdir mount, not read-only `FileRead`.

Reference: agent-builder's launcher (`containment/execution-box/run.sh:507,521,473-474`,
`internal/sandbox/podman/run.go` `validateWorktree`) — adapted to exec-sandbox's non-Podman
backends per ADR 004.

## Scope

- Add `run.workdir` (host path string) to `RunRequest`.
- Validate it before spawn (`validateWorkdir`): blank → no mount (backward compatible); non-blank
  must resolve to an existing absolute directory, else a hard `{error}` (no silent fall-back).
- bubblewrap: `--bind <workdir> /work` (writable, **not** `--ro-bind`) + `--chdir /work`.
- gVisor: writable OCI `/work` bind mount (`options` without `ro`) + `process.cwd = "/work"`
  (`applyWorkdirToOCISpec`, mirroring `applyLimitsToOCISpec`).
- Keep `--unshare-all`/`--die-with-parent`/`--clearenv` and the no-network invariant; only the
  worktree becomes writable — system dirs stay read-only.
- Update `docs/CONTRACT.md`, `docs/spec/{data-model,interfaces,configuration,behaviors,
  fitness-functions}.md` in the same commit (rewrite in place; reconcile with `FileRead{paths}`).

Out of scope: multiple host mounts / mixed ro+rw `run.mounts` list (ADR 004 reopening condition);
implementing `FileRead{paths}`.

## Verification plan

- **Highest level achievable: L5/L6.** This host has **both** `bwrap` and `runsc`, so the
  read-back, write-persist, and cwd assertions run end-to-end under real sandboxes on both tiers.
- **Harness command:** `go test -count=1 ./...`
- **Runtime observation (L6):** a file seeded in the host workdir is read by the payload at
  `/work`; a file the payload writes under `/work` is present in the host dir after the run; the
  payload's `pwd` is `/work`; an absent `run.workdir` leaves `/work` nonexistent and prior behavior
  intact; a nonexistent / non-dir `run.workdir` returns `{error}` and runs nothing. Observed under
  both bwrap and runsc.
- **Fitness (L3):** new F-006 row asserts "only the workdir is writable; system dirs remain ro;
  netns stays unshared"; check command is the workdir test set.

## Definition of done

- `run()` accepts a host workdir, bind-mounts it writable at `/work` with cwd `/work` on **both**
  tiers; a payload reads+writes the host worktree; absent-workdir is backward compatible; the
  bad-path case fails loud.
- Spec reflects the new `run.workdir` field (CONTRACT/data-model/interfaces/configuration/behaviors
  rewritten in place; `FileRead{paths}` reconciled).
- F-006 fitness row added (invariant + check command + asserting test).
- spec-verifier APPROVE before promotion to ✅.
