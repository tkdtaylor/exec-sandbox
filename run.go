// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bufio"
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
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RunRequest is the stdin shape: the v0 run() contract under "run", test/deploy wiring
// (sockets, origin map) under "wiring".
type RunRequest struct {
	Run struct {
		Payload    string            `json:"payload"`
		Profile    map[string]any    `json:"profile"`
		Tier       string            `json:"tier"`
		SecretRefs []string          `json:"secret_refs"`
		Workdir    string            `json:"workdir"` // host dir bind-mounted writable at /work; "" → no mount (ADR 004)
		Env        map[string]string `json:"env"`     // env exported into the sandbox; PATH replaces the bare default; empty → unchanged (ADR 005)
	} `json:"run"`
	Wiring struct {
		VaultSocket   string               `json:"vault_socket"`
		AuditSocket   string               `json:"audit_socket"`
		OriginMap     map[string][2]string `json:"origin_map"`
		RequestID     string               `json:"request_id"`
		InjectionMode string               `json:"injection_mode"`
	} `json:"wiring"`
}

// spawnArgvFn is a test seam: when non-nil, Run() calls it with the exact spawn argv just before
// exec, so a test can assert an env-mode credential value never appears on it (the /proc/<pid>/cmdline
// leak surface — TC-012-03). It is nil in production. Override must be restored via t.Cleanup; not
// goroutine-safe, so tests that set it must not run in parallel.
var spawnArgvFn func([]string)

// Run executes the payload in a bubblewrap sandbox with no network, routing egress through
// the credential-injecting proxy. exec-sandbox owns the network boundary; vault plugs
// credential injection in via vault.inject (pull-triggered push).
func Run(req RunRequest) map[string]any {
	allowlist := netAllowlist(req.Run.Profile)
	verbAllowlist := netVerbAllowlist(req.Run.Profile)
	lim := parseLimits(req.Run.Profile)

	// Resolve the optional writable working directory before any side effect (proxy/vault): a
	// malformed run.workdir fails loud here, never silently falls back to a no-mount run (ADR 004).
	workdir, err := validateWorkdir(req.Run.Workdir)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	// Resolve the optional read-only FileRead{paths} mounts before any side effect: each path must
	// be absolute and exist, else a hard {error} (no run, no silent skip) — same ordering as
	// validateWorkdir so a malformed FileRead cannot trigger proxy/vault (ADR 005).
	fileReads := fileReadPaths(req.Run.Profile)
	if err := validateFileReads(fileReads); err != nil {
		return map[string]any{"error": err.Error()}
	}

	sandboxID := "sbx-" + randHex(6)
	// Signed self-attestation (ADR 014): mint a fresh ephemeral ed25519 keypair, sign the canonical
	// preimage of {sandbox_id, nonce, ts}, and carry the public key + signature in sandbox_identity.
	// The signing PRIVATE key never leaves mintAttestation — it enters none of the result, audit
	// events, sandbox env/args, payload, or stdout (mirrors the F-002 credential discipline).
	sandboxIdentity, err := mintAttestation(sandboxID)
	if err != nil {
		return map[string]any{"error": "attestation mint failed: " + err.Error()}
	}
	emit(req.Wiring.AuditSocket, map[string]any{
		"actor": "exec-sandbox", "action": "spawn", "target": sandboxID, "decision": "allow",
		"context": map[string]any{"tier": req.Run.Tier, "request_id": req.Wiring.RequestID},
	})

	// Build the pristine per-run baseline (ADR 009): a fresh work dir with payload.sh seeded into
	// it, the fresh per-run proxy socket path, and a fresh proxy with an empty credential map. This
	// is the snapshot — the named baseline a leak-proof reset is asserted against. The default
	// one-shot path is snapshot → run → teardown, observationally identical to today's inlined
	// MkdirTemp → write payload.sh → NewEgressProxy → … → RemoveAll + Wipe.
	proxy := NewEgressProxy(allowlist, verbAllowlist, req.Wiring.OriginMap)
	baseline, err := snapshotBaseline(req.Run.Payload, proxy)
	if err != nil {
		return map[string]any{"error": "baseline prepare failed: " + err.Error()}
	}
	defer baseline.teardown() // one-shot terminal cleanup: RemoveAll(work) + proxy.Wipe()
	proxySock := baseline.proxySock
	secretsInjected := []map[string]any{}

	// env-mode credential holder (ADR 015): the single host-side place an env-mode credential value
	// lives. Wiped post-spawn and again at teardown so no host copy survives the run. Distinct from
	// proxy-mode creds (which never enter the sandbox at all — F-002).
	envCreds := NewEnvCredentials()
	defer envCreds.Wipe()

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
			// env-mode (ADR 015): deliberately deliver the credential value into the sandbox env under
			// the vault-specified var_name. The value is held ONLY in envCreds (the single wipe point);
			// it reaches the sandbox off the argv (bwrap --args FD / OCI process.env) and is wiped
			// post-spawn. A response missing var_name/credential is a malformed env delivery: skip it as
			// an inject failure rather than deliver an empty/unnamed var.
			varName := str(resp["var_name"])
			if varName == "" {
				emit(req.Wiring.AuditSocket, map[string]any{
					"actor": "exec-sandbox", "action": "inject_failed", "target": sandboxID,
					"decision": "deny", "context": map[string]any{"request_id": req.Wiring.RequestID},
				})
				continue
			}
			envCreds.Set(varName, str(resp["credential"]))
			secretsInjected = append(secretsInjected,
				map[string]any{"handle_prefix": prefix(handle, 8), "delivery": "env"})
		}
	}

	if err := proxy.Start(proxySock); err != nil {
		// A run that emitted spawn MUST emit a matching terminal event (ADR 013).
		// action:"exit", decision:"deny" + status:"proxy_start_failed" distinguishes this
		// from a clean exit. No credential, no handle, no attestation in the event.
		emit(req.Wiring.AuditSocket, map[string]any{
			"actor": "exec-sandbox", "action": "exit", "target": sandboxID, "decision": "deny",
			"context": map[string]any{
				"status":     "proxy_start_failed",
				"error":      err.Error(),
				"request_id": req.Wiring.RequestID,
			},
		})
		return map[string]any{"error": "proxy start failed: " + err.Error()}
	}
	defer func() { proxy.Stop(); proxy.Wipe() }()

	// payload.sh was seeded into the writable surface by snapshotBaseline (mode 0600); the baseline
	// owns the pristine file set so restore can re-seed exactly it.
	scriptPath := baseline.scriptPath()

	// Tier seam: select the isolation backend by req.run.tier. "" and "bubblewrap" both select
	// Tier-1 (bwrap, unchanged); "gvisor" selects the runsc Tier-2 backend; any other tier is a
	// hard error (no silent fall-back). Every backend enforces the same no-network +
	// proxy-only-egress invariant.
	backend, err := backendFor(req.Run.Tier)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	argv, cleanup, degrades, extraFiles, err := backend.Argv(scriptPath, proxySock, workdir, fileReads, req.Run.Env, envCreds.pairs(), lim)
	if cleanup != nil {
		defer cleanup()
	}
	// The env-mode --args pipe read end (if any) is closed once cmd has consumed it; close on every
	// exit path so the FD does not leak.
	defer func() {
		for _, f := range extraFiles {
			_ = f.Close()
		}
	}()
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
	// Host-side output cap (ADR 007): each stream is captured through a capWriter that retains at
	// most lim.MaxOutputBytes bytes and drops the overflow, without erroring the child's pipe.
	// lim.MaxOutputBytes <= 0 ⇒ unbounded (prior behavior). This sits ABOVE the tier seam — the
	// same cap applies identically under bubblewrap and gVisor; the backend argv/OCI spec are
	// unchanged by it. stdout and stderr are capped independently at the same ceiling.
	stdout := newCapWriter(lim.MaxOutputBytes)
	stderr := newCapWriter(lim.MaxOutputBytes)
	// Test seam: capture the EXACT spawn argv so a test can assert an env-mode credential value never
	// lands on it (TC-012-03 — /proc/<pid>/cmdline absence). nil in production (no overhead).
	if spawnArgvFn != nil {
		spawnArgvFn(argv)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// env-mode delivery (ADR 015): the backend's extraFiles (the bwrap --args pipe read end) become
	// the child's fd 3.. — the env-mode credential value travels through this pipe, never on the argv,
	// so it cannot leak via /proc/<pid>/cmdline.
	cmd.ExtraFiles = extraFiles
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
	runErr := cmd.Run()
	// Wipe clock (ADR 015): the child has been spawned with the env-mode credential (or failed to);
	// the host retains no copy past spawn. Wiping here — not only at the deferred teardown — closes
	// the window as early as possible, mirroring the proxy Wipe() discipline.
	envCreds.Wipe()
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			stderr.Write([]byte(runErr.Error()))
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
			"limits": limitsReport(lim, degraded, outputTruncated(stdout, stderr)),
		},
	}
}

// outputTruncated builds the deterministic-order list of streams whose host-side output cap dropped
// bytes (ADR 007): [] when neither overflowed, ["stdout"] for stdout only, ["stdout","stderr"] when
// both — stdout always first. Mirrors the degraded array's deterministic ordering in limitsReport.
func outputTruncated(stdout, stderr *capWriter) []string {
	truncated := []string{}
	if stdout.overflowed {
		truncated = append(truncated, "stdout")
	}
	if stderr.overflowed {
		truncated = append(truncated, "stderr")
	}
	return truncated
}

// limitsReport is the additive sandbox_status.limits record: the caps that were requested, the list
// of any that degraded (could not be enforced on this host), and the list of streams whose output
// cap dropped bytes. It lets a consumer and the audit trail see exactly which caps were applied
// (ADR 003) and whether captured output was truncated (ADR 007). Zero values mean "no limit
// requested"; an empty output_truncated means no stream was capped.
func limitsReport(lim Limits, degraded, outputTruncated []string) map[string]any {
	return map[string]any{
		"cpu_count":        lim.CPUCount,
		"memory_mb":        lim.MemoryMB,
		"pids":             lim.PidsLimit,
		"disk_mb":          lim.DiskMB,
		"timeout_sec":      int(lim.Timeout / time.Second),
		"max_output_bytes": lim.MaxOutputBytes,
		"degraded":         degraded,
		"output_truncated": outputTruncated,
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
// fileReads are validated absolute host paths bind-mounted READ-ONLY at the same path inside the
// sandbox (ADR 005); env is exported into the sandbox (PATH replaces the bare default). Both are
// empty/nil when absent, leaving prior behavior unchanged.
// envCreds are the env-mode credential {var_name, value} pairs from vault.inject (ADR 015): they are
// delivered into the sandbox environment OFF the spawn argv (bwrap via --args FD; gVisor via the OCI
// process.env file) so the value never lands in /proc/<pid>/cmdline. nil/empty when no env-mode
// handle was injected. extraFiles are passed up to cmd.ExtraFiles (the bwrap --args pipe read end);
// nil when the backend needs none.
type Backend interface {
	Argv(scriptPath, proxySock, workdir string, fileReads []string, env map[string]string, envCreds [][2]string, lim Limits) (argv []string, cleanup func(), degrades []degrade, extraFiles []*os.File, err error)
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

func (bubblewrapBackend) Argv(scriptPath, proxySock, workdir string, fileReads []string, env map[string]string, envCreds [][2]string, lim Limits) ([]string, func(), []degrade, []*os.File, error) {
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
		return nil, nil, nil, nil, err
	}

	// env-mode credentials (ADR 015): when present, the --clearenv + ALL --setenv directives move into
	// a pipe consumed via bwrap --args FD, so the credential VALUE never lands on the literal spawn
	// argv (/proc/<pid>/cmdline). The pipe read end is returned as an extraFile → child fd 3. When no
	// env-mode credential was injected, env stays inline on the argv exactly as before (no pipe).
	var extraFiles []*os.File
	envCredsFD := -1
	if len(envCreds) > 0 {
		pr, err := bwrapEnvArgsPipe(env, envCreds)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		extraFiles = []*os.File{pr}
		envCredsFD = 3 // ExtraFiles[0] is fd 3 in the child
	}

	argv := bwrapArgv(scriptPath, proxySock, workdir, fileReads, env, diskBytes, inner, envCredsFD)

	// cpu_count → taskset affinity prefix on the whole argv (inherited into the sandbox).
	if prefix, d := cpuAffinityPrefix(lim.CPUCount); d != nil {
		degrades = append(degrades, *d)
	} else if prefix != nil {
		argv = append(prefix, argv...)
	}
	return argv, nil, degrades, extraFiles, nil
}

// bwrapEnvArgsPipe builds the read end of a pipe carrying the NUL-separated bwrap args that set up the
// sandbox environment OFF the literal argv (ADR 015): --clearenv, the regular env --setenv pairs, and
// the env-mode credential --setenv pairs. bwrap consumes these via --args FD, so neither the env-mode
// credential value nor any other env value appears in /proc/<pid>/cmdline. A goroutine writes the
// payload and closes the write end; the returned read end is the caller's to close after spawn.
func bwrapEnvArgsPipe(env map[string]string, envCreds [][2]string) (*os.File, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	args := []string{"--clearenv"}
	for _, kv := range envSetenvPairs(env) {
		args = append(args, "--setenv", kv[0], kv[1])
	}
	for _, kv := range envCreds {
		args = append(args, "--setenv", kv[0], kv[1])
	}
	payload := strings.Join(args, "\x00") + "\x00"
	go func() {
		_, _ = pw.WriteString(payload)
		_ = pw.Close()
	}()
	return pr, nil
}

// bwrapArgv builds the Tier-1 sandbox: --unshare-all removes the network namespace entirely; the
// bind-mounted proxy.sock is the only egress. diskBytes > 0 size-caps the writable /tmp tmpfs;
// finalCmd is the in-sandbox command (the payload shell, optionally wrapped by prlimit for the
// memory/pids rlimits — see ADR 003). When workdir is non-empty it is bind-mounted READ-WRITE at
// /work (the one writable host surface) and becomes the payload's cwd; system dirs stay read-only
// and the network stays unshared (ADR 004). Each fileReads path is bind-mounted READ-ONLY
// (--ro-bind, NOT --bind) at the same path; env is exported via --setenv (PATH replaces the bare
// default) — adding read-only host paths and PATH entries opens no egress and no writable surface
// (ADR 005).
// envCredsFD: when >= 0, an env-mode credential is being delivered (ADR 015) and the --clearenv + ALL
// --setenv directives (regular env AND credential) are read by bwrap from that file descriptor via
// --args, keeping every env VALUE off the literal argv (/proc/<pid>/cmdline). When < 0 (the common
// case — no env-mode handle) the env is set inline via --clearenv + --setenv exactly as before.
func bwrapArgv(scriptPath, proxySock, workdir string, fileReads []string, env map[string]string, diskBytes int, finalCmd []string, envCredsFD int) []string {
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
		"--unshare-all", "--die-with-parent")
	if envCredsFD >= 0 {
		// Off-argv env (ADR 015): --clearenv + every --setenv (env + credential) come from the FD, so
		// no env value — least of all the credential — appears in /proc/<pid>/cmdline.
		argv = append(argv, "--args", strconv.Itoa(envCredsFD))
	} else {
		// Inline env (the common, no-credential path), byte-for-byte the prior behavior: --clearenv then
		// PATH (replaces the bare default) and any other entry, in deterministic sorted-key order.
		argv = append(argv, "--clearenv")
		for _, kv := range envSetenvPairs(env) {
			argv = append(argv, "--setenv", kv[0], kv[1])
		}
	}
	for _, d := range []string{"/bin", "/lib", "/lib64", "/sbin"} {
		if _, err := os.Stat(d); err == nil {
			argv = append(argv, "--ro-bind", d, d)
		}
	}
	// FileRead: each caller-specified host path bind-mounted READ-ONLY at the same path (--ro-bind,
	// not the writable --bind). Read-only is load-bearing; this opens no writable surface.
	for _, p := range fileReads {
		argv = append(argv, "--ro-bind", p, p)
	}
	// Writable working directory: --bind (NOT --ro-bind) makes /work read-write, --chdir sets it as
	// the payload's cwd. This is the only writable host mount; the no-network invariant is untouched.
	if workdir != "" {
		argv = append(argv, "--bind", workdir, "/work", "--chdir", "/work")
	}
	return append(argv, finalCmd...)
}

// envSetenvPairs renders the provisioned env (ADR 005) into ordered [key, value] pairs for the
// sandbox: PATH first (the env value if set, else the bare default /usr/bin:/bin), then every other
// entry in sorted-key order so the output is deterministic. An empty/nil env yields just the bare
// PATH pair — byte-for-byte the prior behavior.
func envSetenvPairs(env map[string]string) [][2]string {
	path := "/usr/bin:/bin"
	if p, ok := env["PATH"]; ok {
		path = p
	}
	pairs := [][2]string{{"PATH", path}}
	keys := make([]string, 0, len(env))
	for k := range env {
		if k == "PATH" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pairs = append(pairs, [2]string{k, env[k]})
	}
	return pairs
}

// envList renders the provisioned env as OCI process.env "k=v" strings, in the same deterministic
// order as envSetenvPairs (PATH first, then sorted keys). Empty env ⇒ ["PATH=/usr/bin:/bin"].
func envList(env map[string]string) []string {
	pairs := envSetenvPairs(env)
	out := make([]string, 0, len(pairs))
	for _, kv := range pairs {
		out = append(out, kv[0]+"="+kv[1])
	}
	return out
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

// netVerbAllowlist parses the optional per-host HTTP-verb constraint carried by NetConnect
// capabilities (ADR 008). It returns a host -> allowed-method-set map IN ADDITION to the bare-host
// allowlist netAllowlist produces. Semantics:
//   - A NetConnect entry's optional "methods" array applies to EVERY host in that entry's allowlist.
//   - A host with NO methods constraint (today's only shape) is UNCONSTRAINED — it gets no entry in
//     the map, which the proxy reads as "all verbs allowed" (backward compatible).
//   - An explicitly EMPTY "methods": [] is ALSO unconstrained, NOT deny-all — deny is the absence of
//     a verb from a NON-EMPTY set, never an empty set. Such a host gets no entry either.
//   - Methods are normalized to canonical UPPER-CASE so matching is case-insensitive (ADR 008 §3).
//   - When the same host appears across multiple NetConnect entries, their method sets UNION (the
//     same union-across-entries rule fileReadPaths uses).
//
// The returned map is nil when no host carries a verb constraint (every host unconstrained).
func netVerbAllowlist(profile map[string]any) map[string]map[string]bool {
	var out map[string]map[string]bool
	caps, _ := profile["capabilities"].([]any)
	for _, c := range caps {
		cm, _ := c.(map[string]any)
		if cm["type"] != "NetConnect" {
			continue
		}
		methods := toStringList(cm["methods"])
		if len(methods) == 0 {
			continue // no/empty methods ⇒ unconstrained (all verbs); no map entry
		}
		for _, a := range toStringList(cm["allowlist"]) {
			host := stripPort(a)
			if out == nil {
				out = map[string]map[string]bool{}
			}
			if out[host] == nil {
				out[host] = map[string]bool{}
			}
			for _, m := range methods {
				out[host][strings.ToUpper(m)] = true
			}
		}
	}
	return out
}

// fileReadPaths collects the host paths from every FileRead capability in profile.capabilities
// (mirroring netAllowlist's NetConnect scan): {"type":"FileRead","paths":[…]}. Multiple FileRead
// entries union their path lists; non-FileRead entries are ignored; a missing/empty paths
// contributes nothing. Order within a single entry is preserved (ADR 005).
func fileReadPaths(profile map[string]any) []string {
	var out []string
	caps, _ := profile["capabilities"].([]any)
	for _, c := range caps {
		cm, _ := c.(map[string]any)
		if cm["type"] != "FileRead" {
			continue
		}
		out = append(out, toStringList(cm["paths"])...)
	}
	return out
}

// validateFileReads checks each FileRead path before any side effect: it must be ABSOLUTE and
// EXIST on the host. A relative or nonexistent path is a hard error (the run does not start) — the
// no-silent-fall-back stance ADR 001/003/004/005 commit to. Unlike validateWorkdir, FileRead does
// NOT canonicalize a relative path: a relative FileRead path is rejected outright (the caller must
// supply already-absolute toolchain paths). An empty list validates as "no mounts" (ADR 005).
func validateFileReads(paths []string) error {
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("invalid FileRead path: not absolute: %q", p)
		}
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("invalid FileRead path: %v", err)
		}
	}
	return nil
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

// validateWorkdir resolves the optional host working directory bind-mounted writable at /work
// (ADR 004). A blank path means "no workdir mount" — it returns ("", nil), preserving today's
// behavior. A non-blank path is canonicalized to absolute (filepath.Abs) and must be an EXISTING
// directory; a missing path or a non-directory is a hard error (the run does not start). This is
// the no-silent-fall-back stance and mirrors agent-builder's validateWorktree (trim → abs → stat
// → IsDir).
func validateWorkdir(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("invalid run.workdir: %v", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("invalid run.workdir: %v", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("invalid run.workdir: not a directory: %s", abs)
	}
	return abs, nil
}
