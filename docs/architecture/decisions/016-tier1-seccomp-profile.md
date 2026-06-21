# ADR 016: Tier-1 default-deny seccomp-BPF syscall profile

**Status:** Accepted
**Date:** 2026-06-20
**Deciders:** exec-sandbox maintainers
**Supersedes:** —
**Reopening condition:** (1) **Configurable / per-profile policy** — v1 ships exactly one curated
Tier-1 default. If a consumer needs a per-request or per-profile seccomp policy, that is a new task
and re-opens this ADR (the loader and the `--seccomp` plumbing are the seam it would extend). (2)
**Default-action change** — this ADR fixes `SCMP_ACT_ERRNO(EPERM)`. Moving to `SCMP_ACT_KILL` (kill
the process on a denied syscall, no observable errno) or `SCMP_ACT_LOG` (audit-only) is a different
security posture and re-opens this ADR. (3) **Architecture beyond x86_64** — the committed blob is
pinned to `SCMP_ARCH_X86_64`. Supporting `aarch64` (a second committed blob + arch selection in the
loader) re-opens this ADR. (4) **Tier-2/Tier-3 revisit** — see *Scope* below; if the redundancy
argument for gVisor/Firecracker ever stops holding, re-open.

---

## Context

`bwrapArgv` passed **no** `--seccomp` to bubblewrap, and bubblewrap applies **zero** syscall
filtering on its own. Untrusted Tier-1 code could therefore issue **any** syscall the host kernel
exposes — including the historical kernel-LPE / container-escape launchpads: `keyctl`, `add_key`,
`request_key`, `ptrace`, `process_vm_readv`/`writev`, `userfaultfd`, `bpf`, `perf_event_open`, the
`mount`/`umount2`/`pivot_root` family, `kexec_load`/`kexec_file_load`,
`init_module`/`finit_module`/`delete_module`, and more.

The consequence was that exec-sandbox shipped **less default kernel hardening than stock
`docker run`** (which installs its own default-deny seccomp profile) — on the **default, most-used
tier**. This was the largest unaddressed kernel-attack-surface gap in the project, and it sat on
Tier-1.

## Decision

Tier-1 (bubblewrap) runs every payload under a **default-deny + allowlist** seccomp-BPF profile,
passed to bwrap via `--seccomp <fd>`. The decision has four load-bearing parts.

### 1. Default action: `SCMP_ACT_ERRNO(EPERM)`

A denied syscall returns `-EPERM` to the payload rather than killing the process (`SCMP_ACT_KILL`)
or silently logging (`SCMP_ACT_LOG`). EPERM is the Docker/podman default. It is chosen because:

- It is **observable** — the payload (and our integration test) sees a concrete errno, which makes
  the filter's effect testable end-to-end (the keyctl→EPERM probe is the load-bearing L6 evidence).
- It **narrows without bricking** — a denied call fails gracefully where a `KILL` would abort the
  whole process tree, which is harder to reason about and harsher for benign edge cases.

`clone3` is **allowed** (added to the allowlist) rather than left to the default deny. Modern
runtimes (Rust std, glibc thread spawn) call `clone3` first and, on `EPERM` (as opposed to
`ENOSYS`), do **not** fall back to `clone` — they hard-fail thread creation. Allowing `clone3`
unconditionally matches Docker's current default profile. Namespace isolation does not depend on
filtering `clone3` flags here: bwrap's `--unshare-all` already establishes the namespaces, and
argument-level filtering of `clone3`'s struct argument is unreliable under cBPF.

### 2. Scope: Tier-1 only

A host-side default-deny profile is meaningful **only** on the bubblewrap path:

- **Tier-2 (gVisor/runsc):** the sentry intercepts *every* guest syscall in userspace — gVisor
  **is** the syscall filter; runsc also installs its own host-side seccomp. A host bwrap-style
  profile is redundant. `gvisor.go` is left with a **zero diff** by this change.
- **Tier-3 (Firecracker):** Firecracker self-installs its seccomp filters regardless of the jailer
  (ADR 010 Amendment 1 A1.Q3). Redundant.

### 3. Mechanism: build-time cBPF generation + sha256 pin (NO Go third-party runtime dependency)

The cBPF program is generated **offline at build time** by libseccomp's `seccomp_export_bpf` from a
**plain-text JSON policy** (the committed source of truth). The compiled blob is committed alongside
it and **byte-pinned by sha256**. At spawn, Go only `open(2)`s the blob (stdlib `os`) after
verifying it against the pin (stdlib `crypto/sha256`) and threads the fd into the existing
`exec.Command` via `cmd.ExtraFiles`. libseccomp is **build-time tooling only** — the runtime path
links none of it, preserving the project's stdlib-only invariant and its plain-text-config
principle. This mirrors ADR 010 Amendment 1's `vmlinux`/`base.ext4` pinned-artifact pattern.

Layout (under `seccomp/`):

```
tier1-policy.json   # source of truth — default action + allow/deny lists in plain text
gen.c               # build-time-only cBPF generator (reads policy → seccomp_export_bpf → stdout)
build.sh            # compile gen.c against libseccomp, emit tier1.bpf, refresh the pin
tier1.bpf           # compiled cBPF blob (committed; embedded via go:embed)
tier1.bpf.sha256    # the pin — verified at load, fail-fast on mismatch
```

### 4. Fail-fast loading: no unfiltered fall-back

The loader verifies the embedded blob against the embedded pin and returns a **hard error** on any
mismatch. The bubblewrap backend propagates that error as a run failure — it **never** falls back to
spawning bwrap **without** `--seccomp`. A tampered, stale, or truncated blob aborts the run. This is
the project's "fail fast, crash loudly" stance applied to the kernel-attack-surface boundary.

### FD layout

The seccomp blob is always `cmd.ExtraFiles[0]` → child **fd 3**, so its fd number is deterministic.
The env-mode `--args` pipe (ADR 015), when present, follows as `ExtraFiles[1]` → child **fd 4**.
`bwrapArgv` emits `--seccomp 3` always and `--args 4` only when an env-mode credential is delivered.

## Consequences

- **Positive:** Tier-1 closes the largest kernel-attack-surface gap; the dangerous syscall family is
  denied with EPERM, proven by a real bwrap run (`keyctl`→EPERM). The common case is unaffected
  (write `/work`, exec a tool, reach the proxy over the Unix socket all still work). No new
  third-party dependency. `gvisor.go` is untouched. The profile **adds to** the no-network model —
  `--unshare-all` stays, `--share-net` stays absent.
- **Negative / accepted:** one curated policy for all Tier-1 payloads (no per-profile tuning yet — a
  deferred follow-on). The committed blob is x86_64-pinned. A payload needing a denied syscall fails
  that call (by design); if a legitimate common-case syscall is found missing, it is added to the
  allowlist and the blob re-pinned in the same change (as `clone3` was during implementation).
- **Reproducibility:** re-running `build.sh` on the pinned libseccomp version reproduces the
  committed sha256 (a test asserts this), so the pin is an honest compilation of the policy.
