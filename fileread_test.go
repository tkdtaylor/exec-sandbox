package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileReadRequest builds a minimal RunRequest for the FileRead tests: a given tier, a FileRead
// capability over paths, an env map, an optional writable workdir, and a payload. No NetConnect
// capability is set (these tests exercise the mount + PATH, not egress).
func fileReadRequest(tier string, paths []string, env map[string]string, workdir, payload string) RunRequest {
	var req RunRequest
	req.Run.Payload = payload
	req.Run.Tier = tier
	req.Run.Workdir = workdir
	req.Run.Env = env
	caps := []any{}
	if paths != nil {
		anyPaths := make([]any, len(paths))
		for i, p := range paths {
			anyPaths[i] = p
		}
		caps = append(caps, map[string]any{"type": "FileRead", "paths": anyPaths})
	}
	req.Run.Profile = map[string]any{"capabilities": caps}
	req.Wiring.RequestID = "fileread-test"
	return req
}

// writeTool seeds an executable script in a fresh temp dir and returns (dir, toolPath).
func writeTool(t *testing.T, name, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	tool := filepath.Join(dir, name)
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, tool
}

// TC-001: FileRead{paths} parsing from profile.capabilities (union, ignore non-FileRead, absent⇒nil).
func TestFileReadParsing(t *testing.T) {
	// (a) one FileRead entry → its paths, order preserved.
	profA := map[string]any{"capabilities": []any{
		map[string]any{"type": "FileRead", "paths": []any{"/a", "/b"}},
	}}
	if got := fileReadPaths(profA); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("(a) fileReadPaths = %v, want [/a /b]", got)
	}

	// (b) two FileRead entries + a NetConnect entry → union of FileRead paths, NetConnect ignored.
	profB := map[string]any{"capabilities": []any{
		map[string]any{"type": "FileRead", "paths": []any{"/a"}},
		map[string]any{"type": "NetConnect", "allowlist": []any{"x.example.com:443"}},
		map[string]any{"type": "FileRead", "paths": []any{"/c"}},
	}}
	got := fileReadPaths(profB)
	if len(got) != 2 || got[0] != "/a" || got[1] != "/c" {
		t.Fatalf("(b) fileReadPaths = %v, want union [/a /c]", got)
	}

	// (c) no FileRead entry → nil/empty.
	profC := map[string]any{"capabilities": []any{
		map[string]any{"type": "NetConnect", "allowlist": []any{"x:1"}},
	}}
	if got := fileReadPaths(profC); len(got) != 0 {
		t.Fatalf("(c) fileReadPaths = %v, want empty", got)
	}

	// Edge: a FileRead entry with no/empty paths contributes nothing.
	profD := map[string]any{"capabilities": []any{
		map[string]any{"type": "FileRead"},
		map[string]any{"type": "FileRead", "paths": []any{}},
	}}
	if got := fileReadPaths(profD); len(got) != 0 {
		t.Fatalf("(edge) empty-paths FileRead contributed %v, want nothing", got)
	}
}

// TC-002: validateFileReads rejects relative / nonexistent, accepts absolute-existing; empty⇒ok.
func TestValidateFileReads(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// (a) existing absolute dir + (b) existing absolute file → no error.
	if err := validateFileReads([]string{dir, file}); err != nil {
		t.Fatalf("(a)/(b) validateFileReads(%v) error: %v", []string{dir, file}, err)
	}

	// (c) a relative path → error naming FileRead (NOT canonicalized, unlike validateWorkdir).
	if err := validateFileReads([]string{"rel/dir"}); err == nil {
		t.Fatal("(c) relative FileRead path accepted, want error")
	} else if !strings.Contains(err.Error(), "FileRead") {
		t.Fatalf("(c) error %q does not name FileRead", err)
	}

	// (d) an absolute nonexistent path → error naming the missing path.
	missing := filepath.Join(dir, "does-not-exist")
	if err := validateFileReads([]string{missing}); err == nil {
		t.Fatal("(d) nonexistent FileRead path accepted, want error")
	} else if !strings.Contains(err.Error(), "FileRead") {
		t.Fatalf("(d) error %q does not name FileRead", err)
	}

	// Edge: empty list → "no mounts", no error.
	if err := validateFileReads(nil); err != nil {
		t.Fatalf("(edge) empty list errored: %v", err)
	}
}

// TC-003: a FileRead-mounted marker exe is readable/executable by the payload at the same path (bwrap).
func TestFileReadMountReadableExecutable_Bwrap(t *testing.T) {
	requireBwrap(t)
	tools, tool := writeTool(t, "mytool", "echo tool-ran")
	res := Run(fileReadRequest("bubblewrap", []string{tools}, nil, "", tool+"\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if got := res["stdout"].(string); !strings.Contains(got, "tool-ran") {
		t.Fatalf("payload did not execute FileRead-mounted tool; stdout=%q", got)
	}
}

// TC-004: a FileRead-mounted tool on PATH resolves via `command -v` (bwrap).
func TestFileReadOnPathResolves_Bwrap(t *testing.T) {
	requireBwrap(t)
	tools, tool := writeTool(t, "mytool", "echo tool-ran")
	env := map[string]string{"PATH": tools + ":/usr/bin:/bin"}
	res := Run(fileReadRequest("bubblewrap", []string{tools}, env, "", "command -v mytool\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if got := strings.TrimSpace(res["stdout"].(string)); got != tool {
		t.Fatalf("command -v mytool = %q, want %q (resolved on PATH from the FileRead mount)", got, tool)
	}
}

// TC-005: a write to a FileRead mount fails — read-only, distinct from writable /work (bwrap).
func TestFileReadMountIsReadOnly_Bwrap(t *testing.T) {
	requireBwrap(t)
	tools, _ := writeTool(t, "mytool", "echo tool-ran")
	work := t.TempDir()
	// (a) /work write succeeds; (b) a write under the FileRead mount must fail.
	payload := "echo x > /work/ok.txt && (echo x > " + filepath.Join(tools, "evil.txt") +
		" && echo WROTE-TOOLS || echo tools-readonly)\n"
	res := Run(fileReadRequest("bubblewrap", []string{tools}, nil, work, payload))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if _, err := os.Stat(filepath.Join(work, "ok.txt")); err != nil {
		t.Fatalf("/work write did not persist: %v", err)
	}
	out := res["stdout"].(string)
	if strings.Contains(out, "WROTE-TOOLS") || !strings.Contains(out, "tools-readonly") {
		t.Fatalf("FileRead mount must be read-only; stdout=%q stderr=%q", out, res["stderr"])
	}
	// Ground truth: the host file was never created.
	if _, err := os.Stat(filepath.Join(tools, "evil.txt")); err == nil {
		t.Fatal("FileRead mount was writable: evil.txt persisted to the host")
	}
}

// TC-006: absent FileRead/env ⇒ no extra mounts, bare PATH, command -v resolves nothing (bwrap).
func TestNoFileReadNoEnv_Bwrap(t *testing.T) {
	requireBwrap(t)
	res := Run(fileReadRequest("bubblewrap", nil, nil, "",
		"echo PATH=$PATH; command -v mytool || echo no-tool\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	out := res["stdout"].(string)
	if !strings.Contains(out, "PATH=/usr/bin:/bin") {
		t.Fatalf("expected bare PATH=/usr/bin:/bin, stdout=%q", out)
	}
	if !strings.Contains(out, "no-tool") {
		t.Fatalf("command -v mytool should resolve nothing on a bare PATH, stdout=%q", out)
	}
	// The constructed argv carries no FileRead --ro-bind and only the default --setenv PATH.
	argv, _, _, err := bubblewrapBackend{}.Argv("/p/payload.sh", "/p/proxy.sock", "", nil, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !argvHasTriple(argv, "--setenv", "PATH", "/usr/bin:/bin") {
		t.Fatalf("no-env argv missing default --setenv PATH /usr/bin:/bin: %v", argv)
	}
	// An empty FileRead/env request is treated as absent: same argv.
	argv2, _, _, _ := bubblewrapBackend{}.Argv("/p/payload.sh", "/p/proxy.sock", "", []string{}, map[string]string{}, Limits{})
	if !argvHasTriple(argv2, "--setenv", "PATH", "/usr/bin:/bin") {
		t.Fatalf("empty FileRead/env argv missing default --setenv PATH: %v", argv2)
	}
}

// TC-007: a nonexistent / relative FileRead path fails loud before spawn (no payload runs).
func TestBadFileReadFailsLoud(t *testing.T) {
	// (a) nonexistent absolute path.
	res := Run(fileReadRequest("bubblewrap", []string{"/does/not/exist"}, nil, "", "echo should-not-run\n"))
	assertFileReadError(t, res, "nonexistent")

	// (b) relative path.
	res = Run(fileReadRequest("bubblewrap", []string{"rel/tools"}, nil, "", "echo should-not-run\n"))
	assertFileReadError(t, res, "relative")
}

func assertFileReadError(t *testing.T, res map[string]any, label string) {
	t.Helper()
	errStr, ok := res["error"].(string)
	if !ok {
		t.Fatalf("%s FileRead: expected {error}, got %v", label, res)
	}
	if !strings.Contains(errStr, "FileRead") {
		t.Fatalf("%s FileRead: error %q does not name FileRead", label, errStr)
	}
	// The payload never ran: no result fields (the check is before proxy/vault and spawn).
	if _, ran := res["sandbox_status"]; ran {
		t.Fatalf("%s FileRead: sandbox_status present — the payload ran despite a bad path", label)
	}
	if _, ran := res["stdout"]; ran {
		t.Fatalf("%s FileRead: stdout present — the payload ran despite a bad path", label)
	}
}

// TC-008: bwrap argv carries the read-only FileRead mount + provisioned PATH; netns stays unshared;
// empty ⇒ base unchanged.
func TestFileReadArgv_Bwrap(t *testing.T) {
	env := map[string]string{"PATH": "/abs/tools:/usr/bin:/bin"}
	argv, _, _, err := bubblewrapBackend{}.Argv("/p/payload.sh", "/p/proxy.sock", "", []string{"/abs/tools"}, env, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	// Read-only FileRead mount (--ro-bind, NOT the writable --bind).
	if !argvHasTriple(argv, "--ro-bind", "/abs/tools", "/abs/tools") {
		t.Fatalf("argv missing --ro-bind /abs/tools /abs/tools: %v", argv)
	}
	if argvHasTriple(argv, "--bind", "/abs/tools", "/abs/tools") {
		t.Fatalf("argv binds the FileRead path writable (--bind); must be --ro-bind: %v", argv)
	}
	// Provisioned PATH.
	if !argvHasTriple(argv, "--setenv", "PATH", "/abs/tools:/usr/bin:/bin") {
		t.Fatalf("argv missing --setenv PATH /abs/tools:/usr/bin:/bin: %v", argv)
	}
	// No-network invariant intact.
	if !argvContains(argv, "--unshare-all") {
		t.Fatalf("argv lost --unshare-all (no-network invariant): %v", argv)
	}
	if argvContains(argv, "--share-net") {
		t.Fatalf("argv added --share-net — no-network invariant broken: %v", argv)
	}

	// Empty FileRead/env ⇒ no extra --ro-bind for a FileRead path and the default --setenv PATH.
	base, _, _, _ := bubblewrapBackend{}.Argv("/p/payload.sh", "/p/proxy.sock", "", nil, nil, Limits{})
	if !argvHasTriple(base, "--setenv", "PATH", "/usr/bin:/bin") {
		t.Fatalf("base argv missing default --setenv PATH /usr/bin:/bin: %v", base)
	}
	if argvContains(base, "/abs/tools") {
		t.Fatalf("base argv unexpectedly references a FileRead path: %v", base)
	}
}

// TC-009: FileRead mount + PATH provisioning works end-to-end under gVisor.
func TestFileReadEndToEnd_Gvisor(t *testing.T) {
	requireRunsc(t)
	tools, tool := writeTool(t, "mytool", "echo tool-ran")
	env := map[string]string{"PATH": tools + ":/usr/bin:/bin"}
	res := Run(fileReadRequest("gvisor", []string{tools}, env, "", "command -v mytool; mytool\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("gvisor exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	out := res["stdout"].(string)
	if !strings.Contains(out, tool) {
		t.Fatalf("gvisor: command -v mytool did not resolve to %q; stdout=%q", tool, out)
	}
	if !strings.Contains(out, "tool-ran") {
		t.Fatalf("gvisor: FileRead-mounted tool did not execute; stdout=%q", out)
	}
	status := res["sandbox_status"].(map[string]any)
	if status["tier"] != "gvisor" {
		t.Fatalf("tier = %v, want gvisor", status["tier"])
	}
}

// TC-010: the gVisor OCI spec carries the read-only FileRead mounts + provisioned env; empty⇒base unchanged.
func TestFileReadOCISpec(t *testing.T) {
	// With FileRead paths + env.
	spec := gvisorOCISpec("/p/payload.sh", "/p/proxy.sock")
	applyFileReadToOCISpec(spec, []string{"/abs/tools"})
	applyEnvToOCISpec(spec, map[string]string{"PATH": "/abs/tools:/usr/bin:/bin"})

	var tools map[string]any
	for _, m := range spec["mounts"].([]map[string]any) {
		if m["destination"] == "/abs/tools" {
			tools = m
		}
	}
	if tools == nil {
		t.Fatal("no /abs/tools FileRead mount added")
	}
	if tools["source"] != "/abs/tools" || tools["type"] != "bind" {
		t.Fatalf("FileRead mount = %v, want a bind of /abs/tools", tools)
	}
	if !contains(tools["options"].([]string), "ro") {
		t.Fatalf("FileRead mount options must contain 'ro' (read-only): %v", tools["options"])
	}
	env := spec["process"].(map[string]any)["env"].([]string)
	if !contains(env, "PATH=/abs/tools:/usr/bin:/bin") {
		t.Fatalf("process.env missing provisioned PATH: %v", env)
	}
	// System mounts keep their ro option; the network namespace stays path-less.
	for _, m := range spec["mounts"].([]map[string]any) {
		if m["destination"] == "/usr" && !contains(m["options"].([]string), "ro") {
			t.Fatalf("/usr lost its 'ro' option: %v", m["options"])
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
		t.Fatalf("network namespace gained a path — must stay unshared: %v", netns)
	}

	// Empty FileRead/env ⇒ base spec unchanged: no extra mount, process.env stays bare PATH.
	base := gvisorOCISpec("/p/payload.sh", "/p/proxy.sock")
	applyFileReadToOCISpec(base, nil)
	applyEnvToOCISpec(base, nil)
	for _, m := range base["mounts"].([]map[string]any) {
		if m["destination"] == "/abs/tools" {
			t.Fatal("empty FileRead added a mount")
		}
	}
	if e := base["process"].(map[string]any)["env"].([]string); len(e) != 1 || e[0] != "PATH=/usr/bin:/bin" {
		t.Fatalf("empty env changed process.env to %v, want [PATH=/usr/bin:/bin]", e)
	}
}

// TC-011: no-FileRead / no-env runs produce the same base argv/spec as before this task.
func TestFileReadRegressionBaseUnchanged(t *testing.T) {
	// bwrap base argv with no FileRead/env matches the pre-task shape: default PATH, no FileRead mount.
	argv, _, _, err := bubblewrapBackend{}.Argv("/p/payload.sh", "/p/proxy.sock", "", nil, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !argvHasTriple(argv, "--setenv", "PATH", "/usr/bin:/bin") {
		t.Fatalf("regression: base argv lost default --setenv PATH /usr/bin:/bin: %v", argv)
	}
	if !argvContains(argv, "--unshare-all") || argvContains(argv, "--share-net") {
		t.Fatalf("regression: no-network invariant changed: %v", argv)
	}

	// gVisor base spec with no FileRead/env: process.env is the bare PATH, no FileRead mount.
	spec := gvisorOCISpec("/p/payload.sh", "/p/proxy.sock")
	applyFileReadToOCISpec(spec, nil)
	applyEnvToOCISpec(spec, nil)
	e := spec["process"].(map[string]any)["env"].([]string)
	if len(e) != 1 || e[0] != "PATH=/usr/bin:/bin" {
		t.Fatalf("regression: base process.env = %v, want [PATH=/usr/bin:/bin]", e)
	}
}
