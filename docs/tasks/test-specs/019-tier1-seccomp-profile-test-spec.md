# Test Spec 019: Tier-1 seccomp-BPF default-deny syscall profile

**Linked task:** [`docs/tasks/backlog/019-tier1-seccomp-profile.md`](../backlog/019-tier1-seccomp-profile.md)
**ADR:** an ADR **must be written during implementation** (the seccomp-profile design: Tier-1-only scope, the deny set, the build-time cBPF pinning approach). It takes the **next available ADR number** (sequential-by-creation — ADR 011 is taken by the egress-boundaries decision, so the next free number applies; not bound to the task ID).
**Written:** 2026-06-20

## Context for the test author

Today `bwrapArgv` (`run.go:313-349`) passes **no `--seccomp`** to bubblewrap. bubblewrap applies
**zero syscall filtering** of its own — so untrusted Tier-1 code can issue *any* syscall the host
kernel exposes (`keyctl`, `add_key`, `request_key`, `ptrace`, `process_vm_readv/writev`,
`userfaultfd`, `bpf`, `perf_event_open`, the `mount`/`umount2`/`pivot_root` family,
`kexec_load`/`kexec_file_load`, `init_module`/`finit_module`/`delete_module`, `clone3` with
namespace flags, …). These are the historical launchpads for kernel LPE and container-escape. The
result is that the **default, most-used tier ships less default kernel hardening than stock
`docker run`** (which applies its own default-deny seccomp profile). This is the largest
unaddressed kernel-attack-surface gap in the project, and it sits on Tier-1.

This task closes that gap **for Tier-1 (bubblewrap) only** by passing bwrap a **default-deny +
allowlist** cBPF program via `--seccomp <fd>`. The program is modeled on the Docker/podman default
profile: allow the common-case syscalls a payload shell needs; deny the dangerous-by-default set,
returning `EPERM`/`ENOSYS`.

**Explicitly NOT Tier-2/Tier-3** (record-and-don't-relitigate):
- **Tier-2 (gVisor/runsc):** gVisor's sentry intercepts *every* guest syscall in userspace —
  gVisor **is** the syscall filter; runsc additionally installs its own host-side seccomp. A host
  bwrap-style profile there is redundant.
- **Tier-3 (Firecracker):** Firecracker self-installs its seccomp filters regardless of the jailer
  (ADR-010 Amendment 1 A1.Q3). Redundant there too.

So this task touches **only** the Tier-1 path: `bwrapArgv` and a new seccomp loader. It does **not**
modify `gvisor.go`, and the firecracker backend (tasks 013–018) is out of scope.

### Mechanism + artifact pinning (mirrors ADR-010 A1's vmlinux/rootfs pattern)

The cBPF program is **generated OFFLINE at build time** by libseccomp's `seccomp_export_bpf` from a
**plain-text JSON policy** (the source of truth, committed). The compiled cBPF blob is committed
alongside it and **byte-pinned by sha256**. At spawn, Go only `open(2)`s the blob (stdlib) and
threads the fd into the existing `exec.Command` argv — **NO Go third-party runtime dependency**;
libseccomp is **build-time tooling only**. This satisfies the project's stdlib-only +
plain-text-config principles, exactly as the ADR-010 A1 kernel/rootfs artifacts do.

**Suggested layout** (mirrors `guest/.../*.sha256` from ADR-010 A1):

```
seccomp/
  tier1-policy.json     # source of truth (committed) — allow/deny lists in plain text
  tier1.bpf             # compiled cBPF blob (committed, generated offline by libseccomp)
  tier1.bpf.sha256      # the pin — verified at load, fail-fast on mismatch
  build.sh              # build-time only: seccomp_export_bpf(policy.json) -> tier1.bpf + sha256
```

A stdlib `crypto/sha256` loader verifies `tier1.bpf` against `tier1.bpf.sha256` **before** the fd is
passed to bwrap, and **fails fast / crashes loudly** on mismatch — the project's "fail fast, crash
loudly" principle (a tampered or wrong blob is a hard error, never a silent unfiltered boot).

### Fitness function (mirror the no-`--share-net` invariant pattern)

A new fitness rule asserts (a) the Tier-1 bwrap argv contains `--seccomp`, and (b) a probe payload
invoking a blocked syscall (e.g. `keyctl`) gets `EPERM` — mirroring the existing F-001
no-`--share-net` invariant-test idiom (positive + negative case). **Coordinate with the `fitness:`
umbrella from task 009:** this rule should register into that umbrella once 009 lands (dependency
noted — this task does not re-author the umbrella; it adds one rule that 009's `make fitness`
discovers, the same way task 018's microVM rules coordinate with 009).

### Ground truth to mirror

- `bwrapArgv(scriptPath, proxySock, workdir, fileReads, env, diskBytes, finalCmd)` is the Tier-1
  argv builder (`run.go:313-349`); the `--seccomp <fd>` flag is added here, and the fd-bearing
  `*os.File` must be plumbed so the `exec.Command` keeps it open (via `ExtraFiles`, so the child fd
  number matches the argv `--seccomp <n>`).
- ADR-010 Amendment 1's artifact pin (`vmlinux.sha256` / `base.ext4.sha256`, stdlib `crypto/sha256`,
  fail-fast on mismatch) is the model for the `tier1.bpf.sha256` pin.
- F-001's positive/negative invariant-test idiom (`docs/spec/fitness-functions.md`) is the model for
  the new `--seccomp` + blocked-syscall fitness rule.

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-019-01 | The Tier-1 `bwrapArgv` passes `--seccomp <fd>` to bwrap; the fd refers to the committed, sha256-pinned cBPF blob, threaded so the child sees it at the argv-named fd number. Tier-2 (`gvisor.go`) and Tier-3 are untouched | TC-019-01, TC-019-07 | ✅ |
| REQ-019-02 | A stdlib (`crypto/sha256`) loader verifies `tier1.bpf` against `tier1.bpf.sha256` before use and **fails fast** on mismatch (hard error, no unfiltered fall-back). No Go third-party runtime dependency is added | TC-019-02, TC-019-03 | ✅ |
| REQ-019-03 | The cBPF program is **default-deny + allowlist**: a representative dangerous syscall (`keyctl`) is blocked under Tier-1 — the payload's call returns `EPERM` (or `ENOSYS` per the policy's default action) | TC-019-04 | ✅ |
| REQ-019-04 | The deny set covers the dangerous-by-default family — at minimum `keyctl`/`add_key`/`request_key`, `ptrace`/`process_vm_readv`/`process_vm_writev`, `userfaultfd`, `bpf`, `perf_event_open`, `mount`/`umount2`/`pivot_root`, `kexec_load`/`kexec_file_load`, `init_module`/`finit_module`/`delete_module` — each present in `tier1-policy.json`'s deny list (source-of-truth inspection) | TC-019-05 | ✅ |
| REQ-019-05 | The allowlist does **not** break the common-case payload: an ordinary payload (write to `/work`, exec a tool, talk to `/proxy.sock`) still runs to a normal exit under Tier-1 with the profile applied — the profile narrows, it does not brick the shell | TC-019-06 | ✅ |
| REQ-019-06 | `tier1-policy.json` is the plain-text source of truth and `tier1.bpf` is its compiled output: rebuilding the blob from the committed policy (build.sh) reproduces the committed `tier1.bpf.sha256` (the pin is honest — the blob matches the policy) | TC-019-08 | ✅ |
| REQ-019-07 | A fitness rule asserts (a) the Tier-1 argv carries `--seccomp` and (b) a blocked-syscall probe returns `EPERM`; it has a **negative** case proving it bites (an argv lacking `--seccomp`, or an unfiltered run where `keyctl` would succeed, is rejected). Registered to coordinate with task 009's `fitness:` umbrella | TC-019-09, TC-019-10 | ✅ |
| REQ-019-08 | Spec is updated in the same commit as the implementation: SPEC.md invariants / `behaviors.md` B-008 (or the backend-selection behavior) state that Tier-1 applies a default-deny seccomp profile; `fitness-functions.md` gains the new rule row; `configuration.md`/`data-model.md` note the pinned artifact if surfaced. Present tense, rewritten in place, no future tense | TC-019-11 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] The fitness negative case (an argv without `--seccomp`, an unfiltered `keyctl` success) is specified so the invariant check is provably not a no-op
- [x] The sha256 mismatch fail-fast case is specified (no silent unfiltered fall-back)
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: **Tier-1 only** — `gvisor.go` untouched, firecracker out of scope
- [x] Confirmed: **no Go third-party runtime dependency** — libseccomp is build-time tooling; Go only `open(2)`s the blob

---

## Test cases

### TC-019-01: `bwrapArgv` carries `--seccomp <fd>`

- **Requirement:** REQ-019-01
- **Type:** unit (Go test)
- **Input:** build a baseline Tier-1 argv via `bwrapArgv(...)` with the seccomp loader wired.
- **Expected:** the argv contains the flag `--seccomp` immediately followed by a numeric fd token
  (e.g. `--seccomp 10`). The corresponding `*os.File` is plumbed into the spawn (`cmd.ExtraFiles`)
  so the child's fd number equals the argv token. `--unshare-all` is still present and `--share-net`
  is still absent (the new flag adds to, never weakens, the no-network invariant — re-assert F-001 on
  this shape).

### TC-019-02: sha256 loader accepts the matching blob

- **Requirement:** REQ-019-02
- **Type:** unit (Go test)
- **Input:** the committed `seccomp/tier1.bpf` and `seccomp/tier1.bpf.sha256`.
- **Expected:** the loader opens the blob, computes its `crypto/sha256` digest, finds it equal to the
  pinned value, and returns an open fd / `*os.File` with no error. No third-party import is used
  (stdlib `crypto/sha256` + `os` only).

### TC-019-03: sha256 loader fails fast on a mismatched blob (no unfiltered fall-back)

- **Requirement:** REQ-019-02
- **Type:** unit (Go test, negative)
- **Input:** point the loader at a blob whose bytes do not match the pinned `tier1.bpf.sha256` (a
  tampered/truncated fixture).
- **Expected:** the loader returns a non-nil error (or panics per "crash loudly") and **does not**
  return a usable fd. A run that cannot load a verified profile **fails** — it never falls back to
  spawning bwrap **without** `--seccomp`. Assert the run errors rather than silently running
  unfiltered.

### TC-019-04: a blocked syscall returns EPERM under Tier-1 (the crux)

- **Requirement:** REQ-019-03
- **Type:** integration (Go test, **L6**, skips without bwrap)
- **Input:** run a probe payload through `Run` on Tier-1 (`tier: ""`/`bubblewrap`) that invokes a
  blocked syscall — e.g. a tiny program calling `keyctl(KEYCTL_GET_KEYRING_ID, ...)` (or `syscall`
  directly), reporting the returned errno.
- **Expected:** the payload observes `EPERM` (or `ENOSYS` if the policy's default action is
  `SCMP_ACT_ERRNO(ENOSYS)`/`SCMP_ACT_KILL` — assert the policy-chosen action) from the blocked call.
  The same probe run **without** the seccomp profile would succeed or return a different errno
  (contrast asserted in TC-019-10's negative). This is the load-bearing behavioral proof that the
  filter is actually installed and biting.

### TC-019-05: the deny set is present in the plain-text policy

- **Requirement:** REQ-019-04
- **Type:** inspection (policy file)
- **Input:** parse `seccomp/tier1-policy.json`.
- **Expected:** the deny list contains (at minimum) every name in the required family: `keyctl`,
  `add_key`, `request_key`, `ptrace`, `process_vm_readv`, `process_vm_writev`, `userfaultfd`, `bpf`,
  `perf_event_open`, `mount`, `umount2`, `pivot_root`, `kexec_load`, `kexec_file_load`,
  `init_module`, `finit_module`, `delete_module`. (A table-driven test over the required set,
  asserting each is denied by the policy.)

### TC-019-06: a common-case payload still runs to a normal exit

- **Requirement:** REQ-019-05
- **Type:** integration (Go test, **L6**, skips without bwrap)
- **Input:** run an ordinary payload under Tier-1 with the profile applied — write a file to `/work`,
  exec a small tool, and make a proxied request to `/proxy.sock`.
- **Expected:** the run completes with a normal exit code and the expected output; `/work` write
  persists; the proxied request behaves exactly as it does today (the profile allows the common-case
  syscalls a shell + the proxy client need). The profile **narrows** the surface without bricking the
  payload.

### TC-019-07: `gvisor.go` is untouched; Tier-2/Tier-3 do not get `--seccomp`

- **Requirement:** REQ-019-01
- **Type:** unit (Go test) + diff inspection
- **Input:** build the gVisor OCI spec / argv; inspect `git diff` for `gvisor.go`.
- **Expected:** the gVisor backend argv/spec is byte-for-byte unchanged by this task (no `--seccomp`
  added there — gVisor already filters every syscall). `gvisor.go` has a zero diff. The firecracker
  backend (if present) is likewise untouched.

### TC-019-08: the pinned blob is honestly built from the policy

- **Requirement:** REQ-019-06
- **Type:** build-reproducibility (optional-on-host; skips if libseccomp tooling absent)
- **Input:** run `seccomp/build.sh` to regenerate `tier1.bpf` from `tier1-policy.json`; recompute its
  sha256.
- **Expected:** the regenerated blob's sha256 equals the committed `tier1.bpf.sha256` — proving the
  committed blob is the faithful compilation of the committed policy (the pin is not stale or
  hand-edited). If the build host lacks libseccomp, the test **skips** (mirroring the bwrap/runsc
  skip idiom) — it never silently passes without checking.

### TC-019-09: fitness rule — Tier-1 argv carries `--seccomp` (positive)

- **Requirement:** REQ-019-07
- **Type:** fitness (Go test, L3)
- **Input:** the baseline Tier-1 argv from `bwrapArgv`.
- **Expected:** the fitness assertion helper confirms `--seccomp` is present (and `--unshare-all`
  present / `--share-net` absent). Passes on current code after implementation.

### TC-019-10: fitness rule — the check bites (negative)

- **Requirement:** REQ-019-07
- **Type:** fitness (Go test, negative)
- **Input:** feed the fitness assertion helper a **constructed** Tier-1 argv with `--seccomp`
  removed (simulating a regression), and/or a probe-run path where the profile was not installed.
- **Expected:** the assertion helper returns a non-nil error / the test fails — proving the rule is
  not vacuous (an argv missing `--seccomp`, or an unfiltered run where the blocked `keyctl` probe
  would *succeed*, is rejected). Mirrors the F-001 positive/negative idiom and task 009's pattern.

### TC-019-11: spec updated in the same commit (present tense, in place)

- **Requirement:** REQ-019-08
- **Type:** inspection (spec)
- **Input:** read `docs/spec/SPEC.md`, `docs/spec/behaviors.md` (B-008 / backend selection),
  `docs/spec/fitness-functions.md` after the feat commit.
- **Expected:** the Tier-1 description states that bubblewrap runs with a default-deny seccomp profile
  passed via `--seccomp` (no longer "no syscall filtering"); `fitness-functions.md` has the new rule
  row; any surfaced artifact (`seccomp/tier1.bpf` pin) is noted in `configuration.md`/`data-model.md`
  if it becomes externally visible. All rewritten in place, present tense, no future-tense roadmap
  language.

---

## Post-implementation verification

- [ ] TC-019-01: `bwrapArgv` carries `--seccomp <fd>`; `--unshare-all` kept, no `--share-net`
- [ ] TC-019-02: sha256 loader accepts the matching blob (stdlib only)
- [ ] TC-019-03: sha256 loader fails fast on mismatch — no unfiltered fall-back
- [ ] TC-019-04: blocked `keyctl` returns EPERM under Tier-1 (load-bearing, L6)
- [ ] TC-019-05: the deny set is present in `tier1-policy.json`
- [ ] TC-019-06: a common-case payload still runs to a normal exit
- [ ] TC-019-07: `gvisor.go` untouched; Tier-2/Tier-3 do not get `--seccomp`
- [ ] TC-019-08: the pinned blob is honestly built from the policy (skips without libseccomp)
- [ ] TC-019-09: fitness rule positive — argv carries `--seccomp`
- [ ] TC-019-10: fitness rule negative — the check bites
- [ ] TC-019-11: spec rewritten in place, present tense

## Test framework notes

- Standard Go `testing`. The behavioral cases (TC-019-04, -06) are integration tests that **skip
  without bwrap** (the project's `requireBwrap` idiom) — but when bwrap is present they are the
  load-bearing L6 evidence.
- Keep the `--seccomp`/blocked-syscall assertion in a single fitness helper so task 009's `fitness:`
  umbrella can register it (the Tier-1 analogue of F-001). The negative case (TC-019-10) calls that
  helper on a `--seccomp`-stripped argv.
- The seccomp loader + the new flag wiring go in a new file (e.g. `seccomp.go`) and tests in
  `seccomp_test.go`; `bwrapArgv` in `run.go` gains the `--seccomp <fd>` flag + `ExtraFiles` plumbing.
  Do **not** modify `gvisor.go`.
- **No Go third-party runtime dependency.** libseccomp is invoked only by `seccomp/build.sh` at build
  time; Go reads the resulting blob with stdlib `os` + `crypto/sha256`.
