package main

import (
	"encoding/json"
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
func (gvisorBackend) Argv(scriptPath, proxySock string) ([]string, func(), error) {
	bundle, err := os.MkdirTemp("", "exec-sandbox-oci-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(bundle) }

	rootfs := filepath.Join(bundle, "rootfs")
	if err := os.MkdirAll(rootfs, 0o700); err != nil {
		cleanup()
		return nil, nil, err
	}

	spec := gvisorOCISpec(scriptPath, proxySock)
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), b, 0o600); err != nil {
		cleanup()
		return nil, nil, err
	}

	// runsc consumes the bundle by directory. --network=none is belt-and-suspenders with the
	// empty network namespace in the spec; both express the no-network invariant. --ignore-cgroups
	// lets the run proceed without cgroup write access (no resource limits are configured in v1).
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
	return argv, cleanup, nil
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
