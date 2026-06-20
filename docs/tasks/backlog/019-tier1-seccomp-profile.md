# Task 019: Tier-1 seccomp-BPF default-deny syscall profile

**Status:** ⬜ backlog
**Branch:** `task/019-tier1-seccomp-profile`
**Spec:** [`docs/tasks/test-specs/019-tier1-seccomp-profile-test-spec.md`](../test-specs/019-tier1-seccomp-profile-test-spec.md)
**ADR:** **required during implementation** — a custom default-deny seccomp profile is a significant security decision. Write the ADR (the seccomp-profile design: Tier-1-only scope + the rejected-for-Tier-2/3 rationale, the deny set, the build-time cBPF generation + sha256 pinning approach) taking the **next available ADR number** (sequential-by-creation; not bound to the task ID — ADR 011 is already taken by the egress-boundaries decision). Land the ADR commit before the feat commit.

## Readiness

**📋 READY (design settled).** A primary-source research pass settled the scope, mechanism, and
artifact-pinning approach (below) — there is **no open-question block**. The work is: a plain-text
JSON policy, a build-time-generated + sha256-pinned cBPF blob, a stdlib loader, the `--seccomp <fd>`
plumbing in `bwrapArgv`, and a fitness rule. The ADR is a *write-during-implementation* obligation
(recording a settled decision), not an unresolved gate.

**Dependency-coordination:** the new fitness rule should register into **task 009's `fitness:`
umbrella** once 009 lands — the same coordination task 018's microVM fitness rules have with 009.
This task does not re-author the umbrella; it adds one rule that `make fitness` discovers. 019 is
otherwise independent of the firecracker epic (013–018) and of 009 for its core implementation.

## Problem

`bwrapArgv` (`run.go:313-349`) passes **no `--seccomp`** to bubblewrap, and bubblewrap applies
**zero** syscall filtering on its own. Untrusted Tier-1 code can therefore issue **any** syscall the
host kernel exposes — including the historical kernel-LPE / container-escape launchpads: `keyctl`,
`add_key`, `request_key`, `ptrace`, `process_vm_readv`/`writev`, `userfaultfd`, `bpf`,
`perf_event_open`, the `mount`/`umount2`/`pivot_root` family, `kexec_load`/`kexec_file_load`,
`init_module`/`finit_module`/`delete_module`, `clone3` with namespace flags, and more.

The consequence: exec-sandbox ships **less default kernel hardening than stock `docker run`** (which
installs its own default-deny seccomp profile) — on the **default, most-used tier**. This is the
largest unaddressed kernel-attack-surface gap in the project.

**Why Tier-1 only** (record so it isn't re-litigated):
- **Tier-2 (gVisor/runsc):** the sentry intercepts *every* guest syscall in userspace — gVisor **is**
  the syscall filter; runsc also installs its own host-side seccomp. A host bwrap-style profile is
  redundant.
- **Tier-3 (Firecracker):** Firecracker self-installs its seccomp filters regardless of the jailer
  (ADR-010 Amendment 1 A1.Q3). Redundant.

So a host-side default-deny profile is meaningful **only** on the bubblewrap path.

## Scope

- **Add `seccomp/` artifacts** (mirrors ADR-010 A1's `vmlinux`/`base.ext4` pinned-artifact pattern):
  - `seccomp/tier1-policy.json` — the **plain-text source of truth**: default action +
    allow/deny syscall lists. Default-deny + allowlist, modeled on the Docker/podman default profile:
    allow the common-case syscalls a payload shell + proxy client need; deny the dangerous set,
    returning `EPERM`/`ENOSYS` (the exact default action is an ADR decision — `SCMP_ACT_ERRNO(EPERM)`
    is the Docker-default and the recommended choice).
  - `seccomp/tier1.bpf` — the compiled **cBPF blob**, generated **offline at build time** by
    libseccomp's `seccomp_export_bpf` from the policy, **committed** alongside it.
  - `seccomp/tier1.bpf.sha256` — the **pin**, verified at load.
  - `seccomp/build.sh` — **build-time only**: `seccomp_export_bpf(tier1-policy.json) -> tier1.bpf` +
    refresh the sha256. Documents the libseccomp version / provenance (mirror ADR-010 A1's
    `PROVENANCE` note discipline).
- **Add a stdlib seccomp loader** (e.g. `seccomp.go`): `open(2)`s `tier1.bpf`, verifies it against
  `tier1.bpf.sha256` with `crypto/sha256`, and **fails fast / crashes loudly** on mismatch — a
  tampered/wrong blob is a hard error, **never** a silent fall-back to spawning bwrap *without*
  `--seccomp`. Returns the open `*os.File` to be threaded into the spawn.
- **Wire `--seccomp <fd>` into `bwrapArgv`** (`run.go`): add the flag with the child-visible fd
  number, and plumb the `*os.File` via `cmd.ExtraFiles` so the fd the child sees matches the argv
  token. `--unshare-all` stays; `--share-net` stays absent — the new flag **adds to**, never weakens,
  the no-network invariant.
- **Add a fitness rule** asserting (a) the Tier-1 argv carries `--seccomp` and (b) a blocked-syscall
  probe (`keyctl`) returns `EPERM` under a real bwrap run — mirroring the F-001 no-`--share-net`
  positive/negative idiom. **Register it to coordinate with task 009's `fitness:` umbrella** (note
  the dependency in the rule row; do not re-author the umbrella here).
- **Spec update in the same commit (this is the implementation task):** rewrite the Tier-1
  description in `docs/spec/SPEC.md` (invariants) and `docs/spec/behaviors.md` (B-008 / backend
  selection) to state that bubblewrap runs with a **default-deny seccomp profile** passed via
  `--seccomp` (no longer implicitly "no syscall filtering"); add the new rule row to
  `docs/spec/fitness-functions.md`; note the pinned `seccomp/tier1.bpf` artifact in
  `configuration.md`/`data-model.md` if it becomes externally visible. Present tense, rewritten in
  place, no future tense.

**NO Go third-party runtime dependency.** libseccomp is **build-time tooling** (invoked only by
`seccomp/build.sh`); Go reads the resulting blob with stdlib `os` + `crypto/sha256`. This preserves
the project's stdlib-only invariant and its plain-text-config principle.

Out of scope: any change to `gvisor.go` (Tier-2 already filters every syscall — do **not** add
`--seccomp` there); the firecracker backend (tasks 013–018 — Firecracker self-installs its filters);
re-authoring task 009's `fitness:` umbrella (this task only adds a rule it discovers); a per-profile
or per-request *configurable* seccomp policy (v1 ships **one** curated Tier-1 default — a configurable
policy is a deferred follow-on, not this task).

## Verification plan

- **Highest level achievable: L6 (operator-observed), on a host with bwrap.** The load-bearing
  evidence is a **real bwrap run** under Tier-1 where a blocked syscall (`keyctl`) returns `EPERM`
  and a common-case payload still exits normally. L2 (loader + argv unit tests, sha256 fail-fast
  negative) and L3 (the new fitness rule, positive + negative) are the lower rungs.
- **Harness command:**
  - `go test -count=1 -run 'Seccomp|BwrapArgv|Tier1Seccomp|KeyctlBlocked' ./...` (unit + integration)
  - `go test -count=1 ./...` and `gofmt -l .`
  - `make fitness-tier1-seccomp` (the new rule; once wired into 009's umbrella, also `make fitness`)
- **Runtime observation (L6):** paste the `ok github.com/tkdtaylor/exec-sandbox` line; show the
  blocked-syscall integration test (TC-019-04) **ran un-skipped** under bwrap and the probe payload
  observed `EPERM` from `keyctl`; show the common-case payload (TC-019-06) ran to a normal exit with
  the profile applied (the profile narrows, doesn't brick); show the sha256-mismatch loader test
  (TC-019-03) errors rather than falling back to an unfiltered spawn; show `gvisor.go` has a zero
  diff (TC-019-07); demonstrate the fitness rule's **negative** case fails on a `--seccomp`-stripped
  argv (TC-019-10 — proves it bites).
- **ADR required.** Write the seccomp-profile-design ADR (next available number) **before** the feat
  commit: Tier-1-only scope + the Tier-2/3-redundant rationale, the deny set + default action, and
  the build-time-cBPF + sha256-pin approach.

## Definition of done

- `seccomp/tier1-policy.json` (plain-text source), `seccomp/tier1.bpf` (committed cBPF blob),
  `seccomp/tier1.bpf.sha256` (pin), and `seccomp/build.sh` (build-time generator) exist; the blob is
  the honest compilation of the policy (rebuilding reproduces the pinned sha256 — TC-019-08).
- A stdlib seccomp loader verifies the blob against the pin and **fails fast** on mismatch with **no**
  unfiltered fall-back; **no** Go third-party runtime dependency is added.
- `bwrapArgv` carries `--seccomp <fd>` with the fd plumbed via `ExtraFiles`; `--unshare-all` kept,
  `--share-net` absent; `gvisor.go` unmodified.
- A real Tier-1 bwrap run blocks `keyctl` with `EPERM` (load-bearing) and a common-case payload still
  exits normally; the deny set in `tier1-policy.json` covers the required dangerous family.
- A `fitness-tier1-seccomp` rule asserts the `--seccomp` flag + the blocked-syscall behavior, with a
  negative case proving it is not a no-op; it is registered to coordinate with task 009's `fitness:`
  umbrella.
- The seccomp-profile-design ADR (next available number) is written and committed before the feat
  commit.
- SPEC.md / behaviors.md (B-008) / fitness-functions.md rewritten in place (Tier-1 applies a
  default-deny seccomp profile; new fitness rule row); present tense, no future tense.
- `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE before promotion to ✅.
