# Test Spec 003: writable working-directory mount in `run()`

**Linked task:** [`docs/tasks/active/003-writable-workdir-mount.md`](../active/003-writable-workdir-mount.md)
**ADR:** [`docs/architecture/decisions/004-writable-workdir-mount.md`](../../architecture/decisions/004-writable-workdir-mount.md)
**Written:** 2026-06-18

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-001 | `run.workdir` is validated before spawn: blank ⇒ no mount; non-blank must resolve to an existing absolute **directory**, else a hard `{error}` (no run) | TC-001 (unit), TC-006 (bwrap) | ⏳ |
| REQ-002 | A non-empty `run.workdir` bind-mounts the host dir **writable** at `/work` on **both** backends — a seeded file is readable and a payload-written file persists to the host | TC-002, TC-003 (bwrap), TC-007 (gvisor) | ⏳ |
| REQ-003 | The payload's cwd is `/work` on **both** backends | TC-004 (bwrap), TC-007 (gvisor), TC-008 (gvisor spec) | ⏳ |
| REQ-004 | Absent `run.workdir` ⇒ **no** `/work` mount, cwd unchanged, prior behavior byte-for-byte preserved (backward compatible) | TC-005 (bwrap), TC-008 (spec), TC-010 (regression) | ⏳ |
| REQ-005 | Only `/work` is writable: system dirs stay read-only and the network namespace stays unshared; the mount is `--bind`/non-`ro`, with `--chdir /work` | TC-009 (bwrap + spec) | ⏳ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Edge cases and error paths are covered
- [x] Every REQ-ID has at least one test case
- [x] Success criteria are unambiguous
- [x] Reference pattern (agent-builder `validateWorktree` + `/work,rw` + `--workdir /work`) reproduced before authoring

---

## Test cases

### TC-001: `validateWorkdir` resolves a good dir and rejects blank/missing/non-dir

- **Requirement:** REQ-001
- **Type:** unit (no sandbox; runs everywhere)
- **Input:** (a) `""` and `"   "` (blank); (b) a real temp dir (possibly relative); (c) a path that
  does not exist; (d) a path to a regular file (not a dir).
- **Expected:** (a) returns `("", nil)` — "no workdir mount", no error. (b) returns the **absolute**
  path (`filepath.Abs` of the input), no error — a relative input is canonicalized. (c) and (d)
  return a non-nil error naming `run.workdir` (the run must not proceed). Mirrors agent-builder's
  `validateWorktree` (trim → abs → stat → IsDir).
- **Edge cases:** a relative `.` resolves to an absolute dir; a trailing-slash dir path is accepted.

### TC-002: a file seeded in the host workdir is readable by the payload at `/work` (bwrap)

- **Requirement:** REQ-002 (read)
- **Type:** integration (bwrap; `requireBwrap`)
- **Input:** a host temp dir containing `seed.txt` with contents `hello-from-host`; a run with
  `run.workdir = <that dir>` and payload `cat /work/seed.txt`.
- **Expected:** `exit_code == 0` and `stdout` contains `hello-from-host` — the host file is visible
  inside the sandbox at `/work`.
- **Edge cases:** none — this is the read path.

### TC-003: a file the payload writes under `/work` persists to the host dir after the run (bwrap)

- **Requirement:** REQ-002 (write)
- **Type:** integration (bwrap)
- **Input:** an empty host temp dir; a run with `run.workdir = <that dir>` and payload
  `echo built > /work/out.txt`.
- **Expected:** `exit_code == 0`; **after** `Run` returns, the host file `<dir>/out.txt` exists and
  contains `built`. This proves the mount is **read-write** (a `--ro-bind` would fail the write).
- **Edge cases:** the write also proves the rootfs being read-only does not block writes under the
  explicitly-writable `/work` mount.

### TC-004: the payload's current directory is `/work` (bwrap)

- **Requirement:** REQ-003
- **Type:** integration (bwrap)
- **Input:** a host temp dir; a run with `run.workdir = <dir>` and payload `pwd` (and, as a second
  signal, `echo marker > rel.txt` writing a **relative** path).
- **Expected:** `stdout` trimmed equals `/work`; the relative-path write lands at `<dir>/rel.txt`
  on the host (cwd is `/work`, so a relative write resolves under the workdir).
- **Edge cases:** none.

### TC-005: absent `run.workdir` ⇒ no `/work` mount, prior behavior preserved (bwrap)

- **Requirement:** REQ-004
- **Type:** integration (bwrap)
- **Input:** a run with **no** `run.workdir` (empty) and payload `test ! -e /work && echo no-work`
  followed by `pwd`.
- **Expected:** `exit_code == 0`; `stdout` contains `no-work` (there is no `/work` inside the
  sandbox); `pwd` is **not** `/work` (cwd is unchanged from today). The constructed bwrap argv
  contains **no** `--bind … /work` and **no** `--chdir`.
- **Edge cases:** an all-whitespace `run.workdir` is treated as absent (no mount).

### TC-006: a nonexistent / non-directory `run.workdir` fails loud (bwrap path, before spawn)

- **Requirement:** REQ-001
- **Type:** integration-ish (no real sandbox needed — fails before spawn) but asserted via `Run`
- **Input:** (a) `run.workdir` = a path that does not exist; (b) `run.workdir` = a path to a regular
  file.
- **Expected:** in both cases `Run` returns `{error: "invalid run.workdir: …"}` — there is **no**
  `stdout`/`sandbox_status` (the payload never ran). The error names `run.workdir`. No silent
  fall-back to "run without the mount".
- **Edge cases:** the bad-path check happens **before** the proxy starts / vault is called, so a
  malformed workdir cannot trigger any side effect.

### TC-007: the workdir mount works end-to-end under gVisor (read + write-persist + cwd)

- **Requirement:** REQ-002, REQ-003 (gvisor path)
- **Type:** integration (runsc; `requireRunsc`)
- **Input:** a host temp dir seeded with `seed.txt = hello-from-host`; a `tier: "gvisor"` run with
  `run.workdir = <dir>` and payload `cat /work/seed.txt; pwd; echo built > /work/out.txt`.
- **Expected:** `exit_code == 0`; `stdout` contains `hello-from-host` and a `/work` line (cwd);
  after the run, `<dir>/out.txt` on the host contains `built`. Proves the OCI writable `/work`
  bind + `process.cwd = /work` enforce the same semantics as bwrap. `sandbox_status.tier == "gvisor"`.
- **Edge cases:** skips cleanly when runsc is absent.

### TC-008: the gVisor OCI spec carries the writable `/work` mount and cwd; empty ⇒ base unchanged

- **Requirement:** REQ-003, REQ-004 (host-side record)
- **Type:** unit (no runsc; inspects the generated spec)
- **Input:** `applyWorkdirToOCISpec(spec, "/abs/work")` applied to a base `gvisorOCISpec`; and the
  empty case `applyWorkdirToOCISpec(spec, "")`.
- **Expected:** with a non-empty workdir, `mounts` gains an entry `{destination:/work, type:bind,
  source:/abs/work}` whose `options` **do not contain `"ro"`** (writable), and `process.cwd ==
  "/work"`. With an empty workdir, the spec is **unchanged** — no `/work` mount, `process.cwd`
  stays `"/"`. The base `gvisorOCISpec(scriptPath, proxySock)` signature is unchanged, so existing
  `TestGvisorSpec*` tests remain green.
- **Edge cases:** the existing read-only mounts (`/usr`, `/etc`, `/payload.sh`, `/proxy.sock`) are
  untouched by `applyWorkdirToOCISpec`.

### TC-009: only `/work` is writable — system dirs stay ro, netns stays unshared

- **Requirement:** REQ-005
- **Type:** integration (bwrap) + unit (argv/spec inspection)
- **Input:** a run with `run.workdir = <dir>` and a payload that (a) writes `/work/ok.txt`
  (succeeds), (b) attempts `echo x > /usr/x.txt` (must fail — system dir read-only), and (c) a unit
  assertion on the constructed bwrap argv and OCI spec.
- **Expected:** (a) the `/work` write succeeds; (b) the `/usr` write fails (read-only filesystem) —
  the writable surface is confined to `/work`. (c) the bwrap argv contains `--bind <workdir> /work`
  (writable form, **not** `--ro-bind`) and `--chdir /work`, still carries `--unshare-all`, and adds
  **no** `--share-net`; the OCI spec's `/work` mount options omit `"ro"` while `/usr`/`/etc` keep
  `"ro"`, and the `network` namespace is still present with no path (unshared). The no-network
  invariant holds with the workdir mounted.
- **Edge cases:** the workdir mount does not add any egress path; the only socket bind-mounted in is
  still `/proxy.sock`.

### TC-010: no-workdir runs are byte-for-byte unchanged (regression guard)

- **Requirement:** REQ-004
- **Type:** integration (bwrap) + unit — the existing, **unmodified** `run_test.go` suite
- **Expected:** `go build ./... && go test ./...` green; `TestSandboxReachesAllowlistedHostViaProxy`,
  `TestProxyBlocksNonAllowlistedHost`, `TestNetAllowlistParsing`, and all `gvisor_test.go`
  `TestGvisorSpec*`/`TestBackendFor*` and `limits_test.go` tests pass. A request with no
  `run.workdir` produces the same argv/spec as before this task (modulo the unchanged limits work).
- **Edge cases:** none — this is the regression guard. `run_test.go` is **not** edited.

---

## Post-implementation verification

- [ ] Unit TCs pass everywhere (TC-001, TC-008)
- [ ] bwrap integration TCs pass on a box with bwrap (TC-002..006, TC-009, TC-010)
- [ ] gVisor integration TC passes on a box with runsc (TC-007), skips cleanly otherwise
- [ ] L5/L6: real read-back, write-persist, and cwd==/work observed on this host (bwrap **and** runsc)
- [ ] No regressions in existing tests (TC-010)

## Test framework notes

- Standard Go `testing`. Reuse `requireBwrap` / `requireRunsc`. Build requests with a small helper
  that sets `req.Run.Workdir` and a minimal (no-NetConnect) profile, since these tests do not need
  egress.
- Write-persistence assertions read the **host** file after `Run` returns (the mount is the proof).
- New tests live in a new file (`workdir_test.go`); `run_test.go` is **not** modified. The two
  `limits_test.go` calls to `gvisorBackend{}.Argv(...)` gain the new `workdir` parameter (mechanical
  signature update — they pass `""`).
