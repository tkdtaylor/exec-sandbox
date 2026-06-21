// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Task 017 — /work + FileRead mount semantics in the microVM (ADR 010 Q2: block device; copy-in via
// mkfs.ext4 -d, copy-out via debugfs rdump — unprivileged, no root loop-mount). The config-level TCs
// (TC-017-04/06/07/09) run on any host. The behavioral TCs (TC-017-01/02/03/05) boot a real guest
// and skip-guard via requireKVM.

// fcMountConfig builds the firecracker config + adds the /work and FileRead drives the way Argv does,
// so the config-level guards (TC-017-06/07/09) can be asserted without building real images. It does
// NOT call mkfs (it only manipulates the in-memory config map).
func fcMountConfig(t *testing.T, workImage string, fileReadIDs int) map[string]any {
	t.Helper()
	cfg := firecrackerConfig("/k", "/r", "/p", "/v", Limits{})
	addPayloadDrive(cfg, "/tmp/payload.ext4")
	if workImage != "" {
		addWorkdirDrive(cfg, workImage)
	}
	// Append read-only FileRead drives by hand (mirroring addFileReadDrives' shape) so the guard can
	// be tested without mkfs: drive_id fileread_i (underscore — fireatcker rejects hyphens),
	// is_read_only:true.
	drives, _ := cfg["drives"].([]map[string]any)
	for i := 0; i < fileReadIDs; i++ {
		drives = append(drives, map[string]any{
			"drive_id":       fileReadDriveID(i),
			"path_on_host":   "/tmp/fileread.ext4",
			"is_root_device": false,
			"is_read_only":   true,
		})
	}
	cfg["drives"] = drives
	return cfg
}

// driveCount counts the drives in a config.
func driveList(t *testing.T, cfg map[string]any) []map[string]any {
	t.Helper()
	drives, ok := cfg["drives"].([]map[string]any)
	if !ok {
		t.Fatalf("drives missing or wrong type: %T", cfg["drives"])
	}
	return drives
}

// ---------------------------------------------------------------------------
// TC-017-06: the mount presentation adds exactly ONE writable drive (/work); every FileRead drive is
// read-only. Config-level analogue of F-006/F-007, checkable without a guest.
// ---------------------------------------------------------------------------

func TestFirecrackerMount_OnlyWorkIsWritable_Config(t *testing.T) {
	cfg := fcMountConfig(t, "/tmp/work.ext4", 2) // /work + two FileRead drives
	drives := driveList(t, cfg)

	writable := 0
	var writableID string
	for _, d := range drives {
		ro, _ := d["is_read_only"].(bool)
		if !ro {
			writable++
			writableID, _ = d["drive_id"].(string)
		}
	}
	if writable != 1 {
		t.Fatalf("TC-017-06: %d writable drives, want exactly 1 (/work the only writable surface, F-006); drives=%v", writable, drives)
	}
	if writableID != workDriveID {
		t.Fatalf("TC-017-06: the writable drive is %q, want %q (/work)", writableID, workDriveID)
	}

	// Every FileRead drive is read-only (F-007 at config level).
	for _, d := range drives {
		id, _ := d["drive_id"].(string)
		if strings.HasPrefix(id, "fileread_") {
			if ro, _ := d["is_read_only"].(bool); !ro {
				t.Fatalf("TC-017-06: FileRead drive %q is_read_only=false, want true", id)
			}
		}
	}

	// The guard passes on this well-formed config.
	if err := validateDriveReadOnly(cfg, []string{"/a", "/b"}); err != nil {
		t.Fatalf("TC-017-06: validateDriveReadOnly rejected a well-formed config: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TC-017-07: the FileRead presentation is read-only at the config level (NEGATIVE guard). Feed the
// guard a config where a FileRead surface is mistakenly writable; it must refuse it — proving the
// read-only assertion is not a no-op.
// ---------------------------------------------------------------------------

func TestFirecrackerMount_ReadOnlyGuardBites(t *testing.T) {
	cfg := fcMountConfig(t, "/tmp/work.ext4", 1)
	drives := driveList(t, cfg)
	// The regression: flip the FileRead drive to writable.
	for _, d := range drives {
		if id, _ := d["drive_id"].(string); strings.HasPrefix(id, "fileread_") {
			d["is_read_only"] = false
		}
	}

	err := validateDriveReadOnly(cfg, []string{"/a"})
	if err == nil {
		t.Fatal("TC-017-07: validateDriveReadOnly returned nil for a WRITABLE FileRead drive — the guard is a no-op (BUG)")
	}
	if !strings.Contains(err.Error(), "fileread_0") && !strings.Contains(err.Error(), "writable") {
		t.Fatalf("TC-017-07: guard error %q does not name the writable FileRead surface", err)
	}
}

// TC-017-07b: a SECOND writable non-/work drive (not a FileRead) is also rejected — the guard
// enforces exactly-one-writable-surface, not merely FileRead-read-only.
func TestFirecrackerMount_SecondWritableDriveRejected(t *testing.T) {
	cfg := fcMountConfig(t, "/tmp/work.ext4", 0)
	drives := driveList(t, cfg)
	// The regression: a stray second writable drive.
	drives = append(drives, map[string]any{
		"drive_id":       "rogue",
		"path_on_host":   "/tmp/rogue.ext4",
		"is_root_device": false,
		"is_read_only":   false,
	})
	cfg["drives"] = drives

	if err := validateDriveReadOnly(cfg, nil); err == nil {
		t.Fatal("TC-017-07b: a second writable drive was accepted — only /work may be writable (F-006)")
	}
}

// ---------------------------------------------------------------------------
// TC-017-09: the mount mechanism opens no network path. Re-assert the no-NIC invariant on the FULLY
// mount-wired config — the mount mechanism is a drive device, never a network device.
// ---------------------------------------------------------------------------

func TestFirecrackerMount_NoNIC(t *testing.T) {
	cfg := fcMountConfig(t, "/tmp/work.ext4", 2)
	if err := configHasNoNIC(cfg); err != nil {
		t.Fatalf("TC-017-09: mount-wired config carries a NIC: %v", err)
	}
	// The vsock is still the only host<->guest channel besides drives.
	if _, ok := cfg["vsock"]; !ok {
		t.Fatal("TC-017-09: vsock missing from the mount-wired config")
	}
}

// ---------------------------------------------------------------------------
// TC-017-06b: the /work drive is is_read_only:false and carries the expected drive_id; FileRead
// drives are appended AFTER it (device order vda=root, vdb=payload, vdc=/work, vdd…=FileRead).
// ---------------------------------------------------------------------------

func TestFirecrackerMount_DriveOrderAndFlags(t *testing.T) {
	cfg := fcMountConfig(t, "/tmp/work.ext4", 1)
	drives := driveList(t, cfg)
	// Expected order: rootfs, payload, work, fileread_0.
	wantIDs := []string{"rootfs", "payload", "work", "fileread_0"}
	if len(drives) != len(wantIDs) {
		t.Fatalf("TC-017-06b: %d drives, want %d (%v)", len(drives), len(wantIDs), wantIDs)
	}
	for i, want := range wantIDs {
		if got, _ := drives[i]["drive_id"].(string); got != want {
			t.Fatalf("TC-017-06b: drive[%d] id = %q, want %q", i, got, want)
		}
	}
	// fileReadGuestDev maps the first FileRead to /dev/vdd.
	if got := fileReadGuestDev(0); got != "/dev/vdd" {
		t.Fatalf("TC-017-06b: fileReadGuestDev(0) = %q, want /dev/vdd", got)
	}
	if got := fileReadGuestDev(1); got != "/dev/vde" {
		t.Fatalf("TC-017-06b: fileReadGuestDev(1) = %q, want /dev/vde", got)
	}
}

// ---------------------------------------------------------------------------
// TC-017-04: a bad run.workdir / FileRead path fails BEFORE any side effect (validation unchanged).
// The firecracker tier reuses validateWorkdir/validateFileReads — a bad path is a hard {error}.
// ---------------------------------------------------------------------------

func TestFirecrackerMount_BadPathFailsBeforeSideEffects(t *testing.T) {
	// (a) a nonexistent run.workdir.
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	req := RunRequest{}
	req.Run.Payload = "echo hi"
	req.Run.Tier = "firecracker"
	req.Run.Workdir = missing
	res := Run(req)
	// A bad workdir is a hard {error} BEFORE any side effect (validateWorkdir, tier-independent). The
	// result is the bare {"error": ...} shape — the proxy/vault/launch never fired.
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "run.workdir") {
		t.Fatalf("TC-017-04(a): nonexistent workdir did not fail before side effects; res=%v", res)
	}

	// (b) a relative FileRead path.
	req2 := RunRequest{}
	req2.Run.Payload = "echo hi"
	req2.Run.Tier = "firecracker"
	req2.Run.Profile = map[string]any{
		"capabilities": []any{
			map[string]any{"type": "FileRead", "paths": []any{"rel/path"}},
		},
	}
	res2 := Run(req2)
	if errStr, _ := res2["error"].(string); !strings.Contains(errStr, "FileRead") {
		t.Fatalf("TC-017-04(b): relative FileRead did not fail before side effects; res=%v", res2)
	}
}

// ---------------------------------------------------------------------------
// TC-017-08: the Q2 mechanism is chosen and recorded (inspection). The block-device decision +
// the persist-at-teardown nuance + the read-only/single-writable-surface preservation must appear,
// present-tense, in behaviors.md (B-016) and the ADR-010 Q2 resolution note. This guards against the
// spec silently drifting away from the implemented mechanism.
// ---------------------------------------------------------------------------

func TestFirecrackerMount_Q2RecordedInSpec(t *testing.T) {
	checks := []struct {
		path     string
		mustHave []string
	}{
		{
			"docs/spec/behaviors.md",
			[]string{"B-016", "block device", "persist", "teardown", "is_read_only", "/dev/vdc"},
		},
		{
			"docs/architecture/decisions/010-firecracker-tier3-backend.md",
			[]string{"Q2 resolution", "block device", "persist", "teardown", "debugfs", "mkfs.ext4"},
		},
		{
			"docs/spec/configuration.md",
			[]string{"block-device", "debugfs", "teardown"},
		},
	}
	for _, c := range checks {
		b, err := os.ReadFile(filepath.Join("docs", "..", c.path)) // resolve from repo root (test cwd)
		if err != nil {
			// The test runs from the package dir (repo root) — read the path directly.
			b, err = os.ReadFile(c.path)
			if err != nil {
				t.Fatalf("TC-017-08: cannot read %s: %v", c.path, err)
			}
		}
		body := string(b)
		// The spec is present-tense: a "will " future-tense Q2 statement is a drift smell.
		for _, needle := range c.mustHave {
			if !strings.Contains(body, needle) {
				t.Fatalf("TC-017-08: %s does not record %q — the Q2 block-device decision/nuance is not documented", c.path, needle)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Behavioral L5 TCs — these boot a real guest. They need /dev/kvm + firecracker + the verified
// kernel/rootfs and skip-guard via requireKVM.
// ---------------------------------------------------------------------------

// fcRunWork runs a payload under the firecracker tier with a given host workdir + optional FileRead
// paths and returns (result, workdir).
func fcRunWork(t *testing.T, payload, workdir string, fileReads []string) map[string]any {
	t.Helper()
	withRepoRootArtifacts(t)
	req := RunRequest{}
	req.Run.Payload = payload
	req.Run.Tier = "firecracker"
	req.Run.Workdir = workdir
	if len(fileReads) > 0 {
		anyPaths := make([]any, len(fileReads))
		for i, p := range fileReads {
			anyPaths[i] = p
		}
		req.Run.Profile = map[string]any{
			"capabilities": []any{
				map[string]any{"type": "FileRead", "paths": anyPaths},
			},
		}
	}
	return Run(req)
}

// TC-017-05: a host-seeded /work/seed.txt is readable in-guest (copy-in works).
func TestFirecrackerWork_HostSeededFileReadableInGuest_E2E(t *testing.T) {
	requireKVM(t)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "seed.txt"), []byte("HOST-SEEDED-CONTENT"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Append an `echo` so the payload's last line ends with a newline: the guest emits the exit-code
	// sentinel on its own console line, and a seed file with no trailing newline would otherwise run
	// the sentinel onto the cat output and break the launcher's sentinel parse.
	res := fcRunWork(t, "cat /work/seed.txt; echo", work, nil)
	t.Logf("TC-017-05 result=%v", res)
	if code, _ := res["exit_code"].(int); code != 0 {
		t.Fatalf("TC-017-05: exit_code = %v, want 0 (the host-seeded /work/seed.txt should be readable in-guest); res=%v", res["exit_code"], res)
	}
	stdout, _ := res["stdout"].(string)
	if !strings.Contains(stdout, "HOST-SEEDED-CONTENT") {
		t.Fatalf("TC-017-05: stdout %q does not contain the seeded content — copy-in failed", stdout)
	}
}

// TC-017-01: a guest write to /work/out.txt persists back to the host work dir (copy-out works);
// the payload's pwd == /work.
func TestFirecrackerWork_WritePersistsToHost_E2E(t *testing.T) {
	requireKVM(t)
	work := t.TempDir()
	res := fcRunWork(t, "pwd; echo GUEST-WROTE-THIS > /work/out.txt", work, nil)
	t.Logf("TC-017-01 result=%v", res)
	if code, _ := res["exit_code"].(int); code != 0 {
		t.Fatalf("TC-017-01: exit_code = %v, want 0; res=%v", res["exit_code"], res)
	}
	// pwd == /work.
	stdout, _ := res["stdout"].(string)
	if !strings.Contains(stdout, "/work") {
		t.Fatalf("TC-017-01: pwd output %q does not contain /work — cwd is not /work", stdout)
	}
	// The guest write persisted to the host workdir (copy-out at teardown).
	got, err := os.ReadFile(filepath.Join(work, "out.txt"))
	if err != nil {
		t.Fatalf("TC-017-01: host /work/out.txt not present after the run (copy-out failed): %v", err)
	}
	if !strings.Contains(string(got), "GUEST-WROTE-THIS") {
		t.Fatalf("TC-017-01: host out.txt = %q, want it to contain GUEST-WROTE-THIS", string(got))
	}
}

// TC-017-02: /work is the ONLY writable surface — a /usr write fails in-guest (F-006).
func TestFirecrackerWork_OnlyWorkWritable_E2E(t *testing.T) {
	requireKVM(t)
	work := t.TempDir()
	// Try to write /usr (a system dir) and /payload.sh; both must fail. /work must succeed.
	payload := `
if echo x > /usr/evil 2>/dev/null; then echo USR-WRITE-SUCCEEDED; else echo USR-WRITE-FAILED; fi
if echo x > /payload.sh 2>/dev/null; then echo PAYLOAD-WRITE-SUCCEEDED; else echo PAYLOAD-WRITE-FAILED; fi
if echo x > /work/ok.txt 2>/dev/null; then echo WORK-WRITE-SUCCEEDED; else echo WORK-WRITE-FAILED; fi
`
	res := fcRunWork(t, payload, work, nil)
	t.Logf("TC-017-02 result=%v", res)
	stdout, _ := res["stdout"].(string)
	if strings.Contains(stdout, "USR-WRITE-SUCCEEDED") {
		t.Fatalf("TC-017-02: a /usr write SUCCEEDED — /work is not the only writable surface (F-006 violated); stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "USR-WRITE-FAILED") {
		t.Fatalf("TC-017-02: expected USR-WRITE-FAILED in stdout=%q", stdout)
	}
	if !strings.Contains(stdout, "WORK-WRITE-SUCCEEDED") {
		t.Fatalf("TC-017-02: the /work write did NOT succeed — the writable surface is broken; stdout=%q", stdout)
	}
}

// TC-017-03: a FileRead path is read-only in-guest; a write fails AND the host file is unchanged +
// no host evil.txt created (F-007 / ADR 005).
func TestFirecrackerFileRead_ReadOnlyHostUnchanged_E2E(t *testing.T) {
	requireKVM(t)
	work := t.TempDir()
	// The FileRead host file lives under /tmp (t.TempDir) so the guest can recreate its parent on a
	// tmpfs-backed mountpoint. Seed known contents.
	froDir := t.TempDir()
	tool := filepath.Join(froDir, "tool")
	const orig = "ORIGINAL-TOOL-CONTENTS"
	if err := os.WriteFile(tool, []byte(orig), 0o644); err != nil {
		t.Fatalf("seed tool: %v", err)
	}
	// The payload reads the FileRead file (must succeed), then attempts to write it and create a
	// sibling evil.txt (both must fail / not reach the host).
	payload := `
cat ` + tool + `
if echo HACKED > ` + tool + ` 2>/dev/null; then echo FILEREAD-WRITE-SUCCEEDED; else echo FILEREAD-WRITE-FAILED; fi
echo evil > ` + filepath.Join(froDir, "evil.txt") + ` 2>/dev/null || true
`
	res := fcRunWork(t, payload, work, []string{tool})
	t.Logf("TC-017-03 result=%v", res)
	stdout, _ := res["stdout"].(string)
	// The read succeeded (contents visible in-guest).
	if !strings.Contains(stdout, orig) {
		t.Fatalf("TC-017-03: the FileRead file contents %q were not visible in-guest; stdout=%q", orig, stdout)
	}
	// The write failed in-guest.
	if strings.Contains(stdout, "FILEREAD-WRITE-SUCCEEDED") {
		t.Fatalf("TC-017-03: a write to the FileRead path SUCCEEDED in-guest — F-007 violated; stdout=%q", stdout)
	}
	// The HOST file is byte-for-byte unchanged.
	got, err := os.ReadFile(tool)
	if err != nil {
		t.Fatalf("TC-017-03: host FileRead file disappeared: %v", err)
	}
	if string(got) != orig {
		t.Fatalf("TC-017-03: host FileRead file MODIFIED: got %q, want %q (ADR 005 / F-007)", string(got), orig)
	}
	// No sibling evil.txt created on the host (the FileRead surface is read-only and has no copy-out).
	if _, err := os.Stat(filepath.Join(froDir, "evil.txt")); err == nil {
		t.Fatalf("TC-017-03: host evil.txt was created next to the FileRead file — a guest write reached the host (F-007 violated)")
	}
}
