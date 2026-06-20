// SPDX-License-Identifier: Apache-2.0
package main

// Tests for task 010: terminal audit event on early proxy-start failure (resolve B-007 TODO).
//
// TC-010-01: proxy-start failure emits terminal event after spawn
// TC-010-02: terminal event conforms to audit IPC contract and carries request_id
// TC-010-03: fail-fast contract unchanged — error returned, no payload run
// TC-010-04: success path audit sequence unchanged (no spurious terminal event)
// TC-010-05: empty audit_socket — emission is a no-op, error still returned

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

// stubAuditSocket starts a Unix-socket listener that records every emit message sent to it.
// It returns the socket path, a function to retrieve recorded events in order, and a close
// function. The stub accepts the audit IPC shape {"op":"emit","event":{...}} and responds
// with {}. Events are collected as the full top-level message ({"op","event"} shape).
func stubAuditSocket(t *testing.T) (socketPath string, getEvents func() []map[string]any, closeFn func()) {
	t.Helper()

	dir := t.TempDir()
	socketPath = filepath.Join(dir, "audit.sock")

	var mu sync.Mutex
	var events []map[string]any

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("stubAuditSocket listen: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					var msg map[string]any
					if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
						continue
					}
					mu.Lock()
					events = append(events, msg)
					mu.Unlock()
					c.Write([]byte("{}\n"))
				}
			}(conn)
		}
	}()

	getEvents = func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]map[string]any, len(events))
		copy(cp, events)
		return cp
	}
	closeFn = func() { ln.Close(); <-done }
	return socketPath, getEvents, closeFn
}

// forcedProxyFailureRun builds a RunRequest that will fail at proxy.Start, and returns
// a teardown func. It overrides mkdirTempFn to inject a controlled work dir where the
// proxy socket path is occupied by a non-empty directory, causing net.Listen("unix",...)
// to fail.
//
// Mechanism (settled per test-spec §Test framework notes):
//  1. Create a controlled temp dir (writable so snapshotBaseline can seed payload.sh).
//  2. Under it, create "proxy.sock" as a NON-EMPTY directory:
//     - proxy.Start calls os.Remove("proxy.sock") → fails ENOTEMPTY (silently ignored).
//     - Then net.Listen("unix", "proxy.sock") → fails because a directory occupies the path.
//  3. Override mkdirTempFn (single-use) to return the controlled dir.
//
// This reaches the proxy.Start failure branch (run.go:113), not the baseline-prepare branch.
func forcedProxyFailureRun(t *testing.T, auditSocket, requestID string) (RunRequest, func()) {
	t.Helper()

	controlledDir, err := os.MkdirTemp("", "exec-sandbox-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}

	// Create "proxy.sock" as a non-empty directory — blocks net.Listen without
	// making the parent dir read-only (so snapshotBaseline can still seed payload.sh).
	proxySockDir := filepath.Join(controlledDir, "proxy.sock")
	if err := os.Mkdir(proxySockDir, 0o755); err != nil {
		os.RemoveAll(controlledDir)
		t.Fatalf("mkdir proxy.sock dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proxySockDir, "occupied"), []byte("block"), 0o600); err != nil {
		os.RemoveAll(controlledDir)
		t.Fatalf("write occupied file: %v", err)
	}

	origFn := mkdirTempFn
	mkdirTempFn = func(dir, pattern string) (string, error) {
		mkdirTempFn = origFn // single-use: restore before the listener path is used
		return controlledDir, nil
	}

	teardown := func() {
		os.RemoveAll(controlledDir)
		mkdirTempFn = origFn // restore in case run panicked before the lambda ran
	}

	var req RunRequest
	req.Run.Payload = "echo should-not-run\n"
	req.Run.Tier = "bubblewrap"
	req.Run.Profile = map[string]any{"capabilities": []any{}}
	req.Wiring.AuditSocket = auditSocket
	req.Wiring.RequestID = requestID
	return req, teardown
}

// extractEvent pulls the nested "event" object out of an audit {"op":"emit","event":{...}} msg.
func extractEvent(t *testing.T, msg map[string]any) map[string]any {
	t.Helper()
	ev, ok := msg["event"].(map[string]any)
	if !ok {
		t.Fatalf("audit message has no 'event' key or wrong type: %v", msg)
	}
	return ev
}

// TC-010-01: proxy-start failure emits a terminal audit event after spawn.
func TestProxyFailureEmitsTerminalEventAfterSpawn(t *testing.T) {
	auditSock, getEvents, closeAudit := stubAuditSocket(t)
	defer closeAudit()

	req, teardown := forcedProxyFailureRun(t, auditSock, "req-tc01")
	defer teardown()

	res := Run(req)

	if errVal, ok := res["error"].(string); !ok || !strings.HasPrefix(errVal, "proxy start failed:") {
		t.Fatalf("expected proxy start failed error, got: %v", res)
	}

	events := getEvents()
	if len(events) != 2 {
		t.Fatalf("expected exactly 2 audit events (spawn + terminal failure), got %d: %v", len(events), events)
	}

	// First event: spawn (allow).
	ev0 := extractEvent(t, events[0])
	if ev0["action"] != "spawn" {
		t.Fatalf("event[0] action = %v, want spawn", ev0["action"])
	}
	if ev0["decision"] != "allow" {
		t.Fatalf("event[0] decision = %v, want allow", ev0["decision"])
	}

	// Second event: terminal failure (exit/deny).
	ev1 := extractEvent(t, events[1])
	if ev1["action"] != "exit" {
		t.Fatalf("event[1] action = %v, want exit", ev1["action"])
	}
	if ev1["decision"] != "deny" {
		t.Fatalf("event[1] decision = %v, want deny", ev1["decision"])
	}
}

// TC-010-02: terminal event conforms to audit IPC contract and carries request_id.
func TestProxyFailureTerminalEventShape(t *testing.T) {
	const reqID = "req-xyz"
	auditSock, getEvents, closeAudit := stubAuditSocket(t)
	defer closeAudit()

	req, teardown := forcedProxyFailureRun(t, auditSock, reqID)
	defer teardown()

	res := Run(req)
	if _, hasErr := res["error"]; !hasErr {
		t.Fatalf("expected error result, got: %v", res)
	}

	events := getEvents()
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}

	// Top-level IPC shape: {"op":"emit","event":{...}}.
	msg1 := events[1]
	if got := msg1["op"]; got != "emit" {
		t.Fatalf("event[1] top-level op = %v, want emit", got)
	}
	ev := extractEvent(t, msg1)

	// actor.
	if got := ev["actor"]; got != "exec-sandbox" {
		t.Fatalf("actor = %v, want exec-sandbox", got)
	}
	// action: reuses "exit" so every spawn has a matching exit (ADR 013).
	if got := ev["action"]; got != "exit" {
		t.Fatalf("action = %v, want exit", got)
	}
	// target: sandbox_id (non-empty, "sbx-" prefix).
	target, ok := ev["target"].(string)
	if !ok || !strings.HasPrefix(target, "sbx-") {
		t.Fatalf("target = %v, want sbx-...", ev["target"])
	}
	// decision: deny (failure-flavored).
	if got := ev["decision"]; got != "deny" {
		t.Fatalf("decision = %v, want deny", got)
	}

	// context: must carry status, error, request_id; must NOT carry exit_code.
	ctx, ok := ev["context"].(map[string]any)
	if !ok {
		t.Fatalf("context missing or wrong type: %v", ev["context"])
	}
	if got := ctx["status"]; got != "proxy_start_failed" {
		t.Fatalf("context.status = %v, want proxy_start_failed", got)
	}
	if got, ok := ctx["error"].(string); !ok || got == "" {
		t.Fatalf("context.error missing or empty: %v", ctx["error"])
	}
	if got := ctx["request_id"]; got != reqID {
		t.Fatalf("context.request_id = %v, want %v", got, reqID)
	}
	// Must not carry exit_code (that would let a consumer mistake it for a clean run).
	if _, has := ctx["exit_code"]; has {
		t.Fatalf("context must not carry exit_code on early-failure event: %v", ctx)
	}

	// Spawn and terminal events share the same sandbox_id target.
	ev0 := extractEvent(t, events[0])
	if ev0["target"] != ev["target"] {
		t.Fatalf("spawn and terminal events have different targets: %v vs %v", ev0["target"], ev["target"])
	}
}

// TC-010-03: fail-fast contract unchanged — error returned, no payload run.
func TestProxyFailureReturnsFastNoPayload(t *testing.T) {
	auditSock, _, closeAudit := stubAuditSocket(t)
	defer closeAudit()

	req, teardown := forcedProxyFailureRun(t, auditSock, "req-tc03")
	defer teardown()

	res := Run(req)

	// Must return {error: "proxy start failed: ..."} with the exact prefix.
	errVal, ok := res["error"].(string)
	if !ok {
		t.Fatalf("expected error key in result, got: %v", res)
	}
	if !strings.HasPrefix(errVal, "proxy start failed:") {
		t.Fatalf("error string = %q, want prefix 'proxy start failed:'", errVal)
	}

	// Must NOT return a success result shape.
	for _, k := range []string{"stdout", "stderr", "exit_code", "sandbox_status"} {
		if _, has := res[k]; has {
			t.Fatalf("result must not carry %q on early proxy failure, got: %v", k, res)
		}
	}
}

// TC-010-04: success path audit sequence unchanged — spawn then exit (allow), no spurious event.
// bwrap integration test (full run, real proxy socket, real backend).
func TestSuccessPathAuditSequenceUnchanged_Bwrap(t *testing.T) {
	requireBwrap(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	auditSock, getEvents, closeAudit := stubAuditSocket(t)
	defer closeAudit()

	req := newRunRequest("api.example.com",
		`curl -s -o /dev/null -w "%{http_code}" --unix-socket /proxy.sock http://api.example.com/ping`+"\n",
		map[string][2]string{"api.example.com": {host, port}})
	req.Wiring.AuditSocket = auditSock
	req.Wiring.RequestID = "req-tc04"

	res := Run(req)
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}

	events := getEvents()

	// Exactly 2 events: spawn (allow) → exit (allow). The new early-failure emission must not fire.
	if len(events) != 2 {
		t.Fatalf("expected exactly 2 audit events (spawn+exit), got %d: %v", len(events), events)
	}

	ev0 := extractEvent(t, events[0])
	if ev0["action"] != "spawn" || ev0["decision"] != "allow" {
		t.Fatalf("event[0] = %v, want spawn/allow", ev0)
	}

	ev1 := extractEvent(t, events[1])
	if ev1["action"] != "exit" || ev1["decision"] != "allow" {
		t.Fatalf("event[1] = %v, want exit/allow", ev1)
	}

	// Success exit carries the standard context — not proxy_start_failed.
	ctx1, ok := ev1["context"].(map[string]any)
	if !ok {
		t.Fatalf("success exit event context missing: %v", ev1)
	}
	if _, has := ctx1["exit_code"]; !has {
		t.Fatalf("success exit event missing exit_code: %v", ctx1)
	}
	if ctx1["status"] == "proxy_start_failed" {
		t.Fatalf("success exit event must not have status=proxy_start_failed: %v", ctx1)
	}
	if ctx1["request_id"] != "req-tc04" {
		t.Fatalf("success exit event request_id = %v, want req-tc04", ctx1["request_id"])
	}
}

// TC-010-05: empty audit_socket — emission is a no-op, error still returned.
func TestProxyFailureEmptyAuditSocket(t *testing.T) { // also covers TC-010-06 (spec inspection: B-007 TODO removed, data-model updated — see docs/spec/behaviors.md B-007 and docs/spec/data-model.md audit-event section)
	req, teardown := forcedProxyFailureRun(t, "" /* empty audit_socket */, "req-tc05")
	defer teardown()

	res := Run(req)

	errVal, ok := res["error"].(string)
	if !ok || !strings.HasPrefix(errVal, "proxy start failed:") {
		t.Fatalf("expected proxy start failed error with empty audit_socket, got: %v", res)
	}
}
