package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

	start := time.Now()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bwrapArgv(scriptPath, proxySock)[0], bwrapArgv(scriptPath, proxySock)[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exitCode := 0
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			stderr.WriteString(err.Error())
			exitCode = 127
		}
	}
	durationMs := time.Since(start).Milliseconds()

	emit(req.Wiring.AuditSocket, map[string]any{
		"actor": "exec-sandbox", "action": "exit", "target": sandboxID, "decision": "allow",
		"context": map[string]any{"exit_code": exitCode, "duration_ms": durationMs,
			"request_id": req.Wiring.RequestID},
	})

	return map[string]any{
		"stdout": stdout.String(), "stderr": stderr.String(), "exit_code": exitCode,
		"sandbox_status": map[string]any{
			"sandbox_id": sandboxID, "tier": req.Run.Tier, "duration_ms": durationMs,
			"secrets_injected": secretsInjected, "status": "clean",
		},
	}
}

// bwrapArgv builds the Tier-1 sandbox: --unshare-all removes the network namespace
// entirely; the bind-mounted proxy.sock is the only egress.
func bwrapArgv(scriptPath, proxySock string) []string {
	argv := []string{"bwrap",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc", "/etc",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--ro-bind", scriptPath, "/payload.sh",
		"--bind", proxySock, "/proxy.sock",
		"--unshare-all", "--die-with-parent", "--clearenv",
		"--setenv", "PATH", "/usr/bin:/bin"}
	for _, d := range []string{"/bin", "/lib", "/lib64", "/sbin"} {
		if _, err := os.Stat(d); err == nil {
			argv = append(argv, "--ro-bind", d, d)
		}
	}
	return append(argv, "/usr/bin/sh", "/payload.sh")
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
