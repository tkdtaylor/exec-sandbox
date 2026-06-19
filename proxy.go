// SPDX-License-Identifier: Apache-2.0
package main

import (
	"io"
	"net"
	"net/http"
	"os"
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
	originMap map[string][2]string // host -> {ip, port}
	mu        sync.Mutex
	creds     map[string]Credential // host -> credential
	server    *http.Server
	listener  net.Listener
	client    *http.Client
}

func NewEgressProxy(allowlist []string, originMap map[string][2]string) *EgressProxy {
	al := map[string]bool{}
	for _, h := range allowlist {
		al[h] = true
	}
	return &EgressProxy{
		allowlist: al,
		originMap: originMap,
		creds:     map[string]Credential{},
		client:    &http.Client{},
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
