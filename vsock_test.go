// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// requireKVM skips the test when /dev/kvm is absent or the firecracker binary is not installed
// (mirrors requireBwrap / requireRunsc). The end-to-end guest TCs (TC-014-07/09/10) need a booted
// microVM, which only task 015 provides; until then they MUST skip cleanly rather than silently
// pass.
func requireKVM(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("/dev/kvm not present; skipping firecracker guest integration test (rides on task 015's boot path)")
	}
	if _, err := exec.LookPath("firecracker"); err != nil {
		t.Skip("firecracker binary not installed; skipping firecracker guest integration test")
	}
}

// requireGuestBoot is the hard gate for the L6 end-to-end TCs: they need an actually-bootable guest
// (task 015's launch path). That path is not wired yet, so these TCs skip unconditionally here —
// they will run once task 015 lands. This is honest deferral (a Skip, not a fake pass): the L6
// allow/block/credential evidence belongs to task 015's verification plan, not this slice.
func requireGuestBoot(t *testing.T) {
	t.Helper()
	requireKVM(t)
	t.Skip("guest boot path (task 015) not yet wired; L6 end-to-end allow/block/credential TCs deferred to task 015")
}

// ---------------------------------------------------------------------------
// TC-014-01: the vsock host uds_path is wired to the live EgressProxy.
// ---------------------------------------------------------------------------

// TC-014-01: build the host-side vsock bridge pointed at a started EgressProxy, then drive a request
// through the bridge. A request that crosses the bridge must reach the proxy and be subject to its
// allowlist exactly as a bind-mounted proxy socket would be. proxy.go is reached as-is over the
// vsock-bridge transport — no field of EgressProxy is modified to make this work.
func TestVsockBridgeWiresToLiveEgressProxy(t *testing.T) {
	// A stub origin the proxy forwards an allowlisted request to.
	var originHits int
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("pong"))
	}))
	defer origin.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(origin.URL, "http://"))

	// Start the live EgressProxy on a host socket (the bind-mount endpoint under bwrap/gVisor).
	dir := t.TempDir()
	proxySock := filepath.Join(dir, "egress.sock")
	proxy := NewEgressProxy([]string{"api.example.test"}, nil,
		map[string][2]string{"api.example.test": {host, port}})
	if err := proxy.Start(proxySock); err != nil {
		t.Fatalf("proxy.Start: %v", err)
	}
	defer proxy.Stop()

	// Wire the vsock host bridge: it listens on the vsock uds_path and forwards every byte to the
	// live proxy socket. This is the host side of the firecracker vsock device.
	vsockUDS := filepath.Join(dir, "vsock.sock")
	bridge, err := startVsockBridge(vsockUDS, proxySock)
	if err != nil {
		t.Fatalf("startVsockBridge: %v", err)
	}
	defer bridge.Stop()

	// A client that dials the vsock uds_path (standing in for the guest end of the vsock) must reach
	// the live proxy: an allowlisted host returns 200 and the origin is hit.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", vsockUDS)
			},
		},
	}
	resp, err := client.Get("http://api.example.test/ping")
	if err != nil {
		t.Fatalf("request over vsock bridge failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("allowlisted host over vsock bridge returned %d, want 200", resp.StatusCode)
	}
	if originHits != 1 {
		t.Fatalf("origin hits = %d, want 1 (request did not cross the bridge to the proxy)", originHits)
	}
}

// TC-014-02: proxy.go is byte-for-byte unchanged from the pre-task baseline — the vsock bridge is
// new wiring, not a proxy fork. The baseline sha256 is the hash of proxy.go at the task-014 start
// commit; if proxy.go is edited, this test fails and forces the diff into the open.
func TestProxyGoUnchangedByVsockTask(t *testing.T) {
	const baselineSHA = "8879edc4df4e084d2150743a046c157d35b407ef4f52b7ada1a0a6c70c718553"
	raw, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if got != baselineSHA {
		t.Fatalf("TC-014-02: proxy.go changed (sha256 %s != baseline %s) — the vsock bridge must NOT "+
			"modify the proxy's allowlist/verb/inject logic; it is a transport swap only", got, baselineSHA)
	}
}

// ---------------------------------------------------------------------------
// TC-014-03 / TC-014-04: the wired (vsock-live) config still carries no NIC.
// ---------------------------------------------------------------------------

// TC-014-03: the full firecracker config as wired for a live run (vsock uds pointed at a proxy
// bridge path) still carries no network-interface key — re-asserts the no-NIC invariant on the
// WIRED shape, not just the bare task-013 skeleton.
func TestWiredFirecrackerConfigHasNoNIC(t *testing.T) {
	dir := t.TempDir()
	proxySock := filepath.Join(dir, "egress.sock")
	vsockUDS := filepath.Join(dir, "vsock.sock")

	cfg := wiredFirecrackerConfig("/boot/vmlinux", "/var/lib/fc/rootfs.ext4", "/tmp/payload.sh",
		vsockUDS, proxySock, Limits{CPUCount: 2, MemoryMB: 256})

	if err := configHasNoNIC(cfg); err != nil {
		t.Fatalf("TC-014-03: wired config has a NIC — invariant violated: %v", err)
	}
	// The vsock uds_path must still be present and equal to the host bridge path (the wiring did not
	// drop the egress channel while avoiding a NIC).
	vsock, ok := cfg["vsock"].(map[string]any)
	if !ok {
		t.Fatal("TC-014-03: wired config missing vsock section")
	}
	if got, _ := vsock["uds_path"].(string); got != vsockUDS {
		t.Fatalf("TC-014-03: vsock.uds_path = %q, want %q (host bridge path)", got, vsockUDS)
	}
}

// TC-014-04: the no-NIC guard rejects a config mutated to carry a network-interfaces entry — there
// is no code path that produces a NIC. The microVM equivalent of "there is no --share-net". This
// asserts the guard bites on the WIRED shape, complementing TC-013-05 on the skeleton.
func TestWiredConfigNoNICGuardRejectsNIC(t *testing.T) {
	dir := t.TempDir()
	cfg := wiredFirecrackerConfig("/boot/vmlinux", "/rootfs.ext4", "/payload.sh",
		filepath.Join(dir, "vsock.sock"), filepath.Join(dir, "egress.sock"), Limits{})

	// Mutate the wired config to sneak in a NIC (a regression an attacker or refactor might add).
	cfg["network-interfaces"] = []map[string]any{
		{"iface_id": "eth0", "guest_mac": "AA:FC:00:00:00:01", "host_dev_name": "tap0"},
	}
	if err := configHasNoNIC(cfg); err == nil {
		t.Fatal("TC-014-04: no-NIC guard returned nil for a wired config with a network-interfaces " +
			"entry — the guard is a no-op (BUG)")
	}
}

// ---------------------------------------------------------------------------
// TC-014-05 / TC-014-06: the guest-side shim is a dumb bidirectional byte pump.
// ---------------------------------------------------------------------------

// TC-014-05: the guest-side shim forwards bytes both directions unmodified. Drive it with a fake
// in-guest /proxy.sock client on one end and a fake vsock endpoint on the other, and confirm a byte
// sequence is delivered transparently each direction.
func TestShimIsTransparentBidirectionalPump(t *testing.T) {
	dir := t.TempDir()
	proxySock := filepath.Join(dir, "proxy.sock") // the in-guest /proxy.sock the payload talks to

	// The "vsock" host endpoint: a Unix socket that echoes a known reply for any request and records
	// the request bytes it received. In the real guest this is the vsock to the host bridge.
	vsockUDS := filepath.Join(dir, "vsock-host.sock")
	const upstreamReply = "VSOCK-SIDE-REPLY-9f8e7d"
	var gotFromGuest []byte
	hostDone := make(chan struct{})
	hostLn, err := net.Listen("unix", vsockUDS)
	if err != nil {
		t.Fatalf("listen vsock host: %v", err)
	}
	defer hostLn.Close()
	go func() {
		defer close(hostDone)
		c, err := hostLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, len("REQUEST-FROM-GUEST-1a2b3c"))
		n, _ := io.ReadFull(c, buf)
		gotFromGuest = append([]byte(nil), buf[:n]...)
		c.Write([]byte(upstreamReply))
	}()

	// The shim listens on /proxy.sock (guest side) and dials the vsock host endpoint, pumping bytes
	// both directions. dialVsock is injected as a plain unix dial standing in for AF_VSOCK.
	shim, err := startGuestShim(proxySock, func() (net.Conn, error) { return net.Dial("unix", vsockUDS) })
	if err != nil {
		t.Fatalf("startGuestShim: %v", err)
	}
	defer shim.Stop()

	// The in-guest payload connects to /proxy.sock, sends a request, and reads the reply.
	guestConn, err := net.Dial("unix", proxySock)
	if err != nil {
		t.Fatalf("dial /proxy.sock: %v", err)
	}
	defer guestConn.Close()
	const request = "REQUEST-FROM-GUEST-1a2b3c"
	if _, err := guestConn.Write([]byte(request)); err != nil {
		t.Fatalf("write request to /proxy.sock: %v", err)
	}
	reply := make([]byte, len(upstreamReply))
	if _, err := io.ReadFull(guestConn, reply); err != nil {
		t.Fatalf("read reply from /proxy.sock: %v", err)
	}

	// Both directions must be byte-exact: the request appeared unmodified on the vsock side and the
	// reply came back unmodified to the /proxy.sock side.
	if string(reply) != upstreamReply {
		t.Fatalf("reply on /proxy.sock side = %q, want %q (shim altered the vsock→guest direction)", reply, upstreamReply)
	}
	select {
	case <-hostDone:
	case <-time.After(2 * time.Second):
		t.Fatal("host vsock side never received the request")
	}
	if !bytes.Equal(gotFromGuest, []byte(request)) {
		t.Fatalf("bytes on vsock side = %q, want %q (shim altered the guest→vsock direction)", gotFromGuest, request)
	}
}

// TC-014-06: the shim source contains no HTTP parsing, no allowlist/verb map, and no credential
// storage — its dumbness is an auditable property. It forwards the bytes of a request to a
// NON-allowlisted host and a request carrying no credential identically; the 403 decision is made
// by the host-side EgressProxy, never by the shim.
func TestShimHasNoHTTPNoCredNoAllowlist(t *testing.T) {
	raw, err := os.ReadFile("vsockshim.go")
	if err != nil {
		t.Fatalf("read vsockshim.go: %v", err)
	}
	src := string(raw)

	// Forbidden: any sign the shim parses HTTP, holds a credential, or makes an allowlist/verb
	// decision. These tokens would indicate the trust boundary leaked into the guest.
	forbidden := []string{
		"net/http",      // no HTTP parsing in the guest
		"allowlist",     // no allowlist decision in the guest
		"verbAllowlist", // no verb decision in the guest
		"Authorization", // no credential header handling in the guest
		"Bearer",        // no credential scheme handling in the guest
		"SetCredential", // the shim never holds a credential
		"creds",         // no credential storage
		"blocked-by",    // the block decision is host-side, not in the shim
	}
	for _, tok := range forbidden {
		if strings.Contains(src, tok) {
			t.Fatalf("TC-014-06: vsockshim.go contains %q — the shim must be a dumb byte pump with no "+
				"HTTP/credential/allowlist logic (that stays in the host-side EgressProxy)", tok)
		}
	}

	// Behavioral half: the shim forwards a request to a non-allowlisted host and a no-credential
	// request identically — it has no knowledge of which hosts are allowed. The host-side proxy is
	// what returns 403. Drive the shim against a started EgressProxy via the host bridge.
	dir := t.TempDir()
	proxySock := filepath.Join(dir, "egress.sock")
	proxy := NewEgressProxy([]string{"allowed.example.test"}, nil,
		map[string][2]string{"allowed.example.test": {"127.0.0.1", "1"}})
	if err := proxy.Start(proxySock); err != nil {
		t.Fatalf("proxy.Start: %v", err)
	}
	defer proxy.Stop()

	vsockUDS := filepath.Join(dir, "vsock.sock")
	bridge, err := startVsockBridge(vsockUDS, proxySock)
	if err != nil {
		t.Fatalf("startVsockBridge: %v", err)
	}
	defer bridge.Stop()

	guestProxySock := filepath.Join(dir, "guest-proxy.sock")
	shim, err := startGuestShim(guestProxySock, func() (net.Conn, error) { return net.Dial("unix", vsockUDS) })
	if err != nil {
		t.Fatalf("startGuestShim: %v", err)
	}
	defer shim.Stop()

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", guestProxySock)
			},
		},
	}
	// Non-allowlisted host: the shim forwards the bytes; the host-side proxy returns 403.
	resp, err := client.Get("http://evil.example.net/secret")
	if err != nil {
		t.Fatalf("request through shim failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-allowlisted host returned %d, want 403 (block decision must come from the "+
			"host-side proxy, proving the shim forwarded the bytes without deciding)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "blocked-by-allowlist") {
		t.Fatalf("403 body = %q, want it to contain 'blocked-by-allowlist' (host-side decision)", body)
	}
}

// ---------------------------------------------------------------------------
// TC-014-07: credential value never reaches the guest (positive, end-to-end, L6).
// ---------------------------------------------------------------------------

// TC-014-07: a proxy-mode credential injected host-side appears in NONE of the guest env/args/
// stdout/argv/result; the allowlisted upstream request still succeeds (credential injected after the
// vsock hop). Requires a booted guest — deferred to task 015's launch path (skips here).
func TestCredentialNeverReachesGuest_E2E(t *testing.T) {
	requireGuestBoot(t)
	// Body intentionally unreached until task 015 wires the boot path. When it lands, this drives a
	// real firecracker run with SetCredential(host, {Value:"SENTINEL-SECRET-abc123"}) and asserts the
	// sentinel is absent from guest env/args/stdout, the spawn argv, and result["stdout"], while the
	// allowlisted origin still observes the injected credential header.
}

// ---------------------------------------------------------------------------
// TC-014-08: the guest-surface leak-detector catches a credential that did reach the guest.
// ---------------------------------------------------------------------------

// TC-014-08: assertCredNotInGuest (the microVM F-002 leak-scan, extending the task-009 host surface
// set with guest env/args/stdout) is not vacuous — it returns an error for a constructed guest
// surface set that DOES contain the sentinel. Mirrors task 009 TC-009-08.
func TestCredNotInGuestDetectorRejectsLeak(t *testing.T) {
	const sentinel = "SENTINEL-SECRET-abc123"

	// Negative case A: leak into the guest env.
	leakEnv := guestSurfaces{
		env:    []string{"PATH=/usr/bin", "API_TOKEN=" + sentinel},
		args:   []string{"/usr/bin/sh", "/payload.sh"},
		stdout: "ok\n",
		argv:   []string{"firecracker", "--config-file", "/tmp/vm.json"},
		result: "ok\n",
	}
	if err := assertCredNotInGuest(sentinel, leakEnv); err == nil {
		t.Fatal("TC-014-08: assertCredNotInGuest returned nil for a credential in the guest env — no-op (BUG)")
	}

	// Negative case B: leak into the guest stdout.
	leakStdout := guestSurfaces{
		env:    []string{"PATH=/usr/bin"},
		args:   []string{"/usr/bin/sh", "/payload.sh"},
		stdout: "the token is " + sentinel + "\n",
		argv:   []string{"firecracker"},
		result: "the token is " + sentinel + "\n",
	}
	if err := assertCredNotInGuest(sentinel, leakStdout); err == nil {
		t.Fatal("TC-014-08: assertCredNotInGuest returned nil for a credential in the guest stdout — no-op (BUG)")
	}

	// Negative case C: leak into the guest args.
	leakArgs := guestSurfaces{
		env:    []string{"PATH=/usr/bin"},
		args:   []string{"/usr/bin/sh", "/payload.sh", "--token=" + sentinel},
		stdout: "ok\n",
		argv:   []string{"firecracker"},
		result: "ok\n",
	}
	if err := assertCredNotInGuest(sentinel, leakArgs); err == nil {
		t.Fatal("TC-014-08: assertCredNotInGuest returned nil for a credential in the guest args — no-op (BUG)")
	}

	// Positive control: a clean surface set passes (proves the scan is not always-failing).
	clean := guestSurfaces{
		env:    []string{"PATH=/usr/bin", "HOME=/root"},
		args:   []string{"/usr/bin/sh", "/payload.sh"},
		stdout: "PATH=/usr/bin\nHOME=/root\n",
		argv:   []string{"firecracker", "--config-file", "/tmp/vm.json"},
		result: "PATH=/usr/bin\nHOME=/root\n",
	}
	if err := assertCredNotInGuest(sentinel, clean); err != nil {
		t.Fatalf("TC-014-08: assertCredNotInGuest flagged a clean surface set: %v (false positive)", err)
	}
}

// ---------------------------------------------------------------------------
// TC-014-09 / TC-014-10: allow 200 / block 403 over /proxy.sock-in-guest, direct net fails (L6).
// ---------------------------------------------------------------------------

// TC-014-09: a payload talking to /proxy.sock inside the microVM reaches an allowlisted host (200);
// the stub origin observes exactly one request. Requires a booted guest — deferred to task 015.
func TestGuestReachesAllowlistedHost_E2E(t *testing.T) {
	requireGuestBoot(t)
	// Deferred to task 015: a firecracker run whose profile allowlists api.example.test; the payload
	// GETs http://api.example.test/... via /proxy.sock and gets 200; the origin sees one request.
}

// TC-014-10: a non-allowlisted host is blocked (403); a direct (NIC-less) network attempt fails (no
// route). Requires a booted guest — deferred to task 015.
func TestGuestBlockedNonAllowlistedAndDirectNetFails_E2E(t *testing.T) {
	requireGuestBoot(t)
	// Deferred to task 015: same run; the payload GETs a non-allowlisted host → 403 (zero origin
	// hits); a raw TCP connect from the guest fails because there is no NIC (no route) — the microVM
	// analogue of the gVisor "direct net FAILED-no-network" assertion.
}

// ---------------------------------------------------------------------------
// TC-014-11: Q4 (shim location/lifecycle) resolved and recorded.
// ---------------------------------------------------------------------------

// TC-014-11: docs/spec/behaviors.md records the microVM egress flow (guest /proxy.sock shim → vsock
// → host EgressProxy; credential injected host-side after the hop; no NIC), states the chosen shim
// location/lifecycle as present-tense truth, describes the shim's dumbness as an auditable property,
// and restates the credential-never-in-guest invariant in microVM terms. ADR-010's Q4 carries a
// resolution note pointing to the spec.
func TestQ4ResolvedAndRecorded(t *testing.T) {
	beh, err := os.ReadFile("docs/spec/behaviors.md")
	if err != nil {
		t.Fatalf("read behaviors.md: %v", err)
	}
	bs := string(beh)

	// The microVM egress flow must be recorded: vsock, the guest shim, /proxy.sock, the host proxy.
	for _, needle := range []string{"vsock", "/proxy.sock", "EgressProxy", "no NIC"} {
		if !strings.Contains(bs, needle) {
			t.Fatalf("TC-014-11: behaviors.md does not mention %q in the microVM egress flow", needle)
		}
	}
	// The chosen shim location/lifecycle (rootfs-resident, started by /sbin/init) must be stated.
	if !strings.Contains(bs, "read-only rootfs") && !strings.Contains(bs, "read-only base rootfs") {
		t.Fatal("TC-014-11: behaviors.md does not state the shim ships in the read-only rootfs (Q4 location)")
	}
	if !strings.Contains(bs, "/sbin/init") {
		t.Fatal("TC-014-11: behaviors.md does not state the shim is started by /sbin/init (Q4 lifecycle)")
	}
	// The shim's dumbness must be described as an auditable property (byte pump).
	if !strings.Contains(bs, "byte pump") {
		t.Fatal("TC-014-11: behaviors.md does not describe the shim as a dumb byte pump (auditable dumbness)")
	}
	// No future-tense roadmap language in the microVM egress behavior — the spec is present tense.
	idx := strings.Index(bs, "microVM egress")
	if idx < 0 {
		idx = strings.Index(bs, "vsock")
	}
	region := bs[idx:]
	if end := strings.Index(region, "\n### "); end >= 0 {
		region = region[:end]
	}
	for _, bad := range []string{"will be wired", "will ship", "will implement", "planned to", "in a future"} {
		if strings.Contains(region, bad) {
			t.Fatalf("TC-014-11: microVM egress behavior contains future-tense language %q; the spec is present tense", bad)
		}
	}

	// ADR-010 Q4 must carry a resolution note pointing to the spec.
	adr, err := os.ReadFile("docs/architecture/decisions/010-firecracker-tier3-backend.md")
	if err != nil {
		t.Fatalf("read ADR-010: %v", err)
	}
	as := string(adr)
	if !strings.Contains(as, "Q4") {
		t.Fatal("TC-014-11: ADR-010 has no Q4 reference")
	}
	// The Q4 open-question must be marked resolved (a resolution note), not left as an open question.
	if !strings.Contains(as, "Q4 — vsock shim location and lifecycle (RESOLVED)") &&
		!strings.Contains(as, "Q4 (vsock shim location/lifecycle) — RESOLVED") &&
		!strings.Contains(as, "Q4 resolved") {
		t.Fatal("TC-014-11: ADR-010 Q4 carries no RESOLVED note")
	}
}
