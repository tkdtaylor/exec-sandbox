# Test Spec 011: signed sandbox_identity attestation

**Linked task:** [`docs/tasks/backlog/011-attestation-signatures.md`](../backlog/011-attestation-signatures.md)
**ADR:** ADR 011 (**required before implementation** — settles the signing key / trust root and the attested-fields canonical encoding; see the task's readiness note).
**Written:** 2026-06-20

## Context for the test author

`docs/spec/data-model.md` (~line 196) documents `sandbox_identity` as
`{sandbox_id:"sbx-<6 hex>", attestation:"<16 hex>"}` and flags a TODO:
*"the attestation is currently random bytes, not a signed attestation; v1 adds signatures per the
README."* Today `Run()` mints it as `{"sandbox_id": sandboxID, "attestation": randHex(16)}`
(`run.go:67`) — 16 random bytes that prove nothing and verify against nothing. The README roadmap
lists *"sandbox_identity attestation signatures"* as a deferred v1 item (`README.md:47`).

This task replaces the random-bytes attestation with a **signed attestation**: a signature over the
documented attested fields, verifiable with a public key, where tampering breaks verification.

**Open decision — settle in ADR 011 BEFORE writing code (precondition):**
1. **Signing key / trust root.** Options: (a) **ephemeral per-process ed25519 key** —
   self-attestation, stdlib `crypto/ed25519`, no key-management infra (recommended for v1); the
   public key travels in the `sandbox_identity` so vault can verify the signature is internally
   consistent. (b) **host-provided key** — a longer-lived trust root vault already trusts.
2. **Consumer contract check (load-bearing).** The attestation is presented to **vault** via
   `vault.inject(handle, sandbox_identity, mode)` (`run.go:386-389`; `data-model.md` §vault.inject
   request: `sandbox_identity:{sandbox_id, attestation}`). Before committing to ephemeral
   self-attestation, **confirm how vault consumes `attestation`** — if vault expects a signature it
   can verify against a key **it** already trusts (a host/CA key), ephemeral self-attestation is a
   no-op for vault's trust decision and option (b) is required instead. If the vault consumer
   contract cannot be confirmed from the available block contracts, ADR 011 records the assumption
   and the task is **blocked pending that confirmation** (do not paper over it).
3. **Attested fields + canonical encoding.** What the signature covers (at minimum `sandbox_id`;
   likely a timestamp/nonce to prevent replay) and the exact byte encoding signed, so the verifier
   reconstructs the same preimage.

The test cases below are written against the **recommended** ephemeral-ed25519 self-attestation
shape but are parameterized on "the documented attested fields" / "the public key" so they hold
whichever option ADR 011 picks. If ADR 011 picks the host-key option, the `sandbox_identity` shape
and the verify key source change accordingly and the test author updates the field references.

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-011-01 | `sandbox_identity.attestation` is a cryptographic **signature** over the documented attested fields (per ADR 011), not random bytes; it is minted per run | TC-011-01 | ✅ |
| REQ-011-02 | The attestation **verifies** against the public key (the in-identity public key for self-attestation, or the trusted host key per ADR 011) over the reconstructed preimage of the attested fields | TC-011-02 | ✅ |
| REQ-011-03 | **Tampering breaks verification:** mutating any attested field (e.g. `sandbox_id`) or the signature bytes causes verification to fail | TC-011-03 | ✅ |
| REQ-011-04 | The attestation is presented to `vault.inject` in the `sandbox_identity` exactly as before structurally (the inject request still carries `{sandbox_id, attestation}` plus any added field ADR 011 introduces, e.g. a public key); the IPC contract is not broken | TC-011-04 | ✅ |
| REQ-011-05 | No secret leaks: the **signing private key** (if ephemeral) never appears in the result, the audit events, the sandbox env/args, or stdout; only the public key + signature are externally visible | TC-011-05 | ✅ |
| REQ-011-06 | `docs/spec/data-model.md` `sandbox_identity` section is updated to document the signed attestation (shape, attested fields, verify key source) and the **TODO is removed**; the vault.inject request shape reflects any added field | TC-011-06 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Tamper / negative verification is covered
- [x] Private-key non-leak is covered
- [x] Every REQ-ID has at least one test case
- [ ] **BLOCKER:** ADR 011 written settling the trust root AND the vault-consumer contract confirmed
      (or the assumption recorded + task marked blocked) — this checkbox must be ticked before
      implementation starts

---

## Test cases

### TC-011-01: attestation is a signature, not random bytes

- **Requirement:** REQ-011-01
- **Type:** unit
- **Input:** mint a `sandbox_identity` via the attestation path (the function that replaces
  `randHex(16)` at `run.go:67`).
- **Expected:** `attestation` is a signature of the expected length/encoding for the chosen scheme
  (ed25519 ⇒ 64-byte signature, hex/base64 per ADR 011) over the attested-fields preimage — not a
  16-byte random value. Two mints for the **same** attested fields with the **same** key produce a
  verifiable signature (ed25519 is deterministic; if a nonce is in the preimage, distinct mints
  differ but each still verifies).

### TC-011-02: the attestation verifies against the public key

- **Requirement:** REQ-011-02
- **Type:** unit
- **Input:** a minted `sandbox_identity`; reconstruct the attested-fields preimage per ADR 011;
  obtain the verify key (the in-identity public key for self-attestation, or the configured host
  key).
- **Expected:** `ed25519.Verify(pub, preimage, sig)` (or the chosen scheme's verify) returns
  **true**. A helper `verifyAttestation(identity, pub) bool` returns true on a freshly minted
  identity.

### TC-011-03: tampering breaks verification

- **Requirement:** REQ-011-03
- **Type:** unit (negative)
- **Input:** take a valid identity and produce three tampered variants: (a) flip a byte in
  `sandbox_id`; (b) flip a byte in the signature; (c) (if applicable) flip a byte in an attested
  timestamp/nonce.
- **Expected:** verification returns **false** for every tampered variant — the signature binds the
  attested fields, so any mutation invalidates it.

### TC-011-04: vault.inject still receives a well-formed sandbox_identity

- **Requirement:** REQ-011-04
- **Type:** unit/integration (against a stub vault socket that records the inject request)
- **Input:** a run with one `secret_ref`, wired to a recording stub vault socket.
- **Expected:** the recorded `vault.inject` request carries
  `sandbox_identity:{sandbox_id, attestation, …}` where `attestation` is the signature and any
  ADR-011-added field (e.g. `attestation_pubkey`) is present; the JSON-line IPC contract is intact
  (vault can parse it). The `mode` and `handle` fields are unchanged.

### TC-011-05: the signing private key never leaks

- **Requirement:** REQ-011-05
- **Type:** unit/integration
- **Input:** a run that exercises the attestation path with a sentinel-derived ephemeral key (for
  self-attestation); capture the result map, the emitted audit events, the spawn argv, and stdout.
- **Expected:** the private-key bytes appear in **none** of: `result` (including
  `sandbox_status`), any audit event, the bwrap argv / `--setenv` pairs, the payload, or
  `result["stdout"]`. Only the **public** key and the **signature** are ever externally visible.
  (This mirrors the F-002 credential-non-leak discipline.)

### TC-011-06: data-model spec updated, TODO removed

- **Requirement:** REQ-011-06
- **Type:** inspection (spec)
- **Input:** read `docs/spec/data-model.md` `sandbox_identity` section and the `vault.inject`
  request section after the feat commit.
- **Expected:** the `sandbox_identity` section documents the signed attestation — its shape, the
  attested fields, and the verify-key source (per ADR 011) — and the
  `(TODO: the attestation is currently random bytes …)` line is **removed**. The `vault.inject`
  request shape reflects any added field (e.g. the public key). No future tense.

---

## Post-implementation verification

- [ ] TC-011-01..03: attestation is a verifiable signature; tampering breaks it
- [ ] TC-011-04: vault.inject receives a well-formed identity; IPC intact
- [ ] TC-011-05: private key never leaks into result/audit/sandbox/stdout
- [ ] TC-011-06: data-model TODO removed; vault.inject shape updated
- [ ] ADR 011 written AND vault-consumer contract confirmed (or assumption recorded + blocked)

## Test framework notes

- Standard Go `testing` + stdlib `crypto/ed25519` (no new dependency — preserves the stdlib-only
  invariant / F-003). The verify helper and the mint helper are pure functions over the attested
  fields, unit-testable without `bwrap`.
- Reuse the recording stub vault socket pattern (a Unix listener that decodes the inject request)
  for TC-011-04 — the same JSON-line IPC `vaultInject` uses.
- TC-011-03's tamper cases must reconstruct the **exact** preimage ADR 011 specifies, or the
  negative result is meaningless — keep the preimage construction in one shared helper used by both
  mint and verify so the test cannot drift from the implementation.
