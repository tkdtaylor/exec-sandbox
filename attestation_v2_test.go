// SPDX-License-Identifier: Apache-2.0
package main

// Tests for task 021: host-signed sandbox_identity attestation (ADR 017 — host-held ed25519 key,
// published trust root, vault consumer contract). The v1 self-attestation tests live in
// attestation_test.go and stay green (the transitional path is byte-for-byte unchanged).
//
// TC-021-02: openssl-generated keys interoperate with the loaders/mint/verify.
// TC-021-03: loadSigningKey / loadTrustRoots fail closed on every bad-key variant.
// TC-021-04: the live Run() path presents the pinned seven-key host-signed shape to vault.
// TC-021-05: the recorded signature verifies under the trust root via an independently derived preimage.
// TC-021-06: per-field tamper (+ format + injected in-identity pubkey) breaks verification.
// TC-021-07: a wrong signing key fails against the trust root; a two-key root accepts it.
// TC-021-08: a configured-but-broken key fails closed before proxy/vault/audit side effects.
// TC-021-09: unconfigured keeps the ADR 014 self-attestation byte-shape-identical (transitional).
// TC-021-10: the host private key never leaks into result/audit/argv/stdout, and is not mounted.
// TC-021-12: committed fixtures are honest (verdicts match filenames; valid one is reproducible).

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

// writeSigningKeyFile marshals priv to PEM PKCS#8 and writes it at the given mode.
func writeSigningKeyFile(t *testing.T, path string, priv ed25519.PrivateKey, mode os.FileMode) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS#8: %v", err)
	}
	p := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, p, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod key: %v", err)
	}
}

// writeTrustRootFile writes one or more PKIX public-key PEM blocks concatenated into path.
func writeTrustRootFile(t *testing.T, path string, pubs ...any) {
	t.Helper()
	var buf []byte
	for _, pub := range pubs {
		der, err := x509.MarshalPKIXPublicKey(pub)
		if err != nil {
			t.Fatalf("marshal PKIX: %v", err)
		}
		buf = append(buf, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})...)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write trust root: %v", err)
	}
}

// derivePreimageV2 re-derives the v2 preimage independently of the production helper (TC-021-05):
// a byte-layout bug would pass both mint and verify but fail this cross-check.
func derivePreimageV2(sandboxID, tier, pd, createdAt, nonce string) []byte {
	buf := []byte("exec-sandbox/attestation/v2\n")
	for _, f := range []string{sandboxID, tier, pd, createdAt, nonce} {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(f)))
		buf = append(buf, l[:]...)
		buf = append(buf, f...)
	}
	return buf
}

// --- TC-021-03: loaders fail closed --------------------------------------

func TestLoadSigningKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()

	// (a) nonexistent path
	if _, err := loadSigningKey(filepath.Join(dir, "nope.key")); err == nil {
		t.Fatal("nonexistent key: want error, got nil")
	}

	// (b) garbage bytes (no PEM)
	garbage := filepath.Join(dir, "garbage.key")
	if err := os.WriteFile(garbage, []byte("not a pem file at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSigningKey(garbage); err == nil {
		t.Fatal("garbage key: want error, got nil")
	}

	// (c) a valid PKCS#8 ECDSA key (wrong algorithm)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ecKey)
	ecPath := filepath.Join(dir, "ecdsa.key")
	if err := os.WriteFile(ecPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSigningKey(ecPath); err == nil {
		t.Fatal("ecdsa key: want error (not ed25519), got nil")
	}

	// (d) correct ed25519 key at 0644 → rejected, error mentions permissions
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	loose := filepath.Join(dir, "loose.key")
	writeSigningKeyFile(t, loose, priv, 0o644)
	_, err := loadSigningKey(loose)
	if err == nil {
		t.Fatal("0644 key: want error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "permission") {
		t.Fatalf("0644 key error %q does not mention permissions", err)
	}

	// (e) correct ed25519 key at 0600 → loads, matches
	good := filepath.Join(dir, "good.key")
	writeSigningKeyFile(t, good, priv, 0o600)
	got, err := loadSigningKey(good)
	if err != nil {
		t.Fatalf("0600 key: unexpected error %v", err)
	}
	if !got.Equal(priv) {
		t.Fatal("0600 key: loaded key does not equal the written key")
	}
}

func TestLoadTrustRootsMultiPEMAndFailClosed(t *testing.T) {
	dir := t.TempDir()

	// (f) two concatenated ed25519 PUBLIC KEY blocks → both keys returned
	pub1, _, _ := ed25519.GenerateKey(rand.Reader)
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	twoKey := filepath.Join(dir, "two.pub")
	writeTrustRootFile(t, twoKey, pub1, pub2)
	roots, err := loadTrustRoots(twoKey)
	if err != nil {
		t.Fatalf("two-key trust root: unexpected error %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("two-key trust root: got %d keys, want 2", len(roots))
	}

	// (g) second block is an RSA public key → whole load fails, no partial skip
	rsaPub := ecdsaPubPEM(t) // an ecdsa key stands in for "non-ed25519 PKIX block"
	mixed := filepath.Join(dir, "mixed.pub")
	ed1DER, _ := x509.MarshalPKIXPublicKey(pub1)
	mixedBytes := append(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: ed1DER}), rsaPub...)
	if err := os.WriteFile(mixed, mixedBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	roots, err = loadTrustRoots(mixed)
	if err == nil {
		t.Fatal("mixed trust root: want error, got nil")
	}
	if roots != nil {
		t.Fatalf("mixed trust root: want no keys on error, got %d", len(roots))
	}
}

func ecdsaPubPEM(t *testing.T) []byte {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// --- TC-021-02: openssl interop ------------------------------------------

func TestOpensslKeysInteroperate(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed; skipping interop test")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "k.pem")
	pubPath := filepath.Join(dir, "p.pem")

	run := func(args ...string) {
		cmd := exec.Command("openssl", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("openssl %v: %v\n%s", args, err, out)
		}
	}
	run("genpkey", "-algorithm", "ed25519", "-out", keyPath)
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	run("pkey", "-in", keyPath, "-pubout", "-out", pubPath)

	priv, err := loadSigningKey(keyPath)
	if err != nil {
		t.Fatalf("loadSigningKey on openssl key: %v", err)
	}
	roots, err := loadTrustRoots(pubPath)
	if err != nil {
		t.Fatalf("loadTrustRoots on openssl pub: %v", err)
	}
	id := mintHostAttestation(priv, "sbx-abcdef", "", map[string]any{"x": 1})
	if !verifyHostAttestation(id, roots) {
		t.Fatal("identity minted under openssl key did not verify against openssl-derived trust root")
	}
}

// --- TC-021-06: per-field tamper -----------------------------------------

func TestHostAttestationTamperFails(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	roots := []ed25519.PublicKey{pub}
	profile := map[string]any{"capabilities": []any{"x"}}

	mint := func() map[string]any {
		return mintHostAttestation(priv, "sbx-abcdef", "bubblewrap", profile)
	}

	// Control: an untampered identity must verify (guards against a vacuously-false verifier).
	if !verifyHostAttestation(mint(), roots) {
		t.Fatal("control: freshly minted identity failed verification")
	}

	flipHex := func(s string) string {
		b := []byte(s)
		if b[0] == '0' {
			b[0] = '1'
		} else {
			b[0] = '0'
		}
		return string(b)
	}

	variants := map[string]func(m map[string]any){
		"sandbox_id":  func(m map[string]any) { m["sandbox_id"] = "sbx-ffffff" },
		"tier":        func(m map[string]any) { m["tier"] = "gvisor" },
		"profile_dig": func(m map[string]any) { m["profile_digest"] = flipHex(m["profile_digest"].(string)) },
		"created_at":  func(m map[string]any) { m["created_at"] = "2020-01-01T00:00:01Z" },
		"nonce":       func(m map[string]any) { m["nonce"] = flipHex(m["nonce"].(string)) },
		"attestation": func(m map[string]any) { m["attestation"] = flipHex(m["attestation"].(string)) },
		"format":      func(m map[string]any) { m["attestation_format"] = "host-ed25519/v1" },
	}
	for name, mut := range variants {
		id := mint()
		mut(id)
		if verifyHostAttestation(id, roots) {
			t.Fatalf("variant %q: verification accepted a tampered identity", name)
		}
	}

	// (h) attacker adds an attestation_pubkey that validly signs the TAMPERED preimage; host-mode
	// verification must still reject it (never consults in-identity key material).
	id := mint()
	id["sandbox_id"] = "sbx-ffffff"
	atkPub, atkPriv, _ := ed25519.GenerateKey(rand.Reader)
	pre := derivePreimageV2("sbx-ffffff", id["tier"].(string), id["profile_digest"].(string), id["created_at"].(string), id["nonce"].(string))
	id["attestation"] = hex.EncodeToString(ed25519.Sign(atkPriv, pre))
	id["attestation_pubkey"] = hex.EncodeToString(atkPub)
	if verifyHostAttestation(id, roots) {
		t.Fatal("host-mode verification consulted an in-identity attestation_pubkey — trust boundary breached")
	}
}

// --- TC-021-07: wrong key vs two-key trust root --------------------------

func TestHostAttestationWrongKey(t *testing.T) {
	pubA, _, _ := ed25519.GenerateKey(rand.Reader)
	pubB, privB, _ := ed25519.GenerateKey(rand.Reader)

	id := mintHostAttestation(privB, "sbx-abcdef", "bubblewrap", nil)

	if verifyHostAttestation(id, []ed25519.PublicKey{pubA}) {
		t.Fatal("A-only trust root accepted a B-signed identity")
	}
	if !verifyHostAttestation(id, []ed25519.PublicKey{pubA, pubB}) {
		t.Fatal("A+B trust root rejected a B-signed identity (try-each-key rotation broken)")
	}
}

// --- TC-021-04 + TC-021-05: live Run() presents the host-signed shape ------

func TestRunPresentsHostSignedIdentity(t *testing.T) {
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	keyPath := filepath.Join(dir, "signing.key")
	writeSigningKeyFile(t, keyPath, priv, 0o600)

	sock, getReqs, closeFn := stubVaultSocket(t)
	defer closeFn()

	var req RunRequest
	req.Run.Payload = "true\n"
	req.Run.Tier = "" // normalizes to bubblewrap
	req.Run.Profile = map[string]any{
		"capabilities": []any{
			map[string]any{"type": "NetConnect", "allowlist": []any{"api.example.com:443"}},
		},
	}
	req.Run.SecretRefs = []string{"handle-xyz"}
	req.Wiring.VaultSocket = sock
	req.Wiring.InjectionMode = "proxy"
	req.Wiring.AttestationKey = keyPath

	_ = Run(req) // may error after inject if bwrap absent; we assert on the recorded inject only

	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("recorded %d inject requests, want 1", len(reqs))
	}
	inj := reqs[0]
	if inj["op"] != "inject" || inj["handle"] != "handle-xyz" || inj["mode"] != "proxy" {
		t.Fatalf("inject op/handle/mode wrong: %v", inj)
	}
	si, ok := inj["sandbox_identity"].(map[string]any)
	if !ok {
		t.Fatalf("sandbox_identity not an object: %T", inj["sandbox_identity"])
	}

	// Exactly the seven pinned keys, no more, no attestation_pubkey.
	wantKeys := map[string]bool{
		"sandbox_id": true, "tier": true, "profile_digest": true, "created_at": true,
		"nonce": true, "attestation_format": true, "attestation": true,
	}
	if len(si) != len(wantKeys) {
		t.Fatalf("identity has %d keys %v, want 7", len(si), keysOf(si))
	}
	for k := range si {
		if !wantKeys[k] {
			t.Fatalf("unexpected identity key %q", k)
		}
	}
	if _, present := si["attestation_pubkey"]; present {
		t.Fatal("host-mode identity carries attestation_pubkey — must be absent")
	}

	// Field shapes.
	// sandbox_id is "sbx-" + randHex(6): 6 random bytes = 12 lowercase hex chars (the docs' "sbx-<6
	// hex>" is shorthand for 6 bytes). Assert the actual live-path shape.
	assertMatch(t, "sandbox_id", si["sandbox_id"].(string), `^sbx-[0-9a-f]{12}$`)
	if si["tier"].(string) != "bubblewrap" {
		t.Fatalf("tier = %q, want bubblewrap (normalized)", si["tier"])
	}
	// profile_digest matches the test's own sha256 of json.Marshal of the same profile.
	pb, _ := json.Marshal(req.Run.Profile)
	sum := sha256.Sum256(pb)
	if si["profile_digest"].(string) != hex.EncodeToString(sum[:]) {
		t.Fatalf("profile_digest = %q, want %q", si["profile_digest"], hex.EncodeToString(sum[:]))
	}
	assertMatch(t, "nonce", si["nonce"].(string), `^[0-9a-f]{32}$`)
	assertMatch(t, "attestation", si["attestation"].(string), `^[0-9a-f]{128}$`)
	if si["attestation_format"].(string) != "host-ed25519/v2" {
		t.Fatalf("attestation_format = %q", si["attestation_format"])
	}
	if _, err := time.Parse(time.RFC3339, si["created_at"].(string)); err != nil {
		t.Fatalf("created_at %q not RFC3339: %v", si["created_at"], err)
	}

	// TC-021-05: the recorded signature verifies under the trust root, and via an independently
	// re-derived preimage.
	roots := []ed25519.PublicKey{pub}
	if !verifyHostAttestation(si, roots) {
		t.Fatal("recorded identity did not verify under the trust root")
	}
	pre := derivePreimageV2(si["sandbox_id"].(string), si["tier"].(string), si["profile_digest"].(string), si["created_at"].(string), si["nonce"].(string))
	sig, _ := hex.DecodeString(si["attestation"].(string))
	if !ed25519.Verify(pub, pre, sig) {
		t.Fatal("independently re-derived preimage failed ed25519.Verify — byte layout mismatch")
	}
}

// --- TC-021-08: configured-but-broken key fails closed --------------------

func TestConfiguredBrokenKeyFailsClosed(t *testing.T) {
	dir := t.TempDir()

	vaultSock, getVault, closeVault := stubVaultSocket(t)
	defer closeVault()
	auditSock, getAudit, closeAudit := stubVaultSocket(t)
	defer closeAudit()

	// (b) a 0644 valid key
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	loose := filepath.Join(dir, "loose.key")
	writeSigningKeyFile(t, loose, priv, 0o644)

	for _, keyPath := range []string{filepath.Join(dir, "nonexistent.key"), loose} {
		var req RunRequest
		req.Run.Payload = "true\n"
		req.Run.SecretRefs = []string{"handle-xyz"}
		req.Wiring.VaultSocket = vaultSock
		req.Wiring.AuditSocket = auditSock
		req.Wiring.AttestationKey = keyPath

		res := Run(req)
		if len(res) != 1 {
			t.Fatalf("key %q: result has %d keys %v, want exactly {error}", keyPath, len(res), keysOf(res))
		}
		msg, ok := res["error"].(string)
		if !ok || !strings.HasPrefix(msg, "attestation signing key: ") {
			t.Fatalf("key %q: error = %q, want prefix 'attestation signing key: '", keyPath, res["error"])
		}
	}

	if n := len(getVault()); n != 0 {
		t.Fatalf("vault stub recorded %d connections, want 0 (no inject before fail-closed)", n)
	}
	if n := len(getAudit()); n != 0 {
		t.Fatalf("audit stub recorded %d events, want 0 (no spawn emit before fail-closed)", n)
	}
}

// --- TC-021-09: unconfigured keeps the ADR 014 shape (transitional) -------

func TestUnconfiguredKeepsSelfAttestation(t *testing.T) {
	for _, keyVal := range []string{"__omit__", ""} {
		sock, getReqs, closeFn := stubVaultSocket(t)

		var req RunRequest
		req.Run.Payload = "true\n"
		req.Run.SecretRefs = []string{"handle-xyz"}
		req.Wiring.VaultSocket = sock
		if keyVal != "__omit__" {
			req.Wiring.AttestationKey = keyVal
		}

		_ = Run(req)
		reqs := getReqs()
		closeFn()

		if len(reqs) != 1 {
			t.Fatalf("keyVal=%q: recorded %d injects, want 1", keyVal, len(reqs))
		}
		si := reqs[0]["sandbox_identity"].(map[string]any)
		want := map[string]bool{"sandbox_id": true, "nonce": true, "ts": true, "attestation_pubkey": true, "attestation": true}
		if len(si) != len(want) {
			t.Fatalf("keyVal=%q: identity has %d keys %v, want the 5 ADR 014 keys", keyVal, len(si), keysOf(si))
		}
		for k := range si {
			if !want[k] {
				t.Fatalf("keyVal=%q: unexpected key %q in transitional identity", keyVal, k)
			}
		}
		for _, absent := range []string{"attestation_format", "tier", "profile_digest", "created_at"} {
			if _, present := si[absent]; present {
				t.Fatalf("keyVal=%q: transitional identity carries host-mode key %q", keyVal, absent)
			}
		}
		if !verifyAttestation(si) {
			t.Fatalf("keyVal=%q: v1 verifyAttestation failed on the transitional identity", keyVal)
		}
	}
}

// --- TC-021-10: the host private key never leaks -------------------------

func TestHostKeyNeverLeaks(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	keyPath := filepath.Join(dir, "signing.key")
	writeSigningKeyFile(t, keyPath, priv, 0o600)

	// Leak needles derived from the key material.
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	needles := []string{
		string(der),
		hex.EncodeToString(der),
		base64.StdEncoding.EncodeToString(der),
		hex.EncodeToString(priv.Seed()),
	}

	auditSock, getAudit, closeAudit := stubVaultSocket(t)
	defer closeAudit()

	var argv []string
	spawnArgvFn = func(a []string) { argv = append([]string(nil), a...) }
	t.Cleanup(func() { spawnArgvFn = nil })

	var req RunRequest
	req.Run.Payload = "true\n"
	req.Run.Tier = "bubblewrap"
	req.Wiring.AuditSocket = auditSock
	req.Wiring.AttestationKey = keyPath

	res := Run(req)

	blob, _ := json.Marshal(res)
	surfaces := []string{string(blob), strings.Join(argv, "\x00")}
	if s, ok := res["stdout"].(string); ok {
		surfaces = append(surfaces, s)
	}
	for _, ev := range getAudit() {
		b, _ := json.Marshal(ev)
		surfaces = append(surfaces, string(b))
	}
	for _, surface := range surfaces {
		for _, n := range needles {
			if n != "" && strings.Contains(surface, n) {
				t.Fatalf("host private-key needle leaked into a captured surface")
			}
		}
	}
	// The key file path must not be among the bwrap bind mounts.
	for i, a := range argv {
		if (a == "--bind" || a == "--ro-bind") && i+1 < len(argv) && argv[i+1] == keyPath {
			t.Fatalf("signing key file %q is bind-mounted into the sandbox", keyPath)
		}
	}
	if strings.Contains(strings.Join(argv, "\x00"), keyPath) {
		t.Fatalf("signing key path %q appears in the spawn argv", keyPath)
	}
}

// --- TC-021-12: fixtures are honest --------------------------------------

func TestFixturesAreHonest(t *testing.T) {
	base := "testdata/attestation"
	roots, err := loadTrustRoots(filepath.Join(base, "trust-root.pub"))
	if err != nil {
		t.Fatalf("load fixture trust root: %v", err)
	}

	loadID := func(name string) map[string]any {
		b, err := os.ReadFile(filepath.Join(base, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return m
	}

	cases := map[string]bool{
		"identity-valid.json":               true,
		"identity-tampered-sandbox-id.json": false,
		"identity-tampered-signature.json":  false,
		"identity-wrong-key.json":           false,
	}
	for name, want := range cases {
		got := verifyHostAttestation(loadID(name), roots)
		if got != want {
			t.Fatalf("fixture %s: verify = %v, want %v (filename must match verdict)", name, got, want)
		}
	}

	// The valid fixture's signature is reproducible from signing.key + the file's own attested fields
	// (ed25519 is deterministic): proves the fixture set is internally consistent, not hand-edited.
	keyPEM, err := os.ReadFile(filepath.Join(base, "signing.key"))
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		t.Fatal("signing.key: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse signing.key: %v", err)
	}
	priv := k.(ed25519.PrivateKey)

	valid := loadID("identity-valid.json")
	pre := derivePreimageV2(valid["sandbox_id"].(string), valid["tier"].(string), valid["profile_digest"].(string), valid["created_at"].(string), valid["nonce"].(string))
	reSig := hex.EncodeToString(ed25519.Sign(priv, pre))
	if reSig != valid["attestation"].(string) {
		t.Fatalf("identity-valid.json attestation not reproducible from signing.key: got %s", reSig)
	}
}

// --- small assertion helpers ---------------------------------------------

func assertMatch(t *testing.T, field, val, pattern string) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(val) {
		t.Fatalf("%s = %q does not match %s", field, val, pattern)
	}
}
