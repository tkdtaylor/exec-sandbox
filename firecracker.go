// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// firecrackerBackend is the Tier-3 isolation substrate: it runs the payload inside a Firecracker
// microVM. The load-bearing security property is no-NIC by construction (ADR 010 D2): the
// generated microVM config carries no network-interface key — the microVM analogue of
// bwrap --unshare-all and the gVisor empty netns. The only egress affordance is the vsock device
// wired through the host-side vsock bridge to the live EgressProxy socket (task 014).
//
// Argv (task 015) verifies the pinned kernel + rootfs (sha256, fail-fast on mismatch — A1.Q1),
// builds the per-run bundle (config + the writable payload drive), starts the host-side vsock
// bridge, and returns the spawn argv that launches `firecracker` DIRECTLY under exec-sandbox's
// existing unprivileged `bwrap --unshare-all` + `limits.go` wrapper (NO jailer — A1.Q3). The
// firecracker child is itself contained (non-host uid, all namespaces unshared, cgroup limits,
// chroot) and self-installs its seccomp filters; the cleanup func stops the bridge and removes the
// bundle so no guest or socket outlives the run.
//
// The absence of the firecracker binary / /dev/kvm, or a kernel/rootfs that fails verification,
// surfaces as a spawn error (exit 127), never a silent fall-back to another backend (ADR 010 D1;
// mirroring gvisor.go:18-20).
type firecrackerBackend struct{}

// Argv verifies the pinned guest artifacts, builds the per-run bundle, starts the host-side vsock
// bridge, and returns the bwrap-wrapped `exec-sandbox fc-launch <bundle>` argv plus a cleanup that
// stops the bridge and removes the bundle. A missing/unverifiable artifact, a missing /dev/kvm, or
// an absent firecracker binary is surfaced as a spawn error (exit 127) — never a fall-back.
func (firecrackerBackend) Argv(scriptPath, proxySock, workdir string, fileReads []string, env map[string]string, envCreds [][2]string, lim Limits) ([]string, func(), []degrade, []*os.File, error) {
	bundle, err := os.MkdirTemp("", "exec-sandbox-fc-")
	if err != nil {
		return nil, nil, nil, nil, err
	}
	// cleanup is enriched below with the bridge stop; it always removes the bundle last.
	var bridge *vsockBridge
	cleanup := func() {
		if bridge != nil {
			bridge.Stop()
		}
		os.RemoveAll(bundle)
	}

	// Verify the pinned kernel + rootfs BEFORE any boot (A1.Q1): a sha256 mismatch / missing pin is
	// a hard error here — the backend never boots an unverified artifact and never falls back.
	art, err := loadGuestArtifacts()
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	// The vsock host uds_path lives inside the bundle (per-run socket). firecracker forwards a
	// guest connection on hostVsockPort to <vsockUDS>_<hostVsockPort>, where the bridge listens.
	vsockUDS := filepath.Join(bundle, "proxy.vsock")
	bridgeListen := vsockUDS + "_" + strconv.Itoa(hostVsockPort)
	bridge, err = startVsockBridge(bridgeListen, proxySock)
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	// The untrusted payload is NEVER baked into the read-only base (A1.Q1). It is presented on a
	// separate writable drive (vdb) the backend builds per run from scriptPath; the guest init
	// mounts /dev/vdb read-only and runs /usr/bin/sh on payload.sh from it.
	payloadDrive := filepath.Join(bundle, "payload.ext4")
	if err := buildPayloadDrive(scriptPath, payloadDrive, lim); err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	cfg := firecrackerConfig(art.kernelPath, art.rootfsPath, scriptPath, vsockUDS, lim)
	addPayloadDrive(cfg, payloadDrive)
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

	argv, err := firecrackerArgv(bundle, art, lim)
	if err != nil {
		cleanup()
		return nil, nil, nil, nil, err
	}

	// cpu_count → taskset affinity prefix on the whole argv (inherited into the firecracker child),
	// matching the bwrap/gVisor tiers. A missing taskset degrades (warn + continue), never fails.
	var degrades []degrade
	if prefix, d := cpuAffinityPrefix(lim.CPUCount); d != nil {
		degrades = append(degrades, *d)
	} else if prefix != nil {
		argv = append(prefix, argv...)
	}

	return argv, cleanup, degrades, nil, nil
}

// firecrackerArgv builds the spawn argv that launches firecracker DIRECTLY under the unprivileged
// `bwrap --unshare-all` wrapper (NO jailer — A1.Q3). bwrap supplies the chroot + mnt/pid/ipc/net/
// user namespaces + a non-host uid the jailer would otherwise provide; firecracker self-installs its
// seccomp filters regardless. The in-bwrap command is `exec-sandbox fc-launch <bundle>`, which
// drives the REST API and exits with the guest's exit code.
//
// Bind-mounts (all read-only except /dev/kvm and the bundle):
//   - the exec-sandbox binary (self) — so fc-launch can run in the sandbox,
//   - the firecracker binary,
//   - the verified kernel + rootfs (read-only),
//   - the per-run bundle dir READ-WRITE (the api + vsock sockets + payload drive live there),
//   - /dev/kvm — the ONE device permission Tier-3 needs (rw); no other host device is exposed,
//   - minimal system dirs for the dynamic loader.
//
// No --share-net ever appears: --unshare-all removes the network namespace entirely, the microVM
// analogue of the no-NIC config (the guest's only egress is the vsock bridge, host-side).
func firecrackerArgv(bundle string, art guestArtifacts, lim Limits) ([]string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("cannot resolve exec-sandbox binary path: %w", err)
	}
	fcBin, err := exec.LookPath("firecracker")
	if err != nil {
		return nil, fmt.Errorf("firecracker binary not found on PATH: %w (Tier-3 requires firecracker + /dev/kvm)", err)
	}

	argv := []string{"bwrap",
		"--unshare-all", "--die-with-parent",
		// Non-host uid (A1.Q3, jailer-equivalent identity): --unshare-all creates a new user
		// namespace; --uid/--gid run the firecracker child as an unprivileged 65534 (nobody) INSIDE
		// that namespace — the no-jailer analogue of the jailer's setuid-to-an-unprivileged-uid step.
		// The host-side observable is /proc/<pid>/uid_map "65534 <hostuid> 1" (the in-namespace uid is
		// 65534, not the invoking host uid); the kernel still reports the OWNING host uid in
		// /proc/<pid>/status Uid, so the uid_map — not the status Uid — is what proves the non-host
		// identity (see TC-015-05). /dev/kvm stays reachable: the dev-bind owner maps to nobody in the
		// userns and the device is mode rw for it.
		"--uid", "65534", "--gid", "65534",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc", "/etc",
		"--proc", "/proc",
		"--dev", "/dev",
		// /dev/kvm is the ONE device Tier-3 needs (rw). --dev-bind exposes exactly it (and nothing
		// else) into the otherwise-minimal /dev. No root, no setuid, no caps beyond this (A1.Q3).
		"--dev-bind", "/dev/kvm", "/dev/kvm",
		// The exec-sandbox binary (self) + the firecracker binary, read-only.
		"--ro-bind", self, self,
		"--ro-bind", fcBin, fcBin,
		// The verified kernel + rootfs, read-only.
		"--ro-bind", art.kernelPath, art.kernelPath,
		"--ro-bind", art.rootfsPath, art.rootfsPath,
		// The per-run bundle dir READ-WRITE: the api socket, the vsock bridge socket, and the payload
		// drive all live here; firecracker creates the api socket and connects the vsock here.
		"--bind", bundle, bundle,
	}
	for _, d := range []string{"/bin", "/lib", "/lib64", "/sbin"} {
		if _, err := os.Stat(d); err == nil {
			argv = append(argv, "--ro-bind", d, d)
		}
	}
	// PATH must include the firecracker binary's dir so fc-launch resolves it inside the sandbox
	// (firecracker commonly lives in /usr/local/bin, which is not in the bare default).
	fcDir := filepath.Dir(fcBin)
	path := "/usr/bin:/bin"
	if fcDir != "/usr/bin" && fcDir != "/bin" {
		path = fcDir + ":" + path
	}
	argv = append(argv, "--clearenv", "--setenv", "PATH", path)
	// The in-sandbox command: the launcher drives firecracker and exits with the guest exit code.
	// FC_BINARY names the absolute firecracker path so fc-launch need not rely on PATH resolution.
	argv = append(argv, "--setenv", "FC_BINARY", fcBin)
	argv = append(argv, self, "fc-launch", bundle)
	return argv, nil
}

// addPayloadDrive appends the per-run writable payload drive (vdb) to the firecracker config's
// drives list. It is the second drive (after the read-only root), is_root_device=false; the guest
// init mounts /dev/vdb read-only and runs payload.sh from it. Keeping the payload off the root drive
// is what keeps the base.ext4 digest stable across runs (A1.Q1).
func addPayloadDrive(cfg map[string]any, payloadDrive string) {
	drives, _ := cfg["drives"].([]map[string]any)
	cfg["drives"] = append(drives, map[string]any{
		"drive_id":       "payload",
		"path_on_host":   payloadDrive,
		"is_root_device": false,
		"is_read_only":   true,
	})
}

// buildPayloadDrive creates a small ext4 image at out containing payload.sh (copied from scriptPath).
// It uses `mkfs.ext4 -d` to populate the image without root/loop-mount. disk_mb sizes the image when
// set (default 2 MiB — payload.sh is tiny; the writable /work surface is task 017's concern). A
// missing mkfs.ext4 is a hard error (the guest cannot get its payload), surfaced as a spawn failure.
func buildPayloadDrive(scriptPath, out string, lim Limits) error {
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return fmt.Errorf("cannot build payload drive: mkfs.ext4 not found on PATH: %w", err)
	}
	staging, err := os.MkdirTemp("", "exec-sandbox-fc-payload-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		return fmt.Errorf("read payload %s: %w", scriptPath, err)
	}
	if err := os.WriteFile(filepath.Join(staging, "payload.sh"), data, 0o644); err != nil {
		return err
	}
	sizeMB := 2
	if lim.DiskMB > 0 && lim.DiskMB < 2 {
		sizeMB = 2 // floor so the filesystem metadata fits
	}
	cmd := exec.Command(mkfs, "-q", "-F", "-d", staging, out, fmt.Sprintf("%dM", sizeMB))
	if outBytes, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mkfs.ext4 payload drive: %w: %s", err, strings.TrimSpace(string(outBytes)))
	}
	return nil
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

	// boot-source: PID 1 is /sbin/init (baked into the read-only base), which starts the vsock shim,
	// mounts the per-run payload drive (/dev/vdb) read-only, and runs /usr/bin/sh /payload.sh — the
	// same payload entry point as bwrap and gVisor. Boot args are exactly `console=ttyS0 reboot=k
	// panic=1` (A1.Q1) — reboot=k + panic=1 make a reboot/panic terminate firecracker cleanly — plus
	// pci=off (Firecracker has no PCI) and i8042 quirks to skip the absent legacy keyboard probe.
	// There is deliberately NO `ip=` arg: with no virtio-net device the guest has no NIC to configure
	// (reinforces D2's no-NIC-by-construction at the kernel-cmdline level). scriptPath is unused in
	// the cmdline (the payload arrives on /dev/vdb, not the cmdline).
	_ = scriptPath
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd init=/sbin/init"

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
