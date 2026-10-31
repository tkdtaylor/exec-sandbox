// SPDX-License-Identifier: Apache-2.0
package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Task 022 tests: Reconcile README and roadmap drift with shipped Tier 3 and v2 attestation.
// Spec markers: TC-022-01, TC-022-02, TC-022-03.

// assertReadmeNoDrift inspects README.md content to ensure Tier 3, seccomp-BPF,
// and v2 attestation are documented as shipped, with no stale planned claims.
func assertReadmeNoDrift(content string) error {
	requiredNeedles := []string{
		"firecracker",
		"keygen",
		"verify-attestation",
		"wiring.attestation_key",
		"vsock",
		"seccomp",
		"seccomp/tier1.bpf",
		"origin_map",
	}
	for _, needle := range requiredNeedles {
		if !strings.Contains(strings.ToLower(content), strings.ToLower(needle)) {
			return fmt.Errorf("README.md missing required needle: %q", needle)
		}
	}

	stalePhrases := []string{
		"firecracker (tier 3) planned",
		"tier-3 (firecracker) is planned",
		"tier 3 (firecracker) is planned",
		"tier-3 is planned",
		"tier 3 is planned",
		"| 3 | firecracker microvm | planned",
		"firecracker is not yet wired",
		"firecracker not yet wired",
		"firecracker is planned",
		"firecracker planned",
	}
	contentLower := strings.ToLower(content)
	for _, stale := range stalePhrases {
		if strings.Contains(contentLower, stale) {
			return fmt.Errorf("README.md contains stale planned claim: %q", stale)
		}
	}

	// Verify the isolation tiers table marks Tier 3 as Shipped.
	if !strings.Contains(content, "| 3 | Firecracker microVM | Shipped") {
		return fmt.Errorf("README.md isolation tiers table does not mark Tier 3 as Shipped")
	}

	return nil
}

// assertRoadmapNoDrift inspects docs/plans/roadmap.md content to ensure tasks 009-021
// are marked completed with no stale planned sections.
func assertRoadmapNoDrift(content string) error {
	for id := 9; id <= 21; id++ {
		taskRowPattern := fmt.Sprintf("| %03d |", id)
		if !strings.Contains(content, taskRowPattern) {
			return fmt.Errorf("roadmap.md missing row for task %03d", id)
		}
	}

	stalePhrases := []string{
		"### Planned",
		"📋 ready",
		"📋 filed",
		"⚠️ Q2 (mount mechanism)",
	}
	for _, stale := range stalePhrases {
		if strings.Contains(content, stale) {
			return fmt.Errorf("roadmap.md contains stale planned marker: %q", stale)
		}
	}

	deferredNeedles := []string{
		"CONNECT",
		"Warm microVM pools",
		"Firecracker `env`-mode credential delivery",
	}
	for _, needle := range deferredNeedles {
		if !strings.Contains(content, needle) {
			return fmt.Errorf("roadmap.md missing deferred item: %q", needle)
		}
	}

	return nil
}

// TC-022-01: README.md reflects shipped Tier 3 and v2 attestation.
func TestTC022_01_ReadmeDrift_Positive(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("TC-022-01: read README.md: %v", err)
	}
	if err := assertReadmeNoDrift(string(data)); err != nil {
		t.Fatalf("TC-022-01: assertReadmeNoDrift failed on current README.md: %v", err)
	}
}

// TC-022-01: negative test proving assertReadmeNoDrift rejects stale content.
func TestTC022_01_ReadmeDrift_Negative(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	base := string(data)

	// Mutate 1: insert stale planned phrase.
	bad1 := strings.Replace(base, "Tier 3 (Firecracker microVM with host vsock egress bridge)", "Tier 3 (Firecracker) is planned", 1)
	if err := assertReadmeNoDrift(bad1); err == nil {
		t.Fatal("TC-022-01: assertReadmeNoDrift accepted stale planned text (BUG)")
	}

	// Mutate 2: remove keygen.
	bad2 := strings.ReplaceAll(base, "keygen", "hidden")
	if err := assertReadmeNoDrift(bad2); err == nil {
		t.Fatal("TC-022-01: assertReadmeNoDrift accepted README missing keygen (BUG)")
	}

	// Mutate 3: mark Tier 3 Planned in table.
	bad3 := strings.Replace(base, "| 3 | Firecracker microVM | Shipped", "| 3 | Firecracker microVM | Planned", 1)
	if err := assertReadmeNoDrift(bad3); err == nil {
		t.Fatal("TC-022-01: assertReadmeNoDrift accepted Planned Tier 3 table row (BUG)")
	}
}

// TC-022-02: docs/plans/roadmap.md reflects completed tasks 009 through 021.
func TestTC022_02_RoadmapDrift_Positive(t *testing.T) {
	data, err := os.ReadFile("docs/plans/roadmap.md")
	if err != nil {
		t.Fatalf("TC-022-02: read roadmap.md: %v", err)
	}
	if err := assertRoadmapNoDrift(string(data)); err != nil {
		t.Fatalf("TC-022-02: assertRoadmapNoDrift failed on current roadmap.md: %v", err)
	}
}

// TC-022-02: negative test proving assertRoadmapNoDrift rejects stale roadmap content.
func TestTC022_02_RoadmapDrift_Negative(t *testing.T) {
	data, err := os.ReadFile("docs/plans/roadmap.md")
	if err != nil {
		t.Fatalf("read roadmap.md: %v", err)
	}
	base := string(data)

	// Mutate 1: reintroduce Planned section.
	bad1 := base + "\n### Planned (filed in docs/tasks/backlog/)\n"
	if err := assertRoadmapNoDrift(bad1); err == nil {
		t.Fatal("TC-022-02: assertRoadmapNoDrift accepted Planned section header (BUG)")
	}

	// Mutate 2: drop task 015.
	bad2 := strings.ReplaceAll(base, "| 015 |", "| xxx |")
	if err := assertRoadmapNoDrift(bad2); err == nil {
		t.Fatal("TC-022-02: assertRoadmapNoDrift accepted roadmap missing task 015 (BUG)")
	}

	// Mutate 3: reintroduce ready marker.
	bad3 := base + "\n| 999 | test | 📋 ready |\n"
	if err := assertRoadmapNoDrift(bad3); err == nil {
		t.Fatal("TC-022-02: assertRoadmapNoDrift accepted stale ready marker (BUG)")
	}
}

// TC-022-03: Full test and fitness suite passes under make check.
// Asserts Makefile defines check target and includes test and fitness.
func TestTC022_03_MakeCheckTarget_Configured(t *testing.T) {
	data, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatalf("TC-022-03: read Makefile: %v", err)
	}
	mk := string(data)
	if !strings.Contains(mk, "check: test fitness") {
		t.Fatal("TC-022-03: Makefile missing 'check: test fitness' target")
	}
	if !strings.Contains(mk, "check") {
		t.Fatal("TC-022-03: Makefile missing check target")
	}
}
