// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Limits is the parsed, typed form of profile.limits (docs/CONTRACT.md). A zero field means
// "no limit" — the corresponding cap is simply not applied. The per-backend enforcement
// mechanism (POSIX rlimits + tmpfs sizing + CPU affinity + host-side kill) is ADR 003.
type Limits struct {
	CPUCount       int           // cpu_count:        cores, enforced as taskset affinity; 0 = unset
	MemoryMB       int           // memory_mb:        RLIMIT_AS ceiling, in MiB; 0 = unset
	PidsLimit      int           // pids:             RLIMIT_NPROC; 0 = unset
	DiskMB         int           // disk_mb:          writable-layer (tmpfs) size, in MiB; 0 = unset
	Timeout        time.Duration // timeout_sec:      wall-clock, host-side kill; 0 = unset
	MaxOutputBytes int           // max_output_bytes: per-stream host capture ceiling (bytes); 0 = no cap (unbounded). Host-side, above the tier seam (ADR 007).
}

// parseLimits reads profile.limits into a Limits. A missing limits key, a non-numeric value, or a
// non-positive value is treated as "unset" (no cap). JSON numbers arrive as float64.
func parseLimits(profile map[string]any) Limits {
	var lim Limits
	raw, _ := profile["limits"].(map[string]any)
	if raw == nil {
		return lim
	}
	lim.CPUCount = posInt(raw["cpu_count"])
	lim.MemoryMB = posInt(raw["memory_mb"])
	lim.PidsLimit = posInt(raw["pids"])
	lim.DiskMB = posInt(raw["disk_mb"])
	lim.MaxOutputBytes = posInt(raw["max_output_bytes"])
	if sec := posInt(raw["timeout_sec"]); sec > 0 {
		lim.Timeout = time.Duration(sec) * time.Second
	}
	return lim
}

// posInt coerces a JSON number to a positive int, returning 0 for anything missing, non-numeric,
// or <= 0 — all of which mean "no limit".
func posInt(v any) int {
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return 0
	}
	return int(f)
}

// degrade names a single cap that could not be enforced on this host, with a human-readable reason.
// cpu_count and disk_mb are secondary anti-DoS controls: when the host lacks the affordance they
// degrade (warn + continue) rather than failing the run (ADR 003 / agent-builder ADR 027).
type degrade struct {
	cap    string // the profile.limits field that was not enforced (e.g. "cpu_count")
	reason string // stderr WARNING text naming the degraded control
}

// diskQuotaSupported reports whether the writable layer can be size-capped on this host. The
// writable layer is always a tmpfs (the rootfs is read-only), which is reliably sizeable, so the
// default is true. It is a package-level variable, not an env var, so tests can force the degrade
// path without violating exec-sandbox's no-application-env-vars invariant (ADR 003 / ADR 027).
var diskQuotaSupported = func() bool { return true }

// cpuAffinityPrefix returns the taskset argv prefix that pins a process — and everything it spawns,
// by inheritance — to cores 0..cpuCount-1. cpu_count is a secondary control: when taskset is not on
// PATH it degrades (returns a *degrade, no prefix) rather than failing the run. Returns (nil, nil)
// when no cpu_count limit is requested.
func cpuAffinityPrefix(cpuCount int) ([]string, *degrade) {
	if cpuCount <= 0 {
		return nil, nil
	}
	if _, err := exec.LookPath("taskset"); err != nil {
		return nil, &degrade{"cpu_count",
			"cpu_count limit not enforced: taskset not found on PATH; running without CPU-affinity cap"}
	}
	return []string{"taskset", "-c", fmt.Sprintf("0-%d", cpuCount-1)}, nil
}

// prlimitWrap wraps an in-sandbox command with prlimit so RLIMIT_AS (memory_mb) and RLIMIT_NPROC
// (pids) are applied *inside* the sandbox. Under bubblewrap the user namespace makes the NPROC
// count per-sandbox; setting it on the bwrap parent instead counts the host user's processes
// system-wide and breaks bwrap's own clone() (ADR 003). Returns the command unchanged when neither
// cap is set. memory_mb/pids are load-bearing, so a missing prlimit binary is a hard error, not a
// silent drop.
func prlimitWrap(lim Limits, cmd []string) ([]string, error) {
	var flags []string
	if lim.MemoryMB > 0 {
		flags = append(flags, fmt.Sprintf("--as=%d", lim.MemoryMB*1024*1024))
	}
	if lim.PidsLimit > 0 {
		flags = append(flags, fmt.Sprintf("--nproc=%d", lim.PidsLimit))
	}
	if len(flags) == 0 {
		return cmd, nil
	}
	prlimit, err := exec.LookPath("prlimit")
	if err != nil {
		return nil, errors.New("cannot enforce memory_mb/pids: prlimit not found on PATH")
	}
	out := append([]string{prlimit}, flags...)
	out = append(out, "--")
	return append(out, cmd...), nil
}

// capWriter is the host-side output ceiling for one captured stream (ADR 007). It retains at most
// cap bytes in buf and DISCARDS everything past the ceiling, while always reporting every Write as
// fully consumed (n == len(p), err == nil). This is deliberate: the cap is a host memory guard, not
// a payload signal — if Write returned a short count or an error, os/exec's output-copy goroutine
// could surface a broken pipe to the child and change its exit code or deadlock it. The payload
// runs to its natural completion; only the host's retained copy is truncated.
//
// cap <= 0 means "no cap": every byte is retained (unbounded — the prior behavior). overflowed
// reports whether any byte was dropped — writing exactly cap bytes does NOT set it; writing cap+1
// does. It flags "bytes were dropped," not "the cap was reached."
//
// A capWriter is written by a single stream's copy goroutine and read only after cmd.Run() joins
// those goroutines, so it needs no internal locking; the two streams (stdout/stderr) each get their
// own capWriter and are capped independently at the same ceiling.
type capWriter struct {
	buf        bytes.Buffer
	cap        int  // per-stream ceiling in bytes; <= 0 means unbounded
	overflowed bool // true once any byte has been dropped (len written > cap)
}

// newCapWriter returns a capWriter with the given per-stream ceiling. cap <= 0 ⇒ unbounded.
func newCapWriter(cap int) *capWriter {
	return &capWriter{cap: cap}
}

// Write retains up to the ceiling and drops the rest, but always reports len(p) bytes written with
// a nil error so the child's pipe never sees a short write or error (see the type comment).
func (w *capWriter) Write(p []byte) (int, error) {
	if w.cap <= 0 {
		return w.buf.Write(p)
	}
	remaining := w.cap - w.buf.Len() // free space before the ceiling (can be 0)
	take := len(p)
	if take > remaining {
		take = remaining
		w.overflowed = true // this write had bytes that did not fit — they are dropped
	}
	if take > 0 {
		w.buf.Write(p[:take])
	}
	return len(p), nil
}

// String returns the retained (possibly truncated) output.
func (w *capWriter) String() string { return w.buf.String() }
