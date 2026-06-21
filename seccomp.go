// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// tier1SeccompBlob is the compiled Tier-1 default-deny cBPF program, embedded at build time from
// seccomp/tier1.bpf (generated offline by seccomp/build.sh via libseccomp's seccomp_export_bpf —
// NO Go third-party runtime dependency; this is a plain byte slice read by stdlib os only).
//
//go:embed seccomp/tier1.bpf
var tier1SeccompBlob []byte

// tier1SeccompPin is the committed sha256 pin (seccomp/tier1.bpf.sha256, "<hex>  tier1.bpf"
// format). The loader recomputes the blob's digest and fails fast on any mismatch — a tampered or
// stale blob is a hard error, never a silent fall-back to spawning bwrap WITHOUT --seccomp.
//
//go:embed seccomp/tier1.bpf.sha256
var tier1SeccompPin string

// loadTier1SeccompFn is the loader seam the bubblewrap backend calls. It is loadTier1Seccomp in
// production; a test may swap it to inject a load failure and assert the backend FAILS the run
// (no unfiltered fall-back — TC-019-03). Not goroutine-safe; tests that swap it must not run in
// parallel and must restore it via t.Cleanup.
var loadTier1SeccompFn = loadTier1Seccomp

// loadTier1Seccomp verifies the embedded Tier-1 cBPF blob against its committed sha256 pin and,
// on a match, materializes it as an open *os.File whose fd can be handed to bwrap via --seccomp.
//
// Fail-fast contract (ADR 016, mirroring the ADR-010 A1 vmlinux/rootfs pin): if the blob does not
// match the pin, the loader returns a non-nil error and NO usable file. The caller MUST treat that
// as a hard run failure — there is no degraded path that spawns bwrap unfiltered. This is the
// project's "fail fast, crash loudly" stance applied to the kernel-attack-surface boundary.
//
// The returned *os.File is the caller's to close after the spawn (it is threaded into
// cmd.ExtraFiles so the child inherits the fd). The backing temp file is unlinked immediately, so
// only the open fd keeps it alive — nothing persists on disk past the run.
func loadTier1Seccomp() (*os.File, error) {
	want, err := parseSeccompPin(tier1SeccompPin)
	if err != nil {
		return nil, fmt.Errorf("seccomp: malformed pin: %w", err)
	}
	sum := sha256.Sum256(tier1SeccompBlob)
	got := hex.EncodeToString(sum[:])
	if got != want {
		// Hard error — never fall back to an unfiltered spawn.
		return nil, fmt.Errorf("seccomp: blob sha256 mismatch: have %s, pinned %s", got, want)
	}

	f, err := os.CreateTemp("", "tier1-seccomp-*.bpf")
	if err != nil {
		return nil, fmt.Errorf("seccomp: temp file: %w", err)
	}
	// Unlink now: only the open fd keeps the bytes alive, so nothing is left on disk after the run
	// and there is no path a payload could read or swap.
	_ = os.Remove(f.Name())
	if _, err := f.Write(tier1SeccompBlob); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seccomp: write blob: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seccomp: rewind blob: %w", err)
	}
	return f, nil
}

// verifyTier1SeccompBytes is the pure verification half of the loader, factored out so the
// fail-fast behavior can be unit-tested against an arbitrary (e.g. tampered/truncated) blob
// without touching the embedded one. Returns nil iff blob's sha256 equals the pin's hex digest.
func verifyTier1SeccompBytes(blob []byte, pin string) error {
	want, err := parseSeccompPin(pin)
	if err != nil {
		return fmt.Errorf("seccomp: malformed pin: %w", err)
	}
	sum := sha256.Sum256(blob)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("seccomp: blob sha256 mismatch: have %s, pinned %s", got, want)
	}
	return nil
}

// parseSeccompPin extracts the leading hex digest from a "<sha256>  tier1.bpf" pin line (the
// sha256sum output format). It tolerates surrounding whitespace and a trailing filename field.
func parseSeccompPin(pin string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(pin))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty pin")
	}
	hexDigest := strings.ToLower(fields[0])
	if len(hexDigest) != 64 {
		return "", fmt.Errorf("expected 64-char sha256 hex, got %d chars", len(hexDigest))
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", fmt.Errorf("non-hex pin: %w", err)
	}
	return hexDigest, nil
}
