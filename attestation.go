// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"time"
)

// attestationDomain is the domain-separation prefix mixed into every attestation preimage so a
// signature minted here can never be confused with a signature over some other exec-sandbox byte
// string (ADR 014).
const attestationDomain = "exec-sandbox/attestation/v1\n"

// attestationPreimage builds the canonical, length-prefixed bytes that the attestation signature
// covers (ADR 014). It is the SINGLE shared helper used by both mintAttestation and
// verifyAttestation — the verifier reconstructs byte-for-byte what the minter signed, so mutating
// any attested field necessarily changes the preimage and breaks verification. Encoding:
//
//	attestationDomain + LP(sandbox_id) + LP(nonce) + LP(ts)
//
// where LP(s) is the 4-byte big-endian length of s followed by s's UTF-8 bytes. Length-prefixing
// removes field-boundary ambiguity (no separator injection across fields).
func attestationPreimage(sandboxID, nonce, ts string) []byte {
	buf := make([]byte, 0, len(attestationDomain)+len(sandboxID)+len(nonce)+len(ts)+12)
	buf = append(buf, attestationDomain...)
	for _, f := range []string{sandboxID, nonce, ts} {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(f)))
		buf = append(buf, l[:]...)
		buf = append(buf, f...)
	}
	return buf
}

// mintAttestation generates a fresh ephemeral ed25519 keypair, signs the canonical preimage of the
// attested fields (sandbox_id, nonce, ts) and returns the sandbox_identity map carrying the public
// key + signature (ADR 014). The private key NEVER leaves this function — only the public key and
// the signature are returned, mirroring the F-002 credential-non-leak discipline. The returned map
// is the exact object presented to vault.inject under "sandbox_identity".
func mintAttestation(sandboxID string) (map[string]any, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	nonce := randHex(16)
	ts := time.Now().UTC().Format(time.RFC3339)
	sig := ed25519.Sign(priv, attestationPreimage(sandboxID, nonce, ts))
	// priv goes out of scope here; it is never copied into the returned map, the result, audit
	// events, sandbox env/args, the payload, or stdout.
	return map[string]any{
		"sandbox_id":         sandboxID,
		"nonce":              nonce,
		"ts":                 ts,
		"attestation_pubkey": hex.EncodeToString(pub),
		"attestation":        hex.EncodeToString(sig),
	}, nil
}

// verifyAttestation reconstructs the canonical preimage from the identity's attested fields and
// verifies the signature against the in-identity public key (ADR 014). It returns true iff the
// identity is well-formed (sandbox_id, nonce, ts, attestation_pubkey, attestation all present and
// correctly encoded) AND ed25519.Verify accepts the signature over the reconstructed preimage.
// Tampering with any attested field, the public key, or the signature makes this return false.
func verifyAttestation(identity map[string]any) bool {
	sandboxID, ok1 := identity["sandbox_id"].(string)
	nonce, ok2 := identity["nonce"].(string)
	ts, ok3 := identity["ts"].(string)
	pubHex, ok4 := identity["attestation_pubkey"].(string)
	sigHex, ok5 := identity["attestation"].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return false
	}
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return false
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pub), attestationPreimage(sandboxID, nonce, ts), sig)
}

// ---------------------------------------------------------------------------
// v2 host-signed attestation (ADR 017): a long-lived host-held ed25519 key
// signs the attestation and the identity carries NO pubkey. Vault verifies the
// signature against an operator-published trust root (ADR 017 reopens ADR 014
// condition 2). The v1 helpers above stay untouched for the transitional
// (unconfigured-key) path.
// ---------------------------------------------------------------------------

// attestationDomainV2 is the v2 domain-separation prefix. A distinct domain from v1 guarantees a v1
// signature can never be replayed as a v2 signature and vice versa (ADR 017).
const attestationDomainV2 = "exec-sandbox/attestation/v2\n"

// hostAttestationFormat is the value of the identity's attestation_format field in host mode. Vault
// uses it to discriminate the host-signed shape (this) from the transitional self-attestation shape
// (no attestation_format, carries attestation_pubkey).
const hostAttestationFormat = "host-ed25519/v2"

// attestationPreimageV2 builds the canonical, length-prefixed bytes the host signature covers (ADR
// 017). It is the SINGLE shared helper for mintHostAttestation and verifyHostAttestation. Encoding:
//
//	attestationDomainV2 + LP(sandbox_id) + LP(tier) + LP(profile_digest) + LP(created_at) + LP(nonce)
//
// where LP(s) is the 4-byte big-endian length of s followed by s's UTF-8 bytes, identical to v1's LP.
func attestationPreimageV2(sandboxID, tier, profileDigest, createdAt, nonce string) []byte {
	buf := make([]byte, 0, len(attestationDomainV2)+len(sandboxID)+len(tier)+len(profileDigest)+len(createdAt)+len(nonce)+20)
	buf = append(buf, attestationDomainV2...)
	for _, f := range []string{sandboxID, tier, profileDigest, createdAt, nonce} {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(f)))
		buf = append(buf, l[:]...)
		buf = append(buf, f...)
	}
	return buf
}

// normalizeTier maps the empty tier to the "bubblewrap" default (ADR 017). The normalized value is
// what the attestation binds, so mint and any consumer agree on the same bytes for a default-tier run.
func normalizeTier(tier string) string {
	if tier == "" {
		return "bubblewrap"
	}
	return tier
}

// profileDigest is the lowercase-hex sha256 of json.Marshal(profile) (ADR 017). Go marshals map keys
// sorted, so a JSON-decoded map[string]any digests deterministically; a nil/absent profile marshals
// to the 4 bytes `null` and is digested as such. Binding the digest (not the object) keeps the signed
// preimage bounded while still committing to the exact profile.
func profileDigest(profile map[string]any) string {
	b, err := json.Marshal(profile)
	if err != nil {
		// A JSON-decoded map[string]any always marshals; digest the literal null bytes as a safe,
		// deterministic fallback rather than aborting a run on an impossible error.
		b = []byte("null")
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// mintHostAttestation signs the v2 preimage with the host key and returns the seven-key host-signed
// sandbox_identity (ADR 017). It normalizes the tier, digests the profile, stamps created_at (RFC3339
// UTC) and a fresh 16-byte hex nonce, and carries NO attestation_pubkey: the verify key lives only in
// the operator-published trust root, never in the attacker-presentable identity. The private key does
// not escape this function (it enters none of the result, audit, argv, sandbox env, or stdout).
func mintHostAttestation(priv ed25519.PrivateKey, sandboxID, tier string, profile map[string]any) map[string]any {
	tierNorm := normalizeTier(tier)
	pd := profileDigest(profile)
	createdAt := time.Now().UTC().Format(time.RFC3339)
	nonce := randHex(16)
	sig := ed25519.Sign(priv, attestationPreimageV2(sandboxID, tierNorm, pd, createdAt, nonce))
	return map[string]any{
		"sandbox_id":         sandboxID,
		"tier":               tierNorm,
		"profile_digest":     pd,
		"created_at":         createdAt,
		"nonce":              nonce,
		"attestation_format": hostAttestationFormat,
		"attestation":        hex.EncodeToString(sig),
	}
}

// verifyHostAttestation reconstructs the v2 preimage from the identity's attested fields and returns
// true iff the signature verifies under ANY trust-root key (ADR 017 try-each-key rotation semantics).
// It requires attestation_format == host-ed25519/v2 and all five attested fields present as strings.
// It NEVER consults any in-identity key material: the only verify keys are the passed roots, so an
// attacker cannot smuggle a self-signed pubkey into the identity to pass this check.
func verifyHostAttestation(identity map[string]any, roots []ed25519.PublicKey) bool {
	format, ok := identity["attestation_format"].(string)
	if !ok || format != hostAttestationFormat {
		return false
	}
	sandboxID, ok1 := identity["sandbox_id"].(string)
	tier, ok2 := identity["tier"].(string)
	pd, ok3 := identity["profile_digest"].(string)
	createdAt, ok4 := identity["created_at"].(string)
	nonce, ok5 := identity["nonce"].(string)
	sigHex, ok6 := identity["attestation"].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 {
		return false
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	pre := attestationPreimageV2(sandboxID, tier, pd, createdAt, nonce)
	for _, root := range roots {
		if len(root) == ed25519.PublicKeySize && ed25519.Verify(root, pre, sig) {
			return true
		}
	}
	return false
}

// loadSigningKey reads the host signing key: a PEM PKCS#8 ed25519 private key (ADR 017). It FAILS
// CLOSED on a missing file, a file whose mode carries any group/other permission bits (0600 or
// stricter required), non-PEM/garbage content, a non-PKCS#8 body, or a non-ed25519 key. There is no
// fallback: a configured-but-broken key aborts the run at the caller (Run()).
func loadSigningKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("permissions %#o too open (require 0600 or stricter, no group/other bits)", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS#8 private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing key is %T, not ed25519", key)
	}
	return priv, nil
}

// loadTrustRoots parses every concatenated PEM PKIX/SPKI PUBLIC KEY block from the trust-root file
// into ed25519 public keys (ADR 017). ANY non-ed25519 block fails the WHOLE load (no partial skip):
// a mixed-algorithm trust root is an operator error, not something to silently narrow. An empty or
// block-less file is also an error.
func loadTrustRoots(path string) ([]ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var roots []ed25519.PublicKey
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse PKIX public key: %w", err)
		}
		ed, ok := pub.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("trust-root block is %T, not ed25519", pub)
		}
		roots = append(roots, ed)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("no PEM public key blocks found in %s", path)
	}
	return roots, nil
}
