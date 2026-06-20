// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
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
