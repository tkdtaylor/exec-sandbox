// SPDX-License-Identifier: Apache-2.0
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// gvisorBackend is the Tier-2 isolation substrate: it runs the payload under the gVisor runsc
// OCI runtime. It enforces the same invariant as the bubblewrap backend — a fresh empty network
// namespace (no host/bridged networking, loopback only) with the egress proxy Unix socket as the
// only path out, bind-mounted at /proxy.sock.
type gvisorBackend struct{}

// Argv builds an OCI bundle (config.json + a rootfs dir) in a temp dir and returns the
// `runsc run` argv that executes it, plus a cleanup func that removes the bundle. runsc must be
// on PATH at run time; its absence surfaces as a spawn error (exit_code 127), never a silent
// bubblewrap fall-back.
func (gvisorBackend) Argv(scriptPath, proxySock, workdir string, fileReads []string, env map[string]string, lim Limits) ([]string, func(), []degrade, error) {
	bundle, err := os.MkdirTemp("", "exec-sandbox-oci-")
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() { os.RemoveAll(bundle) }

	rootfs := filepath.Join(bundle, "rootfs")
	if err := os.MkdirAll(rootfs, 0o700); err != nil {
		cleanup()
		return nil, nil, nil, err
	}

	// memory_mb/pids → OCI process.rlimits (the gVisor sentry honors these directly, so they are
	// enforced even under --ignore-cgroups); disk_mb → the /tmp tmpfs size= option (ADR 003).
	spec := gvisorOCISpec(scriptPath, proxySock)
	// Mount the caller-specified FileRead host paths READ-ONLY at the same path (ADR 005); no-op
	// when empty so the base spec is byte-for-byte unchanged.
	applyFileReadToOCISpec(spec, fileReads)
	// Provision the payload's env (PATH replaces the bare default); no-op when env is empty,
	// leaving process.env as the bare PATH=/usr/bin:/bin (ADR 005).
	applyEnvToOCISpec(spec, env)
	// Mount the host working directory writable at /work and set cwd=/work (ADR 004); no-op when
	// workdir is empty, leaving the base spec (and its backward-compatible behavior) unchanged.
	applyWorkdirToOCISpec(spec, workdir)
	degrades := applyLimitsToOCISpec(spec, lim)
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o600); err != nil {
		cleanup()
		return nil, nil, nil, err
	}

	// runsc consumes the bundle by directory. --network=none is belt-and-suspenders with the
	// empty network namespace in the spec; both express the no-network invariant. --ignore-cgroups
	// lets the run proceed without cgroup write access; resource caps are applied via the OCI
	// process.rlimits and tmpfs size (above), which the sentry enforces without host cgroups.
	// --host-uds=open lets the sandboxed payload connect() to an EXISTING host Unix socket (the
	// bind-mounted proxy socket) but never create new ones. The proxy socket is the only host UDS
	// bind-mounted in, so this preserves the invariant: the proxy is the sole egress path.
	containerID := "sbx-" + randHex(6)
	argv := []string{"runsc", "--network=none", "--host-uds=open", "--ignore-cgroups"}
	// When exec-sandbox is not running as root, runsc needs --rootless to set up the gofer/
	// sandbox in a user namespace (matching how the bwrap path runs unprivileged).
	if os.Geteuid() != 0 {
		argv = append(argv, "--rootless")
	}
	argv = append(argv, "run", "--bundle", bundle, containerID)

	// cpu_count → taskset affinity prefix on the runsc argv. gVisor virtualizes the in-box cpu
	// view, so cpu_count is verified host-side (this argv record) rather than in-box (ADR 003 /
	// agent-builder ADR 028).
	if prefix, d := cpuAffinityPrefix(lim.CPUCount); d != nil {
		degrades = append(degrades, *d)
	} else if prefix != nil {
		argv = append(prefix, argv...)
	}
	return argv, cleanup, degrades, nil
}

// applyWorkdirToOCISpec mounts the host working directory writable at /work and sets the payload's
// cwd to /work, in place (ADR 004). It is a no-op when workdir is empty, so the base spec — a
// read-only rootfs/system-dirs and cwd "/" — is byte-for-byte unchanged and prior behavior is
// preserved. The /work mount is the ONLY writable bind of a host path: its options omit "ro"
// (unlike the read-only /usr, /etc, /payload.sh binds), while everything else stays read-only and
// the empty network namespace (no egress) is untouched.
func applyWorkdirToOCISpec(spec map[string]any, workdir string) {
	if workdir == "" {
		return
	}
	mounts, _ := spec["mounts"].([]map[string]any)
	spec["mounts"] = append(mounts, map[string]any{
		"destination": "/work", "type": "bind", "source": workdir,
		"options": []string{"rbind"}, // writable: no "ro" option (cf. the ro system-dir mounts)
	})
	if proc, ok := spec["process"].(map[string]any); ok {
		proc["cwd"] = "/work"
	}
}

// applyFileReadToOCISpec appends a READ-ONLY bind mount per FileRead path, in place (ADR 005). Each
// host path is mounted at the same path inside the sandbox with options ["ro","rbind"] — mirroring
// the read-only system-dir mounts, NOT the writable /work bind. It is a no-op when paths is empty,
// so the base spec is byte-for-byte unchanged. Read-only is load-bearing: a FileRead mount opens no
// writable surface (only /work is writable) and the empty network namespace is untouched.
func applyFileReadToOCISpec(spec map[string]any, paths []string) {
	if len(paths) == 0 {
		return
	}
	mounts, _ := spec["mounts"].([]map[string]any)
	for _, p := range paths {
		mounts = append(mounts, map[string]any{
			"destination": p, "type": "bind", "source": p,
			"options": []string{"ro", "rbind"}, // read-only (cf. the writable /work mount)
		})
	}
	spec["mounts"] = mounts
}

// applyEnvToOCISpec sets process.env from the provisioned env (ADR 005), in place: PATH replaces the
// bare default and any other entry is exported. It is a no-op when env is empty, leaving the base
// process.env as ["PATH=/usr/bin:/bin"] — byte-for-byte the prior behavior. The order is
// deterministic (PATH first, then sorted keys) so the generated config.json is reproducible.
func applyEnvToOCISpec(spec map[string]any, env map[string]string) {
	if len(env) == 0 {
		return
	}
	if proc, ok := spec["process"].(map[string]any); ok {
		proc["env"] = envList(env)
	}
}

// applyLimitsToOCISpec adds the resource caps to an OCI spec in place: RLIMIT_AS (memory_mb) and
// RLIMIT_NPROC (pids) as process.rlimits, and a size= option on the writable /tmp tmpfs (disk_mb).
// disk_mb degrades (warn + continue) when the writable layer can't be size-capped (ADR 003). Zero
// limits add nothing, so the base spec is byte-for-byte unchanged when no limits are requested.
func applyLimitsToOCISpec(spec map[string]any, lim Limits) []degrade {
	var degrades []degrade
	proc, _ := spec["process"].(map[string]any)
	var rlimits []map[string]any
	if lim.MemoryMB > 0 {
		bytes := uint64(lim.MemoryMB) * 1024 * 1024
		rlimits = append(rlimits, map[string]any{"type": "RLIMIT_AS", "hard": bytes, "soft": bytes})
	}
	if lim.PidsLimit > 0 {
		n := uint64(lim.PidsLimit)
		rlimits = append(rlimits, map[string]any{"type": "RLIMIT_NPROC", "hard": n, "soft": n})
	}
	if len(rlimits) > 0 && proc != nil {
		proc["rlimits"] = rlimits
	}
	if lim.DiskMB > 0 {
		if diskQuotaSupported() {
			applyTmpfsSize(spec, "/tmp", lim.DiskMB)
		} else {
			degrades = append(degrades, degrade{"disk_mb",
				"disk_mb limit not enforced: writable-layer size quota unsupported on this host; running without disk quota"})
		}
	}
	return degrades
}

// applyTmpfsSize appends a size=<diskMB>m option to the tmpfs mount at dst (the writable layer),
// capping how much the payload can write there. No-op if the mount is absent.
func applyTmpfsSize(spec map[string]any, dst string, diskMB int) {
	mounts, _ := spec["mounts"].([]map[string]any)
	for _, m := range mounts {
		if d, _ := m["destination"].(string); d != dst {
			continue
		}
		opts, _ := m["options"].([]string)
		m["options"] = append(opts, fmt.Sprintf("size=%dm", diskMB))
		return
	}
}

// gvisorOCISpec builds the OCI runtime spec (config.json contents) for a payload run. It is a
// pure function of the on-host paths so it is unit-testable without runsc installed.
//
// Invariant enforcement:
//   - linux.namespaces includes a "network" namespace with no Path → the runtime creates a fresh,
//     empty network namespace (loopback only; no host, bridged, or shared networking). This is the
//     OCI equivalent of bwrap --unshare-all for the network dimension. There is no host-network
//     sharing and no bridged/CNI config.
//   - The only egress affordance bind-mounted in is the proxy Unix socket, at /proxy.sock.
//   - The root and host system dirs are read-only; the payload is read-only at /payload.sh.
func gvisorOCISpec(scriptPath, proxySock string) map[string]any {
	mounts := []map[string]any{
		{"destination": "/proc", "type": "proc", "source": "proc"},
		{"destination": "/dev", "type": "tmpfs", "source": "tmpfs",
			"options": []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{"destination": "/tmp", "type": "tmpfs", "source": "tmpfs",
			"options": []string{"nosuid", "nodev"}},
		// The payload, read-only.
		{"destination": "/payload.sh", "type": "bind", "source": scriptPath,
			"options": []string{"ro", "rbind"}},
		// The egress proxy socket — the ONLY path out of the sandbox.
		{"destination": "/proxy.sock", "type": "bind", "source": proxySock,
			"options": []string{"rbind"}},
	}
	// Minimal read-only root: bind the host system dirs in, mirroring the bwrap root.
	for _, d := range []string{"/usr", "/etc", "/bin", "/lib", "/lib64", "/sbin"} {
		if _, err := os.Stat(d); err == nil {
			mounts = append(mounts, map[string]any{
				"destination": d, "type": "bind", "source": d,
				"options": []string{"ro", "rbind"},
			})
		}
	}

	return map[string]any{
		"ociVersion": "1.0.0",
		"process": map[string]any{
			"terminal": false,
			"user":     map[string]any{"uid": 0, "gid": 0},
			"args":     []string{"/usr/bin/sh", "/payload.sh"},
			"env":      []string{"PATH=/usr/bin:/bin"},
			"cwd":      "/",
		},
		"root": map[string]any{
			"path":     "rootfs",
			"readonly": true,
		},
		"hostname": "sandbox",
		"mounts":   mounts,
		"linux": map[string]any{
			// No "path" on the network namespace ⇒ a fresh empty netns (no shared host net).
			"namespaces": []map[string]any{
				{"type": "pid"},
				{"type": "ipc"},
				{"type": "uts"},
				{"type": "mount"},
				{"type": "network"},
			},
		},
	}
}
