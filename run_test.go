package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func requireBwrap(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap not installed; skipping sandbox integration test")
	}
}

func newRunRequest(host, payload string, origin map[string][2]string) RunRequest {
	var req RunRequest
	req.Run.Payload = payload
	req.Run.Tier = "bubblewrap"
	req.Run.Profile = map[string]any{
		"capabilities": []any{
			map[string]any{"type": "NetConnect", "allowlist": []any{host + ":443"}},
		},
	}
	req.Wiring.OriginMap = origin
	req.Wiring.RequestID = "test"
	return req
}

// The sandbox reaches an allowlisted origin only through the bind-mounted proxy socket.
func TestSandboxReachesAllowlistedHostViaProxy(t *testing.T) {
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
		t.Fatalf("expected 200 via proxy, got %q", got)
	}
}

// A non-allowlisted host is blocked by the proxy (defense-in-depth; policy denies first).
func TestProxyBlocksNonAllowlistedHost(t *testing.T) {
	requireBwrap(t)
	req := newRunRequest("api.example.com",
		`curl -s -o /dev/null -w "%{http_code}" --unix-socket /proxy.sock http://evil.example.net/ping`+"\n",
		map[string][2]string{"api.example.com": {"127.0.0.1", "1"}})

	res := Run(req)
	if got := strings.TrimSpace(res["stdout"].(string)); got != "403" {
		t.Fatalf("expected 403 from allowlist, got %q (stderr=%q)", got, res["stderr"])
	}
}

func TestNetAllowlistParsing(t *testing.T) {
	profile := map[string]any{"capabilities": []any{
		map[string]any{"type": "NetConnect", "allowlist": []any{"api.example.com:443", "x.test:80"}},
		map[string]any{"type": "FileRead", "paths": []any{"/tmp"}},
	}}
	got := netAllowlist(profile)
	if len(got) != 2 || got[0] != "api.example.com" || got[1] != "x.test" {
		t.Fatalf("unexpected allowlist: %v", got)
	}
}
