// SPDX-License-Identifier: Apache-2.0
package main

// keygen.go implements the two attestation CLI subcommands pinned by ADR 017:
//
//   - `keygen <dir>`            generates the host signing keypair (operator setup).
//   - `verify-attestation <p>`  verifies a host-signed identity on stdin against a trust root.
//
// verify-attestation is vault's executable oracle: vault task 010 (or its tests) can shell out to it
// with the operator-published trust-root.pub to check a recorded identity, matching the byte-level
// verify algorithm documented in ADR 017's "Consumer contract (vault)" section.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// keygenMain implements `exec-sandbox keygen <dir>` (ADR 017). It generates one ed25519 keypair and
// writes <dir>/attestation-signing.key (PEM PKCS#8, 0600) and <dir>/attestation-trust-root.pub (PEM
// PKIX, 0644). It refuses to overwrite an existing file (exit 1) so a re-run cannot silently rotate a
// live key. Exit codes: 0 success, 1 refusal/write error, 2 usage error.
func keygenMain(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: exec-sandbox keygen <dir>")
		return 2
	}
	dir := args[0]
	keyPath := filepath.Join(dir, "attestation-signing.key")
	pubPath := filepath.Join(dir, "attestation-trust-root.pub")

	// Refuse to overwrite either output before writing anything.
	for _, p := range []string{keyPath, pubPath} {
		if _, err := os.Stat(p); err == nil {
			fmt.Fprintf(os.Stderr, "keygen: %s already exists, refusing to overwrite\n", p)
			return 1
		} else if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "keygen: stat %s: %v\n", p, err)
			return 1
		}
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen: generate key:", err)
		return 1
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen: marshal private key:", err)
		return 1
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keygen: marshal public key:", err)
		return 1
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	// O_EXCL guards against a race between the Stat above and the write, and re-asserts no-overwrite.
	if err := writeExclusive(keyPath, keyPEM, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "keygen: write %s: %v\n", keyPath, err)
		return 1
	}
	if err := writeExclusive(pubPath, pubPEM, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "keygen: write %s: %v\n", pubPath, err)
		_ = os.Remove(keyPath) // roll back the private key so a partial run leaves no half-pair
		return 1
	}

	keyAbs, _ := filepath.Abs(keyPath)
	pubAbs, _ := filepath.Abs(pubPath)
	fmt.Printf("signing_key=%s\n", keyAbs)
	fmt.Printf("trust_root=%s\n", pubAbs)
	return 0
}

// writeExclusive writes data to path, creating it with the given mode and failing if it already
// exists (O_EXCL). The explicit mode is applied via Chmod so it is not narrowed only by umask (0644
// under a 022 umask would still be 0644, but 0600 is what the private key requires regardless).
func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Re-assert the mode: O_CREATE mode is subject to umask, and the private key must be exactly 0600.
	return os.Chmod(path, mode)
}

// verifyAttestationMain implements `exec-sandbox verify-attestation <trust-root.pub>` (ADR 017). It
// reads one host-signed sandbox_identity JSON object on stdin and verifies it against the trust root
// via verifyHostAttestation. Exit codes: 0 + "ok" on stdout when it verifies; 1 + reason on stderr on
// a verification failure or an unreadable/unparseable trust root or stdin; 2 on usage error.
func verifyAttestationMain(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: exec-sandbox verify-attestation <trust-root.pub>   (identity JSON on stdin)")
		return 2
	}
	roots, err := loadTrustRoots(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-attestation: trust root:", err)
		return 1
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify-attestation: read stdin:", err)
		return 1
	}
	var identity map[string]any
	if err := json.Unmarshal(data, &identity); err != nil {
		fmt.Fprintln(os.Stderr, "verify-attestation: parse identity:", err)
		return 1
	}
	if !verifyHostAttestation(identity, roots) {
		fmt.Fprintln(os.Stderr, "verify-attestation: signature does not verify against the trust root")
		return 1
	}
	fmt.Println("ok")
	return 0
}
