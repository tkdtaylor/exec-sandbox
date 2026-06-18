# Test Spec 004: `FileRead{paths}` read-only host mounts + payload PATH/env provisioning

**Linked task:** [`docs/tasks/backlog/004-toolchain-mount-and-path.md`](../backlog/004-toolchain-mount-and-path.md)
**ADR:** ADR 005 (to be written during implementation)
**Written:** 2026-06-18

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-001 | `FileRead{paths}` entries are parsed from `run.profile.capabilities` (mirroring `netAllowlist`): `{"type":"FileRead","paths":[…]}`; multiple entries union their paths; absent ⇒ empty | TC-001 (unit) | ⏳ |
| REQ-002 | Each FileRead path is validated before spawn: must be **absolute** and **exist**; a relative or nonexistent path is a hard `{error}` (no run, no silent skip); empty ⇒ no mounts | TC-002 (unit), TC-007 (bwrap, before spawn) | ⏳ |
| REQ-003 | A FileRead path is bind-mounted **read-only** at the same path on **both** backends — a marker file/exe is **readable/executable** by the payload there | TC-003 (bwrap), TC-009 (gvisor) | ⏳ |
| REQ-004 | The caller env input (`run.env`/`run.path`) provisions the payload's PATH on **both** backends — a tool in a FileRead-mounted dir on PATH resolves via `command -v` | TC-004 (bwrap), TC-009 (gvisor), TC-010 (spec) | ⏳ |
| REQ-005 | FileRead mounts are **read-only**: a write to a FileRead path **fails** (EROFS/permission), distinct from `/work` which is writable; the argv/spec use the `ro` form (`--ro-bind` / `options:[ro,rbind]`) | TC-005 (bwrap), TC-008 (argv/spec unit) | ⏳ |
| REQ-006 | Absent `FileRead`/env ⇒ no extra mounts, bare `PATH=/usr/bin:/bin`, prior behavior byte-for-byte preserved (backward compatible) | TC-006 (bwrap), TC-008 (argv/spec), TC-011 (regression) | ⏳ |
| REQ-007 | The network namespace stays **unshared** with FileRead mounted: `--unshare-all` kept, no `--share-net`; OCI `network` namespace path-less; the only egress is still `/proxy.sock` | TC-008 (argv/spec unit) | ⏳ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Edge cases and error paths are covered
- [x] Every REQ-ID has at least one test case
- [x] Success criteria are unambiguous
- [x] Reference pattern (agent-builder `resolve_gate_tools` + `--mount …,ro` + `gate_tool_path` PATH) reproduced before authoring

---

## Test cases

### TC-001: `FileRead{paths}` parsing from `profile.capabilities`

- **Requirement:** REQ-001
- **Type:** unit (no sandbox)
- **Input:** a `profile` with (a) one `{"type":"FileRead","paths":["/a","/b"]}` entry; (b) two
  `FileRead` entries `["/a"]` and `["/c"]` alongside a `NetConnect` entry; (c) no `FileRead` entry.
- **Expected:** (a) the parser returns `["/a","/b"]`. (b) returns the **union** `["/a","/c"]` and is
  unaffected by the `NetConnect` entry (FileRead parser ignores non-FileRead types, mirroring how
  `netAllowlist` ignores non-NetConnect). (c) returns empty/nil. Order within a single entry is
  preserved.
- **Edge cases:** a `FileRead` entry with an empty/missing `paths` contributes nothing.

### TC-002: FileRead path validation rejects relative / nonexistent, accepts absolute-existing

- **Requirement:** REQ-002
- **Type:** unit (no sandbox)
- **Input:** (a) an existing absolute dir; (b) an existing absolute file; (c) a **relative** path
  (e.g. `"rel/dir"`); (d) an absolute path that does **not** exist.
- **Expected:** (a) and (b) validate (no error). (c) returns a non-nil error naming `FileRead`
  (relative paths are rejected — distinct from `validateWorkdir`, which canonicalizes a relative
  workdir; FileRead requires already-absolute paths). (d) returns a non-nil error naming the missing
  path. The run must not proceed on (c)/(d) — no silent skip.
- **Edge cases:** an empty path list validates as "no mounts" (no error).

### TC-003: a FileRead-mounted marker file is readable/executable by the payload (bwrap)

- **Requirement:** REQ-003
- **Type:** integration (bwrap; `requireBwrap`)
- **Input:** a host dir `<tools>` containing an executable script `mytool` that prints `tool-ran`;
  a run with `FileRead` paths `["<tools>"]` and payload `<tools>/mytool` (invoked by its absolute,
  same-as-host path).
- **Expected:** `exit_code == 0` and `stdout` contains `tool-ran` — the host dir is visible inside
  the sandbox at the **same** path and the marker is executable.
- **Edge cases:** none — this is the read/execute path.

### TC-004: a FileRead-mounted tool on PATH resolves via `command -v` (bwrap)

- **Requirement:** REQ-004
- **Type:** integration (bwrap)
- **Input:** a host dir `<tools>` containing an executable `mytool`; a run with `FileRead` paths
  `["<tools>"]`, env that puts `<tools>` on PATH (`run.env = {"PATH":"<tools>:/usr/bin:/bin"}`, or
  `run.path = ["<tools>"]` per the chosen shape), and payload `command -v mytool`.
- **Expected:** `exit_code == 0`; `stdout` (trimmed) equals `<tools>/mytool` — the tool resolves on
  PATH from the FileRead-mounted dir. This is the parity assertion: a curated toolchain dir mounted
  read-only and placed on PATH makes its tools invocable by name, like agent-builder's gate-tools.
- **Edge cases:** without the env input, `command -v mytool` would fail (PATH is bare) — the env
  input is what makes the mount useful.

### TC-005: a write to a FileRead mount fails — read-only, distinct from writable `/work` (bwrap)

- **Requirement:** REQ-005
- **Type:** integration (bwrap)
- **Input:** a host dir `<tools>` (FileRead) **and** a host dir `<work>` (`run.workdir`); payload
  that (a) `echo x > /work/ok.txt` (succeeds) and (b) `echo x > <tools>/evil.txt` (must fail).
- **Expected:** the `/work` write succeeds (writable workdir from task 003); the write under the
  FileRead path **fails** with a read-only-filesystem / permission error — proving `--ro-bind`, not
  `--bind`. After the run, `<tools>/evil.txt` does **not** exist on the host.
- **Edge cases:** the failure is observable in the payload's exit status or stderr; the host file is
  the ground-truth proof the mount was read-only.

### TC-006: absent FileRead/env ⇒ no extra mounts, bare PATH, behavior unchanged (bwrap)

- **Requirement:** REQ-006
- **Type:** integration (bwrap)
- **Input:** a run with **no** `FileRead` capability and **no** env input; payload
  `echo "$PATH"` (and a `command -v mytool` that must fail).
- **Expected:** `exit_code == 0`; `stdout` shows `PATH=/usr/bin:/bin` (the bare default); the
  `command -v mytool` resolves nothing. The constructed bwrap argv contains **no** extra
  `--ro-bind` for a FileRead path and only the default `--setenv PATH /usr/bin:/bin`.
- **Edge cases:** an empty `paths` list and an empty env map are both treated as absent.

### TC-007: a nonexistent / relative FileRead path fails loud before spawn (bwrap path)

- **Requirement:** REQ-002
- **Type:** integration-ish (fails before spawn) asserted via `Run`
- **Input:** (a) `FileRead` paths `["/does/not/exist"]`; (b) `FileRead` paths `["rel/tools"]`.
- **Expected:** in both cases `Run` returns `{error: …}` naming `FileRead`/the bad path — there is
  **no** `stdout`/`sandbox_status` (the payload never ran). The check happens **before** the proxy
  starts / vault is called, so a malformed FileRead cannot trigger any side effect (same ordering as
  `validateWorkdir` in task 003).
- **Edge cases:** no silent fall-back to "run without the mount".

### TC-008: argv/spec carry read-only FileRead mounts + provisioned PATH; netns stays unshared; empty ⇒ base unchanged

- **Requirement:** REQ-005, REQ-006, REQ-007
- **Type:** unit (argv + OCI spec inspection; no sandbox)
- **Input:** (a) `bwrapArgv` / the OCI builder with FileRead paths `["/abs/tools"]` and env
  `{"PATH":"/abs/tools:/usr/bin:/bin"}`; (b) the same with **no** FileRead and **no** env.
- **Expected:** (a) the bwrap argv contains `--ro-bind /abs/tools /abs/tools` (the **read-only**
  form, **not** `--bind`) and a `--setenv PATH /abs/tools:/usr/bin:/bin`; it still carries
  `--unshare-all` and adds **no** `--share-net`. The OCI spec gains a mount
  `{destination:/abs/tools, source:/abs/tools, options:[ro,rbind]}` (contains `"ro"`) and a
  `process.env` entry `PATH=/abs/tools:/usr/bin:/bin`; the `network` namespace is still present with
  no path (unshared) and the only socket bind is still `/proxy.sock`. (b) with no FileRead/env, the
  argv/spec are byte-for-byte the base (default `--setenv PATH /usr/bin:/bin`, no extra mounts) — the
  base `gvisorOCISpec` and `bwrapArgv` outputs are unchanged from before this task.
- **Edge cases:** the existing read-only system mounts (`/usr`, `/etc`, …) and the writable `/work`
  mount (when present) are untouched by the FileRead mutation; FileRead never emits the writable
  `--bind` form.

### TC-009: FileRead mount + PATH provisioning works end-to-end under gVisor

- **Requirement:** REQ-003, REQ-004 (gvisor path)
- **Type:** integration (runsc; `requireRunsc`)
- **Input:** a host dir `<tools>` with an executable `mytool` printing `tool-ran`; a `tier:"gvisor"`
  run with `FileRead` paths `["<tools>"]`, env putting `<tools>` on PATH, and payload
  `command -v mytool; mytool`.
- **Expected:** `exit_code == 0`; `stdout` contains `<tools>/mytool` (resolved on PATH) and
  `tool-ran` (executed). Proves the OCI read-only `<tools>` bind + `process.env` PATH enforce the
  same semantics as bwrap. `sandbox_status.tier == "gvisor"`.
- **Edge cases:** skips cleanly when runsc is absent.

### TC-010: the gVisor OCI spec carries the read-only FileRead mounts and provisioned env; empty ⇒ base unchanged

- **Requirement:** REQ-004 (host-side record), REQ-005, REQ-006
- **Type:** unit (no runsc; inspects the generated spec)
- **Input:** `applyFileReadToOCISpec(spec, ["/abs/tools"])` + the env merge applied to a base
  `gvisorOCISpec`; and the empty case `applyFileReadToOCISpec(spec, nil)` with no env.
- **Expected:** with FileRead paths, `mounts` gains `{destination:/abs/tools, type:bind,
  source:/abs/tools, options:[ro,rbind]}` (options **contain** `"ro"` — read-only) and `process.env`
  carries the provisioned `PATH`. With no paths and no env, the spec is **unchanged** — no extra
  mount, `process.env` stays `["PATH=/usr/bin:/bin"]`. The base `gvisorOCISpec(scriptPath, proxySock)`
  signature is unchanged, so existing `TestGvisorSpec*` tests stay green.
- **Edge cases:** read-only system mounts and any writable `/work` mount are untouched.

### TC-011: no-FileRead / no-env runs are byte-for-byte unchanged (regression guard)

- **Requirement:** REQ-006
- **Type:** integration (bwrap) + unit — the existing `run_test.go` / `workdir_test.go` suites
- **Expected:** `go build ./... && go test ./...` green; `TestSandboxReachesAllowlistedHostViaProxy`,
  `TestProxyBlocksNonAllowlistedHost`, `TestNetAllowlistParsing`, the `gvisor_test.go`
  `TestGvisorSpec*`/`TestBackendFor*`, `limits_test.go`, and the task-003 `workdir_test.go` tests all
  pass. A request with no `FileRead` and no env produces the same argv/spec as before this task.
- **Edge cases:** none — this is the regression guard. `run_test.go` and `workdir_test.go` are
  **not** edited (only the `Backend.Argv` callers gain the new FileRead/env parameters as a
  mechanical signature update, passing empties).

---

## Post-implementation verification

- [ ] Unit TCs pass everywhere (TC-001, TC-002, TC-008, TC-010)
- [ ] bwrap integration TCs pass on a box with bwrap (TC-003..007, TC-011)
- [ ] gVisor integration TC passes on a box with runsc (TC-009), skips cleanly otherwise
- [ ] L5/L6: real read/execute, `command -v` resolution, read-only-write-failure, and unshared-netns
      observed on this host (bwrap **and** runsc)
- [ ] No regressions in existing tests (TC-011)

## Test framework notes

- Standard Go `testing`. Reuse `requireBwrap` / `requireRunsc`. Build requests with a helper that
  sets a `FileRead` capability and the env input on a minimal (no-NetConnect) profile, since these
  tests do not need egress.
- TC-005 reuses the task-003 `run.workdir` to contrast a writable `/work` against the read-only
  FileRead mount in one run — the host-file check after `Run` is the ground-truth proof.
- New tests live in a new file (`fileread_test.go`); `run_test.go` and `workdir_test.go` are **not**
  modified. The `Backend.Argv` signature gains the FileRead paths + env parameters — the existing
  `limits_test.go` / `workdir_test.go` calls to `Argv(...)` gain the new parameters (mechanical
  update; they pass `nil`/empty).
