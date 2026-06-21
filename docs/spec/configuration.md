# Configuration

**Project:** exec-sandbox
**Last updated:** 2026-06-20 (task 016: profile.limits → Firecracker machine-config mapping — cpu_count→vcpu_count, memory_mb→mem_size_mib, disk_mb→writable-drive size, pids→in-guest RLIMIT_NPROC via setpriv; timeout/output stay host-side)

Every knob the system exposes. exec-sandbox has **no config files and reads no application
environment variables** — all configuration arrives inside the stdin `RunRequest`. The tunable
surface is therefore the `wiring` object plus the parts of `run` that shape execution.

Not in this file: what gets configured (the behaviors live in [behaviors.md](behaviors.md));
how values get into the process (the parsing is in code; the *contract* is here).

---

## Configuration files

**None.** exec-sandbox is configured entirely per-request via the stdin JSON. There is no
`config.toml`/`.env`/etc.

## Pinned build-time artifacts

| Artifact | Role | Pin |
|----------|------|-----|
| `seccomp/tier1-policy.json` | Plain-text source of truth for the Tier-1 default-deny seccomp profile (default action `SCMP_ACT_ERRNO(EPERM)` + allow/deny syscall lists) | — |
| `seccomp/tier1.bpf` | Compiled cBPF blob generated offline from the policy by `seccomp/build.sh` (libseccomp `seccomp_export_bpf`); embedded in the binary via `go:embed` and installed at spawn via `bwrap --seccomp <fd>` | `seccomp/tier1.bpf.sha256` |
| `seccomp/tier1.bpf.sha256` | The sha256 pin verified fail-fast by the stdlib loader before the fd reaches bwrap | self |

These are committed artifacts, not runtime configuration: there is no per-request seccomp
knob in v1 (one curated Tier-1 default — ADR 016). libseccomp is **build-time tooling only**
(invoked by `seccomp/build.sh`); the runtime path links none of it and only `open(2)`s the blob
after a stdlib `crypto/sha256` check. Regenerating the blob (`seccomp/build.sh`) reproduces the
committed pin; a mismatch aborts the run rather than spawning bwrap unfiltered.

---

## Request configuration (the `wiring` object)

These fields ride alongside the `run()` contract under `wiring` and tune how a single run is
wired into the surrounding ecosystem. They are part of the stdin `RunRequest`
([data-model.md](data-model.md)), not process-level config.

| Key | Type | Default | Required | Effect |
|-----|------|---------|----------|--------|
| `wiring.vault_socket` | string (path) | `""` | no | Unix socket for `vault.inject`. Empty → injection calls are skipped (no secrets loaded). |
| `wiring.audit_socket` | string (path) | `""` | no | Unix socket for audit `emit`. Empty → emission is a no-op. |
| `wiring.origin_map` | object `{host: [ip, port]}` | `{}` | no | Resolves allowlisted hosts to real origins. A host without a route returns `502 no-route`. |
| `wiring.request_id` | string | `""` | no | Correlation id echoed into every audit event's context. |
| `wiring.injection_mode` | string | `""` | no | Passed verbatim to `vault.inject` as `mode`. `"proxy"` keeps the secret out of the sandbox entirely (loaded onto the host-side proxy, injected into outbound requests). `"env"` delivers the secret into the sandbox process environment under the vault-specified `var_name`, off the spawn argv, and wipes the host-side copy post-spawn (ADR 015). |

Execution-shaping fields under `run`:

| Key | Type | Default | Effect |
|-----|------|---------|--------|
| `run.tier` | string | `""`/`bubblewrap` → Tier-1 (bwrap) | `bubblewrap \| gvisor \| firecracker`. `""`/`bubblewrap` runs bwrap, `gvisor` runs runsc, `firecracker` boots a KVM microVM (verified pinned kernel/rootfs, direct firecracker under `bwrap --unshare-all`, no jailer — see B-015); any other value returns `{error: "tier not implemented: <tier>"}`. The value is echoed into `sandbox_status.tier` and the spawn audit context. |
| `run.profile.capabilities[NetConnect].allowlist` | `[string]` ("host:port") | `[]` | The egress allowlist (ports stripped). Hosts not listed are `403 blocked-by-allowlist` by the proxy. |
| `run.profile.capabilities[NetConnect].methods` | `[string]` (HTTP verbs) | absent ⇒ all verbs | **Optional per-host verb constraint** (ADR 008). Applies to every host in the same entry's `allowlist`; constrains the HTTP methods permitted to those hosts (canonical upper-case, case-insensitive match). **Absent or empty `[]` ⇒ all verbs allowed** (backward compatible) — empty is *unconstrained*, not deny-all. A non-empty set denies any verb not in it with `403 blocked-by-method` and **no** outbound connection (the host check still runs first). Different verb sets per host ⇒ multiple `NetConnect` entries. The verb *decision* is policy-engine's; the proxy **enforces**. |
| `run.profile.capabilities[FileRead].paths` | `[string]` (abs host paths) | `[]` | **Read-only host mounts** (ADR 005). Each path is bind-mounted **read-only** at the **same** path inside the sandbox; multiple `FileRead` entries union their paths. Validated before spawn (each must be absolute + exist, else a hard `{error}`). The read-**write** host dir is `run.workdir`, not `FileRead`. |
| `run.profile.limits` | object | — | **Enforced** resource caps (ADR 003). See the per-field table below. |
| `run.workdir` | string (host path) | `""` | **Writable working directory** (ADR 004). Non-empty → the host dir is bind-mounted **read-write** at `/work` and the payload's cwd is `/work`; validated before spawn (must be an existing directory, else a hard `{error}`). Empty → no `/work` mount (backward compatible). The one writable host surface — system dirs stay read-only, the network stays unshared. |
| `run.env` | object `{string: string}` | `{}` | **Env provisioning** (ADR 005). Each entry is exported into the sandbox; a `PATH` entry **replaces** the bare default `PATH=/usr/bin:/bin`, every other entry is exported as `k=v`. Combined with `FileRead`, `run.env["PATH"]` puts a mounted toolchain dir on PATH. Empty → bare default PATH, no other env (backward compatible). Carries no secret. |
| `run.secret_refs` | `[string]` | `[]` | Opaque vault handles to inject at spawn. |

`run.profile.limits` fields (each optional; missing/zero/non-positive ⇒ that cap is not applied):

| Key | Type | Enforced by | Effect |
|-----|------|-------------|--------|
| `cpu_count` | int (cores) | bwrap/gVisor: `taskset -c 0-(N-1)` affinity on the spawn argv. **Firecracker: `machine-config.vcpu_count`** — the microVM literally has N vCPUs (a **real** cap, **no** `taskset` prefix on the argv) | Pins the sandbox to N cores. Under bwrap/gVisor a **secondary control** that degrades (warn + continue) when `taskset` is absent (under gVisor verified host-side via the argv record, ADR 028); under **Firecracker it is a load-bearing real cap** — the guest reports `nproc == N` (a *stronger* enforcement than the namespace tiers' affinity hint, never a degrade). |
| `memory_mb` | int (MiB) | bwrap: in-sandbox `prlimit --as` (`RLIMIT_AS`); gVisor: OCI `process.rlimits`. **Firecracker: `machine-config.mem_size_mib`** — the guest's hard RAM ceiling | A payload exceeding the ceiling is killed: by the allocator (bwrap/gVisor `RLIMIT_AS`) or **OOM-killed by the guest kernel** (Firecracker — the microVM cannot exceed `mem_size_mib`; exit 137 / "Killed"). |
| `pids` | int | bwrap: in-sandbox `prlimit --nproc` (per-sandbox via the userns); gVisor: OCI `process.rlimits`. **Firecracker: an in-guest `RLIMIT_NPROC`** — the host emits `exec_sandbox.nproc=N` on the kernel cmdline; the guest init applies `ulimit -u N` and **drops the payload to nobody** (`setpriv --reuid 65534`) before running it (the kernel does not enforce NPROC for a uid-0 process, so the privilege drop is what makes the cap bite) | A fork bomb hits the cap ("Cannot fork" / "can't fork: Resource temporarily unavailable"). |
| `disk_mb` | int (MiB) | bwrap/gVisor: `/tmp` tmpfs size (`--size` / tmpfs `size=`). **Firecracker: the size of the writable payload drive (vdb)** | Writes past the cap return ENOSPC. **Secondary control:** degrades (warn + continue, `degraded:[disk_mb]`) on every tier when the writable layer can't be size-capped (`diskQuotaSupported`); the Firecracker drive then falls back to its floor default, never a silent drop. |
| `timeout_sec` | int (s) | host-side `context.WithTimeout` + process-group `SIGKILL` (backend-agnostic, **above** the `tier` seam) | The payload (and its whole process group) is killed at the deadline; `sandbox_status.status` becomes `"timeout"`, `exit_code` `137`. **Not** in the Firecracker config — it is byte-for-byte independent of this cap. |
| `max_output_bytes` | int (bytes) | host-side capture cap in `Run()` — a capping writer per stream, **above** the `tier` seam (backend-agnostic) | Captured stdout/stderr are each retained up to the ceiling; overflow is **dropped** without erroring the payload (its exit is unchanged). stdout/stderr capped **independently** at the same ceiling; identical under bubblewrap, gVisor, and Firecracker (the backend argv/OCI spec / firecracker config are unchanged — the Firecracker config is byte-for-byte independent of this cap). The capped streams are listed in `sandbox_status.limits.output_truncated`. A host memory guard against a payload that floods stdout, distinct from the in-sandbox `memory_mb` rlimit. |

---

## Environment variables

The **application** reads no environment variables. (Inside the sandbox, the environment is
cleared — `bwrap --clearenv` / gVisor `process.env` — and `PATH=/usr/bin:/bin` is set by default.
A caller-supplied `run.env` overrides `PATH` and adds further variables — ADR 005; the credential
invariant is untouched, `run.env` carries no secret.)

**Hook profile env vars** (consumed by `.claude/scripts/`, not the application itself):
- `CLAUDE_HOOK_PROFILE` — `minimal` / `standard` / `strict` (default `standard`)
- `CLAUDE_DISABLED_HOOKS` — comma-separated list of hook names to disable

---

## Runtime flags

None beyond the `run` subcommand — see [interfaces.md](interfaces.md). All runtime input is the
stdin JSON.

---

## Secrets

exec-sandbox never stores or mints secrets. Credentials flow transiently from vault → the
egress proxy and are wiped at teardown.

| Secret | Source | Used for |
|--------|--------|----------|
| (per-request credentials) | `vault.inject` over `wiring.vault_socket` | Injected into allowlisted egress requests (proxy mode); never written to disk, never enters the sandbox |

**Rule:** secrets are never pasted into the chat, never logged, never written into the repo,
and (the project-specific invariant) never enter the sandbox in proxy mode. The
`protect-secrets` hook blocks writes to common credential filenames.

---

## Deployment configuration

| Aspect | Value | Notes |
|--------|-------|-------|
| Binary | `bin/exec-sandbox` (`make build`) | Single static-ish Go binary |
| Runtime dependency | `bwrap` on `PATH` | Tier-1 isolation; integration tests skip if absent |
| Runtime dependency (Tier-2) | `runsc` on `PATH` | gVisor tier; absence is a spawn error (127), never a fall-back; integration tests skip if absent |
| Runtime dependency (Tier-3) | `firecracker` on `PATH` + rw `/dev/kvm` + `mkfs.ext4` | Firecracker tier (B-015). The host user needs rw on `/dev/kvm` (the `kvm` group or an equivalent ACL) — **no root, no setuid, no jailer**. Absence of any of these is a spawn error (127), never a fall-back; integration tests skip without `/dev/kvm`/`firecracker`. |
| Vendored guest artifacts (Tier-3) | `guest/kernel/vmlinux-<ver>` + `vmlinux.sha256`, `guest/rootfs/base.ext4` + `base.ext4.sha256` | Pinned, sha256-verified before boot (fail-fast on mismatch — A1.Q1). Built from source by `guest/rootfs/build.sh` (build-time tooling only; no runtime third-party dep). Provenance in `guest/kernel/config/PROVENANCE`. |
| Ports exposed | none | Egress proxy listens on a per-run Unix socket, not a TCP port |
| Persistent volumes | none | Per-run temp dir, removed on exit |

---

## Defaults policy

Defaults are **safe and closed**: an empty `vault_socket`/`audit_socket` disables that
integration rather than failing; an empty allowlist blocks all egress (default-deny); the
sandbox always runs with no network regardless of any field. Nothing in the request can widen
the sandbox's network access beyond the proxy + allowlist. The optional per-host `NetConnect.methods`
verb constraint (ADR 008) only ever **narrows** egress within an already-allowlisted host — it
cannot widen host access (the host check runs first) and cannot open a new route; absent/empty
`methods` leaves every verb allowed, the prior behavior.

`run.workdir` defaults to `""` — **no writable host mount**. A run only ever gains a writable host
surface when the caller explicitly names a directory, and even then it is the single `/work`
mount; the rootfs and system dirs stay read-only and the network stays unshared regardless. A
malformed `run.workdir` fails the run loudly rather than silently running without the mount.

`FileRead{paths}` defaults to none and `run.env` to `{}` — **no read-only host mounts, bare PATH**.
A run gains read-only host paths only when the caller lists them, and they are mounted **read-only**
(never writable — `/work` stays the only writable surface). FileRead opens no egress and the
network stays unshared regardless. A malformed `FileRead` path (relative or nonexistent) fails the
run loudly before any side effect rather than silently running without the mount.

`profile.limits` defaults to "no limit" per field — an unset cap is simply not applied (limits
*narrow* the sandbox; they never widen it). `max_output_bytes` defaults to "no cap": an unset or
non-positive value captures full output unbounded (byte-for-byte the prior behavior), and the cap,
when set, only *drops* output past the ceiling — it never relaxes any other control. The `cpu_count`
and `disk_mb` caps are **secondary**
anti-DoS controls and degrade gracefully (a stderr `WARNING` + a `sandbox_status.limits.degraded`
entry, never a hard failure) on hosts that can't enforce them — a documented, ADR-003-justified
exception (mirroring agent-builder ADR 027), not a weakening of the load-bearing controls
(no-network, proxy-only egress, memory/pids rlimits, wall-clock kill).
