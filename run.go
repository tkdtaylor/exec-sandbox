package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// RunRequest is the stdin shape: the v0 run() contract under "run", test/deploy wiring
// (sockets, origin map) under "wiring".
type RunRequest struct {
	Run struct {
		Payload    string         `json:"payload"`
		Profile    map[string]any `json:"profile"`
		Tier       string         `json:"tier"`
		SecretRefs []string       `json:"secret_refs"`
	} `json:"run"`
	Wiring struct {
		VaultSocket   string               `json:"vault_socket"`
		AuditSocket   string               `json:"audit_socket"`
		OriginMap     map[string][2]string `json:"origin_map"`
		RequestID     string               `json:"request_id"`
		InjectionMode string               `json:"injection_mode"`
	} `json:"wiring"`
}

// Run executes the payload in a bubblewrap sandbox with no network, routing egress through
// the credential-injecting proxy. exec-sandbox owns the network boundary; vault plugs
// credential injection in via vault.inject (pull-triggered push).
func Run(req RunRequest) map[string]any {
	allowlist := netAllowlist(req.Run.Profile)
	lim := parseLimits(req.Run.Profile)
	sandboxID := "sbx-" + randHex(6)
	sandboxIdentity := map[string]any{"sandbox_id": sandboxID, "attestation": randHex(16)}
	emit(req.Wiring.AuditSocket, map[string]any{
		"actor": "exec-sandbox", "action": "spawn", "target": sandboxID, "decision": "allow",
		"context": map[string]any{"tier": req.Run.Tier, "request_id": req.Wiring.RequestID},
	})

	work, _ := os.MkdirTemp("", "exec-sandbox-")
	defer os.RemoveAll(work)
	proxySock := filepath.Join(work, "proxy.sock")

	proxy := NewEgressProxy(allowlist, req.Wiring.OriginMap)
	secretsInjected := []map[string]any{}

	// pull-triggered push: present {handle, sandbox_identity} to vault.inject at spawn.
	for _, handle := range req.Run.SecretRefs {
		resp, err := vaultInject(req.Wiring.VaultSocket, handle, sandboxIdentity, req.Wiring.InjectionMode)
		if err != nil || resp["error"] != nil {
			emit(req.Wiring.AuditSocket, map[string]any{
				"actor": "exec-sandbox", "action": "inject_failed", "target": sandboxID,
				"decision": "deny", "context": map[string]any{"request_id": req.Wiring.RequestID},
			})
			continue
		}
		if resp["delivery"] == "proxy" {
			b, _ := resp["binding"].(map[string]any)
			host, _ := b["host"].(string)
			proxy.SetCredential(host, Credential{
				Value:  str(resp["credential"]),
				Header: orDefault(str(b["header"]), "Authorization"),
				Scheme: orDefault(str(b["scheme"]), "Bearer"),
			})
			secretsInjected = append(secretsInjected,
				map[string]any{"handle_prefix": prefix(handle, 8), "delivery": "proxy"})
		} else {
			secretsInjected = append(secretsInjected,
				map[string]any{"handle_prefix": prefix(handle, 8), "delivery": "env"})
		}
	}

	if err := proxy.Start(proxySock); err != nil {
		return map[string]any{"error": "proxy start failed: " + err.Error()}
	}
	defer func() { proxy.Stop(); proxy.Wipe() }()

	scriptPath := filepath.Join(work, "payload.sh")
	os.WriteFile(scriptPath, []byte(req.Run.Payload), 0o600)

	// Tier seam: select the isolation backend by req.run.tier. "" and "bubblewrap" both select
	// Tier-1 (bwrap, unchanged); "gvisor" selects the runsc Tier-2 backend; any other tier is a
	// hard error (no silent fall-back). Every backend enforces the same no-network +
	// proxy-only-egress invariant.
	backend, err := backendFor(req.Run.Tier)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	argv, cleanup, degrades, err := backend.Argv(scriptPath, proxySock, lim)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Secondary caps (cpu_count/disk_mb) that the host can't enforce degrade loudly: a stderr
	// WARNING names the control and it is recorded in sandbox_status.limits.degraded (ADR 003).
	degraded := []string{}
	for _, d := range degrades {
		fmt.Fprintf(os.Stderr, "exec-sandbox: WARNING: %s\n", d.reason)
		degraded = append(degraded, d.cap)
	}

	// timeout_sec is enforced host-side: the child runs in its own process group (Setpgid) and the
	// whole group is SIGKILLed when the wall-clock deadline fires, so no descendant outlives it.
	ctx := context.Background()
	cancel := context.CancelFunc(func() {})
	if lim.Timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, lim.Timeout)
	}
	defer cancel()

	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	exitCode := 0
	status := "clean"
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			stderr.WriteString(err.Error())
			exitCode = 127
		}
	}
	durationMs := time.Since(start).Milliseconds()
	if lim.Timeout > 0 && ctx.Err() == context.DeadlineExceeded {
		// The payload was killed by the wall-clock deadline, not by its own exit.
		status = "timeout"
		exitCode = 137 // 128 + SIGKILL, the conventional signal-kill exit code
	}

	emit(req.Wiring.AuditSocket, map[string]any{
		"actor": "exec-sandbox", "action": "exit", "target": sandboxID, "decision": "allow",
		"context": map[string]any{"exit_code": exitCode, "duration_ms": durationMs,
			"status": status, "request_id": req.Wiring.RequestID},
	})

	return map[string]any{
		"stdout": stdout.String(), "stderr": stderr.String(), "exit_code": exitCode,
		"sandbox_status": map[string]any{
			"sandbox_id": sandboxID, "tier": req.Run.Tier, "duration_ms": durationMs,
			"secrets_injected": secretsInjected, "status": status,
			"limits": limitsReport(lim, degraded),
		},
	}
}

// limitsReport is the additive sandbox_status.limits record: the caps that were requested plus the
// list of any that degraded (could not be enforced on this host). It lets a consumer and the audit
// trail see exactly which caps were applied (ADR 003). Zero values mean "no limit requested".
func limitsReport(lim Limits, degraded []string) map[string]any {
	return map[string]any{
		"cpu_count":   lim.CPUCount,
		"memory_mb":   lim.MemoryMB,
		"pids":        lim.PidsLimit,
		"disk_mb":     lim.DiskMB,
		"timeout_sec": int(lim.Timeout / time.Second),
		"degraded":    degraded,
	}
}

// Backend is an isolation substrate selected by the tier seam. Given the on-host payload script
// and proxy socket, it returns the os/exec argv to spawn (argv[0] is the program), an optional
// cleanup func (run after the process exits — nil if nothing to clean up), and an error if the
// backend could not prepare its run. Every backend must enforce the no-network +
// proxy-only-egress invariant.
// Argv builds the spawn argv and applies profile.limits for this backend (ADR 003): memory_mb and
// pids as in-sandbox rlimits, disk_mb as a writable-layer (tmpfs) size cap, cpu_count as a taskset
// affinity prefix on the argv. timeout_sec is enforced backend-agnostically in Run(), not here. Any
// secondary cap that the host can't enforce is returned in degrades (warn + continue), never a
// hard error; an inability to enforce a load-bearing cap is returned as err.
type Backend interface {
	Argv(scriptPath, proxySock string, lim Limits) (argv []string, cleanup func(), degrades []degrade, err error)
}

// backendFor selects the isolation backend for a tier. "" and "bubblewrap" select Tier-1
// (bwrap); "gvisor" selects the runsc Tier-2 backend. Any other tier is a hard error — there is
// no silent fall-back to bubblewrap.
func backendFor(tier string) (Backend, error) {
	switch tier {
	case "", "bubblewrap":
		return bubblewrapBackend{}, nil
	case "gvisor":
		return gvisorBackend{}, nil
	default:
		return nil, errors.New("tier not implemented: " + tier)
	}
}

// bubblewrapBackend is the Tier-1 isolation substrate.
type bubblewrapBackend struct{}

func (bubblewrapBackend) Argv(scriptPath, proxySock string, lim Limits) ([]string, func(), []degrade, error) {
	var degrades []degrade

	// disk_mb → tmpfs --size on /tmp (the only writable layer). Reliably size-cappable on tmpfs;
	// degrades (warn + continue) if a host reports the writable layer can't be sized (ADR 003).
	diskBytes := 0
	if lim.DiskMB > 0 {
		if diskQuotaSupported() {
			diskBytes = lim.DiskMB * 1024 * 1024
		} else {
			degrades = append(degrades, degrade{"disk_mb",
				"disk_mb limit not enforced: writable-layer size quota unsupported on this host; running without disk quota"})
		}
	}

	// memory_mb/pids → in-sandbox prlimit (per-sandbox via the bwrap user namespace).
	inner, err := prlimitWrap(lim, []string{"/usr/bin/sh", "/payload.sh"})
	if err != nil {
		return nil, nil, nil, err
	}

	argv := bwrapArgv(scriptPath, proxySock, diskBytes, inner)

	// cpu_count → taskset affinity prefix on the whole argv (inherited into the sandbox).
	if prefix, d := cpuAffinityPrefix(lim.CPUCount); d != nil {
		degrades = append(degrades, *d)
	} else if prefix != nil {
		argv = append(prefix, argv...)
	}
	return argv, nil, degrades, nil
}

// bwrapArgv builds the Tier-1 sandbox: --unshare-all removes the network namespace entirely; the
// bind-mounted proxy.sock is the only egress. diskBytes > 0 size-caps the writable /tmp tmpfs;
// finalCmd is the in-sandbox command (the payload shell, optionally wrapped by prlimit for the
// memory/pids rlimits — see ADR 003).
func bwrapArgv(scriptPath, proxySock string, diskBytes int, finalCmd []string) []string {
	argv := []string{"bwrap",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc", "/etc",
		"--proc", "/proc", "--dev", "/dev"}
	// --size sets the size of the NEXT --tmpfs, so it must immediately precede the /tmp mount.
	if diskBytes > 0 {
		argv = append(argv, "--size", strconv.Itoa(diskBytes), "--tmpfs", "/tmp")
	} else {
		argv = append(argv, "--tmpfs", "/tmp")
	}
	argv = append(argv,
		"--ro-bind", scriptPath, "/payload.sh",
		"--bind", proxySock, "/proxy.sock",
		"--unshare-all", "--die-with-parent", "--clearenv",
		"--setenv", "PATH", "/usr/bin:/bin")
	for _, d := range []string{"/bin", "/lib", "/lib64", "/sbin"} {
		if _, err := os.Stat(d); err == nil {
			argv = append(argv, "--ro-bind", d, d)
		}
	}
	return append(argv, finalCmd...)
}

func vaultInject(socket, handle string, sandboxIdentity map[string]any, mode string) (map[string]any, error) {
	req := map[string]any{"op": "inject", "handle": handle,
		"sandbox_identity": sandboxIdentity, "mode": mode}
	return ipcCall(socket, req)
}

func emit(socket string, event map[string]any) {
	if socket == "" {
		return
	}
	_, _ = ipcCall(socket, map[string]any{"op": "emit", "event": event})
}

func ipcCall(socket string, req map[string]any) (map[string]any, error) {
	if socket == "" {
		return map[string]any{}, nil
	}
	conn, err := net.DialTimeout("unix", socket, 10*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	b, _ := json.Marshal(req)
	conn.Write(append(b, '\n'))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func netAllowlist(profile map[string]any) []string {
	var out []string
	caps, _ := profile["capabilities"].([]any)
	for _, c := range caps {
		cm, _ := c.(map[string]any)
		if cm["type"] != "NetConnect" {
			continue
		}
		for _, a := range toStringList(cm["allowlist"]) {
			out = append(out, stripPort(a))
		}
	}
	return out
}

func toStringList(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func prefix(s string, n int) string {
	if len(s) < n {
		return s
	}
	return s[:n]
}
