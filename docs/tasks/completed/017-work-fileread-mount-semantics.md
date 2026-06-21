# Task 017: /work + FileRead mount semantics in the microVM

**Status:** ⬜ backlog
**Branch:** `task/017-work-fileread-mount-semantics`
**Spec:** [`docs/tasks/test-specs/017-work-fileread-mount-semantics-test-spec.md`](../test-specs/017-work-fileread-mount-semantics-test-spec.md)
**ADR:** ADR 010 D3 (the writable `/work` + read-only FileRead paths map onto guest-visible drives or a host-shared mechanism). **Resolves ADR 010 Q2** (block device vs virtio-fs vs copy-in/out) as an in-task decision. Preserves ADR 004 (writable `/work`) + ADR 005 (read-only FileRead, single writable surface).

## Readiness — BLOCKED on Q2 (smaller, in-task)

**BLOCKED on Q2** — the `/work` + FileRead presentation mechanism (block device / virtio-fs /
copy-in-out) is **not decided in the repo**. Unlike Q1/Q3 (which gate task 015 and need an ADR
amendment), **Q2 is a smaller in-task decision**: this task picks one mechanism and records it. The
row is `⚠️ planned, BLOCKED on Q2` so the choice is made deliberately — but it does not require an
ADR amendment before starting; the disposition is recorded in `docs/spec/behaviors.md` + an ADR-010
Q2 note as part of the work.

**READY to start once tasks 013 and 015 land AND Q2 is decided** (the decision can be made at the
top of this task). **Dependency position:** 013 → 014 → 015 → **{016, 017}** → 018. Sibling of 016.

## Problem

Bubblewrap and gVisor present `/work` and FileRead paths by **host bind-mount**: `/work` is the
single writable surface (`--bind`/non-`ro`; ADR 004, `run.go:345-347`) and each `FileRead{paths}`
entry is **read-only** (`--ro-bind` / OCI `options:[ro,rbind]`; ADR 005, `run.go:340-342`). A microVM
has **no host bind-mount** (separate kernel, separate VFS), so these paths must reach the guest by
some other mechanism — and the two properties ADR 004/005 guarantee must survive it:

1. `/work` is writable and the **only** writable surface (F-006) — payload writes persist to the host
   work dir; nothing else is writable.
2. FileRead paths are **read-only** (F-007) — a write fails; the host file is never modified/created.

The validation (`validateWorkdir`, `run.go:554`; `validateFileReads`, `run.go:499`) is
tier-independent and **unchanged** — a bad path is still a hard `{error}` before any side effect.
Only the *presentation* of validated paths to the guest changes.

## Q2 — in-task decision to make and record

**Q2 (ADR 010): `/work` + FileRead mount mechanism.** Choose among:
- **block device** — clean isolation boundary, but an image build per run;
- **virtio-fs** — a live host share, larger surface;
- **copy-in/copy-out** — simplest, but doubles I/O and changes "writes persist live" to "writes
  persist at teardown."

Whatever is chosen, **read-only-ness of FileRead and the single-writable-surface property must be
preserved** (ADR 010 D3 / Q2). Record the decision (and any semantic nuance, e.g. copy-in/out's
"persist at teardown") in `docs/spec/behaviors.md` + an ADR-010 Q2 note.

## Scope

- **Present the validated `run.workdir` as a writable `/work`** in the guest (cwd=/work), via the Q2
  mechanism; payload writes persist to the host work dir (ADR 004 semantics).
- **Present each validated FileRead path READ-ONLY** to the guest; a write fails in-guest and the host
  file is never modified/created (ADR 005 / F-007).
- **Keep `/work` the only writable surface** — rootfs, system dirs, `/payload.sh`, and every FileRead
  surface are read-only (F-006). Exactly one writable drive/share in the generated config.
- **Add a read-only guard** (config-level) that rejects a FileRead surface accidentally made writable
  (the negative case proving the check bites).
- **Re-assert the no-NIC invariant** on the mount-wired config (the mount mechanism is a drive/share
  device, never a network device; the vsock stays the only host↔guest channel besides drives).
- **Spec update in the same commit:** `docs/spec/behaviors.md` gains the microVM mount flow + the Q2
  decision; `docs/spec/configuration.md` notes the Firecracker presentation of `run.workdir` +
  `FileRead.paths`; ADR-010 carries a Q2 resolution note.

Out of scope: the writable-drive *sizing* (task 016 maps `disk_mb` → drive size; this task decides
what backs the drive); the egress path (014); teardown + fitness (018). Reuse
`validateWorkdir`/`validateFileReads` unchanged.

## Verification plan

- **Highest level achievable: L5 (per ADR-010 decomposition).** A validation harness on a booted
  guest: a host-seeded `/work` file is read in-guest, a guest write persists back to the host, a
  FileRead write fails and the host file is unchanged, and only `/work` is writable. Requires
  `/dev/kvm` + firecracker (rides on task 015); the config-level read-only guard + no-NIC + bad-path
  tests are L2 and run without `/dev/kvm`.
- **Harness command:** `go test -count=1 -run 'FirecrackerWork|FirecrackerFileRead|MountReadOnly|FirecrackerMount' ./...`;
  the in-guest mount TCs under `/dev/kvm`; `go test -count=1 ./...`; `gofmt -l .`.
- **Runtime observation (L5):** paste the host-seeded `/work/seed.txt` read-back line (TC-017-05); the
  guest `/work/out.txt` write persisted to the host dir + `pwd==/work` line (TC-017-01); the FileRead
  write-fails + host-file-unchanged + no host `evil.txt` line (TC-017-03); the "only `/work`
  writable; `/usr` write fails" line (TC-017-02). Show the config-level read-only guard rejects a
  writable FileRead surface (TC-017-07) and the mount-wired config carries no NIC (TC-017-09).
- **No ADR.** Record the **Q2 resolution** as a note on ADR-010 + the spec.

## Definition of done

- Q2 decided + recorded (`behaviors.md` present tense + ADR-010 Q2 note), with read-only-ness +
  single-writable-surface documented as preserved.
- `/work` is writable in-guest (cwd=/work) and persists to the host; a host-seeded `/work` file is
  readable in-guest.
- Each FileRead path is read-only in-guest; a write fails and the host file is unchanged/uncreated.
- `/work` is the only writable surface (config: exactly one writable drive/share; behavioral: `/usr`
  + `/payload.sh` writes fail); the read-only guard rejects a constructed writable FileRead surface.
- The mount-wired config carries no `network-interface` key; a bad workdir/FileRead path still fails
  before any side effect (validation unchanged).
- `behaviors.md` + `configuration.md` updated in place; no future tense.
- `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE + recorded L5 evidence before promotion to ✅.
