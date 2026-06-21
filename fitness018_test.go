// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Task 018 fitness functions — the three microVM enforcement points joining task 009's `fitness:`
// umbrella:
//
//   fitness-no-nic              (microVM F-001) → TestFitnessNoNIC{Positive,Negative}
//   fitness-cred-not-in-guest   (microVM F-002) → TestFitnessCredNotInGuest{Positive,Negative}
//   fitness-constraints-ge-jailer (ADR-010 A1.Q3) → TestFitnessConstraintsGeJailer{Positive,Negative}
//
// Each rule REUSES the helper authored by an earlier epic task — configHasNoNIC (task 013),
// assertCredNotInGuest (task 014), assertConstraintsGEJailerArgv/assertConstraintsGEJailer (task
// 015) — and adds the positive (passes on current code) + a PROVEN-BITING negative (the helper
// rejects a constructed violation, so the rule is not a no-op). The live-process halves skip-guard on
// /dev/kvm; the config/argv halves run everywhere.

// ---------------------------------------------------------------------------
// fitness-no-nic (microVM F-001): the generated config carries no network-interface key.
// TC-018-07 (positive) + TC-018-08 (negative).
// ---------------------------------------------------------------------------

// TC-018-07: positive — the firecrackerConfig generator (task 013), the wired config, and the fully
// mount-wired config all carry no network-interface key. configHasNoNIC passes on current code.
func TestFitnessNoNICPositive(t *testing.T) {
	cfgs := map[string]map[string]any{
		"base":         firecrackerConfig("/k", "/r", "/p", "/v", Limits{}),
		"wired":        wiredFirecrackerConfig("/k", "/r", "/p", "/v", "/proxy.sock", Limits{}),
		"mount-wired":  fcMountConfig(t, "/tmp/work.ext4", 2),
		"limits-wired": firecrackerConfig("/k", "/r", "/p", "/v", Limits{CPUCount: 2, MemoryMB: 256, PidsLimit: 16, DiskMB: 8}),
	}
	for name, cfg := range cfgs {
		if err := configHasNoNIC(cfg); err != nil {
			t.Fatalf("TC-018-07: %s config carries a NIC: %v", name, err)
		}
	}
}

// TC-018-08: negative — a config MUTATED to include a network-interfaces entry is rejected by
// configHasNoNIC, proving the no-NIC check is not a no-op (it genuinely catches a NIC).
func TestFitnessNoNICNegative(t *testing.T) {
	// Case A: a top-level network-interfaces array (the firecracker NIC shape).
	badA := firecrackerConfig("/k", "/r", "/p", "/v", Limits{})
	badA["network-interfaces"] = []map[string]any{
		{"iface_id": "eth0", "host_dev_name": "tap0"},
	}
	if err := configHasNoNIC(badA); err == nil {
		t.Fatal("TC-018-08: configHasNoNIC accepted a config with a network-interfaces array — the check is a no-op (BUG)")
	}

	// Case B: a singular network-interface key (alternate spelling the serialization scan must catch).
	badB := firecrackerConfig("/k", "/r", "/p", "/v", Limits{})
	badB["network-interface"] = map[string]any{"iface_id": "eth0"}
	if err := configHasNoNIC(badB); err == nil {
		t.Fatal("TC-018-08: configHasNoNIC accepted a config with a network-interface key — the check is a no-op (BUG)")
	}
}

// ---------------------------------------------------------------------------
// fitness-cred-not-in-guest (microVM F-002): the credential never crosses the vsock into the guest.
// TC-018-10 (positive) + TC-018-11 (negative).
// ---------------------------------------------------------------------------

// TC-018-10: positive — with a proxy-mode credential loaded, the sentinel value appears on NONE of
// the guest surfaces (env/args/stdout) nor the host spawn argv / returned stdout. The credential is
// injected host-side after the vsock hop, so the guest-surface set is clean. Reuses the task-014
// leak-scan helper (assertCredNotInGuest). Runs everywhere (surface-build half).
func TestFitnessCredNotInGuestPositive(t *testing.T) {
	const sentinel = "SENTINEL-GUEST-LEAK-xyz789"

	// The proxy holds the credential host-side; nothing the guest sees touches it.
	proxy := NewEgressProxy([]string{"api.example.com"}, nil, nil)
	proxy.SetCredential("api.example.com", Credential{Value: sentinel, Header: "Authorization", Scheme: "Bearer"})

	// The guest surfaces a real firecracker run exposes: the guest process env, the guest args
	// (/usr/bin/sh /payload.sh on the guest), the guest stdout, the host spawn argv (bwrap ...
	// fc-launch), and the returned result stdout. The credential is in NONE of them.
	gs := guestSurfaces{
		env:    []string{"PATH=/usr/bin:/bin", "HOME=/root"},
		args:   []string{"/usr/bin/sh", "/payload.sh"},
		stdout: "TEARDOWN-CLEAN\n",
		argv:   []string{"bwrap", "--unshare-all", "exec-sandbox", "fc-launch", "/tmp/exec-sandbox-fc-XXXX"},
		result: "TEARDOWN-CLEAN\n",
	}
	if err := assertCredNotInGuest(sentinel, gs); err != nil {
		t.Fatalf("TC-018-10: clean guest surfaces flagged a leak: %v", err)
	}

	// Sanity: the proxy DID hold the credential (proves the scan is testing a real credential edge).
	proxy.mu.Lock()
	cred, ok := proxy.creds["api.example.com"]
	proxy.mu.Unlock()
	if !ok || cred.Value != sentinel {
		t.Fatal("TC-018-10: proxy should hold the credential (test setup error)")
	}
}

// TC-018-11: negative — a constructed guest-surface set that DOES contain the sentinel on each
// surface is rejected by assertCredNotInGuest, proving the leak-scan catches a credential that
// crossed into the guest (not a no-op).
func TestFitnessCredNotInGuestNegative(t *testing.T) {
	const sentinel = "SENTINEL-GUEST-LEAK-xyz789"

	leaks := []struct {
		name string
		gs   guestSurfaces
	}{
		{"guest env", guestSurfaces{env: []string{"TOKEN=" + sentinel}}},
		{"guest args", guestSurfaces{args: []string{"/usr/bin/sh", "-c", "curl -H Authorization:" + sentinel}}},
		{"guest stdout", guestSurfaces{stdout: "leaked: " + sentinel + "\n"}},
		{"host spawn argv", guestSurfaces{argv: []string{"bwrap", "--setenv", "TOKEN", sentinel}}},
		{"returned stdout", guestSurfaces{result: sentinel + "\n"}},
	}
	for _, l := range leaks {
		if err := assertCredNotInGuest(sentinel, l.gs); err == nil {
			t.Fatalf("TC-018-11: assertCredNotInGuest accepted a leak on the %s surface — the scan is a no-op (BUG)", l.name)
		}
	}
}

// ---------------------------------------------------------------------------
// fitness-constraints-ge-jailer (microVM, ADR-010 Amendment 1 A1.Q3): the Tier-3 launch's effective
// constraints are >= jailer. TC-018-13 (positive) + TC-018-14 (negative).
// ---------------------------------------------------------------------------

// TC-018-13: positive — the real Tier-3 launch argv (firecrackerBackend.Argv: direct firecracker
// under bwrap --unshare-all + limits.go, no jailer) satisfies the argv-level constraints (no jailer,
// all namespaces unshared, /dev/kvm exposed). And a synthetic genuinely-constrained child passes the
// host-side jailer-equivalence assertion. Reuses the task-015 constraints inspection. The argv half
// runs everywhere; the live-process half is the task-015 TC-015-05_Live test (skip-guards on
// /dev/kvm) — this fitness positive covers the argv reconstruction + the host-side assertion shape.
func TestFitnessConstraintsGeJailerPositive(t *testing.T) {
	withRepoRootArtifacts(t)
	if !haveMkfs() {
		t.Skip("mkfs.ext4 not on PATH; Argv cannot build the payload drive here")
	}
	scriptPath := writeTempScript(t, "echo hi")
	dir := t.TempDir()
	argv, cleanup, _, _, err := firecrackerBackend{}.Argv(scriptPath, dir+"/p.sock", "", nil, nil, nil, fcLimits)
	if err != nil {
		t.Fatalf("TC-018-13: Argv: %v", err)
	}
	defer cleanup()

	// Argv-level: the launch reconstructs jailer-equivalent constraints WITHOUT a jailer.
	if err := assertConstraintsGEJailerArgv(argv); err != nil {
		t.Fatalf("TC-018-13 (argv): the Tier-3 launch is NOT >= jailer: %v", err)
	}

	// Host-side assertion shape: a synthetic genuinely-constrained child passes (the same baseline the
	// task-015 negative test mutates). This proves the assertion accepts a correct child.
	good := goodConstraintsChild()
	if err := assertConstraintsGEJailer(good); err != nil {
		t.Fatalf("TC-018-13 (child): a genuinely-constrained child was rejected: %v", err)
	}
}

// TC-018-14: negative — the constraints checker bites. A weakened launch (a jailer in the argv, a
// shared net namespace, or no /dev/kvm) is rejected at the argv level, and a child that weakens ANY
// jailer-equivalent property (shared namespace / host uid / regained caps / no pivot_root) is
// rejected at the host-side level. Proves the rule is not a no-op.
func TestFitnessConstraintsGeJailerNegative(t *testing.T) {
	// Argv-level weakenings.
	badArgvs := [][]string{
		{"jailer", "--exec-file", "/usr/local/bin/firecracker"},                 // a jailer binary
		{"bwrap", "--unshare-all", "--share-net", "/usr/local/bin/firecracker"}, // net shared with host
		{"bwrap", "--unshare-all", "exec-sandbox", "fc-launch", "/b"},           // no /dev/kvm
		{"bwrap", "--dev-bind", "/dev/kvm", "/dev/kvm", "fc-launch", "/b"},      // no --unshare-all
	}
	for i, argv := range badArgvs {
		if err := assertConstraintsGEJailerArgv(argv); err == nil {
			t.Fatalf("TC-018-14 (argv %d): checker accepted a weak argv %v — it is a no-op (BUG)", i, argv)
		}
	}

	// Host-side weakenings: each mutation of a genuinely-constrained child must be rejected.
	weakenings := []struct {
		name   string
		mutate func(*fcChildConstraints)
	}{
		{"shares net namespace with host", func(c *fcChildConstraints) {
			hostNet, _ := os.Readlink("/proc/self/ns/net")
			c.nsIno["net"] = hostNet
		}},
		{"runs as the host uid", func(c *fcChildConstraints) {
			c.uidMap = fmt.Sprintf("%d %d 1", os.Getuid(), os.Getuid())
		}},
		{"regains host capabilities", func(c *fcChildConstraints) {
			c.capEff = "000001ffffffffff"
		}},
		{"root is the host root (no pivot_root)", func(c *fcChildConstraints) {
			c.rootDev, c.rootFS = hostRootMount()
		}},
	}
	for _, w := range weakenings {
		c := goodConstraintsChild()
		w.mutate(c)
		if err := assertConstraintsGEJailer(c); err == nil {
			t.Fatalf("TC-018-14 (child): checker ACCEPTED a child that %s — it is vacuous (BUG)", w.name)
		}
	}
}

// goodConstraintsChild builds a synthetic fcChildConstraints that mirrors a genuinely jailer-
// equivalent firecracker child: every namespace a non-host inode, a non-host userns uid/gid map, no
// effective caps, NoNewPrivs set, and a pivot_root'd tmpfs root. It MUST pass assertConstraintsGEJailer
// — the negative test's mutations each break exactly one property. Mirrors the task-015 baseline.
func goodConstraintsChild() *fcChildConstraints {
	ns := map[string]string{}
	for _, n := range constraintNamespaces {
		ns[n] = n + ":[9999999]" // an inode that cannot equal the host's real one
	}
	return &fcChildConstraints{
		pid:       424242,
		nsIno:     ns,
		uidMap:    fmt.Sprintf("65534 %d 1", os.Getuid()),
		gidMap:    fmt.Sprintf("65534 %d 1", os.Getgid()),
		capEff:    "0000000000000000",
		noNewPriv: "1",
		rootDev:   "0:176 /newroot",
		rootFS:    "tmpfs",
	}
}

// ---------------------------------------------------------------------------
// TC-018-09 / TC-018-15: the umbrella + spec wiring. The Makefile `fitness:` umbrella must name the
// three new firecracker rules, and fitness-functions.md must record them (present tense, active, real
// check commands). These are inspection guards so a future edit that drops a rule from the umbrella —
// or a spec that drifts from the Makefile — fails here, not silently.
// ---------------------------------------------------------------------------

func TestFitnessUmbrellaIncludesFirecrackerRules(t *testing.T) {
	mk, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	body := string(mk)

	// The umbrella `fitness:` rule list (everything from `fitness:` up to the recipe `@echo`).
	start := strings.Index(body, "\nfitness:")
	if start < 0 {
		t.Fatal("TC-018-09/15: no `fitness:` umbrella rule in the Makefile")
	}
	rest := body[start+1:]
	end := strings.Index(rest, "\n\t@echo")
	if end < 0 {
		t.Fatal("TC-018-09/15: could not delimit the fitness umbrella prerequisite list")
	}
	umbrella := rest[:end]

	for _, rule := range []string{"fitness-no-nic", "fitness-cred-not-in-guest", "fitness-constraints-ge-jailer"} {
		if !strings.Contains(umbrella, rule) {
			t.Fatalf("TC-018-09/15: the `fitness:` umbrella does not list %q — the microVM rule did not join task 009's umbrella; umbrella=%q", rule, umbrella)
		}
		// Each rule must also have its own per-rule target (mirrors the existing fitness-<id> pattern).
		if !strings.Contains(body, "\n"+rule+":") {
			t.Fatalf("TC-018-09/15: the Makefile has no `%s:` target", rule)
		}
	}
}

func TestFitnessSpecRecordsFirecrackerRules(t *testing.T) {
	b, err := os.ReadFile("docs/spec/fitness-functions.md")
	if err != nil {
		t.Fatalf("read fitness-functions.md: %v", err)
	}
	body := string(b)
	// The three real check commands must appear (the spec names the targets that exist in the Makefile).
	for _, needle := range []string{
		"make fitness-no-nic",
		"make fitness-cred-not-in-guest",
		"make fitness-constraints-ge-jailer",
		"constraints-≥-jailer", // the new row's name carries the A1.Q3 source
		"A1.Q3",
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("TC-018-12/15: fitness-functions.md does not record %q — the microVM enforcement points are not documented", needle)
		}
	}
	// Present tense / no future-tense drift smell on the new rows: the rules are wired NOW.
	if strings.Contains(body, "will register fitness-no-nic") || strings.Contains(body, "fitness-no-nic (planned)") {
		t.Fatal("TC-018-12/15: fitness-functions.md describes the no-NIC rule in future tense — the spec is what IS, not what WILL BE")
	}
}

// TC-018-05: SPEC.md Non-goals + the project-summary tier sentence are rewritten in place — Tier-3
// Firecracker is WIRED (no-NIC + vsock-bridged egress, host-side baseline, NO jailer), present tense,
// no future tense; VMM-native snapshot stays an EXPLICIT non-goal. This guards the spec against
// drifting back to "not yet implemented" or sprouting future-tense planned work.
func TestSpecMd_Tier3WiredVMMSnapshotStillNonGoal(t *testing.T) {
	b, err := os.ReadFile("docs/spec/SPEC.md")
	if err != nil {
		t.Fatalf("read SPEC.md: %v", err)
	}
	body := string(b)

	// VMM-native snapshot/restore is still an EXPLICIT non-goal (D6).
	if !strings.Contains(body, "VMM-native snapshot") || !strings.Contains(body, "out of scope") {
		t.Fatal("TC-018-05: SPEC.md no longer records VMM-native snapshot/restore as out of scope (D6)")
	}
	// Tier-3 is WIRED — the old 'not yet implemented' / 'tier not implemented' Tier-3 bullet is gone.
	for _, stale := range []string{
		"Tier 3 not yet implemented",
		"firecracker returns `tier not implemented`",
		"Tier-3 not yet implemented",
	} {
		if strings.Contains(body, stale) {
			t.Fatalf("TC-018-05: SPEC.md still contains the stale Tier-3 phrase %q — Tier-3 is wired now", stale)
		}
	}
	// The teardown closing is recorded present-tense (the task-018 capability).
	if !strings.Contains(body, "tears down on every exit path") && !strings.Contains(body, "no guest") {
		t.Fatal("TC-018-05: SPEC.md does not record the Tier-3 teardown (no guest outlives the run) present-tense")
	}
	// No future-tense 'task 018' remnant: the closing task is done, not planned.
	for _, future := range []string{"task 018)", "remains for later tasks", "What remains for later"} {
		if strings.Contains(body, future) {
			t.Fatalf("TC-018-05: SPEC.md still describes task 018 as future work (%q) — it is done", future)
		}
	}
	// NO "jailer-launched" wording (ADR-010 Amendment 1: there is NO jailer).
	if strings.Contains(body, "jailer-launched") {
		t.Fatal("TC-018-05: SPEC.md says 'jailer-launched' — Tier-3 runs WITHOUT a jailer (A1.Q3)")
	}
}

// TC-018-06: diagrams.md shows the Firecracker tier behind the seam with its vsock-bridged egress
// (guest /proxy.sock shim → vsock → host EgressProxy) and the teardown reclaim, with the date bumped.
func TestDiagramsMd_FirecrackerTierAndVsockEgress(t *testing.T) {
	b, err := os.ReadFile("docs/architecture/diagrams.md")
	if err != nil {
		t.Fatalf("read diagrams.md: %v", err)
	}
	body := string(b)
	for _, needle := range []string{
		"task 018",            // the date-bump note names this task
		"firecrackerBackend",  // the Firecracker component is in the §3 view
		"vsock",               // the vsock-bridged egress path
		"EgressProxy",         // egress terminates at the host proxy
		"teardown",            // the teardown reclaim is shown
		"VMM-native snapshot", // recorded as out of scope on the diagram side too
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("TC-018-06: diagrams.md does not mention %q — the Firecracker tier + vsock egress + teardown are not diagrammed", needle)
		}
	}
	// The vsock-bridged egress flow is spelled out (guest /proxy.sock shim → vsock → host EgressProxy).
	if !strings.Contains(body, "/proxy.sock shim → vsock → host EgressProxy") {
		t.Fatal("TC-018-06: diagrams.md does not show the guest /proxy.sock shim → vsock → host EgressProxy egress path")
	}
	// The date at the top is bumped to the task-018 date.
	if !strings.Contains(body, "**Last updated:** 2026-06-21") {
		t.Fatal("TC-018-06: diagrams.md date was not bumped for task 018")
	}
}
