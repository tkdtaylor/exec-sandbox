// SPDX-License-Identifier: Apache-2.0
package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// newBaseline builds a pristine baseline with the given payload and a fresh proxy (no allowlist,
// empty origin map — these tests inspect host-side state, not egress). The baseline is torn down at
// test end.
func newBaseline(t *testing.T, payload string) *sandboxBaseline {
	t.Helper()
	proxy := NewEgressProxy(nil, nil, nil)
	b, err := snapshotBaseline(payload, proxy)
	if err != nil {
		t.Fatalf("snapshotBaseline: %v", err)
	}
	t.Cleanup(b.teardown)
	return b
}

// restoreWritableSurface removes everything under dir — the host-side reset restore() performs on
// the writable surface (here the caller-supplied /work host dir).
func restoreWritableSurface(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read writable surface: %v", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			t.Fatalf("restore writable surface: %v", err)
		}
	}
}

// TC-008-01: snapshot captures the pristine baseline (payload.sh seeded, empty credential map),
// before any payload runs; snapshotting twice yields equal baselines.
func TestSnapshotCapturesPristineBaseline(t *testing.T) {
	b := newBaseline(t, "echo hi\n")

	// The writable surface is exactly payload.sh.
	if got := b.writableSurface(); !reflect.DeepEqual(got, []string{"payload.sh"}) {
		t.Fatalf("pristine writable surface = %v, want [payload.sh]", got)
	}
	// payload.sh holds the payload contents.
	data, err := os.ReadFile(b.scriptPath())
	if err != nil {
		t.Fatalf("read payload.sh: %v", err)
	}
	if string(data) != "echo hi\n" {
		t.Fatalf("payload.sh = %q, want %q", data, "echo hi\n")
	}
	// The credential map is empty.
	if got := b.credentialHosts(); len(got) != 0 {
		t.Fatalf("pristine credential hosts = %v, want empty", got)
	}
	// The proxy socket path is under the work dir (fresh per-run path).
	if filepath.Dir(b.proxySock) != b.work {
		t.Fatalf("proxySock %q not under work %q", b.proxySock, b.work)
	}

	// Idempotent capture: a second pristine baseline with the same payload has the same surface +
	// empty creds (equal baselines).
	b2 := newBaseline(t, "echo hi\n")
	if !reflect.DeepEqual(b.writableSurface(), b2.writableSurface()) {
		t.Fatalf("two pristine baselines differ: %v vs %v", b.writableSurface(), b2.writableSurface())
	}
	if len(b2.credentialHosts()) != 0 {
		t.Fatalf("second baseline has credentials: %v", b2.credentialHosts())
	}
}

// TC-008-02: restore returns the sandbox to the captured baseline — scratch file gone, credential
// map empty; restoring an already-pristine baseline is a no-op (no error).
func TestRestoreReturnsToBaseline(t *testing.T) {
	b := newBaseline(t, "echo hi\n")

	// Mutate the writable surface and the credential map (phase-1 mutations).
	if err := os.WriteFile(filepath.Join(b.work, "scratch.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatalf("write scratch: %v", err)
	}
	b.proxy.SetCredential("api.example.com", Credential{Value: "s3cr3t", Header: "Authorization", Scheme: "Bearer"})

	// Restore.
	if err := b.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// scratch.txt is gone; the surface is exactly payload.sh again.
	if got := b.writableSurface(); !reflect.DeepEqual(got, []string{"payload.sh"}) {
		t.Fatalf("after restore writable surface = %v, want [payload.sh]", got)
	}
	// The credential map is empty.
	if got := b.credentialHosts(); len(got) != 0 {
		t.Fatalf("after restore credential hosts = %v, want empty", got)
	}
	// payload.sh contents survive the restore (re-seeded).
	data, _ := os.ReadFile(b.scriptPath())
	if string(data) != "echo hi\n" {
		t.Fatalf("payload.sh after restore = %q, want %q", data, "echo hi\n")
	}

	// Restoring an already-pristine baseline is a no-op (no error).
	if err := b.restore(); err != nil {
		t.Fatalf("restore on pristine baseline errored: %v", err)
	}
	if got := b.writableSurface(); !reflect.DeepEqual(got, []string{"payload.sh"}) {
		t.Fatalf("double-restore surface = %v, want [payload.sh]", got)
	}
}

// TC-008-03: no file/env state leaks across a restore — the restored state is byte-for-byte equal to
// a freshly-built baseline (the load-bearing restored == fresh diff). The diff covers both the
// writable-surface file set AND each file's contents.
func TestNoStateLeaksAcrossRestore(t *testing.T) {
	payload := "echo run\n"
	dirty := newBaseline(t, payload)
	fresh := newBaseline(t, payload)

	// Phase-1 mutations on the "dirty" baseline: a secret-looking file + a nested dir.
	if err := os.WriteFile(filepath.Join(dirty.work, "secret.env"), []byte("API_KEY=leak"), 0o600); err != nil {
		t.Fatalf("write secret.env: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dirty.work, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirty.work, "subdir", "nested"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write nested: %v", err)
	}
	dirty.proxy.SetCredential("api.example.com", Credential{Value: "leak", Header: "Authorization", Scheme: "Bearer"})

	// Restore the dirty baseline.
	if err := dirty.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// restored == fresh: writable surface file set is equal.
	if got, want := dirty.writableSurface(), fresh.writableSurface(); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored surface %v != fresh surface %v (leak)", got, want)
	}
	// restored == fresh: payload.sh contents equal.
	gotData, _ := os.ReadFile(dirty.scriptPath())
	wantData, _ := os.ReadFile(fresh.scriptPath())
	if !reflect.DeepEqual(gotData, wantData) {
		t.Fatalf("restored payload.sh %q != fresh %q", gotData, wantData)
	}
	// restored == fresh: credential map equal (both empty).
	if got, want := dirty.credentialHosts(), fresh.credentialHosts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored creds %v != fresh creds %v (leak)", got, want)
	}
	// Explicit: the phase-1 secret file does NOT survive.
	if _, err := os.Stat(filepath.Join(dirty.work, "secret.env")); !os.IsNotExist(err) {
		t.Fatalf("secret.env survived restore (err=%v) — state leak", err)
	}
}

// TC-008-05: a restored sandbox keeps the no-network + proxy-only invariant — the spawn argv it
// builds still carries --unshare-all with no --share-net, the proxy credential map is empty after
// restore (no stale credential re-bound), and the socket is the same fresh per-run path (no stale
// socket reused). Restore never widens egress.
func TestRestoredSandboxKeepsNoNetworkInvariant(t *testing.T) {
	b := newBaseline(t, "echo hi\n")

	// Dirty it, then restore.
	_ = os.WriteFile(filepath.Join(b.work, "scratch"), []byte("x"), 0o600)
	b.proxy.SetCredential("api.example.com", Credential{Value: "v"})
	socketBefore := b.proxySock
	if err := b.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// The argv a restored baseline builds is identical to a fresh one's: --unshare-all, no --share-net.
	argv := bwrapArgv(b.scriptPath(), b.proxySock, "", nil, nil, 0, []string{"/usr/bin/sh", "/payload.sh"}, -1)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--unshare-all") {
		t.Fatalf("restored argv missing --unshare-all: %v", argv)
	}
	if strings.Contains(joined, "--share-net") {
		t.Fatalf("restored argv leaked --share-net: %v", argv)
	}
	// The proxy socket is bound (--bind <sock> /proxy.sock) as the only egress and is the SAME fresh
	// per-run path — never a stale socket from another run.
	if b.proxySock != socketBefore {
		t.Fatalf("proxy socket changed across restore: %q -> %q", socketBefore, b.proxySock)
	}
	if !strings.Contains(joined, b.proxySock+" /proxy.sock") {
		t.Fatalf("restored argv missing fresh proxy socket bind: %v", argv)
	}
	// The credential map is empty after restore — no stale credential re-bound.
	if got := b.credentialHosts(); len(got) != 0 {
		t.Fatalf("restored credential hosts = %v, want empty (no stale cred)", got)
	}
}

// TC-008-06: no credential leaks across a restore — after loading a credential (simulating a
// proxy-mode injection in run 1) and restoring, the proxy's credential map is empty (restore
// subsumes Wipe()).
func TestNoCredentialLeaksAcrossRestore(t *testing.T) {
	b := newBaseline(t, "echo hi\n")

	b.proxy.SetCredential("api.example.com", Credential{Value: "run1-secret", Header: "Authorization", Scheme: "Bearer"})
	b.proxy.SetCredential("db.internal", Credential{Value: "run1-db", Header: "X-Token", Scheme: "Token"})
	if got := b.credentialHosts(); len(got) != 2 {
		t.Fatalf("pre-restore credential hosts = %v, want 2", got)
	}

	if err := b.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if got := b.credentialHosts(); len(got) != 0 {
		t.Fatalf("post-restore credential hosts = %v, want empty — run-1 credential leaked", got)
	}
	// Direct proxy-state check: a run-2 request to the same host carries no run-1 credential.
	b.proxy.mu.Lock()
	_, has := b.proxy.creds["api.example.com"]
	b.proxy.mu.Unlock()
	if has {
		t.Fatalf("run-1 credential for api.example.com survived restore")
	}
}

// TC-008-04: a second real run on a restored sandbox cannot see the first run's files (bwrap). Run 1
// writes /work/leak.txt; restore; run 2 cannot find leak.txt — the writable surface is pristine.
func TestSecondRunCannotSeeFirstRunFiles_Bwrap(t *testing.T) {
	requireBwrap(t)

	// A host workdir bind-mounted writable at /work is the writable surface run 1 mutates.
	workdir := t.TempDir()

	// Run 1: write a leak file under /work (persists to the host workdir).
	req1 := newRunRequest("api.example.com", "echo run1-data > /work/leak.txt\n", nil)
	req1.Run.Workdir = workdir
	res1 := Run(req1)
	if res1["exit_code"].(int) != 0 {
		t.Fatalf("run1 exit_code = %v, stderr=%q", res1["exit_code"], res1["stderr"])
	}
	if _, err := os.Stat(filepath.Join(workdir, "leak.txt")); err != nil {
		t.Fatalf("run1 did not write leak.txt: %v", err)
	}

	// Restore the writable surface (the host workdir) to pristine via the same host-side reset
	// restore() performs on a baseline: remove everything the payload wrote under /work. (ADR 009 Q4
	// — the host-side reset applies to the caller-supplied writable surface.)
	restoreWritableSurface(t, workdir)

	// Run 2 on the restored writable surface: leak.txt must be gone.
	req2 := newRunRequest("api.example.com",
		"if [ -f /work/leak.txt ]; then echo LEAKED; else echo CLEAN; fi\n", nil)
	req2.Run.Workdir = workdir
	res2 := Run(req2)
	if res2["exit_code"].(int) != 0 {
		t.Fatalf("run2 exit_code = %v, stderr=%q", res2["exit_code"], res2["stderr"])
	}
	if got := strings.TrimSpace(res2["stdout"].(string)); got != "CLEAN" {
		t.Fatalf("run2 saw run1's file: stdout=%q, want CLEAN (leak across restore)", got)
	}
}

// TC-008-07: a one-shot run with no reuse is byte-for-byte unchanged (regression). A normal Run()
// still reaches an allowlisted host through the proxy and returns the full result schema — the
// snapshot+immediate-teardown default path is observationally identical to today.
func TestOneShotRunUnchanged_Bwrap(t *testing.T) {
	requireBwrap(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	req := newRunRequest("api.example.com",
		`curl -s -o /dev/null -w "%{http_code}" --unix-socket /proxy.sock http://api.example.com/ping`+"\n",
		map[string][2]string{"api.example.com": {host, port}})

	res := Run(req)
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if got := strings.TrimSpace(res["stdout"].(string)); got != "200" {
		t.Fatalf("one-shot run via proxy = %q, want 200", got)
	}
	// The full result schema is present (unchanged contract).
	ss, ok := res["sandbox_status"].(map[string]any)
	if !ok {
		t.Fatalf("missing sandbox_status: %v", res)
	}
	for _, k := range []string{"sandbox_id", "tier", "duration_ms", "secrets_injected", "status", "limits"} {
		if _, ok := ss[k]; !ok {
			t.Fatalf("sandbox_status missing %q: %v", k, ss)
		}
	}
}
