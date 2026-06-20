// SPDX-License-Identifier: Apache-2.0
package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// hitRecorder is a stub origin that records the method + count of every request it receives, so a
// blocked verb can be proven to produce ZERO upstream hits (no outbound connection happened).
type hitRecorder struct {
	mu      sync.Mutex
	methods []string
	hits    int32
}

func (h *hitRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&h.hits, 1)
		h.mu.Lock()
		h.methods = append(h.methods, r.Method)
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	}
}

func (h *hitRecorder) count() int { return int(atomic.LoadInt32(&h.hits)) }

// originFor spins up a hit-recording stub origin and returns it plus the {ip, port} the proxy's
// originMap needs to reach it.
func originFor(t *testing.T) (*hitRecorder, [2]string, func()) {
	t.Helper()
	rec := &hitRecorder{}
	srv := httptest.NewServer(rec.handler())
	ip, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		srv.Close()
		t.Fatalf("split origin URL: %v", err)
	}
	return rec, [2]string{ip, port}, srv.Close
}

// doVia issues a request to the proxy's handle() for the given host+method, returning the response
// recorder. It exercises the handler directly (no Unix socket needed for unit tests).
func doVia(p *EgressProxy, method, host string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+host+"/ping", nil)
	req.Host = host + ":443"
	w := httptest.NewRecorder()
	p.handle(w, req)
	return w
}

// TC-007-01: NetConnect parsing yields a per-host verb set; absent/empty ⇒ all-verbs (no entry).
func TestNetVerbAllowlistParsing(t *testing.T) {
	// (a) a NetConnect entry naming methods for api.example.com
	profA := map[string]any{"capabilities": []any{
		map[string]any{"type": "NetConnect", "allowlist": []any{"api.example.com:443"},
			"methods": []any{"GET", "HEAD"}},
	}}
	gotA := netVerbAllowlist(profA)
	setA := gotA["api.example.com"]
	if !setA["GET"] || !setA["HEAD"] || len(setA) != 2 {
		t.Fatalf("(a) api.example.com set = %v, want {GET,HEAD}", setA)
	}

	// (b) a NetConnect entry with NO methods ⇒ unconstrained ⇒ no map entry at all.
	profB := map[string]any{"capabilities": []any{
		map[string]any{"type": "NetConnect", "allowlist": []any{"api.example.com:443"}},
	}}
	gotB := netVerbAllowlist(profB)
	if _, ok := gotB["api.example.com"]; ok {
		t.Fatalf("(b) host with no methods should be unconstrained (no entry), got %v", gotB)
	}

	// (c) two NetConnect entries for different hosts with different sets ⇒ each keeps its own.
	profC := map[string]any{"capabilities": []any{
		map[string]any{"type": "NetConnect", "allowlist": []any{"read.example.com:443"},
			"methods": []any{"GET"}},
		map[string]any{"type": "NetConnect", "allowlist": []any{"write.example.com:443"},
			"methods": []any{"GET", "POST"}},
	}}
	gotC := netVerbAllowlist(profC)
	if !gotC["read.example.com"]["GET"] || len(gotC["read.example.com"]) != 1 {
		t.Fatalf("(c) read.example.com = %v, want {GET}", gotC["read.example.com"])
	}
	if !gotC["write.example.com"]["GET"] || !gotC["write.example.com"]["POST"] || len(gotC["write.example.com"]) != 2 {
		t.Fatalf("(c) write.example.com = %v, want {GET,POST}", gotC["write.example.com"])
	}

	// Edge case: an explicitly empty methods:[] is "unconstrained", NOT deny-all (no entry).
	profEmpty := map[string]any{"capabilities": []any{
		map[string]any{"type": "NetConnect", "allowlist": []any{"api.example.com:443"},
			"methods": []any{}},
	}}
	gotEmpty := netVerbAllowlist(profEmpty)
	if _, ok := gotEmpty["api.example.com"]; ok {
		t.Fatalf("empty methods:[] must be unconstrained (no entry), got %v", gotEmpty)
	}

	// The bare-domain allowlist (existing host-level check) is still produced unchanged.
	if al := netAllowlist(profA); len(al) != 1 || al[0] != "api.example.com" {
		t.Fatalf("bare allowlist regressed: %v", al)
	}
}

// TC-007-02: an allowed verb to an allowed host is forwarded (credential injection unchanged).
func TestProxyForwardsAllowedVerb(t *testing.T) {
	rec, origin, closeOrigin := originFor(t)
	defer closeOrigin()

	p := NewEgressProxy([]string{"api.example.com"},
		map[string]map[string]bool{"api.example.com": {"GET": true}},
		map[string][2]string{"api.example.com": origin})
	p.SetCredential("api.example.com", Credential{Value: "tok", Header: "Authorization", Scheme: "Bearer"})

	w := doVia(p, "GET", "api.example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("allowed GET status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if rec.count() != 1 {
		t.Fatalf("origin hits = %d, want 1 (allowed verb must be forwarded)", rec.count())
	}
}

// TC-007-03: a disallowed verb to an allowed host is 403'd and NOT forwarded (no upstream, no cred).
func TestProxyBlocksDisallowedVerb(t *testing.T) {
	rec, origin, closeOrigin := originFor(t)
	defer closeOrigin()

	p := NewEgressProxy([]string{"api.example.com"},
		map[string]map[string]bool{"api.example.com": {"GET": true}},
		map[string][2]string{"api.example.com": origin})

	w := doVia(p, "POST", "api.example.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("disallowed POST status = %d, want 403", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "blocked-by-method" {
		t.Fatalf("403 body = %q, want %q (distinct from host block)", body, "blocked-by-method")
	}
	if rec.count() != 0 {
		t.Fatalf("origin hits = %d, want 0 — a blocked verb must produce NO outbound connection", rec.count())
	}
}

// TC-007-04: a non-allowlisted host is 403'd regardless of method (host check precedes verb check).
func TestProxyHostCheckPrecedesVerbCheck(t *testing.T) {
	rec, origin, closeOrigin := originFor(t)
	defer closeOrigin()

	// Only api.example.com is allowlisted (verb {GET}); other.example.com is not listed at all.
	p := NewEgressProxy([]string{"api.example.com"},
		map[string]map[string]bool{"api.example.com": {"GET": true}},
		map[string][2]string{"api.example.com": origin})

	// A GET (an otherwise-allowed method) to an UNLISTED host is blocked by the HOST check first.
	w := doVia(p, "GET", "other.example.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("unlisted host status = %d, want 403", w.Code)
	}
	if body := strings.TrimSpace(w.Body.String()); body != "blocked-by-allowlist" {
		t.Fatalf("403 body = %q, want %q (host block, not verb block)", body, "blocked-by-allowlist")
	}
	if rec.count() != 0 {
		t.Fatalf("origin hits = %d, want 0 for an unlisted host", rec.count())
	}
}

// TC-007-07: verb matching is case-insensitive / normalized to canonical upper-case.
func TestProxyVerbMatchingCaseInsensitive(t *testing.T) {
	rec, origin, closeOrigin := originFor(t)
	defer closeOrigin()

	p := NewEgressProxy([]string{"api.example.com"},
		map[string]map[string]bool{"api.example.com": {"GET": true}},
		map[string][2]string{"api.example.com": origin})

	for _, m := range []string{"get", "Get", "GET"} {
		w := doVia(p, m, "api.example.com")
		if w.Code != http.StatusOK {
			t.Fatalf("method %q status = %d, want 200 (case-insensitive match)", m, w.Code)
		}
	}
	if rec.count() != 3 {
		t.Fatalf("origin hits = %d, want 3 (all case variants of GET forwarded)", rec.count())
	}

	// A method not in the set (any case) is still rejected.
	w := doVia(p, "delete", "api.example.com")
	if w.Code != http.StatusForbidden {
		t.Fatalf("lowercase 'delete' status = %d, want 403", w.Code)
	}
}

// TC-007-08 (unit half): no methods constraint ⇒ all verbs allowed (regression / backward compat).
func TestProxyUnconstrainedHostAllowsAllVerbs(t *testing.T) {
	rec, origin, closeOrigin := originFor(t)
	defer closeOrigin()

	// nil verbAllowlist — exactly the shape every pre-task-007 caller used.
	p := NewEgressProxy([]string{"api.example.com"}, nil,
		map[string][2]string{"api.example.com": origin})

	for _, m := range []string{"GET", "POST", "DELETE"} {
		w := doVia(p, m, "api.example.com")
		if w.Code != http.StatusOK {
			t.Fatalf("unconstrained host: method %q status = %d, want 200", m, w.Code)
		}
	}
	if rec.count() != 3 {
		t.Fatalf("origin hits = %d, want 3 (unconstrained host forwards every verb)", rec.count())
	}

	// An explicitly EMPTY set is also unconstrained, not deny-all.
	p2 := NewEgressProxy([]string{"api.example.com"},
		map[string]map[string]bool{"api.example.com": {}},
		map[string][2]string{"api.example.com": origin})
	w := doVia(p2, "POST", "api.example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("empty verb set: POST status = %d, want 200 (empty ⇒ unconstrained, not deny-all)", w.Code)
	}
}

// --- Integration (bwrap): the verb mechanism end-to-end through the real sandbox + proxy socket ---

// newVerbRunRequest builds a RunRequest with a NetConnect verb constraint on the allowlisted host.
func newVerbRunRequest(host, payload string, methods []string, origin map[string][2]string) RunRequest {
	var req RunRequest
	req.Run.Payload = payload
	req.Run.Tier = "bubblewrap"
	ms := make([]any, len(methods))
	for i, m := range methods {
		ms[i] = m
	}
	req.Run.Profile = map[string]any{
		"capabilities": []any{
			map[string]any{"type": "NetConnect", "allowlist": []any{host + ":443"}, "methods": ms},
		},
	}
	req.Wiring.OriginMap = origin
	req.Wiring.RequestID = "test"
	return req
}

// TC-007-05: end-to-end — an allowed verb reaches the allowlisted host via the proxy (sandbox).
func TestSandboxAllowedVerbReachesHost(t *testing.T) {
	requireBwrap(t)
	rec, origin, closeOrigin := originFor(t)
	defer closeOrigin()

	req := newVerbRunRequest("api.example.com",
		`curl -s -o /dev/null -w "%{http_code}" --unix-socket /proxy.sock http://api.example.com/ping`+"\n",
		[]string{"GET"},
		map[string][2]string{"api.example.com": origin})

	res := Run(req)
	if res["exit_code"].(int) != 0 {
		t.Fatalf("exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if got := strings.TrimSpace(res["stdout"].(string)); got != "200" {
		t.Fatalf("allowed GET expected 200 via proxy, got %q", got)
	}
	if rec.count() != 1 {
		t.Fatalf("origin hits = %d, want 1 (allowed verb reaches origin)", rec.count())
	}
}

// TC-007-06: end-to-end — a disallowed verb is blocked, origin sees nothing (sandbox, invariant).
func TestSandboxDisallowedVerbBlockedOriginSeesNothing(t *testing.T) {
	requireBwrap(t)
	rec, origin, closeOrigin := originFor(t)
	defer closeOrigin()

	// Verb set is {GET}; the payload attempts a POST.
	req := newVerbRunRequest("api.example.com",
		`curl -s -X POST -o /dev/null -w "%{http_code}" --unix-socket /proxy.sock http://api.example.com/ping`+"\n",
		[]string{"GET"},
		map[string][2]string{"api.example.com": origin})

	res := Run(req)
	if got := strings.TrimSpace(res["stdout"].(string)); got != "403" {
		t.Fatalf("disallowed POST expected 403, got %q (stderr=%q)", got, res["stderr"])
	}
	if rec.count() != 0 {
		t.Fatalf("origin hits = %d, want 0 — the block is at the proxy with NO outbound connection", rec.count())
	}
}

// TC-007-08 (integration half): no methods constraint ⇒ all verbs forwarded end-to-end (regression).
func TestSandboxUnconstrainedHostForwardsAllVerbs(t *testing.T) {
	requireBwrap(t)
	rec, origin, closeOrigin := originFor(t)
	defer closeOrigin()

	// Today's only shape: NetConnect with no "methods". POST must still reach the origin.
	req := newRunRequest("api.example.com",
		`curl -s -X POST -o /dev/null -w "%{http_code}" --unix-socket /proxy.sock http://api.example.com/ping`+"\n",
		map[string][2]string{"api.example.com": origin})

	res := Run(req)
	if got := strings.TrimSpace(res["stdout"].(string)); got != "200" {
		t.Fatalf("unconstrained host POST expected 200, got %q (stderr=%q)", got, res["stderr"])
	}
	if rec.count() != 1 {
		t.Fatalf("origin hits = %d, want 1 (unconstrained host forwards POST as before)", rec.count())
	}
}
