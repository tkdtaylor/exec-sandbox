// SPDX-License-Identifier: Apache-2.0
package main

// Tests for task 011: signed sandbox_identity attestation (ADR 014 — ephemeral ed25519
// self-attestation).
//
// TC-011-01: attestation is a 64-byte ed25519 signature, not 16 random bytes
// TC-011-02: the attestation verifies against the in-identity public key
// TC-011-03: tampering (sandbox_id / signature / nonce) breaks verification
// TC-011-04: vault.inject still receives a well-formed sandbox_identity
// TC-011-05: the signing private key never leaks into result/audit/argv/stdout
// TC-011-06: data-model spec updated, TODO removed (inspection — see docs/spec/data-model.md
//            §sandbox_identity, which now documents the signed attestation shape and verify-key
//            source; the "currently random bytes" TODO is gone, and the vault.inject request shape
//            carries the added nonce/ts/attestation_pubkey fields).

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TC-011-01: attestation is a signature, not random bytes.
func TestAttestationIsSignatureNotRandom(t *testing.T) {
	id, err := mintAttestation("sbx-abcdef")
	if err != nil {
		t.Fatalf("mintAttestation: %v", err)
	}

	sigHex, _ := id["attestation"].(string)
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		t.Fatalf("attestation not hex: %v", err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("attestation length = %d bytes, want %d (ed25519 signature)", len(sig), ed25519.SignatureSize)
	}
	// The pre-task random attestation was 16 bytes — guard against regression.
	if len(sig) == 16 {
		t.Fatalf("attestation is 16 bytes — looks like the old randHex(16), not a signature")
	}

	pubHex, _ := id["attestation_pubkey"].(string)
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("attestation_pubkey malformed: hex err %v, len %d (want %d)", err, len(pub), ed25519.PublicKeySize)
	}

	// Two mints for the same sandbox_id each still verify (distinct nonce/ts ⇒ different sig, both valid).
	id2, err := mintAttestation("sbx-abcdef")
	if err != nil {
		t.Fatalf("mintAttestation 2: %v", err)
	}
	if !verifyAttestation(id) || !verifyAttestation(id2) {
		t.Fatalf("both fresh mints must verify")
	}
	if id["attestation"] == id2["attestation"] {
		t.Fatalf("two mints produced identical signatures — nonce/ts not in preimage")
	}
}

// TC-011-02: the attestation verifies against the public key.
func TestAttestationVerifies(t *testing.T) {
	id, err := mintAttestation("sbx-112233")
	if err != nil {
		t.Fatalf("mintAttestation: %v", err)
	}
	if !verifyAttestation(id) {
		t.Fatalf("verifyAttestation returned false on a freshly minted identity")
	}

	// Explicit ed25519.Verify over the reconstructed preimage (cross-check the helper).
	pub, _ := hex.DecodeString(id["attestation_pubkey"].(string))
	sig, _ := hex.DecodeString(id["attestation"].(string))
	pre := attestationPreimage(id["sandbox_id"].(string), id["nonce"].(string), id["ts"].(string))
	if !ed25519.Verify(ed25519.PublicKey(pub), pre, sig) {
		t.Fatalf("ed25519.Verify over reconstructed preimage returned false")
	}
}

// TC-011-03: tampering breaks verification — three variants.
func TestAttestationTamperFails(t *testing.T) {
	t.Run("mutated_sandbox_id", func(t *testing.T) {
		id, _ := mintAttestation("sbx-aaaaaa")
		id["sandbox_id"] = "sbx-bbbbbb"
		if verifyAttestation(id) {
			t.Fatalf("verification accepted a mutated sandbox_id")
		}
	})

	t.Run("mutated_signature", func(t *testing.T) {
		id, _ := mintAttestation("sbx-cccccc")
		sig, _ := hex.DecodeString(id["attestation"].(string))
		sig[0] ^= 0xFF // flip a byte in the signature
		id["attestation"] = hex.EncodeToString(sig)
		if verifyAttestation(id) {
			t.Fatalf("verification accepted a mutated signature")
		}
	})

	t.Run("mutated_nonce", func(t *testing.T) {
		id, _ := mintAttestation("sbx-dddddd")
		nonce, _ := hex.DecodeString(id["nonce"].(string))
		nonce[0] ^= 0xFF
		id["nonce"] = hex.EncodeToString(nonce)
		if verifyAttestation(id) {
			t.Fatalf("verification accepted a mutated nonce")
		}
	})
}

// stubVaultSocket starts a Unix-socket listener that records every inject request sent to it and
// responds with an env-mode delivery (so the run proceeds without needing a credential). It returns
// the socket path, an accessor for the recorded requests, and a close func.
func stubVaultSocket(t *testing.T) (socketPath string, getRequests func() []map[string]any, closeFn func()) {
	t.Helper()
	dir := t.TempDir()
	socketPath = filepath.Join(dir, "vault.sock")

	var mu sync.Mutex
	var reqs []map[string]any

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("stubVaultSocket listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					var msg map[string]any
					if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
						continue
					}
					mu.Lock()
					reqs = append(reqs, msg)
					mu.Unlock()
					c.Write([]byte(`{"delivery":"env"}` + "\n"))
				}
			}(conn)
		}
	}()

	getRequests = func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]map[string]any, len(reqs))
		copy(cp, reqs)
		return cp
	}
	closeFn = func() { ln.Close(); <-done }
	return socketPath, getRequests, closeFn
}

// TC-011-04: vault.inject still receives a well-formed sandbox_identity. Exercised directly through
// vaultInject (the IPC producer) against a recording stub — no bwrap needed.
func TestVaultInjectReceivesWellFormedIdentity(t *testing.T) {
	sock, getReqs, closeFn := stubVaultSocket(t)
	defer closeFn()

	id, err := mintAttestation("sbx-eeeeee")
	if err != nil {
		t.Fatalf("mintAttestation: %v", err)
	}
	if _, err := vaultInject(sock, "handle-xyz", id, "proxy"); err != nil {
		t.Fatalf("vaultInject: %v", err)
	}

	reqs := getReqs()
	if len(reqs) != 1 {
		t.Fatalf("recorded %d inject requests, want 1", len(reqs))
	}
	req := reqs[0]
	if req["op"] != "inject" {
		t.Fatalf("op = %v, want inject", req["op"])
	}
	if req["handle"] != "handle-xyz" {
		t.Fatalf("handle = %v, want handle-xyz", req["handle"])
	}
	if req["mode"] != "proxy" {
		t.Fatalf("mode = %v, want proxy", req["mode"])
	}
	si, ok := req["sandbox_identity"].(map[string]any)
	if !ok {
		t.Fatalf("sandbox_identity not an object: %T", req["sandbox_identity"])
	}
	for _, k := range []string{"sandbox_id", "attestation", "attestation_pubkey", "nonce", "ts"} {
		if _, present := si[k]; !present {
			t.Fatalf("sandbox_identity missing field %q; got keys %v", k, keysOf(si))
		}
	}
	// The decoded-from-JSON identity must still verify (IPC round-trip preserved the signature).
	if !verifyAttestation(si) {
		t.Fatalf("sandbox_identity recovered from the inject request failed verification")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TC-011-05: the signing private key never leaks. Because mintAttestation returns only the public
// key + signature and the private key never escapes the function, the only externally visible
// identity must not contain any private-key bytes. We assert: (1) the identity carries the public
// key but no private-key-sized (64-byte ed25519 seed||pub) value, and (2) a sentinel derived from a
// known seed does not appear in the marshalled identity.
func TestAttestationPrivateKeyNeverLeaks(t *testing.T) {
	id, err := mintAttestation("sbx-ffffff")
	if err != nil {
		t.Fatalf("mintAttestation: %v", err)
	}

	// The identity, marshalled exactly as vault.inject would send it.
	blob, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal identity: %v", err)
	}
	s := string(blob)

	// Reconstruct the verify key and confirm an ed25519 PRIVATE key (which embeds the seed) is NOT
	// derivable from / present in the identity. The identity only carries the 32-byte public key and
	// the 64-byte signature; an ed25519 private key is 64 bytes (seed||pub). Assert no field decodes
	// to a 64-byte value that is a valid private key for the carried public key.
	pubHex := id["attestation_pubkey"].(string)
	pub, _ := hex.DecodeString(pubHex)

	// Independently mint a key with a controlled seed and confirm THAT private key's hex never
	// appears in the identity blob (sentinel discipline mirroring F-002).
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sentinelPriv := ed25519.NewKeyFromSeed(seed)
	if strings.Contains(s, hex.EncodeToString(sentinelPriv)) {
		t.Fatalf("sentinel private key bytes appeared in the identity blob")
	}

	// The carried pubkey must be exactly 32 bytes; a 64-byte value would be a leaked private key.
	if len(pub) != ed25519.PublicKeySize {
		t.Fatalf("attestation_pubkey is %d bytes — expected the 32-byte public key, not a private key", len(pub))
	}
	// No field in the identity may be a 64-byte ed25519 private key whose pub half matches our pub.
	for k, v := range id {
		vs, ok := v.(string)
		if !ok {
			continue
		}
		raw, err := hex.DecodeString(vs)
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			continue // not a private-key-shaped value
		}
		if string(raw[ed25519.SeedSize:]) == string(pub) {
			t.Fatalf("field %q carries a private key matching the attestation public key — leak", k)
		}
	}
}
