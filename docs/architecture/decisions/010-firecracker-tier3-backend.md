# ADR 010 — Firecracker (microVM) Tier-3 backend behind the tier seam

**Status:** accepted — the core decision (Option A: Firecracker behind the seam, no-NIC + vsock-bridged egress) is committed. Q1 (kernel/rootfs sourcing) + Q3 (jailer privilege model) are **resolved in Amendment 1 (2026-06-20)** — Q1: build `vmlinux` + Alpine RO rootfs from source as pinned build-time artifacts; Q3: **no jailer** — run `firecracker` directly under the existing unprivileged `bwrap --unshare-all` + `limits.go` model. Q2 (mount mechanism) is resolved in task 017; Q4 (vsock shim location) is resolved in task 014. The task decomposition (013–018) carries the work; see the coverage tracker for the dependency-ordered status.
**Date:** 2026-06-20 (accepted 2026-06-20)
**Related:** ADR 001 D7 (tier seam: `bubblewrap | gvisor | firecracker`), ADR 002 (gVisor Tier-2
backend — the OCI bundle/spec pattern this ADR extends), ADR 006 (hyperlight Tier-4 watching
brief — Firecracker is sequenced *before* it), ADR 009 (snapshot/restore baseline — the VMM-level
snapshot deferred there is in scope to revisit here). Source analysis:
`docs/architecture/prior-art.md` ("firecracker — detail", net-new candidate #4: "best-aligned VMM
candidate to wire first").

## Context

ADR-001 D7 established the `tier` seam as the project's deliberate composability boundary, and
ADR-002 D7.1 turned it into a real dispatch point: `backendFor(tier)` maps the requested tier to a
`Backend` implementation that returns the `os/exec` argv to spawn, while the orchestration in
`Run()` (allowlist parse, identity mint, audit emit, `vault.inject` loop, proxy start,
stdout/stderr/exit capture, result assembly) stays tier-independent. Today the seam wires Tier-1
bubblewrap and Tier-2 gVisor; `req.run.tier == "firecracker"` is accepted by the contract but
`backendFor` returns `tier not implemented: firecracker` (SPEC.md Non-goals; `run.go` `backendFor`
default arm).

This ADR records the **decision to implement Tier-3 as Firecracker** — a KVM-based microVM
monitor — behind the existing seam, and pins down the one part that is genuinely a design question
rather than mechanical adapter work: **how the no-network + proxy-only-egress invariant is
re-enforced when the isolation unit is a microVM with its own guest kernel and its own network
stack.** Bubblewrap enforces the invariant with `--unshare-all`; gVisor enforces it with an empty
OCI `network` namespace plus `runsc --network=none`. A microVM has neither construct — it has a
virtio device model where networking is an *attachable device*. The invariant must be re-expressed
in those terms, and that re-expression is the crux of this ADR.

The prior-art analysis already concluded Firecracker is the best-aligned VMM to wire first and that
"no new ADR is needed — implementing it is a future task, not a design decision." That is true of
the adapter mechanics (drive a REST-over-Unix-socket API from Go the way `gvisor.go` drives
`runsc`). It is **not** true of the egress model, the workload→microVM mapping, the
Firecracker-vs-Kata choice, and the v1 scope boundary — those are decisions, and recording them is
what keeps a future implementer from re-deriving (or quietly getting wrong) the security model. This
ADR refines ADR-001 D7 and ADR-002; it does not supersede any decision, and every invariant from
ADR-001 D3/D4 (no network, proxy-only egress, credential never enters the sandbox) is preserved and
re-asserted in microVM terms.

## Decisions

### D1 — Firecracker is the Tier-3 backend (over Kata), driven via its REST-over-Unix-socket API

`req.run.tier == "firecracker"` selects a new `firecrackerBackend` behind `backendFor`. The backend
follows the ADR-002 pattern: it is a pure-ish config generator plus a spawn argv. It generates the
microVM configuration (machine config, boot source, root drive, vsock device — see D2/D3), writes
it to an ephemeral per-run bundle dir (mirroring the gVisor OCI bundle), and returns the argv that
launches Firecracker under its jailer pointed at that config. Firecracker exposes a REST API on a
Unix socket (`PUT /machine-config`, `/boot-source`, `/drives/...`, `/vsock`, then `PUT
/actions {InstanceStart}`); the backend speaks that API host-side, the same way `gvisor.go` shells
out to `runsc`. Absence of the `firecracker`/`jailer` binary or `/dev/kvm` is a spawn error
(`exit_code 127`), never a silent fall-back to a weaker tier.

**Why Firecracker over Kata for v1** (the recorded rationale; see Options below for the full
trade-off):

- **No guest-format adapter.** Firecracker boots a full guest kernel + rootfs and runs *arbitrary
  Linux binaries* — it maps directly onto exec-sandbox's "run an arbitrary payload as
  `/usr/bin/sh /payload.sh`" model. This is the single biggest fit reason and the one that
  distinguishes it from hyperlight (Tier-4, which runs only `no_std` ELF/Wasm — ADR 006 D2).
- **Minimal, auditable surface owned by us.** Firecracker is one stripped-down VMM process (no BIOS,
  no PCI) with a jailer that drops privileges before the VMM starts. We drive it directly. Kata is a
  *runtime stack* (containerd shim + agent-in-guest + a VMM underneath, often Firecracker or QEMU)
  that re-introduces an OCI/CRI control plane we would have to trust and configure to *not* attach a
  network — the opposite of "we own the egress boundary explicitly."
- **The egress invariant is expressible by omission.** Because the NIC is an opt-in virtio device,
  "no network" is achieved by simply *not configuring* a `network-interface` — the microVM analogue
  of `--unshare-all`. Kata's default CNI/networking would have to be actively disabled, which is the
  wrong default to be fighting against in a security-critical box.
- **Production-proven and Apache-2.0**, matching this project's license, and with native
  snapshot/restore (D5) that aligns with the VMM-snapshot variant deferred in ADR 009.

Kata is recorded as the rejected alternative, not removed from the universe: if a future workload
needs full OCI-image compatibility or an in-guest agent, Kata becomes worth re-evaluating in its own
ADR. For v1, Firecracker is the smaller, more directly-owned surface.

### D2 — The egress invariant: NO virtio-net device + a vsock bridge to the host proxy (the crux)

This is the load-bearing decision. The no-network + proxy-only-egress invariant is re-enforced in
microVM terms as follows:

- **No NIC, ever.** The microVM is configured with **no `network-interface` device**. Firecracker's
  virtio-net is opt-in; by simply never issuing the `PUT /network-interfaces/...` call, the guest
  has *no network device at all* — no host TAP, no bridge, no route, no netstack reachable from
  outside loopback. This is the microVM analogue of `bwrap --unshare-all` and gVisor's
  `--network=none` + empty netns: **no NIC = no network, by construction.** Adding a NIC is
  forbidden by the same rule that forbids `--share-net` (CLAUDE.md invariant); a fitness function
  should assert the generated Firecracker config contains no `network-interface` key.
- **Proxy reached over virtio-vsock, not a bind-mount.** A microVM cannot bind-mount a host Unix
  socket the way bubblewrap and gVisor do — the guest is a separate kernel with its own VFS. Instead
  the backend configures a **virtio-vsock device** (`PUT /vsock` with a host-side `uds_path`).
  vsock gives a host↔guest byte channel that is **not network** (no IP, no routing table, no
  netstack egress — it cannot reach anything but the host side of the vsock). The host side of the
  vsock terminates at the **existing `EgressProxy`** (`proxy.go`), unchanged: the proxy still enforces the
  domain + per-host verb allowlist and injects credentials host-side.
- **A guest-side shim presents `/proxy.sock` to the payload.** So the payload's contract is
  unchanged across tiers (it always talks to a Unix socket at `/proxy.sock`), a tiny guest-side
  forwarder in the rootfs listens on `/proxy.sock` inside the guest and forwards bytes over the
  vsock channel to the host proxy. The payload sees exactly the `/proxy.sock` it sees under
  bubblewrap and gVisor; it does not know or care that the transport beneath is vsock. The shim is
  a dumb byte-pump — it does **not** parse HTTP, hold credentials, or make allowlist decisions; all
  of that stays in the host-side `EgressProxy` exactly as today.
- **The credential never enters the guest.** The proxy injects the credential header host-side,
  *after* the request crosses the vsock back to the host proxy — identical to the bind-mount case.
  The guest only ever speaks the plaintext proxied request to its local `/proxy.sock`; the credential
  value never appears in guest env, args, stdout, or memory. This preserves the ADR-001 D4 / CLAUDE.md
  invariant verbatim. The vsock bridge is a *transport substitution* for the bind-mount, not a change
  to the trust boundary.

**Rejected egress alternative: TAP device + nftables.** Attaching a virtio-net NIC backed by a host
TAP, then constraining egress with host nftables rules (DNAT only to the proxy, drop everything
else), was considered and **rejected**. It re-introduces a real, fully-functional guest netstack and
a real host network device, then tries to fence it off with firewall rules — exactly the "add a
route, then try to block most of it" shape the project's no-network invariant exists to forbid. A
single misconfigured or omitted nftables rule is a silent egress hole; "no NIC" has no such failure
mode because there is nothing to misconfigure. The no-NIC + vsock model is strictly smaller surface
and fails *closed* (no device → no egress) rather than fails *open* (device present, rule missing →
egress). This rejection is itself load-bearing: never add a real NIC or route to the microVM.

### D3 — Workload → microVM mapping: guest kernel image + rootfs + jailer, mirroring the OCI bundle

The gVisor backend writes an OCI bundle (`config.json` + rootfs) to a temp dir; the Firecracker
backend writes the microVM analogue to a per-run bundle dir and tears it down at teardown
(`defer cleanup()`):

- **Boot source** — a guest kernel image (`vmlinux`) and a minimal kernel cmdline. The payload runs
  as `/usr/bin/sh /payload.sh` inside the guest, matching every other tier's entry point.
- **Root drive** — a root filesystem image containing a minimal userland (sh + the system dirs the
  payload needs) **plus the guest-side vsock→`/proxy.sock` shim** (D2). The payload (`payload.sh`)
  and any `FileRead` paths are presented to the guest; the writable `/work` and read-only FileRead
  mounts (ADR 004/005) map onto guest-visible drives or a host-shared mechanism — **the precise
  mechanism is an open question (Q2)**, since a microVM has no host bind-mount.
- **Jailer** — Firecracker is launched under its `jailer`, which sets up the cgroup/namespace
  barrier, chroots the VMM, and drops privileges before the VMM process starts (defence-in-depth
  around the VMM itself, layered *under* the in-guest isolation). The jailer is part of the v1
  decision, not optional polish: it is how the host process running an untrusted guest is itself
  contained.

The base config is a pure function of the on-host paths (kernel image, rootfs image, payload path,
proxy vsock uds path), so it is unit-testable without `/dev/kvm` or the Firecracker binary present —
exactly as `gvisorOCISpec` is unit-testable without `runsc`.

### D4 — Limits mapping: profile.limits → machine-config vCPU/mem; disk → drive sizing

`profile.limits` (ADR 003, `limits.go`) maps onto Firecracker's machine config, which is a *better*
fit than the rlimit/tmpfs approximations the namespace tiers use:

- `cpu_count` → `machine-config.vcpu_count` (the guest literally has that many vCPUs — a real cap,
  not a host-side `taskset` affinity hint). This is a stronger enforcement than Tier-1/Tier-2, where
  `cpu_count` is a host-side affinity prefix.
- `memory_mb` → `machine-config.mem_size_mib` (the guest's total RAM ceiling — the microVM cannot
  exceed it, vs the namespace tiers' `RLIMIT_AS`).
- `disk_mb` → the size of the writable drive / writable layer presented to the guest.
- `pids` → an in-guest rlimit applied by the guest-side launcher (the guest kernel owns its own pid
  space; `RLIMIT_NPROC` is set inside the guest, analogous to the `prlimit` wrap under bubblewrap).
- `timeout_sec` and `max_output_bytes` are enforced **host-side, above the tier seam, unchanged** —
  `Run()` already kills the spawned process group on the wall-clock deadline (ADR 003) and caps each
  captured stream through a `capWriter` (ADR 007). The Firecracker process is the spawned child; the
  same host-side machinery applies with no backend involvement. Caps the host genuinely cannot apply
  degrade loudly (warn + continue) exactly as today; a load-bearing cap that cannot be applied is a
  hard error, never a silent drop.

### D5 — Snapshot/teardown integration: host-side baseline stays; VMM snapshot is opt-in, deferred

The snapshot/restore reset boundary (ADR 009, `snapshot.go`) is **host-side and tier-independent
today** — it covers the host work dir, `payload.sh`, and the host-side proxy credential map, and
explicitly does *not* reach inside a tier's kernel root. That stays true for Firecracker in v1: the
default one-shot path is snapshot-baseline → run microVM → teardown (terminate the microVM, remove
the bundle dir, wipe proxy creds). Teardown must terminate the Firecracker process and reclaim its
jailer chroot/cgroup so no guest outlives the run.

Firecracker's **native VMM-level snapshot/restore** (full + diff, ~5–30 ms restore — the Lambda
SnapStart mechanism) is the kernel-snapshot variant ADR 009 deferred as an open question. It is
**explicitly out of v1 scope** here (D6): wiring it is a *separate* future decision/task, gated on
the one-shot Firecracker tier landing first. The documented Firecracker caveat that "guest network
connectivity is not guaranteed across resume" is a non-issue precisely because the guest has no
network (D2).

### D6 — Out of scope for v1 (explicit)

To keep the first Tier-3 increment shippable and the surface auditable, v1 **excludes**:

- **VMM-level snapshot/restore** (native Firecracker snapshots). Host-side baseline reset only; VMM
  snapshot is a separate gated decision (D5).
- **Warm-pool / microVM reuse across runs.** Every run is a fresh microVM (the "every run is
  ephemeral" non-goal holds); pooling is a throughput optimization deferred until the one-shot path
  is proven.
- **Any network device** under any flag — no virtio-net, no TAP, no nftables egress path. This is
  not "deferred," it is *forbidden* by the invariant (D2).
- **In-guest agent / OCI-image compatibility** (the Kata shape). v1 boots a curated minimal
  kernel+rootfs, not an arbitrary OCI image.
- **Multi-drive / block-device passthrough** beyond the root drive + the writable layer needed for
  `/work`.

## Options considered

### Option A — Firecracker, no-NIC + vsock-bridged proxy (recommended)

One microVM per run via the Firecracker REST API under the jailer; no `network-interface` device; the
host `EgressProxy` reached over a virtio-vsock device with a dumb guest-side shim presenting
`/proxy.sock` to the payload.

- **Pros**
  - No guest-format adapter — arbitrary Linux payload runs as-is, matching every other tier.
  - "No network" is true by construction (no NIC), the microVM analogue of `--unshare-all`; fails
    closed.
  - Minimal, directly-owned surface (one VMM process + jailer); no extra control plane to trust.
  - Native snapshot/restore available later (D5); production-proven, Apache-2.0.
- **Cons**
  - Requires `/dev/kvm` (bare metal or nested virt) — a real host prerequisite the namespace tiers
    don't have.
  - We must source and maintain a guest kernel image + rootfs + the vsock shim (Q1/Q2 below).
  - vsock transport substitution adds a guest-side component to keep dumb and audited.
- **Sketch:** `firecrackerBackend.Argv` writes `{vmlinux, rootfs.img, payload.sh, vsock uds}` config
  to a bundle dir, returns the `jailer ... firecracker --api-sock <sock> --config-file <cfg>` argv;
  the backend PUTs machine-config/boot-source/drives/vsock then `InstanceStart`; the host vsock side
  is wired to the existing `EgressProxy`.

### Option B — Firecracker with a virtio-net NIC + host TAP + nftables egress fence

Attach a NIC backed by a host TAP, give the guest a real netstack, and constrain egress to the proxy
with host nftables (DNAT to proxy IP:port, default-drop).

- **Pros**
  - Payload could use ordinary TCP/HTTP to a fixed proxy address with no guest-side shim.
  - Closer to how general microVM networking is usually demonstrated.
- **Cons**
  - Re-introduces a full guest netstack and a real host network device — the exact thing the
    no-network invariant forbids.
  - Fails *open*: one missing/incorrect nftables rule is a silent egress hole. No such failure mode
    exists when there is no NIC.
  - Larger host surface (TAP, bridge, firewall ruleset) to provision and verify per run.
- **Sketch:** `PUT /network-interfaces/eth0` with a host TAP; host nftables DNAT/drop rules scoped
  to the run; guest default route to the proxy. **Rejected** — see D2.

### Option C — Kata Containers as the Tier-3 backend

Use the Kata runtime (containerd/CRI shim + in-guest agent + an underlying VMM) so the existing OCI
bundle from the gVisor path is reused at the VM boundary.

- **Pros**
  - Reuses an OCI bundle shape; full OCI-image compatibility; an in-guest agent handles workload
    lifecycle.
  - A maintained, off-the-shelf runtime stack rather than direct VMM wiring.
- **Cons**
  - Much larger trust/config surface (shim + agent + VMM + CNI) we'd have to drive to *not* network —
    fighting an opinionated default in a security box.
  - Networking is on by default (CNI); "no network" becomes active disabling, the wrong default.
  - Heavier dependency chain (containerd ecosystem) vs one Apache-2.0 VMM binary; less directly owned.
- **Sketch:** install Kata + a container runtime, hand it the OCI bundle with networking disabled and
  a vsock/socket for the proxy; rely on Kata's agent to launch the payload. **Rejected** for v1 —
  re-evaluate in its own ADR only if OCI-image or in-guest-agent compatibility becomes a requirement.

## Recommendation

**Option A — Firecracker with no-NIC + vsock-bridged proxy.** The deciding factor is the **blast
radius of an egress mistake**: Option A makes "no network" a *structural* property (there is no NIC,
so there is nothing to misconfigure), where Options B and C make it a *configuration* property that
fails open if a single rule or default is wrong. In a box whose entire reason to exist is the
no-network + proxy-only-egress invariant, fail-closed-by-omission beats fail-open-by-configuration.
The secondary deciding factor is **owned surface**: Firecracker-direct is one VMM process + jailer we
drive ourselves, matching the ADR-002 pattern of driving `runsc` directly, whereas Kata adds a
runtime control plane we'd have to trust. Firecracker also needs no guest-format adapter (unlike the
Tier-4 hyperlight candidate), so the payload contract is unchanged across all tiers.

## Decision

Adopt **Option A**: implement Tier-3 as Firecracker behind `backendFor`, with no virtio-net device
and the host `EgressProxy` reached over a virtio-vsock bridge with a dumb guest-side shim presenting
`/proxy.sock`. Run the VMM under the jailer. Map `profile.limits` onto machine-config vCPU/mem/drive
sizing; keep `timeout_sec`/`max_output_bytes` host-side above the seam. Keep the host-side snapshot
baseline; defer native VMM snapshot. **Status: accepted** — the core decision is committed; Q1–Q4
remain task-scoped open questions (a normal accepted-ADR state, see the status line). This ADR
scopes the work; the task decomposition (tasks 013–018) carries it.

## Consequences

- The no-network + proxy-only-egress invariant now has a **third** enforcement point alongside
  `bwrapArgv` (`--unshare-all`) and `gvisorOCISpec` (empty netns + `--network=none`): the Firecracker
  config's *absence* of a `network-interface` plus the vsock-bridged proxy. A fitness function should
  assert the generated config contains no `network-interface` key (the microVM analogue of F-001).
- A new host prerequisite appears **for the firecracker tier only**: `/dev/kvm` + the
  `firecracker`/`jailer` binaries. Their absence skips the integration test (mirroring `requireBwrap`
  / the runsc skip) and yields a spawn error for an actual firecracker run — never a silent fall-back.
- exec-sandbox now owns a small **guest-side artifact** (the vsock→`/proxy.sock` shim, plus a curated
  guest kernel + rootfs). This is new surface to build, audit, and keep dumb; it is the cost of the
  microVM tier and is bounded to that tier.
- The `run()` contract, audit shape, and `EgressProxy` are **unchanged**. The payload still talks to
  `/proxy.sock`; the credential still never enters the guest; `sandbox_status.tier` echoes
  `firecracker`. What gets harder: a microVM run is heavier (≈125 ms cold boot, `/dev/kvm` required)
  than a namespace/userspace-kernel run, so Tier-3 is for the highest-risk actions, not the default.
- ADR 009's deferred kernel-snapshot open question now has a concrete owner (Firecracker native
  snapshot) and a concrete gate (the one-shot Firecracker tier must land first).

## Open questions (flagged — not resolved from the repo)

These could **not** be settled from the current repository and must be decided during
implementation (likely in the first task or a follow-up ADR amendment):

- **Q1 — Guest kernel + rootfs sourcing.** **RESOLVED → see Amendment 1 (2026-06-20).** Where the
  `vmlinux` guest kernel image and the minimal rootfs come from (build from source, vendor a pinned
  prebuilt, or generate at first run) is not established anywhere in the repo. This affects
  reproducibility, supply-chain scanning (`dep-scan` does not cover a kernel image), and binary size.
  **Decide before the rootfs/boot task.**
- **Q2 — `/work` and `FileRead` mount semantics in a microVM.** Bubblewrap/gVisor bind-mount host
  paths (ADR 004/005). A microVM has no host bind-mount; the writable `/work` and read-only FileRead
  paths must be presented via a block device, a virtio-fs share, or a copy-in/copy-out at
  boot/teardown. Each has different isolation and performance trade-offs and none is decided. The
  read-only-ness of FileRead and the single-writable-surface property (ADR 005) must be preserved
  whatever mechanism is chosen.
- **Q3 — Jailer privilege/runtime model.** **RESOLVED → see Amendment 1 (2026-06-20).** The jailer
  expects specific chroot/cgroup/uid setup and often elevated setup privileges; how that reconciles
  with exec-sandbox's unprivileged (`--rootless`-style) operation on hosts where the namespace tiers
  run unprivileged is unresolved. May constrain which hosts can run Tier-3.
- **Q4 — vsock shim location and lifecycle.** Whether the guest-side `/proxy.sock` shim ships inside
  the rootfs image, is injected at boot, or is the guest `init` itself — and how its dumbness is
  audited — is a design choice for the egress task.

## Amendment 1 (2026-06-20) — Q1 + Q3 resolved

This amendment resolves the two open questions that gated task 015: **Q1 (guest kernel + rootfs
sourcing)** and **Q3 (jailer privilege model)**. It does **not** rewrite the D1–D6 decision body
above — that record stands; the one substantive change it makes to the original text is dropping the
Firecracker **jailer** in favour of running the `firecracker` binary directly under exec-sandbox's
existing unprivileged model (see A1.Q3). Where D1/D3/Option A/Decision say "under the jailer," read
"directly, with bwrap `--unshare-all` + `limits.go` supplying jailer-equivalent isolation" per A1.Q3
below. The egress model (D2), limits mapping (D4), snapshot stance (D5), and scope boundary (D6) are
unchanged. Q2 (task 017) and Q4 (task 014) remain task-scoped open questions.

### A1.Q1 — Guest kernel + rootfs sourcing (RESOLVED)

**Kernel.** Build the guest kernel **from source as build-time tooling** (CLAUDE.md permits
build-time tooling; this keeps runtime third-party deps at zero). Target the newest
upstream-supported kernel line per Firecracker's `kernel-policy.md`, with **linux 6.1 as the floor**.

> **FLAG for task 015:** linux 6.1's upstream support ends **2026-09-02** (~2.5 months from this
> amendment). Task 015 must pin the **newest non-EOL line available in upstream
> `resources/guest_configs/` at build time**, preferring 6.1's successor if one is published. Do not
> hard-code 6.1 if a newer supported line exists.

Build the **uncompressed x86_64 ELF `vmlinux`** (`make vmlinux`) — `bzImage` is **not** the supported
x86_64 boot path for Firecracker. Boot args are `console=ttyS0 reboot=k panic=1`; with **no
virtio-net device** the guest kernel simply has no NIC, so **do not add an `ip=` arg** (there is no
interface to configure — this reinforces D2's no-NIC-by-construction stance at the kernel-cmdline
level).

**Vendor three pinned things into the repo** (the pin is the supply-chain control — `dep-scan` does
not cover a kernel image):

1. the `vmlinux` artifact,
2. its `sha256`,
3. a copied-in `microvm-kernel-ci-x86_64-<ver>.config` **plus a `PROVENANCE` note** recording the
   upstream Firecracker commit + the linux git tag the build came from.

**Rootfs.** A minimal **Alpine** ext4 image, built reproducibly at build time, mounted
**`is_read_only: true`**. The RO base bakes in the only two trusted guest binaries — the
project-authored **vsock→`/proxy.sock` forwarding shim** (part of the TCB, task 014) and `/sbin/init`.
The **per-run untrusted payload is never baked into the base** (that would churn the base digest and
defeat scan-once/pin-once); it is injected by **copy-in to a separate writable drive** (or the
existing writable `/work` surface, per Q2/task 017). Pin `base.ext4` by `sha256`.

**Verification.** A Go loader using stdlib `crypto/sha256` verifies **both** digests
(`vmlinux.sha256`, `base.ext4.sha256`) before the firecracker backend uses the paths, and
**fails fast / crashes loudly** on any mismatch (the project's "fail fast, crash loudly" principle —
a tampered or wrong artifact is a hard error, never a silent boot). The RO base maps cleanly onto the
snapshot/restore reset boundary (D5): scan once, reuse every run, reset for free. Both artifacts are
ordinary files scannable by the project's `code-scanner`; **no runtime Go dependency is added**.

**Recommended file layout** (record for task 015):

```
guest/
  kernel/  vmlinux-<ver>  vmlinux.sha256  config/microvm-kernel-ci-x86_64-<ver>.config  config/PROVENANCE
  rootfs/  base.ext4  base.ext4.sha256  build.sh   # RO base: vsock shim + /sbin/init; build.sh is build-time only
```

### A1.Q3 — Jailer privilege model (RESOLVED): no jailer; direct firecracker, unprivileged

**Decision: do NOT adopt the Firecracker jailer.** Run the `firecracker` binary **directly** under
exec-sandbox's existing unprivileged model, reconstructing jailer-equivalent isolation with the
`bwrap --unshare-all` + `limits.go` machinery the project already owns. This supersedes D1/D3/Option
A/Decision's "under the jailer" wording.

**Rationale (tied to the untrusted-code threat model):**

1. **The jailer requires root.** Its minimum capability set has been officially "to be determined"
   for years — there is no maintainer-certified cap-only profile; the union it exercises is
   `CAP_SYS_ADMIN + CAP_CHOWN + CAP_MKNOD + CAP_SETUID/SETGID + CAP_SYS_CHROOT`, i.e. effectively
   root. Adopting it would make **Tier-3 the one tier demanding a privileged host** — a structural
   regression precisely on the tier meant for the *highest-risk* code, and a far larger attack
   surface than the narrow `/dev/kvm` device permission.
2. **Firecracker self-installs its seccomp filters regardless of the jailer.** The highest-value
   syscall-attack-surface reduction does **not** depend on the jailer. Every *other* jailer layer
   (chroot, mnt/pid/ipc/net namespaces, cgroup caps, per-instance uid) is **already constructed** by
   exec-sandbox's `bwrap --unshare-all` + `limits.go` — arguably more thoroughly in the unprivileged
   path.
3. **Firecracker's own `prod-host-setup.md` blesses "process constraints equal or more restrictive
   than the jailer"** as the production contract — the contract is jailer-*equivalent* constraints,
   not the jailer binary specifically. **Kata's rootless-VMM pattern** (run the VMM directly as a
   non-root user with `kvm` as a supplemental group, `crw-rw---- root:kvm`) is the documented non-root
   precedent.

**Host-requirement statement for Tier-3 (verbatim — also recorded in task 015):**

> Tier-3 (Firecracker) requires KVM-capable hardware and the exec-sandbox host user to be a member of
> the `kvm` group (or an equivalent ACL granting rw on `/dev/kvm`). It requires NO root, NO setuid
> launcher, and NO elevated capabilities beyond `/dev/kvm` access — preserving the Tier-1/2
> unprivileged invariant. The bwrap `--unshare-all` wrapper supplies the chroot + mnt/pid/ipc/net
> namespaces + cgroup limits the jailer would otherwise provide; firecracker self-installs its
> seccomp filters regardless.

**Accepted risk + test obligation.** By skipping the jailer, exec-sandbox takes on responsibility for
faithfully reproducing jailer-equivalent constraints. This is discharged with a **new fitness
function** — the microVM analogue alongside the no-`network-interface` rule — asserting **Tier-3
effective constraints ≥ jailer**:

- runs as a non-host uid,
- all namespaces unshared (none shared with the host),
- cgroup limits applied,
- chroot / `pivot_root` in effect,
- and (with A1.Q1) the credential / host FS never leaks into the guest.

This fitness rule is **registered in task 018** (alongside the no-NIC fitness function it already
owns) and its assertion is **exercised in task 015's verification plan**.

### Amendment 1 consequences

- D1/D3/Option A/Decision's "jailer" references now mean "direct `firecracker` under bwrap
  `--unshare-all` + `limits.go`." No jailer binary is a Tier-3 prerequisite; the prerequisites are
  `/dev/kvm` rw (via `kvm` group) + the `firecracker` binary + the pinned guest kernel/rootfs. Tier-3
  stays **unprivileged**, consistent with Tier-1/Tier-2.
- The repo gains a small **build-time** kernel/rootfs build path (`guest/.../build.sh`) and three
  vendored, pinned, scannable artifacts (`vmlinux`, `base.ext4`, their `sha256`s + a `PROVENANCE`
  note). Runtime third-party deps remain **zero**; a stdlib `crypto/sha256` loader gates use of the
  artifacts and fails closed on mismatch.
- The Tier-3 host-prerequisite story is now strictly smaller than the original jailer-based one: the
  one new device permission (`/dev/kvm`) replaces "a privileged/root setup phase."
- Task 018 owns an **additional** fitness rule (constraints ≥ jailer) beyond the no-NIC and
  cred-not-in-guest rules; task 015's verification plan exercises it. Task 015 is **unblocked**.
