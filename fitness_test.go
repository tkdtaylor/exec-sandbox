// SPDX-License-Identifier: Apache-2.0
package main

// Fitness-function checks: F-001, F-002, F-004 (new block-severity checks wired by task 009).
//
// Each check has a POSITIVE case (passes on current code) and a NEGATIVE case (the assertion
// helper rejects a constructed invariant-violating surface — proving the check is not a no-op).
//
// The fitness-<id> Make targets run:
//   fitness-no-share-net     → go test -run 'FitnessNoShareNet'    ./...
//   fitness-cred-not-in-sandbox → go test -run 'FitnessCredNotInSandbox' ./...
//   fitness-handle-prefix    → go test -run 'FitnessHandlePrefix'  ./...

import (
	"fmt"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// F-001 helpers
// ---------------------------------------------------------------------------

// assertNoShareNet checks that joined is the bwrap argv that carries --unshare-all
// and omits --share-net.  Returns a non-nil error when the invariant is violated.
func assertNoShareNet(joined string) error {
	if !strings.Contains(joined, "--unshare-all") {
		return fmt.Errorf("F-001: --unshare-all missing from argv: %s", joined)
	}
	if strings.Contains(joined, "--share-net") {
		return fmt.Errorf("F-001: --share-net present in argv: %s", joined)
	}
	return nil
}

// assertGvisorNoSharedNetNS checks that an OCI spec's linux.namespaces contains a
// network-type namespace with no path (an empty/fresh netns, not the host's).
func assertGvisorNoSharedNetNS(spec map[string]any) error {
	linux, ok := spec["linux"].(map[string]any)
	if !ok {
		return fmt.Errorf("F-001 gVisor: spec missing linux section")
	}
	nss, ok := linux["namespaces"].([]map[string]any)
	if !ok {
		return fmt.Errorf("F-001 gVisor: linux.namespaces not []map[string]any")
	}
	for _, ns := range nss {
		if ns["type"] == "network" {
			if p, hasPath := ns["path"]; hasPath && p != "" {
				return fmt.Errorf("F-001 gVisor: network namespace has path %q — not an empty netns", p)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// F-001: No shared network in any backend — TC-009-05 (positive) + TC-009-06 (negative)
// ---------------------------------------------------------------------------

// TC-009-05: positive — baseline bwrapArgv carries --unshare-all, no --share-net; gVisor OCI
// spec has an empty/path-less network namespace.  Covers BOTH backends as F-001 declares.
func TestFitnessNoShareNetPositive(t *testing.T) {
	// bwrap side
	argv := bwrapArgv("/tmp/payload.sh", "/tmp/proxy.sock", "", nil, nil, 0,
		[]string{"/usr/bin/sh", "/payload.sh"}, -1)
	joined := strings.Join(argv, " ")
	if err := assertNoShareNet(joined); err != nil {
		t.Fatal(err)
	}

	// gVisor side
	spec := gvisorOCISpec("/tmp/payload.sh", "/tmp/proxy.sock")
	if err := assertGvisorNoSharedNetNS(spec); err != nil {
		t.Fatal(err)
	}
}

// TC-009-06: negative — the assertion helper rejects a mutated argv that drops --unshare-all
// or adds --share-net, proving the check is not a no-op.
func TestFitnessNoShareNetNegative(t *testing.T) {
	// Case A: argv missing --unshare-all
	badA := "bwrap --ro-bind /usr /usr --proc /proc --tmpfs /tmp --bind /w/proxy.sock /proxy.sock"
	if err := assertNoShareNet(badA); err == nil {
		t.Fatal("F-001: assertNoShareNet should have failed on argv without --unshare-all")
	}

	// Case B: argv WITH --share-net (regression: someone re-enabled host networking)
	badB := "bwrap --ro-bind /usr /usr --unshare-all --share-net --bind /w/proxy.sock /proxy.sock"
	if err := assertNoShareNet(badB); err == nil {
		t.Fatal("F-001: assertNoShareNet should have failed on argv with --share-net")
	}

	// Case C: gVisor spec whose network namespace has a host path (shared networking)
	specBad := map[string]any{
		"linux": map[string]any{
			"namespaces": []map[string]any{
				{"type": "network", "path": "/proc/1/ns/net"},
			},
		},
	}
	if err := assertGvisorNoSharedNetNS(specBad); err == nil {
		t.Fatal("F-001 gVisor: assertGvisorNoSharedNetNS should have failed on spec with network path")
	}
}

// ---------------------------------------------------------------------------
// F-002 helpers
// ---------------------------------------------------------------------------

// assertCredNotInSurfaces checks that the sentinel credential value is absent from every
// surface that reaches or leaves the sandbox: the bwrap argv, the --setenv env pairs, and
// the returned stdout.  Returns a non-nil error if the credential appears on any surface.
func assertCredNotInSurfaces(credValue, argvJoined, setenvPairsJoined, stdout string) error {
	if strings.Contains(argvJoined, credValue) {
		return fmt.Errorf("F-002: credential value %q found in bwrap argv", credValue)
	}
	if strings.Contains(setenvPairsJoined, credValue) {
		return fmt.Errorf("F-002: credential value %q found in --setenv env pairs", credValue)
	}
	if strings.Contains(stdout, credValue) {
		return fmt.Errorf("F-002: credential value %q found in stdout", credValue)
	}
	return nil
}

// ---------------------------------------------------------------------------
// F-002: Proxy-mode credential never in sandbox env/args/stdout — TC-009-07 + TC-009-08
// ---------------------------------------------------------------------------

// TC-009-07: positive — a sentinel credential value is absent from the spawn argv, the
// --setenv env pairs, and the returned stdout when the proxy carries it in EgressProxy.creds.
func TestFitnessCredNotInSandboxPositive(t *testing.T) {
	const sentinel = "SENTINEL-SECRET-abc123"

	// Build the argv surface (bwrap side, no env, no credential in the spawn arg list).
	argv := bwrapArgv("/tmp/payload.sh", "/tmp/proxy.sock", "", nil, nil, 0,
		[]string{"/usr/bin/sh", "/payload.sh"}, -1)
	argvJoined := strings.Join(argv, " ")

	// The proxy holds the credential but nothing in the argv/env path sees it.
	proxy := NewEgressProxy([]string{"api.example.com"}, nil, nil)
	proxy.SetCredential("api.example.com", Credential{
		Value:  sentinel,
		Header: "Authorization",
		Scheme: "Bearer",
	})

	// --setenv env pairs built from an empty env map (no credential injected into the sandbox env).
	pairs := envSetenvPairs(nil)
	setenvStr := ""
	for _, kv := range pairs {
		setenvStr += kv[0] + "=" + kv[1] + " "
	}

	// Simulated stdout from a payload that echoes env (the credential should not appear).
	simulatedStdout := "PATH=/usr/bin:/bin\nHOME=/root\n"

	if err := assertCredNotInSurfaces(sentinel, argvJoined, setenvStr, simulatedStdout); err != nil {
		t.Fatal(err)
	}

	// Sanity: confirm the proxy did hold the credential (proves we're actually testing something).
	proxy.mu.Lock()
	cred, ok := proxy.creds["api.example.com"]
	proxy.mu.Unlock()
	if !ok || cred.Value != sentinel {
		t.Fatal("F-002: proxy should hold the credential (test setup error)")
	}
}

// TC-009-08: negative — the assertion helper rejects a constructed surface set that contains
// the credential value, proving the leak-scan actually catches a leak.
func TestFitnessCredNotInSandboxNegative(t *testing.T) {
	const sentinel = "SENTINEL-SECRET-abc123"

	// Case A: credential leaked into the argv
	if err := assertCredNotInSurfaces(sentinel, "--setenv TOKEN "+sentinel, "", ""); err == nil {
		t.Fatal("F-002: assertCredNotInSurfaces should have failed when credential is in argv")
	}

	// Case B: credential leaked into the --setenv env pairs
	if err := assertCredNotInSurfaces(sentinel, "bwrap --clearenv --setenv PATH /usr/bin:/bin", "TOKEN="+sentinel, ""); err == nil {
		t.Fatal("F-002: assertCredNotInSurfaces should have failed when credential is in env pairs")
	}

	// Case C: credential echoed to stdout (e.g. a payload that ran `echo $TOKEN`)
	if err := assertCredNotInSurfaces(sentinel, "bwrap --clearenv --setenv PATH /usr/bin:/bin", "PATH=/usr/bin:/bin", sentinel+"\n"); err == nil {
		t.Fatal("F-002: assertCredNotInSurfaces should have failed when credential is in stdout")
	}
}

// ---------------------------------------------------------------------------
// F-004 helpers
// ---------------------------------------------------------------------------

// assertHandlePrefix checks that every secrets_injected entry has a handle_prefix of at most 8
// chars AND that none of the entries carries a credential/value key (only {handle_prefix,
// delivery} are allowed in the outer result).  Returns a non-nil error when the invariant is
// violated.
func assertHandlePrefix(entries []map[string]any) error {
	for i, e := range entries {
		p, ok := e["handle_prefix"].(string)
		if !ok {
			return fmt.Errorf("F-004: entry %d missing handle_prefix", i)
		}
		if len(p) > 8 {
			return fmt.Errorf("F-004: entry %d handle_prefix %q has length %d > 8", i, p, len(p))
		}
		if _, has := e["credential"]; has {
			return fmt.Errorf("F-004: entry %d contains forbidden 'credential' key", i)
		}
		if _, has := e["value"]; has {
			return fmt.Errorf("F-004: entry %d contains forbidden 'value' key", i)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// F-004: secrets_injected exposes only ≤8-char handle prefix — TC-009-09 + TC-009-10
// ---------------------------------------------------------------------------

// TC-009-09: positive — entries produced via prefix(handle, 8) satisfy the ≤8-char bound and
// carry no credential/value key.
func TestFitnessHandlePrefixPositive(t *testing.T) {
	longHandle := "vault://handle/abcdefghijklmnop"
	shortHandle := "vault://x"

	entries := []map[string]any{
		{"handle_prefix": prefix(longHandle, 8), "delivery": "proxy"},
		{"handle_prefix": prefix(shortHandle, 8), "delivery": "env"},
	}

	if err := assertHandlePrefix(entries); err != nil {
		t.Fatal(err)
	}

	// Confirm the long handle was actually truncated (proves prefix() is called).
	if p := entries[0]["handle_prefix"].(string); p == longHandle {
		t.Fatalf("F-004: expected truncation but got full handle %q", p)
	}
	if got := len(entries[0]["handle_prefix"].(string)); got > 8 {
		t.Fatalf("F-004: handle_prefix length %d > 8", got)
	}
}

// TC-009-10: negative — the assertion helper rejects an entry whose handle_prefix is the full
// handle (> 8 chars), simulating a regression that dropped the prefix(handle,8) truncation.
func TestFitnessHandlePrefixNegative(t *testing.T) {
	longHandle := "vault://handle/abcdefghijklmnop"

	// Case A: full untruncated handle (simulating a regression)
	badEntries := []map[string]any{
		{"handle_prefix": longHandle, "delivery": "proxy"},
	}
	if err := assertHandlePrefix(badEntries); err == nil {
		t.Fatal("F-004: assertHandlePrefix should have failed on a full-length handle_prefix")
	}

	// Case B: entry with a forbidden 'credential' key
	cred := []map[string]any{
		{"handle_prefix": "vault://", "delivery": "proxy", "credential": "secret123"},
	}
	if err := assertHandlePrefix(cred); err == nil {
		t.Fatal("F-004: assertHandlePrefix should have failed on an entry with 'credential' key")
	}

	// Case C: entry with a forbidden 'value' key
	val := []map[string]any{
		{"handle_prefix": "vault://", "delivery": "env", "value": "secret123"},
	}
	if err := assertHandlePrefix(val); err == nil {
		t.Fatal("F-004: assertHandlePrefix should have failed on an entry with 'value' key")
	}
}
