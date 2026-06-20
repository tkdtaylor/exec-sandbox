# Test Spec 017: /work + FileRead mount semantics in the microVM

**Linked task:** [`docs/tasks/backlog/017-work-fileread-mount-semantics.md`](../backlog/017-work-fileread-mount-semantics.md)
**ADR:** ADR 010 D3 (the writable `/work` + read-only FileRead paths map onto guest-visible drives or a host-shared mechanism). Resolves ADR 010 **Q2** (block device vs virtio-fs vs copy-in/out) as an in-task decision recorded in `docs/spec/` + an ADR-010 note. Preserves ADR 004 (writable `/work`) and ADR 005 (read-only FileRead + single-writable-surface).
**Written:** 2026-06-20

## Context for the test author

Bubblewrap and gVisor present `/work` and `FileRead` paths by **host bind-mount**: `/work` is the
single writable surface (`--bind`/non-`ro`; ADR 004), and each `FileRead{paths}` entry is bound
**read-only** (`--ro-bind` / OCI `options:[ro,rbind]`; ADR 005). A microVM has **no host
bind-mount** — the guest is a separate kernel with its own VFS. The writable `/work` and the
read-only FileRead paths must be presented to the guest by some other mechanism, and the two
properties ADR 004/005 guarantee must survive that mechanism:

1. **`/work` is writable and is the ONLY writable host surface** — a payload's writes to `/work`
   persist back to the host work dir; nothing else the payload can reach is writable.
2. **FileRead paths are read-only** — a write to a FileRead path fails; the host file is never
   modified or created.

### ADR 010 Q2 — in-task decision to record

The mechanism is **not decided in the repo**: it could be a per-path **block device**, a
**virtio-fs** share, or a **copy-in/copy-out** at boot/teardown. Each has different isolation and
performance trade-offs (a block device is a clean isolation boundary but needs an image build per
run; virtio-fs is a live host share with a larger surface; copy-in/out is simplest but doubles I/O
and changes the "writes persist live" semantics to "writes persist at teardown"). **This task picks
one and records it** in `docs/spec/behaviors.md` + an ADR-010 Q2 note. Q2 is a smaller in-task
decision than Q1/Q3 (which gate task 015 entirely) — the row is marked `⚠️ planned, BLOCKED on Q2`
so the disposition is made deliberately, but it does not require an ADR amendment before starting.

Whatever mechanism is chosen, **the read-only-ness of FileRead and the single-writable-surface
property must be preserved** (ADR 010 D3 / Q2 closing sentence).

Ground truth to mirror:
- `validateWorkdir` (`run.go:554-570`) and `validateFileReads` (`run.go:499-509`) run **before any
  side effect** — a bad path is a hard `{error}`, no silent skip. This validation is tier-independent
  and UNCHANGED; this task only changes how the validated paths are *presented* to the guest.
- The bwrap `/work` is `--bind` (writable) + `--chdir /work` (`run.go:345-347`); FileRead is
  `--ro-bind` (`run.go:340-342`). The gVisor analogues are `applyWorkdirToOCISpec` (writable,
  cwd=/work) and `applyFileReadToOCISpec` (`options:[ro,rbind]`).
- The single-writable-surface invariant is fitness rule F-006; FileRead-read-only is F-007. The
  microVM presentation must keep both true.

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-017-01 | The validated `run.workdir` is presented to the guest as a writable `/work` and the payload's cwd is `/work`; writes to `/work` persist back to the host work dir (ADR 004 semantics preserved by the chosen Q2 mechanism) | TC-017-01, TC-017-05 | ✅ |
| REQ-017-02 | `/work` is the ONLY writable surface reachable by the payload — the rootfs, system dirs, `/payload.sh`, and every FileRead path are read-only (single-writable-surface invariant, F-006, preserved in microVM terms) | TC-017-02, TC-017-06 | ✅ |
| REQ-017-03 | Each validated `FileRead{paths}` entry is presented READ-ONLY to the guest; a write to it fails inside the guest and the host file is never modified/created (ADR 005 / F-007 preserved) | TC-017-03, TC-017-07 | ✅ |
| REQ-017-04 | A bad `run.workdir` or FileRead path still fails the run BEFORE any side effect (validation is tier-independent and unchanged) — the firecracker tier does not weaken the no-silent-skip stance | TC-017-04 | ✅ |
| REQ-017-05 | The Q2 mechanism (block device / virtio-fs / copy-in-out) is chosen and recorded in `docs/spec/behaviors.md` + an ADR-010 Q2 note; the no-NIC + vsock-only-egress invariant is untouched (the mount mechanism opens no network path) | TC-017-08, TC-017-09 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] The read-only-write-fails and host-file-unchanged assertions are specified (F-007 in microVM terms)
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: Q2 (mount mechanism) is an in-task decision to record, with read-only-ness + single-writable-surface preserved whatever is chosen
- [x] Target verification level: L5 (validation harness: host-seeded file read in-guest, guest write persisted to host, FileRead write fails + host file unchanged) — requires a booted guest; skip-guard when absent

---

## Test cases

### TC-017-01: /work is writable in the guest and persists to the host (positive)

- **Requirement:** REQ-017-01
- **Type:** integration (Go test) — target L5, requires a booted guest
- **Input:** a firecracker run with a `run.workdir` host dir; the payload writes
  `/work/out.txt` with known contents and prints `pwd`.
- **Expected:** after the run, the host work dir contains `out.txt` with the expected contents
  (the guest write persisted back per the chosen Q2 mechanism), and the payload's `pwd == /work`.
  Analogous to `TestWorkdirEndToEnd_*`. Skip-guard when prerequisites absent.

### TC-017-02: /work is the ONLY writable surface (F-006 in microVM terms)

- **Requirement:** REQ-017-02
- **Type:** integration (Go test) — target L5
- **Input:** a payload that attempts to write `/usr/evil` (a system dir) and `/payload.sh`.
- **Expected:** both writes fail (read-only); only the `/work` write succeeds. The rootfs and
  system dirs are read-only inside the guest — the single-writable-surface property holds.

### TC-017-03: FileRead path is read-only in the guest; host file never modified (F-007)

- **Requirement:** REQ-017-03
- **Type:** integration (Go test) — target L5
- **Input:** a firecracker run whose profile FileRead-mounts a host file `/host/tool`; the payload
  reads it (succeeds) then attempts to write to it.
- **Expected:** the read succeeds (the file's contents are visible in-guest); the write fails; the
  **host** `/host/tool` is byte-for-byte unchanged and no sibling `evil.txt` is created on the host.
  Mirrors `TestFileReadMountIsReadOnly_Bwrap` (host ground-truth check).

### TC-017-04: a bad workdir / FileRead path fails before any side effect (unchanged)

- **Requirement:** REQ-017-04
- **Type:** unit (Go test)
- **Input:** a firecracker run with a nonexistent `run.workdir`, and separately a relative FileRead
  path.
- **Expected:** `Run` returns `{error}` before the proxy/vault/launch side effects fire — identical
  to the bubblewrap/gVisor behavior (`validateWorkdir`/`validateFileReads` are tier-independent and
  unchanged). The firecracker tier does not silently run unmounted.

### TC-017-05: host-seeded file in /work is readable in-guest

- **Requirement:** REQ-017-01
- **Type:** integration (Go test) — target L5
- **Input:** seed `/work/seed.txt` on the host before the run; the payload reads it.
- **Expected:** the guest reads `seed.txt` with the seeded contents — the writable surface is the
  same host work dir, presented live (or copied in, per Q2). Mirrors the workdir read-back tests.

### TC-017-06: the mount presentation adds no writable host surface beyond /work

- **Requirement:** REQ-017-02
- **Type:** unit (Go test)
- **Input:** inspect the generated drive/share config for a run with `/work` + two FileRead paths.
- **Expected:** exactly one writable drive/share (the `/work` surface); every FileRead surface is
  marked read-only in the config (the `is_read_only`/equivalent flag is set). No extra writable
  device is present. This is the config-level analogue of F-006/F-007, checkable without a guest.

### TC-017-07: the FileRead presentation is read-only at the config level (negative guard)

- **Requirement:** REQ-017-03
- **Type:** unit (Go test, negative)
- **Input:** feed the read-only guard a config where a FileRead surface is mistakenly writable.
- **Expected:** the guard returns a non-nil error / the build path refuses it — proving the
  read-only assertion is not a no-op (it would catch a FileRead surface accidentally made writable).

### TC-017-08: Q2 mechanism chosen and recorded

- **Requirement:** REQ-017-05
- **Type:** inspection (spec + ADR)
- **Input:** read `docs/spec/behaviors.md` (the microVM mount flow) and the ADR-010 Q2 note after
  the feat commit.
- **Expected:** the chosen mechanism (block device / virtio-fs / copy-in-out) is stated as current
  truth in `behaviors.md` (present tense), with the read-only-ness + single-writable-surface
  properties documented as preserved; ADR-010's Q2 carries a resolution note pointing to the spec.
  If copy-in/out is chosen, the "writes persist at teardown, not live" nuance is documented.

### TC-017-09: the mount mechanism opens no network path

- **Requirement:** REQ-017-05
- **Type:** unit (Go test)
- **Input:** the full firecracker config for a run with `/work` + FileRead paths, serialized.
- **Expected:** still no `network-interface`/`network-interfaces` key (re-assert the no-NIC
  invariant on the mount-wired shape); the mount mechanism is a drive/share device, never a network
  device. The vsock is the only host↔guest channel besides the drives.

---

## Post-implementation verification

- [ ] TC-017-01/05: /work writable + persists; host-seeded file readable in-guest (L5)
- [ ] TC-017-02/06: /work is the only writable surface (config + behavioral)
- [ ] TC-017-03/07: FileRead read-only in-guest + host file unchanged; config guard rejects a writable FileRead
- [ ] TC-017-04: bad path fails before side effects (validation unchanged)
- [ ] TC-017-08: Q2 mechanism chosen + recorded in behaviors.md + ADR-010 note
- [ ] TC-017-09: mount mechanism opens no network path (no-NIC re-asserted)

## Test framework notes

- Standard Go `testing`. The config-level tests (TC-017-04/06/07/09) run without `/dev/kvm`. The
  behavioral mount tests (TC-017-01/02/03/05) need a booted guest (depends on task 015) and MUST
  skip-guard when prerequisites are absent.
- Reuse `validateWorkdir`/`validateFileReads` (`run.go`) unchanged — this task only changes the
  *presentation* of validated paths to the guest, not the validation.
- **Depends on task 013 (config skeleton) and task 015 (guest boot) landing first; BLOCKED on Q2**
  (the mount mechanism) being decided. Mark the coverage row `⚠️ planned, BLOCKED on Q2`.
