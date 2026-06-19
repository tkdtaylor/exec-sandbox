// SPDX-License-Identifier: Apache-2.0
package main

import (
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// Limits is the parsed, typed form of profile.limits (docs/CONTRACT.md). A zero field means
// "no limit" — the corresponding cap is simply not applied. The per-backend enforcement
// mechanism (POSIX rlimits + tmpfs sizing + CPU affinity + host-side kill) is ADR 003.
type Limits struct {
	CPUCount  int           // cpu_count:  cores, enforced as taskset affinity; 0 = unset
	MemoryMB  int           // memory_mb:  RLIMIT_AS ceiling, in MiB; 0 = unset
	PidsLimit int           // pids:       RLIMIT_NPROC; 0 = unset
	DiskMB    int           // disk_mb:    writable-layer (tmpfs) size, in MiB; 0 = unset
	Timeout   time.Duration // timeout_sec: wall-clock, host-side kill; 0 = unset
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
