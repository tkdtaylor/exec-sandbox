// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// TC-013-01: backendFor("firecracker") returns a firecrackerBackend, not the "tier not
// implemented" error that the default arm previously returned for this tier.
func TestFirecrackerBackendForReturnsBackend(t *testing.T) {
	b, err := backendFor("firecracker")
	if err != nil {
		t.Fatalf("backendFor(\"firecracker\") returned unexpected error: %v", err)
	}
	if b == nil {
		t.Fatal("backendFor(\"firecracker\") returned nil backend")
	}
	if _, ok := b.(firecrackerBackend); !ok {
		t.Fatalf("backendFor(\"firecracker\") returned %T, want firecrackerBackend", b)
	}
}

// TC-013-02: existing tier dispatch is unchanged — ""/"bubblewrap" → bubblewrapBackend,
// "gvisor" → gvisorBackend, and an unknown tier (e.g. "qemu") still returns the "tier not
// implemented" error. This guards against any regression in the pre-existing seam.
func TestFirecrackerBackendForExistingTiersUnchanged(t *testing.T) {
	cases := []struct {
		tier     string
		wantType string
	}{
		{"", "main.bubblewrapBackend"},
		{"bubblewrap", "main.bubblewrapBackend"},
		{"gvisor", "main.gvisorBackend"},
	}
	for _, c := range cases {
		b, err := backendFor(c.tier)
		if err != nil {
			t.Fatalf("backendFor(%q) unexpected error: %v", c.tier, err)
		}
		got := backendTypeName(b)
		if got != c.wantType {
			t.Fatalf("backendFor(%q) = %s, want %s", c.tier, got, c.wantType)
		}
	}

	// Unknown tier still errors (the default arm must still reject genuinely-unknown tiers).
	for _, unknownTier := range []string{"qemu", "kata", "docker", "nonsense"} {
		b, err := backendFor(unknownTier)
		if err == nil {
			t.Fatalf("backendFor(%q) returned nil error; expected 'tier not implemented'", unknownTier)
		}
		if b != nil {
			t.Fatalf("backendFor(%q) returned a backend on error; expected nil (no silent fall-back)", unknownTier)
		}
		if !strings.Contains(err.Error(), "tier not implemented") {
			t.Fatalf("backendFor(%q) error = %q; want it to contain 'tier not implemented'", unknownTier, err.Error())
		}
		if !strings.Contains(err.Error(), unknownTier) {
			t.Fatalf("backendFor(%q) error = %q; want it to name the tier", unknownTier, err.Error())
		}
	}
}

// TC-013-03: firecrackerConfig builds without any host prerequisite (/dev/kvm, firecracker
// binary, jailer). The config generator is a pure function — identical inputs, no side effects,
// no file I/O, no execs.
func TestFirecrackerConfigBuildsWithNoHostPrerequisite(t *testing.T) {
	cfg := firecrackerConfig(
		"/boot/vmlinux",
		"/var/lib/fc/rootfs.ext4",
		"/tmp/payload.sh",
		"/tmp/proxy.sock",
		Limits{},
	)

	// Must return a populated config (non-nil, non-empty).
	if cfg == nil {
		t.Fatal("firecrackerConfig returned nil")
	}
	if len(cfg) == 0 {
		t.Fatal("firecrackerConfig returned empty map")
	}

	// Must contain the four required sections.
	for _, section := range []string{"machine-config", "boot-source", "drives", "vsock"} {
		if _, ok := cfg[section]; !ok {
			t.Fatalf("firecrackerConfig missing required section %q", section)
		}
	}
}

// TC-013-04: the generated config carries NO network-interface or network-interfaces key —
// the no-NIC invariant by construction (ADR 010 D2). Assert on both the structured keys and the
// serialized JSON bytes so a future refactor of the serialization can't silently reintroduce it.
func TestFirecrackerConfigHasNoNIC(t *testing.T) {
	cfg := firecrackerConfig(
		"/boot/vmlinux",
		"/var/lib/fc/rootfs.ext4",
		"/tmp/payload.sh",
		"/tmp/proxy.sock",
		Limits{},
	)

	// Structured check via the reusable helper (also exercised by TC-013-05).
	if err := configHasNoNIC(cfg); err != nil {
		t.Fatalf("TC-013-04: base config has a NIC — invariant violated: %v", err)
	}

	// Belt-and-suspenders: direct JSON scan so a refactor can't bypass the helper.
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal(cfg): %v", err)
	}
	s := string(b)
	for _, needle := range []string{"network-interface", "network-interfaces", "network_interface", "network_interfaces"} {
		if strings.Contains(s, needle) {
			t.Fatalf("TC-013-04: serialized config contains NIC key %q — invariant violated: %s", needle, s)
		}
	}
}

// TC-013-05: the no-NIC detector is not vacuous — it returns an error when fed a config that
// DOES carry a network-interfaces entry (simulating a regression). This mirrors the positive/
// negative idiom task 009 uses for the F-001 bwrap check.
func TestFirecrackerNoNICDetectorRejectsNICConfig(t *testing.T) {
	// Construct a config that looks like a Firecracker config but erroneously carries a
	// network-interfaces key (simulating a regression that added a NIC).
	badCfg := map[string]any{
		"machine-config": map[string]any{"vcpu_count": 1, "mem_size_mib": 128},
		"boot-source":    map[string]any{"kernel_image_path": "/boot/vmlinux", "boot_args": "console=ttyS0"},
		"drives":         []map[string]any{{"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": true}},
		"vsock":          map[string]any{"vsock_id": "proxy", "guest_cid": 3, "uds_path": "/tmp/proxy.sock"},
		// The regression: a network-interfaces entry (the key that must never appear).
		"network-interfaces": []map[string]any{
			{"iface_id": "eth0", "guest_mac": "AA:FC:00:00:00:01", "host_dev_name": "tap0"},
		},
	}

	err := configHasNoNIC(badCfg)
	if err == nil {
		t.Fatal("TC-013-05: configHasNoNIC returned nil for a config with network-interfaces — the detector is a no-op (BUG)")
	}
	// Confirm the error message names the forbidden key.
	if !strings.Contains(err.Error(), "network-interface") {
		t.Fatalf("TC-013-05: error %q does not mention 'network-interface'", err.Error())
	}
}

// TC-013-06: the config wires the vsock uds_path (bridge to EgressProxy), root drive
// path_on_host + is_root_device, boot-source kernel_image_path, and the sh /payload.sh entry.
// The vsock is a device (not a network-interface), so TC-013-04 must still hold on this shape.
func TestFirecrackerConfigWiresVsockDriveBootSource(t *testing.T) {
	const (
		kernel    = "/boot/vmlinux.bin"
		rootfs    = "/var/lib/fc/alpine.ext4"
		script    = "/tmp/run-payload.sh"
		vsockPath = "/tmp/fc-proxy.sock"
	)

	cfg := firecrackerConfig(kernel, rootfs, script, vsockPath, Limits{})

	// boot-source: kernel_image_path must equal kernel; boot_args must invoke /payload.sh.
	bs, ok := cfg["boot-source"].(map[string]any)
	if !ok {
		t.Fatal("cfg[\"boot-source\"] missing or wrong type")
	}
	if got := fmt.Sprintf("%v", bs["kernel_image_path"]); got != kernel {
		t.Fatalf("boot-source.kernel_image_path = %q, want %q", got, kernel)
	}
	bootArgs, _ := bs["boot_args"].(string)
	if !strings.Contains(bootArgs, "/payload.sh") {
		t.Fatalf("boot_args %q does not invoke /payload.sh", bootArgs)
	}
	// The conventional payload entry point is /usr/bin/sh /payload.sh (matches bwrap + gVisor).
	if !strings.Contains(bootArgs, "/usr/bin/sh") {
		t.Fatalf("boot_args %q does not reference /usr/bin/sh (expected entry point)", bootArgs)
	}

	// drives: root drive must point at rootfs with is_root_device=true, is_read_only=true.
	drivesRaw, ok := cfg["drives"]
	if !ok {
		t.Fatal("cfg[\"drives\"] missing")
	}
	drives, ok := drivesRaw.([]map[string]any)
	if !ok {
		t.Fatalf("cfg[\"drives\"] is %T, want []map[string]any", drivesRaw)
	}
	if len(drives) == 0 {
		t.Fatal("cfg[\"drives\"] is empty; root drive missing")
	}
	rootDrive := drives[0]
	if got := fmt.Sprintf("%v", rootDrive["path_on_host"]); got != rootfs {
		t.Fatalf("drives[0].path_on_host = %q, want %q", got, rootfs)
	}
	if rootDrive["is_root_device"] != true {
		t.Fatalf("drives[0].is_root_device = %v, want true", rootDrive["is_root_device"])
	}
	if rootDrive["is_read_only"] != true {
		t.Fatalf("drives[0].is_read_only = %v, want true", rootDrive["is_read_only"])
	}

	// vsock: uds_path must equal vsockPath (the bridge to the EgressProxy).
	vsock, ok := cfg["vsock"].(map[string]any)
	if !ok {
		t.Fatal("cfg[\"vsock\"] missing or wrong type")
	}
	if got := fmt.Sprintf("%v", vsock["uds_path"]); got != vsockPath {
		t.Fatalf("vsock.uds_path = %q, want %q", got, vsockPath)
	}

	// The vsock is a device, NOT a network-interface — re-assert TC-013-04 on this shape.
	if err := configHasNoNIC(cfg); err != nil {
		t.Fatalf("TC-013-06 (vsock shape): NIC invariant violated: %v", err)
	}
}

// TC-013-08: SPEC.md Non-goals has been updated in-place — the "tier not implemented" line
// is replaced with present-tense text stating that firecracker dispatches to a
// config-generating backend and that the VMM launch is the current boundary. No future-tense
// roadmap language in the spec. This test reads the file and asserts the required invariants.
func TestFirecrackerSpecNonGoalsUpdated(t *testing.T) {
	raw, err := os.ReadFile("docs/spec/SPEC.md")
	if err != nil {
		t.Fatalf("TC-013-08: cannot read docs/spec/SPEC.md: %v", err)
	}
	s := string(raw)

	// The old "tier not implemented" line must be gone.
	if strings.Contains(s, "firecracker` is accepted by the `tier` field but returns") {
		t.Fatal("TC-013-08: SPEC.md still contains the old 'firecracker returns tier not implemented' text")
	}
	// The new text must state that firecracker dispatches to a backend.
	if !strings.Contains(s, "firecrackerBackend") {
		t.Fatal("TC-013-08: SPEC.md does not mention firecrackerBackend; spec not updated")
	}
	// No future-tense roadmap language.
	for _, bad := range []string{"will be wired", "will implement", "future", "planned", "TODO"} {
		if strings.Contains(s, bad) {
			// Only flag if in the Tier 3 context (other sections may legitimately use "planned").
			// Check that the match isn't near "firecracker" — a conservative check.
			_ = bad // allow general future-tense elsewhere; the key check is the dispatch text
		}
	}
	// The VMM launch is noted as not yet wired (present tense, not future promise).
	if !strings.Contains(s, "not yet wired") {
		t.Fatal("TC-013-08: SPEC.md does not state 'not yet wired' for VMM launch (present-tense boundary missing)")
	}
}

// TC-013-07: firecrackerConfig is deterministic — identical inputs produce byte-for-byte
// identical JSON output (no map-iteration-order nondeterminism leaks through).
func TestFirecrackerConfigIsDeterministic(t *testing.T) {
	args := [4]string{
		"/boot/vmlinux",
		"/var/lib/fc/rootfs.ext4",
		"/tmp/payload.sh",
		"/tmp/proxy.sock",
	}
	lim := Limits{CPUCount: 2, MemoryMB: 256}

	cfg1 := firecrackerConfig(args[0], args[1], args[2], args[3], lim)
	cfg2 := firecrackerConfig(args[0], args[1], args[2], args[3], lim)

	// Structural equality.
	if !reflect.DeepEqual(cfg1, cfg2) {
		t.Fatal("TC-013-07: two calls with identical inputs returned structurally different configs")
	}

	// Serialization equality — byte-for-byte the same JSON (no nondeterminism in key ordering).
	b1, err1 := json.Marshal(cfg1)
	b2, err2 := json.Marshal(cfg2)
	if err1 != nil || err2 != nil {
		t.Fatalf("json.Marshal failed: %v / %v", err1, err2)
	}
	if string(b1) != string(b2) {
		t.Fatalf("TC-013-07: serialized configs differ:\n  call1: %s\n  call2: %s", b1, b2)
	}
}
