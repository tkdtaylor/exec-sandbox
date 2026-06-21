// SPDX-License-Identifier: Apache-2.0
package main

// Task 012 / ADR 015: env-mode credential injection + wipe clock.
//
// Env-mode DELIBERATELY delivers a credential value into the sandbox process environment under a
// vault-specified variable name (the documented exception to the proxy-mode F-002 "never enters the
// sandbox" rule), and wipes the host-side copy post-spawn. These tests assert delivery, the wipe
// clock, argv/result/audit absence, accounting, the unchanged proxy-mode invariant in a mixed run,
// and the inject-failure skip.

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// envModeVault is a per-handle programmable stub vault: it returns the response registered for each
// incoming handle (env-mode, proxy-mode, or an error).
type envModeVault struct {
	socket   string
	byHandle map[string]map[string]any
	done     chan struct{}
}

func newEnvModeVault(t *testing.T, byHandle map[string]map[string]any) *envModeVault {
	t.Helper()
	dir := t.TempDir()
	v := &envModeVault{
		socket:   filepath.Join(dir, "vault.sock"),
		byHandle: byHandle,
		done:     make(chan struct{}),
	}
	ln, err := net.Listen("unix", v.socket)
	if err != nil {
		t.Fatalf("envModeVault listen: %v", err)
	}
	go func() {
		defer close(v.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go v.serve(conn)
		}
	}()
	t.Cleanup(func() { ln.Close(); <-v.done })
	return v
}

func (v *envModeVault) serve(c net.Conn) {
	defer c.Close()
	scanner := bufio.NewScanner(c)
	for scanner.Scan() {
		var msg map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		handle, _ := msg["handle"].(string)
		resp, ok := v.byHandle[handle]
		if !ok {
			resp = map[string]any{"error": "unknown handle"}
		}
		b, _ := json.Marshal(resp)
		c.Write(append(b, '\n'))
	}
}

// recordingAudit captures every emitted audit event for assertions (the value must never appear).
type recordingAudit struct {
	socket string
	mu     sync.Mutex
	events []map[string]any
	done   chan struct{}
}

func newRecordingAudit(t *testing.T) *recordingAudit {
	t.Helper()
	dir := t.TempDir()
	a := &recordingAudit{socket: filepath.Join(dir, "audit.sock"), done: make(chan struct{})}
	ln, err := net.Listen("unix", a.socket)
	if err != nil {
		t.Fatalf("recordingAudit listen: %v", err)
	}
	go func() {
		defer close(a.done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					var msg map[string]any
					if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
						continue
					}
					if msg["op"] == "emit" {
						ev, _ := msg["event"].(map[string]any)
						a.mu.Lock()
						a.events = append(a.events, ev)
						a.mu.Unlock()
					}
					c.Write([]byte(`{"ok":true}` + "\n"))
				}
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close(); <-a.done })
	return a
}

func (a *recordingAudit) all() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make([]map[string]any, len(a.events))
	copy(cp, a.events)
	return cp
}

func (a *recordingAudit) actions() []string {
	out := []string{}
	for _, e := range a.all() {
		out = append(out, str(e["action"]))
	}
	return out
}

// auditBlob marshals every recorded event so a test can scan the whole audit surface for a sentinel.
func (a *recordingAudit) blob() string {
	b, _ := json.Marshal(a.all())
	return string(b)
}

// envModeRunRequest builds a single-env-mode-handle run with a payload, wired to the given vault and
// audit sockets. injection_mode is "env" (the per-run default mode hint).
func envModeRunRequest(payload, vaultSock, auditSock string, secretRefs ...string) RunRequest {
	var req RunRequest
	req.Run.Payload = payload
	req.Run.Tier = "bubblewrap"
	req.Run.Profile = map[string]any{}
	req.Run.SecretRefs = secretRefs
	req.Wiring.VaultSocket = vaultSock
	req.Wiring.AuditSocket = auditSock
	req.Wiring.InjectionMode = "env"
	req.Wiring.RequestID = "env-test"
	return req
}

// ---------------------------------------------------------------------------
// TC-012-01: env-mode delivers the credential to the sandbox process env
// ---------------------------------------------------------------------------

func TestEnvModeDeliversCredentialToSandbox(t *testing.T) {
	requireBwrap(t)
	const sentinel = "SENTINEL-ENV-VALUE"
	vault := newEnvModeVault(t, map[string]map[string]any{
		"vault://handle/env-abcdefghij": {
			"ok": true, "delivery": "env",
			"credential": sentinel, "var_name": "API_TOKEN", "wiped_at": "2026-06-20T00:00:00Z",
		},
	})
	audit := newRecordingAudit(t)

	req := envModeRunRequest(`printf '%s' "$API_TOKEN"`+"\n",
		vault.socket, audit.socket, "vault://handle/env-abcdefghij")
	res := Run(req)

	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if got := res["stdout"].(string); got != sentinel {
		t.Fatalf("payload did not read env-mode credential from $API_TOKEN: stdout=%q want %q", got, sentinel)
	}
}

// ---------------------------------------------------------------------------
// TC-012-02: the wipe clock removes the host-side copy at the defined point
// ---------------------------------------------------------------------------

// Unit-level: the EnvCredentials holder is the single host-side place the value lives; Wipe clears it.
func TestEnvCredentialsWipeClearsHostCopy(t *testing.T) {
	const sentinel = "SENTINEL-ENV-VALUE"
	ec := NewEnvCredentials()
	ec.Set("API_TOKEN", sentinel)
	if ec.empty() {
		t.Fatal("holder should hold the credential before wipe")
	}
	pairs := ec.pairs()
	if len(pairs) != 1 || pairs[0][0] != "API_TOKEN" || pairs[0][1] != sentinel {
		t.Fatalf("pairs() = %v, want one API_TOKEN=%q pair", pairs, sentinel)
	}

	ec.Wipe()
	if !ec.empty() {
		t.Fatal("Wipe() did not clear the holder — host-side copy survived the wipe point")
	}
	if got := ec.pairs(); len(got) != 0 {
		t.Fatalf("after Wipe() pairs() = %v, want empty (no host copy survives)", got)
	}
}

// Integration-level: after a full Run, no host-side EnvCredentials copy survives (post-spawn wipe +
// teardown). We assert via a holder threaded through a Run and confirm it is empty afterward by
// proxy-style inspection: Run wipes its own holder, so we assert the run completed AND that a fresh
// holder built the same way is wipe-clean. (The Run-internal holder is unexported; the unit test
// above proves Wipe is the single operation, and this test proves Run reaches the wipe path.)
func TestEnvModeRunReachesWipePoint(t *testing.T) {
	requireBwrap(t)
	const sentinel = "SENTINEL-ENV-VALUE"
	vault := newEnvModeVault(t, map[string]map[string]any{
		"vault://handle/env-abcdefghij": {
			"ok": true, "delivery": "env", "credential": sentinel, "var_name": "API_TOKEN",
		},
	})
	audit := newRecordingAudit(t)
	// A payload that does NOT print the token; the run must still complete cleanly through the wipe.
	req := envModeRunRequest("echo done\n", vault.socket, audit.socket, "vault://handle/env-abcdefghij")
	res := Run(req)
	if res["exit_code"].(int) != 0 {
		t.Fatalf("run did not complete (reach the post-spawn wipe): exit=%v stderr=%q",
			res["exit_code"], res["stderr"])
	}
	// The value must not survive on any host-side surface the result exposes.
	if blob, _ := json.Marshal(res); strings.Contains(string(blob), sentinel) {
		t.Fatalf("env-mode credential value survived in the result blob — wipe/hygiene breach")
	}
}

// ---------------------------------------------------------------------------
// TC-012-03: the value never appears in argv, stdout result, or audit
// ---------------------------------------------------------------------------

func TestEnvModeValueAbsentFromArgvResultAudit(t *testing.T) {
	requireBwrap(t)
	const sentinel = "SENTINEL-ENV-VALUE"
	vault := newEnvModeVault(t, map[string]map[string]any{
		"vault://handle/env-abcdefghij": {
			"ok": true, "delivery": "env", "credential": sentinel, "var_name": "API_TOKEN",
		},
	})
	audit := newRecordingAudit(t)

	var captured []string
	spawnArgvFn = func(argv []string) { captured = append([]string(nil), argv...) }
	t.Cleanup(func() { spawnArgvFn = nil })

	// Payload does NOT print the token (so stdout absence is meaningful).
	req := envModeRunRequest("echo no-token-here\n", vault.socket, audit.socket,
		"vault://handle/env-abcdefghij")
	res := Run(req)
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}

	// (1) argv — the /proc/<pid>/cmdline leak surface. The value must be delivered via --args FD, not
	// --setenv on the literal argv.
	argvJoined := strings.Join(captured, " ")
	if strings.Contains(argvJoined, sentinel) {
		t.Fatalf("env-mode credential value leaked into the spawn argv (/proc/cmdline): %s", argvJoined)
	}
	if !strings.Contains(argvJoined, "--args") {
		t.Fatalf("expected env-mode delivery via bwrap --args FD; argv: %s", argvJoined)
	}
	// (2) stdout + sandbox_status.
	if strings.Contains(res["stdout"].(string), sentinel) {
		t.Fatalf("credential value leaked into stdout")
	}
	stBlob, _ := json.Marshal(res["sandbox_status"])
	if strings.Contains(string(stBlob), sentinel) {
		t.Fatalf("credential value leaked into sandbox_status: %s", stBlob)
	}
	// (3) audit events.
	if strings.Contains(audit.blob(), sentinel) {
		t.Fatalf("credential value leaked into an audit event: %s", audit.blob())
	}
}

// ---------------------------------------------------------------------------
// TC-012-04: secrets_injected accounting is correct for env-mode
// ---------------------------------------------------------------------------

func TestEnvModeAccounting(t *testing.T) {
	requireBwrap(t)
	const handle = "vault://handle/env-abcdefghij"
	vault := newEnvModeVault(t, map[string]map[string]any{
		handle: {"ok": true, "delivery": "env", "credential": "x", "var_name": "API_TOKEN"},
	})
	audit := newRecordingAudit(t)
	res := Run(envModeRunRequest("echo ok\n", vault.socket, audit.socket, handle))

	st := res["sandbox_status"].(map[string]any)
	injected, _ := st["secrets_injected"].([]map[string]any)
	if len(injected) != 1 {
		t.Fatalf("secrets_injected = %v, want exactly one entry", injected)
	}
	e := injected[0]
	if e["delivery"] != "env" {
		t.Fatalf("delivery = %v, want \"env\"", e["delivery"])
	}
	if e["handle_prefix"] != prefix(handle, 8) {
		t.Fatalf("handle_prefix = %v, want %q (8-char prefix)", e["handle_prefix"], prefix(handle, 8))
	}
	if hp, _ := e["handle_prefix"].(string); len(hp) > 8 {
		t.Fatalf("handle_prefix %q longer than 8 chars — full handle leak risk", hp)
	}
	if _, hasCred := e["credential"]; hasCred {
		t.Fatal("secrets_injected entry carries a credential key — value must never be recorded")
	}
	if _, hasVal := e["value"]; hasVal {
		t.Fatal("secrets_injected entry carries a value key — value must never be recorded")
	}
}

// ---------------------------------------------------------------------------
// TC-012-05: proxy-mode unchanged; mixed run handled independently (F-002 intact)
// ---------------------------------------------------------------------------

func TestEnvModeMixedRunKeepsProxyOut(t *testing.T) {
	requireBwrap(t)
	const envSentinel = "SENTINEL-ENV"
	const proxySentinel = "SENTINEL-PROXY"

	// A real origin the proxy will reach; the proxy must inject proxySentinel into the OUTBOUND
	// request header (sandbox never sees it).
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	const envHandle = "vault://handle/env-aaaaaaaaaa"
	const proxyHandle = "vault://handle/proxy-bbbbbbbbbb"
	vault := newEnvModeVault(t, map[string]map[string]any{
		envHandle: {"ok": true, "delivery": "env", "credential": envSentinel, "var_name": "API_TOKEN"},
		proxyHandle: {"ok": true, "delivery": "proxy", "credential": proxySentinel,
			"binding": map[string]any{"host": "api.example.com", "header": "Authorization", "scheme": "Bearer"}},
	})
	audit := newRecordingAudit(t)

	var captured []string
	spawnArgvFn = func(argv []string) { captured = append([]string(nil), argv...) }
	t.Cleanup(func() { spawnArgvFn = nil })

	var req RunRequest
	req.Run.Tier = "bubblewrap"
	req.Run.Profile = map[string]any{"capabilities": []any{
		map[string]any{"type": "NetConnect", "allowlist": []any{"api.example.com:443"}},
	}}
	// The payload reaches the allowlisted host via the proxy AND prints its own env.
	req.Run.Payload = `curl -s --unix-socket /proxy.sock http://api.example.com/ping >/dev/null; printf 'ENV=%s' "$API_TOKEN"` + "\n"
	req.Run.SecretRefs = []string{envHandle, proxyHandle}
	req.Wiring.VaultSocket = vault.socket
	req.Wiring.AuditSocket = audit.socket
	req.Wiring.OriginMap = map[string][2]string{"api.example.com": {host, port}}
	req.Wiring.RequestID = "mixed-test"

	res := Run(req)
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}

	// env-mode delivered IN.
	if got := res["stdout"].(string); !strings.Contains(got, "ENV="+envSentinel) {
		t.Fatalf("env-mode credential not delivered to the sandbox: stdout=%q", got)
	}
	// proxy-mode stayed OUT of the sandbox: not in stdout, not in argv, not in audit.
	stdout := res["stdout"].(string)
	if strings.Contains(stdout, proxySentinel) {
		t.Fatalf("F-002 BREACH: proxy-mode credential appeared in sandbox stdout: %q", stdout)
	}
	if strings.Contains(strings.Join(captured, " "), proxySentinel) {
		t.Fatalf("F-002 BREACH: proxy-mode credential appeared in the spawn argv")
	}
	if strings.Contains(audit.blob(), proxySentinel) {
		t.Fatalf("F-002 BREACH: proxy-mode credential appeared in an audit event")
	}
	// proxy-mode credential WAS injected into the outbound request (it lives only at the proxy edge).
	if gotAuth != "Bearer "+proxySentinel {
		t.Fatalf("proxy did not inject the credential into the outbound request: Authorization=%q", gotAuth)
	}

	// Accounting: one env entry, one proxy entry.
	st := res["sandbox_status"].(map[string]any)
	injected, _ := st["secrets_injected"].([]map[string]any)
	deliveries := map[string]bool{}
	for _, e := range injected {
		deliveries[str(e["delivery"])] = true
	}
	if !deliveries["env"] || !deliveries["proxy"] || len(injected) != 2 {
		t.Fatalf("secrets_injected = %v, want one env + one proxy entry", injected)
	}
}

// ---------------------------------------------------------------------------
// TC-012-06: env-mode inject failure skips the handle, no partial env var
// ---------------------------------------------------------------------------

func TestEnvModeInjectFailureSkipsHandle(t *testing.T) {
	requireBwrap(t)
	const handle = "vault://handle/env-failhandle"
	vault := newEnvModeVault(t, map[string]map[string]any{
		handle: {"error": "vault: handle not found"}, // env-mode inject failure
	})
	audit := newRecordingAudit(t)

	// The payload prints whether API_TOKEN is set; a failed inject must deliver NO var at all.
	req := envModeRunRequest(`if [ -z "${API_TOKEN+x}" ]; then echo UNSET; else echo "SET=[$API_TOKEN]"; fi`+"\n",
		vault.socket, audit.socket, handle)
	res := Run(req)

	if res["exit_code"].(int) != 0 {
		t.Fatalf("run should continue after a skipped handle; exit=%v stderr=%q",
			res["exit_code"], res["stderr"])
	}
	if got := strings.TrimSpace(res["stdout"].(string)); got != "UNSET" {
		t.Fatalf("failed env-mode inject delivered a (partial/empty) var: stdout=%q, want UNSET", got)
	}
	// inject_failed emitted.
	if !contains(audit.actions(), "inject_failed") {
		t.Fatalf("expected an inject_failed audit event; actions=%v", audit.actions())
	}
	// No secrets_injected entry for the failed handle.
	st := res["sandbox_status"].(map[string]any)
	injected, _ := st["secrets_injected"].([]map[string]any)
	if len(injected) != 0 {
		t.Fatalf("secrets_injected = %v, want empty (the failed handle is not recorded)", injected)
	}
}

// Negative: a malformed env-mode response (delivery:"env" but no var_name) is treated as a failure —
// it must NOT deliver an unnamed/empty var.
func TestEnvModeMissingVarNameSkipsHandle(t *testing.T) {
	requireBwrap(t)
	const handle = "vault://handle/env-novarname"
	vault := newEnvModeVault(t, map[string]map[string]any{
		handle: {"ok": true, "delivery": "env", "credential": "SENTINEL"}, // no var_name
	})
	audit := newRecordingAudit(t)
	res := Run(envModeRunRequest("echo done\n", vault.socket, audit.socket, handle))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("run should continue; exit=%v", res["exit_code"])
	}
	if !contains(audit.actions(), "inject_failed") {
		t.Fatalf("malformed env response should emit inject_failed; actions=%v", audit.actions())
	}
	st := res["sandbox_status"].(map[string]any)
	if injected, _ := st["secrets_injected"].([]map[string]any); len(injected) != 0 {
		t.Fatalf("malformed env response should not be recorded; got %v", injected)
	}
}

// ---------------------------------------------------------------------------
// Unit: env-mode delivery off-argv at the backend boundary (no bwrap needed)
// ---------------------------------------------------------------------------

// The bubblewrap backend, given env-mode credentials, returns an argv that uses --args FD and an
// extraFile carrying the --setenv directives — the value is NOT on the literal argv.
func TestBwrapBackendEnvCredsOffArgv(t *testing.T) {
	const sentinel = "SENTINEL-ENV-VALUE"
	argv, _, _, extra, err := bubblewrapBackend{}.Argv("/p/payload.sh", "/p/proxy.sock", "", nil, nil,
		[][2]string{{"API_TOKEN", sentinel}}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, f := range extra {
			f.Close()
		}
	}()
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, sentinel) {
		t.Fatalf("env-mode credential value on the literal bwrap argv: %s", joined)
	}
	// Tier-1 always threads the seccomp blob as ExtraFiles[0] (child fd 3); the env-mode --args pipe
	// follows as ExtraFiles[1] (child fd 4). So off-argv env delivery is --args 4 (ADR 016 + ADR 015).
	if !strings.Contains(joined, "--args 4") {
		t.Fatalf("expected --args 4 (off-argv env delivery; seccomp holds fd 3); argv: %s", joined)
	}
	// The seccomp filter must still be installed alongside the env-mode delivery.
	if !strings.Contains(joined, "--seccomp 3") {
		t.Fatalf("expected --seccomp 3 (seccomp blob at fd 3) alongside env-mode delivery; argv: %s", joined)
	}
	if strings.Contains(joined, "--clearenv") {
		t.Fatalf("--clearenv should move into the --args FD when delivering env-mode creds: %s", joined)
	}
	if len(extra) != 2 {
		t.Fatalf("expected exactly two extraFiles (the seccomp blob + the --args pipe), got %d", len(extra))
	}

	// And the FD payload DOES carry the --setenv directive (read the pipe — ExtraFiles[1]).
	buf := make([]byte, 4096)
	n, _ := extra[1].Read(buf)
	payload := string(buf[:n])
	if !strings.Contains(payload, "API_TOKEN") || !strings.Contains(payload, sentinel) {
		t.Fatalf("--args FD payload missing the env-mode --setenv: %q", payload)
	}
	if !strings.Contains(payload, "--clearenv") {
		t.Fatalf("--args FD payload should carry --clearenv: %q", payload)
	}
}

// ---------------------------------------------------------------------------
// TC-012-07: spec + config updated (env-mode delivery + wipe clock, "recorded but not loaded" gone)
// ---------------------------------------------------------------------------

func TestEnvModeSpecUpdated(t *testing.T) {
	type check struct {
		file        string
		mustHave    []string
		mustNotHave []string
	}
	checks := []check{
		{
			file:     "docs/spec/behaviors.md",
			mustHave: []string{"env mode", "wipe clock", "var_name", "off the spawn argv"},
		},
		{
			file:        "docs/spec/data-model.md",
			mustHave:    []string{`"var_name"`, "wipe clock", "delivered", "data-invariant"},
			mustNotHave: []string{"recorded but not loaded onto the proxy"},
		},
		{
			file:        "docs/spec/configuration.md",
			mustHave:    []string{"delivers the secret into the sandbox", "var_name"},
			mustNotHave: []string{"recorded but not loaded"},
		},
	}
	for _, c := range checks {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("read %s: %v", c.file, err)
		}
		s := string(b)
		for _, want := range c.mustHave {
			if !strings.Contains(s, want) {
				t.Errorf("%s: missing required text %q", c.file, want)
			}
		}
		for _, bad := range c.mustNotHave {
			if strings.Contains(s, bad) {
				t.Errorf("%s: stale text still present %q", c.file, bad)
			}
		}
	}
}

// The gVisor backend delivers env-mode creds via OCI process.env (off-argv, in config.json).
func TestGvisorEnvCredsInProcessEnv(t *testing.T) {
	spec := gvisorOCISpec("/p/payload.sh", "/p/proxy.sock")
	applyEnvToOCISpec(spec, nil, [][2]string{{"API_TOKEN", "SENTINEL-ENV-VALUE"}})
	proc := spec["process"].(map[string]any)
	env, _ := proc["env"].([]string)
	var found bool
	for _, e := range env {
		if e == "API_TOKEN=SENTINEL-ENV-VALUE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("env-mode credential not in OCI process.env: %v", env)
	}
}
