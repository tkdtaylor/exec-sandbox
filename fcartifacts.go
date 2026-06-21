// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Pinned guest-artifact locations (ADR 010 A1.Q1). The kernel + rootfs are vendored into the repo
// under guest/ and each is pinned by a sibling .sha256 file. The pin is the supply-chain control:
// dep-scan does not cover a kernel image, so a stdlib crypto/sha256 loader verifies BOTH digests
// before the firecracker backend uses the paths and FAILS FAST on mismatch — no boot from an
// unverified artifact, no silent fall-back to a weaker tier.
//
// These are relative to the directory holding the running exec-sandbox binary (and, for tests, the
// repo root via guestArtifactRoots). Resolution tries each candidate root in order and returns the
// first that exists; absence of the artifacts is a hard error surfaced as a spawn failure (exit
// 127), mirroring the firecracker-binary-absent behavior.
const (
	guestKernelRel    = "guest/kernel/vmlinux-6.1.176"
	guestKernelShaRel = "guest/kernel/vmlinux.sha256"
	guestRootfsRel    = "guest/rootfs/base.ext4"
	guestRootfsShaRel = "guest/rootfs/base.ext4.sha256"
)

// guestArtifacts holds the verified on-host paths to the pinned kernel + rootfs. It is produced
// only by loadGuestArtifacts, which verifies both sha256 digests first — so a guestArtifacts value
// is evidence that both artifacts matched their pin.
type guestArtifacts struct {
	kernelPath string
	rootfsPath string
}

// guestArtifactRoots is the ordered list of directories under which the guest/ artifact tree is
// searched. It is a package-level var so tests can point it at the repo root; in production it is
// the directory of the exec-sandbox binary plus the current working directory. The repo layout
// vendors guest/ at the repo root, which is also the test working directory, so "." covers the
// common cases.
var guestArtifactRoots = func() []string {
	roots := []string{}
	if exe, err := os.Executable(); err == nil {
		roots = append(roots, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		roots = append(roots, wd)
	}
	roots = append(roots, ".")
	return roots
}

// resolveGuestArtifact returns the first candidate root under which rel exists, or "" if none does.
func resolveGuestArtifact(rel string) string {
	for _, root := range guestArtifactRoots() {
		p := filepath.Join(root, rel)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// loadGuestArtifacts resolves the pinned kernel + rootfs, verifies BOTH sha256 digests against
// their sibling .sha256 pins, and returns the verified paths. A missing artifact, a missing/blank
// pin, or ANY digest mismatch is a HARD error (fail fast / crash loudly — ADR 010 A1.Q1): the
// backend never boots an unverified artifact and never falls back to a weaker tier. The error is
// surfaced up through Argv as a spawn failure (exit 127).
func loadGuestArtifacts() (guestArtifacts, error) {
	kernelPath := resolveGuestArtifact(guestKernelRel)
	if kernelPath == "" {
		return guestArtifacts{}, fmt.Errorf("guest kernel not found (%s): pinned artifact missing — cannot boot Tier-3", guestKernelRel)
	}
	rootfsPath := resolveGuestArtifact(guestRootfsRel)
	if rootfsPath == "" {
		return guestArtifacts{}, fmt.Errorf("guest rootfs not found (%s): pinned artifact missing — cannot boot Tier-3", guestRootfsRel)
	}
	if err := verifyPinnedDigest(kernelPath, resolveGuestArtifact(guestKernelShaRel)); err != nil {
		return guestArtifacts{}, fmt.Errorf("guest kernel verification failed: %w", err)
	}
	if err := verifyPinnedDigest(rootfsPath, resolveGuestArtifact(guestRootfsShaRel)); err != nil {
		return guestArtifacts{}, fmt.Errorf("guest rootfs verification failed: %w", err)
	}
	return guestArtifacts{kernelPath: kernelPath, rootfsPath: rootfsPath}, nil
}

// verifyPinnedDigest computes the sha256 of the file at artifactPath and compares it to the hex
// digest stored in pinPath (the first whitespace-delimited token of the file, the `sha256sum`
// output convention). A missing/blank pin or any mismatch returns a non-nil error — the artifact is
// rejected, never used. This is the fail-fast gate; it has no fall-back branch by design.
func verifyPinnedDigest(artifactPath, pinPath string) error {
	if pinPath == "" {
		return fmt.Errorf("no pin file for %s — refusing to use an unpinned artifact", artifactPath)
	}
	want, err := readPinnedDigest(pinPath)
	if err != nil {
		return err
	}
	got, err := fileSHA256(artifactPath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("sha256 mismatch for %s: got %s, pinned %s (tampered or wrong artifact — refusing to boot)",
			artifactPath, got, want)
	}
	return nil
}

// readPinnedDigest reads the hex sha256 from a `sha256sum`-style pin file (first token of the
// first non-empty line). A blank or token-less file is an error (no pin = no boot).
func readPinnedDigest(pinPath string) (string, error) {
	raw, err := os.ReadFile(pinPath)
	if err != nil {
		return "", fmt.Errorf("read pin %s: %w", pinPath, err)
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty pin file %s — refusing to use an unpinned artifact", pinPath)
	}
	return strings.ToLower(fields[0]), nil
}

// fileSHA256 streams the file through crypto/sha256 (no full read into memory — the kernel image is
// large) and returns the lowercase hex digest.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
