// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"path/filepath"
	"sort"
)

// mkdirTempFn is the function snapshotBaseline uses to create the per-run work directory.
// It defaults to os.MkdirTemp. Tests may override it to inject a pre-configured directory
// (e.g. one with a pre-bound proxy socket to force proxy.Start to fail) — see
// proxy_failure_audit_test.go. Override must be restored via t.Cleanup; this is not
// goroutine-safe so tests that override must not run in parallel.
var mkdirTempFn = os.MkdirTemp

// sandboxBaseline is the pristine per-run state Run() builds before the payload executes: the host
// work dir (the writable surface), the payload.sh contents seeded into it, and a fresh EgressProxy
// with an empty credential map. It is the named "baseline" a leak-proof reset can be asserted
// against (ADR 009). It is INTERNAL — snapshot/restore is a reuse mechanism behind the unchanged
// one-shot run() contract; nothing in the result schema is derived from it.
//
// The boundary is host-side only and tier-independent (ADR 009 Q1/Q4): it covers the host work dir,
// the payload.sh, and the host-side proxy credential map — the same state under bubblewrap and
// gVisor — and does NOT reach inside a tier's kernel root.
type sandboxBaseline struct {
	work      string       // host temp dir bind-mounted writable at /work-equivalent (the writable surface)
	payload   string       // payload.sh contents, re-seeded on restore
	proxySock string       // fresh per-run /proxy.sock path under work (never a stale socket reused)
	proxy     *EgressProxy // fresh proxy with an empty credential map
}

// snapshotBaseline builds the pristine baseline: a fresh temp work dir with payload.sh (mode 0600)
// seeded into it, the fresh per-run proxy socket path, and the given fresh proxy. It captures the
// state BEFORE any payload runs (TC-008-01). The returned baseline's writable surface contains
// exactly payload.sh and the credential map is empty.
func snapshotBaseline(payload string, proxy *EgressProxy) (*sandboxBaseline, error) {
	work, err := mkdirTempFn("", "exec-sandbox-")
	if err != nil {
		return nil, err
	}
	b := &sandboxBaseline{
		work:      work,
		payload:   payload,
		proxySock: filepath.Join(work, "proxy.sock"),
		proxy:     proxy,
	}
	if err := b.seed(); err != nil {
		_ = os.RemoveAll(work)
		return nil, err
	}
	return b, nil
}

// seed writes the pristine writable surface: exactly payload.sh (mode 0600). It is the single
// source of "what a fresh writable surface contains," used both at snapshot and by restore so the
// two are equal by construction.
func (b *sandboxBaseline) seed() error {
	return os.WriteFile(filepath.Join(b.work, "payload.sh"), []byte(b.payload), 0o600)
}

// scriptPath is the host path of the seeded payload.sh inside the writable surface.
func (b *sandboxBaseline) scriptPath() string {
	return filepath.Join(b.work, "payload.sh")
}

// restore returns the sandbox to the captured baseline (ADR 009): it wipes the writable surface back
// to exactly the pristine file set (re-seeding only payload.sh — any file the payload wrote under
// /work is gone) and clears the proxy credential map (subsuming proxy.Wipe(), the credential half of
// the ephemeral non-goal). After restore the writable surface and the credential map equal a
// freshly-built baseline (TC-008-02/03/06). The proxy socket path is unchanged — it is the SAME
// fresh per-run path, never a stale socket from another run; restore re-binds no stale credential.
//
// restore never widens egress: it touches only the host writable surface and the host-side
// credential map. The spawn argv/OCI spec a restored baseline builds are identical to a fresh one's
// (--unshare-all / path-less OCI netns, no --share-net), because they are rebuilt from the same
// bwrapArgv/gvisorOCISpec path (ADR 009).
func (b *sandboxBaseline) restore() error {
	// Remove everything under the writable surface, then re-seed the pristine file set. Removing the
	// dir contents (not the dir itself) keeps the bind-mount target and the fresh proxy socket path
	// stable while guaranteeing nothing the payload wrote survives.
	entries, err := os.ReadDir(b.work)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(b.work, e.Name())); err != nil {
			return err
		}
	}
	if err := b.seed(); err != nil {
		return err
	}
	b.proxy.Wipe() // clear the credential map — restore subsumes Wipe()
	return nil
}

// teardown is the one-shot terminal cleanup: remove the entire work dir and wipe the proxy creds.
// This is the existing Run() teardown (os.RemoveAll(work) + proxy.Wipe()) expressed against the
// baseline — observationally identical to today.
func (b *sandboxBaseline) teardown() {
	if b.work != "" {
		_ = os.RemoveAll(b.work)
	}
	b.proxy.Wipe()
}

// writableSurface returns the sorted set of file names currently under the writable surface. It is
// the observable "state" the restored == fresh diff compares (TC-008-03): a pristine baseline yields
// exactly ["payload.sh"]. Directories are included by name so a payload that mkdir's under /work is
// detected as a leak if it survives a restore.
func (b *sandboxBaseline) writableSurface() []string {
	entries, err := os.ReadDir(b.work)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// credentialHosts returns the sorted set of hosts the proxy currently holds a credential for. A
// pristine/restored baseline yields an empty slice — the credential-map-empty half of the leak proof
// (TC-008-05/06).
func (b *sandboxBaseline) credentialHosts() []string {
	b.proxy.mu.Lock()
	defer b.proxy.mu.Unlock()
	hosts := make([]string, 0, len(b.proxy.creds))
	for h := range b.proxy.creds {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}
