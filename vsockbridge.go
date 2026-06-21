// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
)

// vsockBridge is the HOST side of the Firecracker virtio-vsock egress channel. It listens on the
// vsock device's host-side uds_path (the path the generated firecracker config names — task 013's
// firecrackerConfig "vsock".uds_path) and forwards every byte, in both directions, to the live
// EgressProxy's Unix socket. It is a pure transport substitution for the bind-mount that
// bubblewrap and gVisor use: the proxy (proxy.go) is reached AS-IS over this transport. The bridge
// makes no allowlist/verb/credential decision — all of that stays in EgressProxy.handle, unchanged.
//
// In a real firecracker run the guest opens AF_VSOCK to the host CID; firecracker terminates that
// vsock at the host-side uds_path. The bridge accepts those connections and pumps bytes to the
// proxy socket — so from the proxy's point of view a vsock-backed request is indistinguishable from
// a bind-mounted one (same Unix-socket HTTP request, same allowlist/inject path).
type vsockBridge struct {
	ln        net.Listener
	proxySock string
	wg        sync.WaitGroup
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	closed    bool
}

// startVsockBridge binds the host vsock uds_path and forwards accepted connections to proxySock
// (the live EgressProxy socket). Each accepted connection is bidirectionally pumped to a fresh dial
// of the proxy socket. The bridge owns no credential and parses no HTTP — it is a byte relay.
func startVsockBridge(vsockUDS, proxySock string) (*vsockBridge, error) {
	_ = os.Remove(vsockUDS)
	ln, err := net.Listen("unix", vsockUDS)
	if err != nil {
		return nil, fmt.Errorf("vsock bridge listen %q: %w", vsockUDS, err)
	}
	_ = os.Chmod(vsockUDS, 0o600)
	b := &vsockBridge{ln: ln, proxySock: proxySock, conns: map[net.Conn]struct{}{}}
	b.wg.Add(1)
	go b.acceptLoop()
	return b, nil
}

func (b *vsockBridge) acceptLoop() {
	defer b.wg.Done()
	for {
		guestSide, err := b.ln.Accept()
		if err != nil {
			return // listener closed
		}
		b.track(guestSide)
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			defer b.untrack(guestSide)
			defer guestSide.Close()
			proxySide, err := net.Dial("unix", b.proxySock)
			if err != nil {
				return // proxy not reachable; drop the connection (fail closed)
			}
			b.track(proxySide)
			defer b.untrack(proxySide)
			defer proxySide.Close()
			pump(guestSide, proxySide)
		}()
	}
}

func (b *vsockBridge) track(c net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		c.Close()
		return
	}
	b.conns[c] = struct{}{}
}

func (b *vsockBridge) untrack(c net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.conns, c)
}

// Stop closes the listener and all live connections and waits for the pump goroutines to drain.
func (b *vsockBridge) Stop() {
	b.mu.Lock()
	b.closed = true
	for c := range b.conns {
		c.Close()
	}
	b.mu.Unlock()
	b.ln.Close()
	b.wg.Wait()
}

// pump copies bytes between two connections in both directions until either side closes. It is the
// single shared transport primitive: a transparent, byte-exact relay with no buffering-and-parsing,
// no rewriting, and no dropping. Both the host vsock bridge and the guest shim use it — neither
// inspects the bytes.
func pump(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		// Half-close the write side so the peer sees EOF and its own copy direction can finish.
		if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// wiredFirecrackerConfig builds the firecracker microVM config for a LIVE run: it is exactly
// firecrackerConfig (task 013) with the vsock host-side uds_path pointed at the vsock bridge path
// that fronts the live EgressProxy at proxySock. The proxySock is recorded in the bridge wiring,
// not in the config (firecracker only knows the vsock uds_path; the host-side bridge owns the hop
// from there to the proxy). This function exists so the no-NIC invariant can be re-asserted on the
// WIRED shape (TC-014-03/04), not just the bare skeleton (TC-013-04).
//
// proxySock is accepted to make the wiring explicit at the call site and to keep this signature
// stable for task 015 (which wires the launch path); it is deliberately NOT serialized into the
// guest-visible config — the credential edge and the proxy socket live host-side only.
func wiredFirecrackerConfig(kernelPath, rootfsPath, scriptPath, vsockUDS, proxySock string, lim Limits) map[string]any {
	_ = proxySock // host-side only; never serialized into the guest config (F-002 boundary)
	return firecrackerConfig(kernelPath, rootfsPath, scriptPath, vsockUDS, lim)
}

// guestSurfaces is the microVM F-002 leak-scan surface set. It extends the task-009 host-side
// surfaces (spawn argv, env pairs, stdout — assertCredNotInSurfaces) with the GUEST-visible
// surfaces a credential must never reach: the guest process env, the guest args, and the guest
// stdout. argv is the host spawn argv and result is the returned result["stdout"]; both are carried
// here so a single helper covers every surface the credential could leak through. Task 018's
// fitness-cred-not-in-guest reuses this type + assertCredNotInGuest.
type guestSurfaces struct {
	env    []string // guest process environment (k=v entries)
	args   []string // guest process args (e.g. /usr/bin/sh /payload.sh ...)
	stdout string   // guest process stdout
	argv   []string // host spawn argv (firecracker ...)
	result string   // returned result["stdout"]
}

// assertCredNotInGuest returns a non-nil error if credValue appears on ANY guest or host surface in
// gs. It is the microVM analogue of assertCredNotInSurfaces (task 009 F-002): the credential is
// injected host-side AFTER the vsock hop, so it must be absent from the guest env/args/stdout, the
// host spawn argv, and the returned stdout. A clean surface set returns nil. Non-vacuous: the
// negative tests (TC-014-08) construct leaks on each surface and require an error.
func assertCredNotInGuest(credValue string, gs guestSurfaces) error {
	if credValue == "" {
		return fmt.Errorf("F-002 (microVM): empty credValue is not a valid leak-scan input")
	}
	for _, e := range gs.env {
		if strings.Contains(e, credValue) {
			return fmt.Errorf("F-002 (microVM): credential value found in guest env entry %q", e)
		}
	}
	if a := strings.Join(gs.args, " "); strings.Contains(a, credValue) {
		return fmt.Errorf("F-002 (microVM): credential value found in guest args %q", a)
	}
	if strings.Contains(gs.stdout, credValue) {
		return fmt.Errorf("F-002 (microVM): credential value found in guest stdout")
	}
	if v := strings.Join(gs.argv, " "); strings.Contains(v, credValue) {
		return fmt.Errorf("F-002 (microVM): credential value found in host spawn argv %q", v)
	}
	if strings.Contains(gs.result, credValue) {
		return fmt.Errorf("F-002 (microVM): credential value found in returned result stdout")
	}
	return nil
}
