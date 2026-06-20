// SPDX-License-Identifier: Apache-2.0
package main

import (
	"sort"
	"sync"
)

// EnvCredentials is the single host-side holder for env-mode credential values (ADR 015). Unlike a
// proxy-mode credential — which never enters the sandbox (F-002) — an env-mode credential is
// DELIBERATELY delivered into the sandbox process environment under a vault-specified variable name.
// Keeping it in one place makes the wipe a single, testable operation (Wipe clears the map), mirroring
// EgressProxy.creds + EgressProxy.Wipe().
//
// The value is delivered to the child at spawn and the host-side copy is wiped post-spawn / at
// teardown; the host retains no env-mode credential past the run.
type EnvCredentials struct {
	mu   sync.Mutex
	vars map[string]string // var_name -> value
}

// NewEnvCredentials builds an empty holder.
func NewEnvCredentials() *EnvCredentials {
	return &EnvCredentials{vars: map[string]string{}}
}

// Set records an env-mode credential under its vault-specified variable name (called after a
// delivery:"env" vault.inject response). A later Set for the same name overwrites.
func (e *EnvCredentials) Set(varName, value string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars[varName] = value
}

// pairs returns the {var_name, value} pairs in deterministic (sorted-name) order, so the delivery
// surface (the bwrap --args FD payload / OCI process.env) is reproducible. Empty when nothing was set.
func (e *EnvCredentials) pairs() [][2]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, 0, len(e.vars))
	for k := range e.vars {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([][2]string, 0, len(names))
	for _, k := range names {
		out = append(out, [2]string{k, e.vars[k]})
	}
	return out
}

// empty reports whether any env-mode credential is currently held.
func (e *EnvCredentials) empty() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.vars) == 0
}

// Wipe clears all held env-mode credential values (the wipe clock, ADR 015): called post-spawn (the
// child already has the value) and again at teardown, so no host-side copy survives the run.
func (e *EnvCredentials) Wipe() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.vars = map[string]string{}
}
