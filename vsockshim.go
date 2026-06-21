// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

// The guest-side egress shim. Q4 (ADR-010) resolution: this shim SHIPS BAKED INTO THE READ-ONLY
// rootfs base as part of the trusted computing base (consistent with task 015's pinned RO Alpine
// base, which carries only /sbin/init plus this forwarder), is STARTED BY /sbin/init AT BOOT, and
// is kept dumb BY CONSTRUCTION — it is a bidirectional byte pump and nothing more. Its dumbness is
// an auditable property: this file imports only net + io + os + sync (no HTTP package), keeps no
// host set and no method map, and does no secret-header handling. The payload's contract is
// unchanged across tiers — it always talks to a Unix socket at /proxy.sock; the shim forwards those
// bytes over AF_VSOCK to the host bridge, which terminates at the live host-side EgressProxy. The
// egress decision (which hosts are reachable, which methods, which secret to inject) stays
// host-side; the shim has no knowledge of which hosts are reachable and holds no secret, so it
// cannot leak one. A test (TestShimHasNoHTTPNoCredNoAllowlist) greps this file to keep it dumb.

// guestShim presents /proxy.sock inside the guest and pumps bytes between each accepted client and
// a fresh dial of the host-side vsock channel. It decides nothing.
type guestShim struct {
	ln        net.Listener
	dialVsock func() (net.Conn, error)
	wg        sync.WaitGroup
	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	closed    bool
}

// startGuestShim listens on proxySock (the in-guest /proxy.sock) and forwards every accepted
// connection over a fresh dialVsock() to the host. dialVsock abstracts the AF_VSOCK dial so the
// pump can be exercised in a unit test over a plain Unix socket (TC-014-05/06); in the booted guest
// it dials the vsock CID/port that firecracker bridges to the host uds_path.
func startGuestShim(proxySock string, dialVsock func() (net.Conn, error)) (*guestShim, error) {
	_ = os.Remove(proxySock)
	ln, err := net.Listen("unix", proxySock)
	if err != nil {
		return nil, fmt.Errorf("guest shim listen %q: %w", proxySock, err)
	}
	_ = os.Chmod(proxySock, 0o600)
	s := &guestShim{ln: ln, dialVsock: dialVsock, conns: map[net.Conn]struct{}{}}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

func (s *guestShim) acceptLoop() {
	defer s.wg.Done()
	for {
		client, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		s.track(client)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(client)
			defer client.Close()
			up, err := s.dialVsock()
			if err != nil {
				return // host side unreachable; drop (fail closed — no egress without the bridge)
			}
			s.track(up)
			defer s.untrack(up)
			defer up.Close()
			relay(client, up)
		}()
	}
}

func (s *guestShim) track(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		c.Close()
		return
	}
	s.conns[c] = struct{}{}
}

func (s *guestShim) untrack(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

// Stop closes the listener and all live connections and waits for the relays to drain.
func (s *guestShim) Stop() {
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.ln.Close()
	s.wg.Wait()
}

// relay copies bytes between the two ends in both directions until either closes. It is a
// transparent, byte-exact pump: no buffering-and-parsing, no rewriting, no dropping. (A sibling of
// the host bridge's pump; kept local so the shim is self-contained and trivially auditable as a
// dumb forwarder.)
func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}
