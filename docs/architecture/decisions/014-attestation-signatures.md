# ADR 014: Signed `sandbox_identity` attestation (ephemeral ed25519 self-attestation)

**Status:** Accepted
**Date:** 2026-06-20
**Deciders:** exec-sandbox maintainers
**Supersedes:** —
**Reopening condition:** (1) **Workload attestation deferred** — this ADR attests the *identity*
(`sandbox_id`), not the payload contents or the tier. If a consumer later needs to bind the
attestation to *what ran* (payload hash, tier, profile), open a new task; that is a different
preimage and a different trust question. (2) **Host-key trust root revisit** — if vault (or a
future consumer) later requires the attestation signature to chain to a key **it** controls (a
host/CA key), the ephemeral self-attestation here is insufficient for that consumer's trust
decision and option (b) below must be implemented. The reopening trigger is a documented vault
contract change requiring host-key verification.

---

## Context

`Run()` mints `sandbox_identity` as `{"sandbox_id": sandboxID, "attestation": randHex(16)}`
(`run.go:67`) — 16 random bytes that prove nothing and verify against nothing.
`docs/spec/data-model.md` flags the gap; the README lists *"sandbox_identity attestation
signatures"* as deferred v1 work. The attestation is presented to **vault** via
`vault.inject(handle, sandbox_identity, mode)`. A real attestation lets a consumer verify the
identity was minted by exec-sandbox and binds to a specific `sandbox_id`.

Two preconditions had to be settled before implementation:

1. **Trust root / signing key.**
   - **(a) Ephemeral per-process ed25519 self-attestation** — stdlib `crypto/ed25519`, no
     key-management infra; the public key travels in `sandbox_identity` so the consumer can verify
     internal consistency.
   - **(b) Host-provided key** — a longer-lived trust root vault already trusts (host/CA key).
     Required *only* if vault's trust decision must chain to a key **it** controls.

2. **Vault consumer contract check (load-bearing).** If vault verifies the signature against a key
   it already trusts, ephemeral self-attestation is a no-op for vault's trust decision and (b) is
   mandatory.

## Decision

**Adopt option (a): ephemeral per-process ed25519 self-attestation.** Each `Run()` generates a
fresh ed25519 keypair (`crypto/ed25519`, stdlib — preserves F-003), signs the canonical preimage of
the attested fields, and carries the **public key** in `sandbox_identity` alongside the signature.
The **private key never leaves the function scope** — it enters none of the result, audit events,
sandbox env/args, payload, or stdout (mirrors the F-002 credential-non-leak discipline).

### Vault-consumer-contract reading that justifies (a)

The orchestrator confirmed against `interface-contracts.md` (v1) that
vault's trust anchor for the `handle ↔ sandbox_identity` binding is **not** a host-controlled key
chain. Vault validates the binding via:

- **SO_PEERCRED** on a uid-restricted `0600` Unix socket (the caller's uid is checked at the
  kernel) — D1,
- an **unguessable single-use capability handle**, and
- **first-use sandbox binding** (the handle binds to the first `sandbox_id` that presents it) — D5.

Vault does **not** chain trust to a host-controlled key and does **not** require host-key signature
verification of `attestation`. Therefore self-attestation is **not a no-op**: it proves the
identity is *well-formed* and *internally consistent* (the signature verifies against the
in-identity public key over a preimage that binds `sandbox_id`), which is exactly the consistency
property a consumer can check without external key infrastructure. The actual trust boundary is the
uid-restricted socket + single-use handle binding, which exec-sandbox already owns. Option (b) would
add key-management infra for a trust property vault does not consume — deferred to the reopening
condition above.

### Attested fields + canonical encoding

The signature covers a **canonical, length-prefixed preimage** of three attested fields:

| Field | Source | Purpose |
|-------|--------|---------|
| `sandbox_id` | the per-run `sbx-<6 hex>` id | binds the attestation to this sandbox |
| `nonce` | fresh `crypto/rand` 16 bytes, hex | replay resistance (distinct per mint) |
| `ts` | mint wall-clock, RFC3339 / Unix seconds | freshness / replay window |

**Canonical encoding (`attestationPreimage`):** a single helper builds the signed bytes as the
domain-separated, length-prefixed concatenation

```
"exec-sandbox/attestation/v1\n" + LP(sandbox_id) + LP(nonce) + LP(ts)
```

where `LP(s)` is the 4-byte big-endian length of `s` followed by `s`'s UTF-8 bytes. Length-prefixing
removes any field-boundary ambiguity (no separator-injection across fields). **Mint and verify call
the same `attestationPreimage` helper** — the verifier reconstructs byte-for-byte the bytes the
minter signed, so a tamper test that mutates any attested field necessarily changes the preimage and
breaks verification.

### `sandbox_identity` shape after this ADR

```json
{
  "sandbox_id": "sbx-<6 hex>",
  "nonce": "<16 hex bytes>",
  "ts": "<RFC3339 timestamp>",
  "attestation_pubkey": "<32-byte ed25519 public key, hex>",
  "attestation": "<64-byte ed25519 signature, hex>"
}
```

`attestation` is the hex-encoded 64-byte ed25519 signature; `attestation_pubkey` is the hex-encoded
32-byte verify key. `verifyAttestation(identity)` rebuilds the preimage from
`{sandbox_id, nonce, ts}`, decodes `attestation_pubkey` + `attestation`, and returns
`ed25519.Verify(pub, preimage, sig)`.

## Consequences

- **Positive:** stdlib-only (F-003 preserved); no key-management infra; tampering with any attested
  field or the signature breaks verification; the private key is ephemeral and unexported.
- **Negative / accepted:** self-attestation does not by itself prove "minted by *this* host's
  trusted exec-sandbox" to a consumer that lacks an out-of-band channel — that property is supplied
  by the uid-restricted socket boundary, not the signature. Documented as reopening condition (2).
- **IPC contract:** `vault.inject` still receives `sandbox_identity` as a JSON object under the same
  key; the object gains `nonce`, `ts`, `attestation_pubkey` and `attestation` changes from 16 hex
  random bytes to a 64-byte signature. `handle`/`mode` unchanged. The JSON-line shape stays
  parseable.
