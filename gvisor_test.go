// SPDX-License-Identifier: Apache-2.0
package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// requireRunsc skips the test when runsc is not installed (mirrors requireBwrap in run_test.go),
// so the suite stays green on machines without gVisor.
func requireRunsc(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("runsc (gVisor) not installed; skipping gvisor integration test")
	}
}

// TC-002: tier == "gvisor" selects the runsc backend; "" and "bubblewrap" select bubblewrap; an
// unknown tier returns a clear "tier not implemented" error rather than silently falling back.
func TestBackendForRoutesByTier(t *testing.T) {
	cases := []struct {
		tier string
		want string // backend Go type name
	}{
		{"", "main.bubblewrapBackend"},
		{"bubblewrap", "main.bubblewrapBackend"},
		{"gvisor", "main.gvisorBackend"},
	}
	for _, c := range cases {
		b, err := backendFor(c.tier)
		if err != nil {
			t.Fatalf("backendFor(%q) unexpected error: %v", c.tier, err)
		}
		if got := backendTypeName(b); got != c.want {
			t.Fatalf("backendFor(%q) = %s, want %s", c.tier, got, c.want)
		}
	}
}

func TestBackendForUnknownTierErrors(t *testing.T) {
	// "firecracker" is now wired (task 013) — only genuinely-unknown tiers must error.
	for _, tier := range []string{"kata", "docker", "nonsense"} {
		b, err := backendFor(tier)
		if err == nil {
			t.Fatalf("backendFor(%q) returned nil error; expected 'tier not implemented'", tier)
		}
		if b != nil {
			t.Fatalf("backendFor(%q) returned a backend on error; expected nil (no silent fall-back)", tier)
		}
		if !strings.Contains(err.Error(), "tier not implemented") {
			t.Fatalf("backendFor(%q) error = %q, want it to contain 'tier not implemented'", tier, err.Error())
		}
		if !strings.Contains(err.Error(), tier) {
			t.Fatalf("backendFor(%q) error = %q, want it to name the tier", tier, err.Error())
		}
	}
}

func backendTypeName(b Backend) string {
	switch b.(type) {
	case bubblewrapBackend:
		return "main.bubblewrapBackend"
	case gvisorBackend:
		return "main.gvisorBackend"
	case firecrackerBackend:
		return "main.firecrackerBackend"
	default:
		return "unknown"
	}
}

// TC-003: the gVisor OCI spec declares no network namespace path (a fresh empty netns), mirroring
// bwrap --unshare-all, and contains nothing that grants host or bridged networking.
func TestGvisorSpecHasNoSharedNetwork(t *testing.T) {
	spec := gvisorOCISpec("/work/payload.sh", "/work/proxy.sock")

	linux, ok := spec["linux"].(map[string]any)
	if !ok {
		t.Fatal("spec missing linux section")
	}
	namespaces, ok := linux["namespaces"].([]map[string]any)
	if !ok {
		t.Fatal("spec.linux.namespaces missing or wrong type")
	}

	var netNS map[string]any
	for _, ns := range namespaces {
		if ns["type"] == "network" {
			netNS = ns
		}
	}
	if netNS == nil {
		t.Fatal("spec declares no network namespace; the sandbox must be in a fresh empty netns")
	}
	// A network namespace WITH a path joins an existing (host/shared) netns. The invariant is an
	// EMPTY path → a fresh, isolated namespace (loopback only).
	if p, present := netNS["path"]; present && p != "" {
		t.Fatalf("network namespace has path %q; expected an empty/absent path (fresh netns)", p)
	}

	// Defense-in-depth: the spec must not carry any host/bridged-network affordance.
	for _, ns := range namespaces {
		if path, ok := ns["path"].(string); ok && ns["type"] == "network" && path != "" {
			t.Fatalf("network namespace joins an existing netns via %q — forbidden", path)
		}
	}
}

// TC-004: the proxy Unix socket is the only egress affordance bind-mounted in, at /proxy.sock; no
// other network mount/device is present.
func TestGvisorSpecMountsOnlyProxySocketForEgress(t *testing.T) {
	const proxySrc = "/work/proxy.sock"
	spec := gvisorOCISpec("/work/payload.sh", proxySrc)

	mounts, ok := spec["mounts"].([]map[string]any)
	if !ok {
		t.Fatal("spec.mounts missing or wrong type")
	}

	var proxyMount map[string]any
	for _, m := range mounts {
		dst, _ := m["destination"].(string)
		// The egress socket is the only mount whose source is the proxy socket.
		if src, _ := m["source"].(string); src == proxySrc {
			proxyMount = m
		}
		// No mount may expose a host network device or socket other than the proxy.
		switch dst {
		case "/proxy.sock":
			// handled below
		default:
			if strings.HasSuffix(dst, ".sock") {
				t.Fatalf("unexpected socket mount at %q — only /proxy.sock is allowed as egress", dst)
			}
		}
	}

	if proxyMount == nil {
		t.Fatal("proxy socket is not bind-mounted into the sandbox")
	}
	if dst, _ := proxyMount["destination"].(string); dst != "/proxy.sock" {
		t.Fatalf("proxy socket mounted at %q, want /proxy.sock (matches what payloads expect)", dst)
	}
	if typ, _ := proxyMount["type"].(string); typ != "bind" {
		t.Fatalf("proxy socket mount type = %q, want bind", typ)
	}
}

// e2e (TC-006 path): under runsc, an allowlisted host is reachable through the bind-mounted proxy
// socket, a non-allowlisted host is blocked (403), and sandbox_status.tier == "gvisor". Skips
// cleanly when runsc is absent.
//
// This test also exercises TC-005 (the run() contract is unchanged across tiers): it asserts the
// gvisor run yields the same {stdout, stderr, exit_code, sandbox_status{...,tier}} shape as the
// bubblewrap path, with sandbox_status.tier echoing the requested "gvisor". The bubblewrap side of
// TC-001/TC-005/TC-007 is the existing, unmodified run_test.go suite
// (TestSandboxReachesAllowlistedHostViaProxy / TestProxyBlocksNonAllowlistedHost /
// TestNetAllowlistParsing) — kept green by the backendFor seam routing ""/"bubblewrap" to the
// unchanged bwrapArgv path.
func TestGvisorRunReachesAllowlistedHostAndBlocksOthers(t *testing.T) {
	requireRunsc(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))

	// Allowlisted host reached through the proxy → 200.
	allowReq := gvisorRunRequest("api.example.com",
		`curl -s -o /dev/null -w "%{http_code}" --unix-socket /proxy.sock http://api.example.com/ping`+"\n",
		map[string][2]string{"api.example.com": {host, port}})
	res := Run(allowReq)
	if res["exit_code"].(int) != 0 {
		t.Fatalf("allowlisted run exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	if got := strings.TrimSpace(res["stdout"].(string)); got != "200" {
		t.Fatalf("expected 200 via proxy under gvisor, got %q (stderr=%q)", got, res["stderr"])
	}
	// TC-005: the result shape is identical across tiers — same top-level keys and
	// sandbox_status keys, with tier echoing "gvisor". No new/removed fields.
	for _, k := range []string{"stdout", "stderr", "exit_code", "sandbox_status"} {
		if _, ok := res[k]; !ok {
			t.Fatalf("result missing top-level key %q; contract shape changed", k)
		}
	}
	status, ok := res["sandbox_status"].(map[string]any)
	if !ok {
		t.Fatal("sandbox_status missing or wrong type")
	}
	for _, k := range []string{"sandbox_id", "tier", "duration_ms", "secrets_injected", "status"} {
		if _, ok := status[k]; !ok {
			t.Fatalf("sandbox_status missing key %q; contract shape changed", k)
		}
	}
	if status["tier"] != "gvisor" {
		t.Fatalf("sandbox_status.tier = %v, want \"gvisor\"", status["tier"])
	}

	// Non-allowlisted host blocked by the proxy → 403.
	blockReq := gvisorRunRequest("api.example.com",
		`curl -s -o /dev/null -w "%{http_code}" --unix-socket /proxy.sock http://evil.example.net/ping`+"\n",
		map[string][2]string{"api.example.com": {"127.0.0.1", "1"}})
	bres := Run(blockReq)
	if got := strings.TrimSpace(bres["stdout"].(string)); got != "403" {
		t.Fatalf("expected 403 for non-allowlisted host under gvisor, got %q (stderr=%q)", got, bres["stderr"])
	}
}

func gvisorRunRequest(host, payload string, origin map[string][2]string) RunRequest {
	var req RunRequest
	req.Run.Payload = payload
	req.Run.Tier = "gvisor"
	req.Run.Profile = map[string]any{
		"capabilities": []any{
			map[string]any{"type": "NetConnect", "allowlist": []any{host + ":443"}},
		},
	}
	req.Wiring.OriginMap = origin
	req.Wiring.RequestID = "gvisor-test"
	return req
}
