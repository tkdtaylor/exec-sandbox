// SPDX-License-Identifier: Apache-2.0
package main

// CLI tests for task 021 (ADR 017): the `keygen` and `verify-attestation` subcommands, driven as the
// real built binary so exit codes and stdout/stderr match the pinned contract.
//
// TC-021-01: keygen writes the pinned pair (filenames, modes, output lines, no-overwrite, usage exit).
// TC-021-11: verify-attestation is a working oracle (exit 0/1/2 exactly, per fixture).

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// binaryPath builds the exec-sandbox binary once and returns its path.
func binaryPath(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "exec-sandbox-bin")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "exec-sandbox")
		cmd := exec.Command("go", "build", "-o", builtBin, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Logf("go build output:\n%s", out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build binary: %v", buildErr)
	}
	return builtBin
}

// runBin runs the built binary with the given args and stdin, returning stdout, stderr, exit code.
func runBin(t *testing.T, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(binaryPath(t), args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run %v: %v", args, err)
		}
	}
	return out.String(), errb.String(), code
}

// TC-021-01: keygen writes the pinned pair.
func TestKeygenWritesPinnedPair(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "attestation-signing.key")
	pubPath := filepath.Join(dir, "attestation-trust-root.pub")

	stdout, stderr, code := runBin(t, "", "keygen", dir)
	if code != 0 {
		t.Fatalf("keygen exit %d, stderr=%q", code, stderr)
	}
	keyAbs, _ := filepath.Abs(keyPath)
	pubAbs, _ := filepath.Abs(pubPath)
	wantOut := "signing_key=" + keyAbs + "\ntrust_root=" + pubAbs + "\n"
	if stdout != wantOut {
		t.Fatalf("keygen stdout:\n%q\nwant:\n%q", stdout, wantOut)
	}

	// Modes: 0600 key, 0644 pub.
	if fi, _ := os.Stat(keyPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %o, want 600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(pubPath); fi.Mode().Perm() != 0o644 {
		t.Fatalf("pub mode = %o, want 644", fi.Mode().Perm())
	}

	// Key PEM-decodes to an ed25519 private key; pub to the matching public key.
	keyPEM, _ := os.ReadFile(keyPath)
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "PRIVATE KEY" {
		t.Fatalf("key block missing or wrong type: %+v", kb)
	}
	kAny, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	priv := kAny.(ed25519.PrivateKey)

	pubPEM, _ := os.ReadFile(pubPath)
	pb, _ := pem.Decode(pubPEM)
	if pb == nil || pb.Type != "PUBLIC KEY" {
		t.Fatalf("pub block missing or wrong type: %+v", pb)
	}
	pAny, err := x509.ParsePKIXPublicKey(pb.Bytes)
	if err != nil {
		t.Fatalf("parse pub: %v", err)
	}
	pub := pAny.(ed25519.PublicKey)
	if !priv.Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatal("keygen private key does not match the written public key")
	}

	// Re-run refuses to overwrite (exit 1) and leaves both files byte-identical.
	keyBefore, _ := os.ReadFile(keyPath)
	pubBefore, _ := os.ReadFile(pubPath)
	_, stderr2, code2 := runBin(t, "", "keygen", dir)
	if code2 != 1 {
		t.Fatalf("re-run keygen exit %d, want 1 (stderr=%q)", code2, stderr2)
	}
	if !strings.Contains(strings.ToLower(stderr2), "exist") {
		t.Fatalf("re-run stderr %q does not mention 'exists'", stderr2)
	}
	keyAfter, _ := os.ReadFile(keyPath)
	pubAfter, _ := os.ReadFile(pubPath)
	if !bytes.Equal(keyBefore, keyAfter) || !bytes.Equal(pubBefore, pubAfter) {
		t.Fatal("re-run keygen modified existing files")
	}

	// No dir arg → exit 2.
	if _, _, c := runBin(t, "", "keygen"); c != 2 {
		t.Fatalf("keygen with no dir exit %d, want 2", c)
	}
}

// TC-021-11: verify-attestation is a working oracle.
func TestVerifyAttestationSubcommand(t *testing.T) {
	root := "testdata/attestation/trust-root.pub"

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("testdata/attestation", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// Valid → exit 0, stdout "ok".
	stdout, stderr, code := runBin(t, read("identity-valid.json"), "verify-attestation", root)
	if code != 0 {
		t.Fatalf("valid identity exit %d, stderr=%q", code, stderr)
	}
	if strings.TrimSpace(stdout) != "ok" {
		t.Fatalf("valid identity stdout=%q, want ok", stdout)
	}

	// Each tampered/wrong-key fixture → exit 1 + non-empty stderr.
	for _, name := range []string{
		"identity-tampered-sandbox-id.json",
		"identity-tampered-signature.json",
		"identity-wrong-key.json",
	} {
		_, stderr, code := runBin(t, read(name), "verify-attestation", root)
		if code != 1 {
			t.Fatalf("%s exit %d, want 1", name, code)
		}
		if strings.TrimSpace(stderr) == "" {
			t.Fatalf("%s: empty stderr on failure", name)
		}
	}

	// No arg → exit 2.
	if _, _, c := runBin(t, read("identity-valid.json"), "verify-attestation"); c != 2 {
		t.Fatalf("no-arg verify-attestation exit %d, want 2", c)
	}

	// Nonexistent trust root → exit 1 with a stderr reason.
	_, stderr, code = runBin(t, read("identity-valid.json"), "verify-attestation", "testdata/attestation/does-not-exist.pub")
	if code != 1 || strings.TrimSpace(stderr) == "" {
		t.Fatalf("nonexistent trust root exit %d stderr=%q, want 1 + reason", code, stderr)
	}

	// Garbage stdin → exit 1.
	if _, _, c := runBin(t, "this is not json", "verify-attestation", root); c != 1 {
		t.Fatalf("garbage stdin exit %d, want 1", c)
	}
}
