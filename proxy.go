// SPDX-License-Identifier: Apache-2.0
package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// Credential the proxy injects into allowlisted outbound requests. The sandbox never sees
// this — it lives only here, at the injection edge.
type Credential struct {
	Value  string
	Header string
	Scheme string
}

// EgressProxy is the sandbox's ONLY path out. It listens on a Unix socket (bind-mounted
// into the sandbox), enforces the domain allowlist, and injects credentials into allowlisted
// requests before forwarding to the real origin.
type EgressProxy struct {
	allowlist map[string]bool
	// verbAllowlist holds the optional per-host HTTP-verb constraint (ADR 008). A host with an
	// entry here may only use the methods in its (non-empty, canonical-upper-case) set; a host with
	// NO entry is UNCONSTRAINED — every verb is allowed (the backward-compatible default). The verb
	// check only NARROWS egress within an already-allowlisted host; it never widens host access.
	verbAllowlist map[string]map[string]bool
	originMap     map[string][2]string // host -> {ip, port}
	mu            sync.Mutex
	creds         map[string]Credential // host -> credential
	server        *http.Server
	listener      net.Listener
	client        *http.Client
}

// NewEgressProxy builds the per-run proxy. allowlist is the set of bare hosts permitted egress;
// verbAllowlist is the optional host -> allowed-method-set map (ADR 008) — a nil map, or a host
// absent from it, means that host is unconstrained (all verbs allowed, today's behavior). The verb
// sets are expected canonical upper-case (the parser normalizes them); handle() upper-cases the
// request method before comparing.
func NewEgressProxy(allowlist []string, verbAllowlist map[string]map[string]bool, originMap map[string][2]string) *EgressProxy {
	al := map[string]bool{}
	for _, h := range allowlist {
		al[h] = true
	}
	return &EgressProxy{
		allowlist:     al,
		verbAllowlist: verbAllowlist,
		originMap:     originMap,
		creds:         map[string]Credential{},
		client:        &http.Client{},
	}
}

// SetCredential loads a credential for a host (called after vault.inject in proxy mode).
func (p *EgressProxy) SetCredential(host string, c Credential) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creds[host] = c
}

// Wipe clears all injected credentials (called at sandbox teardown).
func (p *EgressProxy) Wipe() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creds = map[string]Credential{}
}

// Start binds the Unix socket and serves until Stop.
func (p *EgressProxy) Start(socketPath string) error {
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(socketPath, 0o600)
	p.listener = ln
	p.server = &http.Server{Handler: http.HandlerFunc(p.handle)}
	go p.server.Serve(ln)
	return nil
}

func (p *EgressProxy) Stop() {
	if p.server != nil {
		p.server.Close()
	}
}

func (p *EgressProxy) handle(w http.ResponseWriter, r *http.Request) {
	host := stripPort(r.Host)
	if !p.allowlist[host] {
		http.Error(w, "blocked-by-allowlist", http.StatusForbidden)
		return
	}
	// Verb check (ADR 008) — AFTER the host check (an unlisted host is already blocked above
	// regardless of method) and BEFORE any upstream request is built or sent. A host with a
	// non-empty verb set may only use the methods in it; an absent/empty set is unconstrained.
	// The method is normalized to canonical upper-case so matching is case-insensitive. A blocked
	// verb returns 403 with a DISTINCT body (blocked-by-method, vs the host block's
	// blocked-by-allowlist) and does NOT open an outbound connection or inject a credential — the
	// check only NARROWS egress.
	if allowed := p.verbAllowlist[host]; len(allowed) > 0 {
		if !allowed[strings.ToUpper(r.Method)] {
			http.Error(w, "blocked-by-method", http.StatusForbidden)
			return
		}
	}
	origin, ok := p.originMap[host]
	if !ok {
		http.Error(w, "no-route", http.StatusBadGateway)
		return
	}
	out, err := http.NewRequest(r.Method, "http://"+origin[0]+":"+origin[1]+r.URL.Path, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out.Host = host
	p.mu.Lock()
	cred, hasCred := p.creds[host]
	p.mu.Unlock()
	if hasCred { // inject the credential the sandbox never possessed
		out.Header.Set(cred.Header, cred.Scheme+" "+cred.Value)
	}
	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
