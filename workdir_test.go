package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workdirRequest builds a minimal RunRequest for the workdir tests: a given tier, host workdir, and
// payload, with no NetConnect capability (these tests exercise the mount, not egress).
func workdirRequest(tier, workdir, payload string) RunRequest {
	var req RunRequest
	req.Run.Payload = payload
	req.Run.Tier = tier
	req.Run.Workdir = workdir
	req.Wiring.RequestID = "workdir-test"
	return req
}

// argvHasPair reports whether argv contains the contiguous pair [a, b] (e.g. "--chdir", "/work").
func argvHasPair(argv []string, a, b string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == a && argv[i+1] == b {
			return true
		}
	}
	return false
}

func argvHasTriple(argv []string, a, b, c string) bool {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == a && argv[i+1] == b && argv[i+2] == c {
			return true
		}
	}
	return false
}

func argvContains(argv []string, s string) bool {
	for _, x := range argv {
		if x == s {
			return true
		}
	}
	return false
}

// TC-001: validateWorkdir resolves a good dir to absolute and rejects blank/missing/non-dir.
func TestValidateWorkdir(t *testing.T) {
	// (a) blank → ("", nil), "no workdir mount".
	for _, blank := range []string{"", "   ", "\t"} {
		got, err := validateWorkdir(blank)
		if err != nil || got != "" {
			t.Fatalf("validateWorkdir(%q) = (%q, %v), want (\"\", nil)", blank, got, err)
		}
	}

	// (b) a real dir → its absolute path, no error. Use a relative path to prove canonicalization.
	dir := t.TempDir()
	rel, err := filepath.Rel(mustGetwd(t), dir)
	if err == nil { // only assert the relative case when a relative form exists
		got, err := validateWorkdir(rel)
		if err != nil {
			t.Fatalf("validateWorkdir(%q) unexpected error: %v", rel, err)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("validateWorkdir(%q) = %q, want an absolute path", rel, got)
		}
		if got != dir {
			t.Fatalf("validateWorkdir(%q) = %q, want %q", rel, got, dir)
		}
	}

	// (c) a path that does not exist → error naming run.workdir.
	missing := filepath.Join(dir, "does-not-exist")
	if got, err := validateWorkdir(missing); err == nil {
		t.Fatalf("validateWorkdir(%q) = (%q, nil), want an error", missing, got)
	} else if !strings.Contains(err.Error(), "run.workdir") {
		t.Fatalf("validateWorkdir(%q) error = %q, want it to name run.workdir", missing, err)
	}

	// (d) a regular file (not a dir) → error.
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := validateWorkdir(file); err == nil {
		t.Fatalf("validateWorkdir(%q) = (%q, nil), want an error for a non-directory", file, got)
	} else if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("validateWorkdir(%q) error = %q, want 'not a directory'", file, err)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// TC-002: a file seeded in the host workdir is readable by the payload at /work (bwrap).
func TestWorkdirSeededFileReadable_Bwrap(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("hello-from-host"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Run(workdirRequest("bubblewrap", dir, "cat /work/seed.txt\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if got := strings.TrimSpace(res["stdout"].(string)); got != "hello-from-host" {
		t.Fatalf("payload read %q from /work/seed.txt, want %q", got, "hello-from-host")
	}
}

// TC-003: a file the payload writes under /work persists to the host dir after the run (bwrap).
func TestWorkdirWritePersists_Bwrap(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	res := Run(workdirRequest("bubblewrap", dir, "echo built > /work/out.txt\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	// The proof the mount is read-write: the host file exists after Run returns.
	b, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("host /work/out.txt not persisted: %v", err)
	}
	if strings.TrimSpace(string(b)) != "built" {
		t.Fatalf("persisted out.txt = %q, want %q", strings.TrimSpace(string(b)), "built")
	}
}

// TC-004: the payload's current directory is /work (bwrap) — pwd and a relative write both prove it.
func TestWorkdirIsCwd_Bwrap(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	res := Run(workdirRequest("bubblewrap", dir, "pwd; echo marker > rel.txt\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if got := strings.TrimSpace(res["stdout"].(string)); got != "/work" {
		t.Fatalf("pwd = %q, want /work", got)
	}
	// A relative write resolves under the cwd → lands in the host workdir.
	if _, err := os.Stat(filepath.Join(dir, "rel.txt")); err != nil {
		t.Fatalf("relative write did not land in /work (cwd not /work): %v", err)
	}
}

// TC-005: absent run.workdir ⇒ no /work mount, cwd unchanged, prior behavior preserved (bwrap).
func TestNoWorkdirNoMount_Bwrap(t *testing.T) {
	requireBwrap(t)
	// An all-whitespace workdir is treated as absent (no mount).
	res := Run(workdirRequest("bubblewrap", "   ", "test ! -e /work && echo no-work; pwd\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	out := res["stdout"].(string)
	if !strings.Contains(out, "no-work") {
		t.Fatalf("expected /work to be absent (no mount), stdout=%q", out)
	}
	if strings.Contains(out, "/work") {
		t.Fatalf("cwd should not be /work without a workdir, stdout=%q", out)
	}
	// The constructed argv carries no /work bind and no --chdir.
	argv, _, _, err := bubblewrapBackend{}.Argv("/p/payload.sh", "/p/proxy.sock", "", Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if argvContains(argv, "/work") || argvContains(argv, "--chdir") {
		t.Fatalf("no-workdir argv must not bind /work or set --chdir: %v", argv)
	}
}

// TC-006: a nonexistent / non-directory run.workdir fails loud before spawn (no payload runs).
func TestBadWorkdirFailsLoud(t *testing.T) {
	dir := t.TempDir()

	// (a) nonexistent path.
	missing := filepath.Join(dir, "nope")
	res := Run(workdirRequest("bubblewrap", missing, "echo should-not-run\n"))
	assertWorkdirError(t, res, "nonexistent")

	// (b) a regular file, not a dir.
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res = Run(workdirRequest("bubblewrap", file, "echo should-not-run\n"))
	assertWorkdirError(t, res, "file")
}

func assertWorkdirError(t *testing.T, res map[string]any, label string) {
	t.Helper()
	errStr, ok := res["error"].(string)
	if !ok {
		t.Fatalf("%s workdir: expected {error}, got %v", label, res)
	}
	if !strings.Contains(errStr, "run.workdir") {
		t.Fatalf("%s workdir: error %q does not name run.workdir", label, errStr)
	}
	// The payload never ran: no result fields.
	if _, ran := res["sandbox_status"]; ran {
		t.Fatalf("%s workdir: sandbox_status present — the payload ran despite a bad workdir", label)
	}
	if _, ran := res["stdout"]; ran {
		t.Fatalf("%s workdir: stdout present — the payload ran despite a bad workdir", label)
	}
}

// TC-007: the workdir mount works end-to-end under gVisor (read + write-persist + cwd).
func TestWorkdirEndToEnd_Gvisor(t *testing.T) {
	requireRunsc(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("hello-from-host"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Run(workdirRequest("gvisor", dir, "cat /work/seed.txt; pwd; echo built > /work/out.txt\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("gvisor exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	out := res["stdout"].(string)
	if !strings.Contains(out, "hello-from-host") {
		t.Fatalf("gvisor payload did not read seeded /work file, stdout=%q", out)
	}
	if !strings.Contains(out, "/work") {
		t.Fatalf("gvisor cwd not /work, stdout=%q", out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "out.txt"))
	if err != nil {
		t.Fatalf("gvisor host /work/out.txt not persisted: %v", err)
	}
	if strings.TrimSpace(string(b)) != "built" {
		t.Fatalf("gvisor persisted out.txt = %q, want %q", strings.TrimSpace(string(b)), "built")
	}
	status := res["sandbox_status"].(map[string]any)
	if status["tier"] != "gvisor" {
		t.Fatalf("tier = %v, want gvisor", status["tier"])
	}
}

// TC-008: the gVisor OCI spec carries the writable /work mount and cwd; empty ⇒ base unchanged.
func TestWorkdirOCISpec(t *testing.T) {
	// Non-empty workdir: a writable /work bind mount and process.cwd == /work.
	spec := gvisorOCISpec("/work/payload.sh", "/work/proxy.sock")
	applyWorkdirToOCISpec(spec, "/abs/work")

	mounts, _ := spec["mounts"].([]map[string]any)
	var work map[string]any
	for _, m := range mounts {
		if m["destination"] == "/work" {
			work = m
		}
	}
	if work == nil {
		t.Fatal("no /work mount added")
	}
	if work["source"] != "/abs/work" || work["type"] != "bind" {
		t.Fatalf("/work mount = %v, want a bind of /abs/work", work)
	}
	for _, o := range work["options"].([]string) {
		if o == "ro" {
			t.Fatalf("/work mount is read-only (has 'ro'); must be writable: %v", work["options"])
		}
	}
	if cwd := spec["process"].(map[string]any)["cwd"]; cwd != "/work" {
		t.Fatalf("process.cwd = %v, want /work", cwd)
	}

	// Empty workdir: the base spec is unchanged — no /work mount, cwd stays "/".
	base := gvisorOCISpec("/work/payload.sh", "/work/proxy.sock")
	applyWorkdirToOCISpec(base, "")
	for _, m := range base["mounts"].([]map[string]any) {
		if m["destination"] == "/work" {
			t.Fatal("empty workdir added a /work mount")
		}
	}
	if cwd := base["process"].(map[string]any)["cwd"]; cwd != "/" {
		t.Fatalf("empty workdir changed cwd to %v, want /", cwd)
	}
}

// TC-009: only /work is writable — system dirs stay ro, the netns stays unshared.
func TestOnlyWorkdirWritable_Bwrap(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	// (a) /work write succeeds; (b) /usr write fails (read-only system dir).
	payload := "echo ok > /work/ok.txt && (echo x > /usr/x.txt && echo WROTE-USR || echo usr-readonly)\n"
	res := Run(workdirRequest("bubblewrap", dir, payload))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if _, err := os.Stat(filepath.Join(dir, "ok.txt")); err != nil {
		t.Fatalf("/work write did not persist: %v", err)
	}
	out := res["stdout"].(string)
	if strings.Contains(out, "WROTE-USR") || !strings.Contains(out, "usr-readonly") {
		t.Fatalf("/usr must stay read-only; stdout=%q stderr=%q", out, res["stderr"])
	}

	// (c) argv/spec inspection: bwrap binds /work writable (--bind, not --ro-bind), sets --chdir,
	// keeps --unshare-all, adds no --share-net.
	argv, _, _, err := bubblewrapBackend{}.Argv("/p/payload.sh", "/p/proxy.sock", dir, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !argvHasTriple(argv, "--bind", dir, "/work") {
		t.Fatalf("argv missing writable --bind %s /work: %v", dir, argv)
	}
	if argvHasTriple(argv, "--ro-bind", dir, "/work") {
		t.Fatalf("argv binds /work read-only; must be writable: %v", argv)
	}
	if !argvHasPair(argv, "--chdir", "/work") {
		t.Fatalf("argv missing --chdir /work: %v", argv)
	}
	if !argvContains(argv, "--unshare-all") {
		t.Fatalf("argv lost --unshare-all (no-network invariant): %v", argv)
	}
	if argvContains(argv, "--share-net") {
		t.Fatalf("argv added --share-net — the no-network invariant is broken: %v", argv)
	}

	// OCI spec: /work mount writable while /usr keeps "ro"; network namespace still present, no path.
	spec := gvisorOCISpec("/p/payload.sh", "/p/proxy.sock")
	applyWorkdirToOCISpec(spec, dir)
	for _, m := range spec["mounts"].([]map[string]any) {
		opts, _ := m["options"].([]string)
		switch m["destination"] {
		case "/work":
			for _, o := range opts {
				if o == "ro" {
					t.Fatalf("OCI /work mount is read-only: %v", opts)
				}
			}
		case "/usr", "/etc":
			if !contains(opts, "ro") {
				t.Fatalf("OCI %v mount lost its 'ro' option: %v", m["destination"], opts)
			}
		}
	}
	ns := spec["linux"].(map[string]any)["namespaces"].([]map[string]any)
	var netns map[string]any
	for _, n := range ns {
		if n["type"] == "network" {
			netns = n
		}
	}
	if netns == nil {
		t.Fatal("OCI spec lost its network namespace (no-network invariant)")
	}
	if _, hasPath := netns["path"]; hasPath {
		t.Fatalf("network namespace gained a path — it must stay empty/unshared: %v", netns)
	}
}
