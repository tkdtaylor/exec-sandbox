// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// fc-launch is the in-bwrap launcher for the Firecracker Tier-3 backend. firecrackerBackend.Argv
// returns `bwrap [--unshare-all + binds + limits] -- exec-sandbox fc-launch <bundle>`; this is the
// `fc-launch <bundle>` half, running INSIDE the bwrap sandbox (no jailer — ADR 010 A1.Q3). It:
//
//   1. spawns the `firecracker` binary with a per-run API socket,
//   2. drives the REST-over-Unix-socket API IN ORDER — machine-config -> boot-source -> drives ->
//      vsock -> actions{InstanceStart} (NEVER /network-interfaces — no NIC at the API level, D2),
//   3. streams the guest serial console (firecracker's stdout) to its OWN stdout, stripping kernel
//      log lines and extracting the `__EXEC_SANDBOX_EXIT__ N` sentinel the guest init emits,
//   4. exits with the guest's exit code.
//
// Because fc-launch's process exit code IS the guest's exit code, Run()'s host-side capture block
// (exec.CommandContext + capWriter + process-group SIGKILL on the deadline + exit-code mapping) maps
// it unchanged: clean exit -> guest code; non-zero -> guest code; timeout -> the whole process group
// (fc-launch + firecracker) is SIGKILLed and Run() maps it to 137. The vsock egress bridge runs
// host-side (started by Argv, outside bwrap); fc-launch only drives the VMM.
//
// The exit-code sentinel and console framing live here, NOT in Run(), so the firecracker child is a
// well-behaved member of the unchanged host capture path.

const (
	fcExitSentinel = "__EXEC_SANDBOX_EXIT__"
	// hostVsockPort must match the guest shim's proxyPort: the guest dials (CID 2, hostVsockPort);
	// firecracker forwards that to <vsock_uds>_<hostVsockPort> on the host, where the bridge listens.
	hostVsockPort = 1024
)

// fcLaunchMain is the entry point for `exec-sandbox fc-launch <bundle>`. It returns the process exit
// code (the guest's exit code, or 127 on a spawn/launch failure — never a silent success).
func fcLaunchMain(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "fc-launch: usage: fc-launch <bundle-dir>")
		return 127
	}
	bundle := args[0]
	cfgPath := filepath.Join(bundle, "vm-config.json")
	cfg, err := readVMConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fc-launch: %v\n", err)
		return 127
	}
	apiSock := filepath.Join(bundle, "fc-api.sock")
	_ = os.Remove(apiSock)

	// Spawn firecracker with the API socket. Its stdout/stderr (the guest serial console) are piped
	// so we can extract the guest stdout + exit sentinel. The firecracker binary is named by
	// FC_BINARY (an absolute path the backend bound into the sandbox) or resolved on PATH; its
	// absence is a spawn error (exit 127), never a fall-back.
	fcBin := os.Getenv("FC_BINARY")
	if fcBin == "" {
		fcBin = "firecracker"
	}
	fc := exec.Command(fcBin, "--api-sock", apiSock)
	consoleR, consoleW, err := os.Pipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fc-launch: pipe: %v\n", err)
		return 127
	}
	fc.Stdout = consoleW
	fc.Stderr = consoleW
	// Own process group so we can reap firecracker if the API drive fails midway.
	fc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := fc.Start(); err != nil {
		consoleW.Close()
		consoleR.Close()
		fmt.Fprintf(os.Stderr, "fc-launch: cannot spawn firecracker: %v\n", err)
		return 127
	}
	consoleW.Close() // our copy; firecracker holds the write end

	// Parse the console in a goroutine: forward guest stdout to our stdout, capture the exit code.
	exitCh := make(chan int, 1)
	go func() { exitCh <- streamConsole(consoleR, os.Stdout) }()

	// Drive the REST API in order. A failure here means the guest never started — reap firecracker
	// and surface 127.
	if err := driveFirecrackerAPI(apiSock, cfg, 10*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "fc-launch: REST drive failed: %v\n", err)
		_ = syscall.Kill(-fc.Process.Pid, syscall.SIGKILL)
		fc.Wait()
		consoleR.Close()
		return 127
	}

	// Wait for firecracker to exit (the guest powers off via reboot=k after the payload finishes).
	fcErr := fc.Wait()
	consoleR.Close()
	guestExit := <-exitCh

	if guestExit >= 0 {
		return guestExit // the sentinel was seen — authoritative guest exit code
	}
	// No sentinel: the guest crashed/panicked before emitting it. Surface firecracker's own failure
	// as a spawn error rather than a fake success.
	if fcErr != nil {
		fmt.Fprintf(os.Stderr, "fc-launch: guest produced no exit sentinel; firecracker: %v\n", fcErr)
	} else {
		fmt.Fprintln(os.Stderr, "fc-launch: guest produced no exit sentinel")
	}
	return 127
}

// streamConsole reads the firecracker serial console line-by-line, writes guest payload output to
// out (stripping kernel `[ ... ]` log lines and firecracker's own JSON log lines), and returns the
// guest exit code parsed from the `__EXEC_SANDBOX_EXIT__ N` sentinel — or -1 if the sentinel never
// appeared. Only lines before the sentinel are guest stdout; the sentinel and the firecracker
// shutdown chatter that follows are dropped.
func streamConsole(r io.Reader, out io.Writer) int {
	exit := -1
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, fcExitSentinel) {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if n, err := strconv.Atoi(fields[1]); err == nil {
					exit = n
				}
			}
			// Stop forwarding: everything after the sentinel is shutdown noise, not guest stdout.
			break
		}
		if isConsoleNoise(line) {
			continue
		}
		fmt.Fprintln(out, line)
	}
	return exit
}

// isConsoleNoise reports whether a console line is kernel/firecracker log chatter rather than guest
// payload stdout. Kernel log lines start with a `[   t.tttttt]` timestamp; firecracker's structured
// log lines are timestamped ISO-8601 and contain `[anonymous-instance`.
func isConsoleNoise(line string) bool {
	t := strings.TrimLeft(line, " ")
	if strings.HasPrefix(t, "[") {
		// Kernel ring-buffer line: `[    0.123456] ...`.
		if i := strings.IndexByte(t, ']'); i > 0 {
			inner := strings.TrimSpace(t[1:i])
			if _, err := strconv.ParseFloat(inner, 64); err == nil {
				return true
			}
		}
	}
	if strings.Contains(line, "[anonymous-instance") {
		return true // firecracker's own structured log
	}
	return false
}

// vmConfig is the decoded vm-config.json the backend wrote: the four REST bodies plus the api flow.
// It is decoded (not passed as raw JSON) so driveFirecrackerAPI can PUT each section as its own
// request in the firecracker-required order, and so the no-network-interfaces property is structural
// (there is simply no field for it).
type vmConfig struct {
	MachineConfig json.RawMessage   `json:"machine-config"`
	BootSource    json.RawMessage   `json:"boot-source"`
	Drives        []json.RawMessage `json:"drives"`
	Vsock         json.RawMessage   `json:"vsock"`
}

func readVMConfig(path string) (*vmConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read vm-config %s: %w", path, err)
	}
	var cfg vmConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse vm-config: %w", err)
	}
	return &cfg, nil
}

// fcRESTStep is one PUT in the firecracker boot sequence: the API path and the JSON body.
type fcRESTStep struct {
	path string
	body []byte
}

// firecrackerBootSequence returns the ORDERED list of REST PUTs that boot the microVM:
// machine-config -> boot-source -> drives... -> vsock -> actions{InstanceStart}. It NEVER includes
// /network-interfaces (no NIC at the API level — ADR 010 D2). This is a pure function of the decoded
// config, so TC-015-03 can assert the order (and the no-NIC property) against it without /dev/kvm.
func firecrackerBootSequence(cfg *vmConfig) []fcRESTStep {
	steps := []fcRESTStep{
		{"/machine-config", cfg.MachineConfig},
		{"/boot-source", cfg.BootSource},
	}
	for _, d := range cfg.Drives {
		id := driveID(d)
		steps = append(steps, fcRESTStep{"/drives/" + id, d})
	}
	if len(cfg.Vsock) > 0 {
		steps = append(steps, fcRESTStep{"/vsock", cfg.Vsock})
	}
	steps = append(steps, fcRESTStep{"/actions", []byte(`{"action_type":"InstanceStart"}`)})
	return steps
}

// driveID extracts the drive_id from a drive body for the /drives/<id> path; defaults to "rootfs".
func driveID(d json.RawMessage) string {
	var m struct {
		DriveID string `json:"drive_id"`
	}
	if json.Unmarshal(d, &m) == nil && m.DriveID != "" {
		return m.DriveID
	}
	return "rootfs"
}

// driveFirecrackerAPI dials the firecracker API Unix socket and issues firecrackerBootSequence's
// PUTs in order. It waits for the socket to appear (firecracker creates it asynchronously after
// spawn) up to timeout. Each non-2xx response aborts the sequence with an error (no partial boot).
func driveFirecrackerAPI(apiSock string, cfg *vmConfig, timeout time.Duration) error {
	client := unixHTTPClient(apiSock)
	if err := waitForSocket(apiSock, timeout); err != nil {
		return err
	}
	for _, step := range firecrackerBootSequence(cfg) {
		if err := putFirecracker(client, step.path, step.body); err != nil {
			return fmt.Errorf("PUT %s: %w", step.path, err)
		}
	}
	return nil
}

// putFirecracker issues one PUT to the firecracker API over the Unix-socket HTTP client and errors
// on any non-2xx status.
func putFirecracker(client *http.Client, path string, body []byte) error {
	req, err := http.NewRequest(http.MethodPut, "http://localhost"+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	return nil
}

// unixHTTPClient builds an http.Client whose transport dials the given Unix socket for every
// request — the firecracker REST-over-Unix-socket convention (mirrors how gvisor.go shells out to a
// host binary; here the "binary" is a REST API on a socket).
func unixHTTPClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// waitForSocket blocks until path is a connectable Unix socket or timeout elapses.
func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("firecracker API socket %s did not appear within %s", path, timeout)
}
