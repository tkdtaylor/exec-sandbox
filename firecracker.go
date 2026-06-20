// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// firecrackerBackend is the Tier-3 isolation substrate: it runs the payload inside a Firecracker
// microVM. The load-bearing security property is no-NIC by construction (ADR 010 D2): the
// generated microVM config carries no network-interface key — the microVM analogue of
// bwrap --unshare-all and the gVisor empty netns. The only egress affordance is the vsock device
// wired to the host-side EgressProxy socket (task 014).
//
// In this slice (task 013), Argv builds the microVM config and writes it to a per-run bundle dir
// but does NOT launch the VMM — the VMM launch is wired in task 015. The load-bearing deliverable
// is the config generator and the no-NIC invariant, not a running guest.
//
// The absence of the firecracker binary surfaces as a spawn error (exit 127), never a silent
// fall-back to another backend (ADR 010 D1; mirroring gvisor.go:18-20).
type firecrackerBackend struct{}

// Argv builds the Firecracker microVM config, writes it to a temp bundle dir, and returns the
// spawn argv (currently a stub — VMM launch is task 015). The cleanup func removes the bundle.
// No /dev/kvm or firecracker binary is required to build the config (pure function; this slice
// is unit-testable on any host).
func (firecrackerBackend) Argv(scriptPath, proxySock, workdir string, fileReads []string, env map[string]string, envCreds [][2]string, lim Limits) ([]string, func(), []degrade, []*os.File, error) {
	bundle, err := os.MkdirTemp("", "exec-sandbox-fc-")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cleanup := func() { os.RemoveAll(bundle) }

	// The vsock UDS path lives inside the bundle so each run gets its own socket path.
	vsockUDS := filepath.Join(bundle, "proxy.sock")

	// kernelPath and rootfsPath: resolved from profile or well-known locations (task 015 wires
	// the real paths; for this slice we use empty-string placeholders since no guest boots here).
	kernelPath := ""
	rootfsPath := ""

	cfg := firecrackerConfig(kernelPath, rootfsPath, scriptPath, vsockUDS, lim)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}
	cfgPath := filepath.Join(bundle, "vm-config.json")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	// STUB: the VMM launch argv is wired in task 015. Returning ["firecracker", "--no-api",
	// "--config-file", cfgPath] here would produce a working invocation once the binary is
	// present; for now it surfaces as exit 127 (binary absent) rather than a silent fall-back —
	// matching the gVisor pattern (runsc absent → exit 127, not bubblewrap fall-back).
	argv := []string{"firecracker", "--no-api", "--config-file", cfgPath}

	return argv, cleanup, nil, nil, nil
}

// firecrackerConfig builds the Firecracker microVM configuration as a pure function of the
// supplied on-host paths. It issues NO network-interface body (ADR 010 D2): the no-NIC
// invariant is enforced by omission — the key is never present, not set to nil or false.
//
// The returned map is serialization-deterministic: field order is fixed (struct-like ordering
// via a deterministic map key sequence), mirroring gvisorOCISpec's reproducibility. Identical
// inputs produce a byte-for-byte identical JSON output (TC-013-07).
//
// Config sections:
//   - machine-config: vCPU + memory (limits → task 016 maps the details; zero values here)
//   - boot-source:    kernel image path + boot args running /usr/bin/sh /payload.sh
//   - drives:         root drive (rootfs, read-only, root_device=true)
//   - vsock:          host-side UDS path bridging to the EgressProxy (task 014)
//
// Deliberately absent: network-interfaces (D2 — no NIC by omission).
func firecrackerConfig(kernelPath, rootfsPath, scriptPath, vsockUDS string, lim Limits) map[string]any {
	// machine-config: vcpu_count and mem_size_mib from limits (task 016 owns the full mapping;
	// sensible defaults for the skeleton so the config is valid structure today).
	vcpuCount := 1
	if lim.CPUCount > 0 {
		vcpuCount = lim.CPUCount
	}
	memMiB := 128
	if lim.MemoryMB > 0 {
		memMiB = lim.MemoryMB
	}

	// boot-source: the payload entry point is /usr/bin/sh /payload.sh — identical to bwrap and
	// gVisor (every tier uses the same payload delivery convention). The script path is
	// bind-presented inside the guest as /payload.sh (task 017 wires the mount; for the
	// skeleton, scriptPath is recorded in boot_args for traceability).
	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off script=%s init=/usr/bin/sh -- /payload.sh", scriptPath)

	return map[string]any{
		"machine-config": map[string]any{
			"vcpu_count":        vcpuCount,
			"mem_size_mib":      memMiB,
			"smt":               false,
			"track_dirty_pages": false,
		},
		"boot-source": map[string]any{
			"kernel_image_path": kernelPath,
			"boot_args":         bootArgs,
		},
		"drives": []map[string]any{
			{
				"drive_id":       "rootfs",
				"path_on_host":   rootfsPath,
				"is_root_device": true,
				"is_read_only":   true,
			},
		},
		"vsock": map[string]any{
			"vsock_id":  "proxy",
			"guest_cid": 3,
			"uds_path":  vsockUDS,
		},
		// network-interfaces is intentionally absent — no-NIC by omission (ADR 010 D2).
		// DO NOT add a network-interfaces key here; see configHasNoNIC.
	}
}

// configHasNoNIC asserts that cfg contains no network-interface or network-interfaces key at any
// level, in both its structured form and its JSON serialization. It returns a non-nil error if
// either check fails, and nil if the config is NIC-free.
//
// This helper is reused by:
//   - firecracker_test.go TC-013-04 (base config) and TC-013-05 (negative: detector bites on a
//     crafted NIC config)
//   - task 014's vsock-bridge egress tests
//   - task 018's fitness-no-nic target (the microVM analogue of fitness F-001)
func configHasNoNIC(cfg map[string]any) error {
	// Structural check: scan every top-level key and descend one level.
	nicKeys := []string{"network-interface", "network-interfaces", "network_interface", "network_interfaces"}
	for _, k := range nicKeys {
		if _, present := cfg[k]; present {
			return fmt.Errorf("microVM config contains forbidden NIC key %q (ADR 010 D2: no-NIC by omission)", k)
		}
	}

	// Serialization check: stringify and scan for the substring — catches a future refactor
	// that serializes a nested key under a different top-level name.
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("configHasNoNIC: marshal failed: %w", err)
	}
	s := string(b)
	for _, needle := range []string{"network-interface", "network_interface"} {
		if strings.Contains(s, needle) {
			return errors.New("microVM config JSON contains network-interface substring (ADR 010 D2)")
		}
	}
	return nil
}
