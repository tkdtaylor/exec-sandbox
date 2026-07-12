# ADR 017: Attestation trust root (host ed25519 signing key, published trust root, vault consumer contract)

**Status:** Accepted
**Date:** 2026-07-12
**Deciders:** exec-sandbox maintainers
**Amends:** ADR 014 (reopening condition 2). ADR 014 chose ephemeral per-run ed25519 self-attestation and reserved reopening condition (2): *"if vault (or a future consumer) later requires the attestation signature to chain to a key it controls (a host/CA key) ... option (b) must be implemented. The reopening trigger is a documented vault contract change requiring host-key verification."* That trigger has now fired: vault's task 010 (sibling repo) verifies a signed attestation against an operator-published trust root instead of treating `sandbox_id` as an opaque string. This ADR implements ADR 014's deferred **option (b)** and pins the exec-sandbox side of that contract.
**Supersedes:** the readiness question of task 011 (`docs/tasks/completed/011-attestation-signatures.md`), which was blocked on *"an ADR + a vault-consumer contract on the trust root"* and was unblocked and completed by ADR 014. This ADR upgrades that decision; it does not reopen task 011.

---

## Context

Task 011 was blocked on exactly one decision: *"an ADR + a vault-consumer contract on the trust root"* (ephemeral ed25519 vs a host-provided key). ADR 014 resolved that block by choosing **(a) ephemeral per-run ed25519 self-attestation**: `Run()` mints a fresh keypair per run, signs the length-prefixed preimage `"exec-sandbox/attestation/v1\n" + LP(sandbox_id) + LP(nonce) + LP(ts)`, and carries `attestation_pubkey` inside `sandbox_identity`. The attestation is therefore **not random bytes** (the pre-011 state); random bytes survive only as the `nonce` component. Task 011 is completed, not blocked.

Ephemeral self-attestation proves internal consistency only. The verify key travels inside the attacker-presentable identity object, so a valid signature proves "this identity is well-formed", never "minted by this host's trusted exec-sandbox" (ADR 014, Consequences, accepted-negative). Vault's task 010 upgrades vault's binding check: vault now **cryptographically verifies** the presented attestation against a trust root the operator publishes to it, out of band. That check requires the signature to chain to a **host-held key the identity does not carry**, which is precisely ADR 014's option (b), deferred behind reopening condition (2).

Nothing pins today: which key, where the private key lives, what bytes are signed, how the public key reaches vault, how keys rotate, or what JSON shape vault verifies. Task 021 (implementation) and vault task 010 (verification) both need those answers fixed in one normative place before either writes code. This ADR is that place.

### Constraints (repo facts, not choices)

- **Stdlib only** (AGENTS.md tech stack; F-003). `crypto/ed25519`, `crypto/x509`, `encoding/pem`, `crypto/sha256`, `encoding/json` are all stdlib and sufficient.
- **No config files, no application env vars** (`docs/spec/configuration.md`: "all configuration arrives inside the stdin `RunRequest`"). The signing-key location must therefore be a `wiring` field, not an env var or a config file.
- **No future tense in `docs/spec/`** (AGENTS.md Never list). The vault-consumable contract shapes live in this ADR (ADRs record decisions, including not-yet-implemented ones); `docs/CONTRACT.md` and `docs/spec/*` are rewritten in task 021's feat commit, in the same commit as the code that makes them present-tense true.
- The repo is a **single `main` package** (`docs/spec/interfaces.md`): vault cannot `import` a verify function. The contract must be executable by vault via copied fixtures plus a documented byte-level algorithm, and (per task 021) a `verify-attestation` subcommand vault's tests can shell out to.

## Decision

Adopt ADR 014's **option (b): a long-lived host-held ed25519 signing key with an out-of-band published public key (the trust root)**, replacing the per-run ephemeral key for the signing role. When configured, `Run()` signs a **v2** attestation over `{sandbox_id, tier, profile_digest, created_at, nonce}` with the host key and presents a signature-only identity (no pubkey) to `vault.inject`. Vault verifies that signature against the operator-published trust root. When the host key is unconfigured, `Run()` keeps ADR 014's ephemeral self-attestation byte-for-byte, explicitly labeled **transitional**.

### 1. Trust root (key type, private-key file, permissions, generation)

- **Key type: ed25519** (stdlib `crypto/ed25519`). One long-lived **host** keypair, per host / per deployment, operator-owned, replacing the per-run ephemeral keypair for the signing role.
- **Private key file: PEM, PKCS#8** (`-----BEGIN PRIVATE KEY-----`), parsed with stdlib `crypto/x509.ParsePKCS8PrivateKey` plus `encoding/pem`. A non-ed25519 key in the file is an error (fail closed), never a fallback.
- **Permissions:** the file is created **0600**; the loader **rejects** a key file with any group/other permission bits set (fail closed). The openssl generation doc includes the required `chmod 600`.
- **Generation, both ways:**
  - `exec-sandbox keygen <dir>` (implemented in task 021) writes `<dir>/attestation-signing.key` (0600) and `<dir>/attestation-trust-root.pub` (0644), refusing to overwrite existing files.
  - openssl equivalent:
    ```
    openssl genpkey -algorithm ed25519 -out attestation-signing.key && chmod 600 attestation-signing.key && openssl pkey -in attestation-signing.key -pubout -out attestation-trust-root.pub
    ```

### 2. Key path into the binary (`wiring.attestation_key`)

The signing-key path rides in the stdin `RunRequest` as a new optional field **`wiring.attestation_key`** (string, host path to the PEM private key, default `""`), alongside `wiring.vault_socket`. `docs/spec/configuration.md` pins "no config files and reads no application environment variables", so per-request `wiring` is the only compliant channel: not an env var, not a config file. `""` means host signing is not configured, which selects the transitional self-attestation path.

### 3. What is signed (attested fields + canonical preimage)

Attested fields, all strings:

| Field | Source | Notes |
|-------|--------|-------|
| `sandbox_id` | the per-run `sbx-<6 hex>` id | binds to this sandbox |
| `tier` | `run.tier`, **normalized**: `""` ⇒ `"bubblewrap"`, else verbatim | binds the isolation tier |
| `profile_digest` | lowercase-hex sha256 of `json.Marshal(req.Run.Profile)` (Go marshals map keys sorted, deterministic for a JSON-decoded `map[string]any`; a nil/absent profile marshals to the 4 bytes `null` and is digested as such) | binds the profile without embedding an unbounded object |
| `created_at` | `time.Now().UTC().Format(time.RFC3339)` | freshness window for the consumer |
| `nonce` | 16 fresh `crypto/rand` bytes, lowercase hex (32 chars) | replay resistance, carried over from ADR 014 |

Canonical preimage (extends ADR 014's proven length-prefixed scheme under a new domain string):

```
"exec-sandbox/attestation/v2\n" + LP(sandbox_id) + LP(tier) + LP(profile_digest) + LP(created_at) + LP(nonce)
```

where `LP(s)` is the 4-byte big-endian length of `s` followed by `s`'s UTF-8 bytes, identical to ADR 014's `LP`. Mint and verify call one shared `attestationPreimageV2` helper, same discipline as v1.

**RFC 8785 JCS alternative, weighed and rejected.** JCS (canonical JSON) was considered as the signed-preimage syntax and rejected. Go's standard library has no JCS implementation; `encoding/json` HTML-escapes `<`, `>`, `&` (not JCS-compatible string escaping) and JCS number canonicalization is easy to get subtly wrong. The length-prefixed scheme already exists in `attestationPreimage`, is trivially portable to vault's verifier, and has a shared mint/verify helper precedent (ADR 014). Note the profile object itself still enters the attestation via sorted-key `json.Marshal` bytes (as the `profile_digest` input), which is where the ecosystem's canonical-JSON instinct is honored, but as a digest input, not as the signed preimage syntax.

### 4. Published trust root for vault

The trust root is a plain file of **one or more concatenated PEM `PUBLIC KEY` (PKIX/SPKI) blocks**, each an ed25519 key (`x509.MarshalPKIXPublicKey` output, parseable by `x509.ParsePKIXPublicKey`). A non-ed25519 block is a load error (fail closed, no skip). The operator generates it with `keygen` (or `openssl pkey -pubout`) and **passes the same file to both binaries**: to exec-sandbox only for its own `verify-attestation` helper and tests, and to vault as the verification trust anchor (vault task 010's input). exec-sandbox does **not** publish the key anywhere else: no socket handshake, no in-identity pubkey in host mode.

### 5. Rotation

- Generate the new keypair.
- **Append** the new PEM `PUBLIC KEY` block to the trust-root file and distribute it to vault (vault accepts a signature verifying under **any** listed key).
- Switch `wiring.attestation_key` to the new private key.
- After the freshness window drains, remove the retired PEM block.

No signature-versioning field is needed for rotation because verification is **try-each-key**: the consumer accepts iff any trust-root key verifies the signature.

### 6. Fail-closed rule

If `wiring.attestation_key` is non-empty and the file is missing, unreadable, malformed PEM, non-PKCS#8, or not ed25519, or its mode carries any group/other bits, `Run()` returns a hard `{error}` **before any side effect** (no proxy start, no `vault.inject`, no spawn audit emit), the same pre-side-effect ordering discipline as `validateWorkdir` / `validateFileReads` (ADR 004 / 005). There is **no silent fallback** from a configured-but-broken key to self-attestation.

### 7. Transitional path (unconfigured)

`wiring.attestation_key == ""` keeps ADR 014's ephemeral self-attestation **byte-for-byte** (the `{sandbox_id, nonce, ts, attestation_pubkey, attestation}` shape over the v1 preimage, NOT the pre-011 random bytes, which no longer exist). This path is explicitly **transitional**. Its retirement condition: once vault requires host attestation ecosystem-wide, a follow-on task flips the unconfigured case to fail-closed (an empty `wiring.attestation_key` becomes an error instead of a self-attestation fallback). That follow-on is out of scope here; this ADR only records the condition.

## Consumer contract (vault)

This section is normative and self-contained: vault task 010 implements verification from this section alone, without reading exec-sandbox source.

### Host-signed `sandbox_identity` JSON shape

```json
{
  "sandbox_id": "sbx-<6 hex>",
  "tier": "bubblewrap" | "gvisor" | "firecracker",
  "profile_digest": "<64 lowercase hex>",
  "created_at": "<RFC3339 UTC>",
  "nonce": "<32 lowercase hex>",
  "attestation_format": "host-ed25519/v2",
  "attestation": "<128 lowercase hex = 64-byte ed25519 signature>"
}
```

There is **no `attestation_pubkey`** key in host mode: the verify key comes from the published trust root, never from the attacker-controllable identity object.

### Verify algorithm

1. `attestation_format` must equal `"host-ed25519/v2"`.
2. The five attested fields (`sandbox_id`, `tier`, `profile_digest`, `created_at`, `nonce`) must be present as strings.
3. Rebuild the v2 preimage exactly as pinned in section 3:
   `"exec-sandbox/attestation/v2\n" + LP(sandbox_id) + LP(tier) + LP(profile_digest) + LP(created_at) + LP(nonce)`, `LP(s)` = 4-byte big-endian `len(s)` followed by `s`'s UTF-8 bytes.
4. Hex-decode `attestation`; it must be exactly 64 bytes.
5. `ed25519.Verify(pub, preimage, sig)` against **each** trust-root key; accept iff any key passes.
6. Freshness (`created_at` window) and replay (`nonce` uniqueness) policy is the **consumer's** decision, out of exec-sandbox scope; vault task 010 decides it.

### Mode discrimination

An identity carrying `attestation_pubkey` and no `attestation_format` is the **transitional self-attestation** (the ADR 014 shape, v1 preimage). Vault chooses, per its own policy, whether to accept that shape during the transition window. A host-mode identity is distinguished by `attestation_format == "host-ed25519/v2"` and the absence of `attestation_pubkey`.

### Conformance vectors

Task 021 ships copy-able fixtures under `testdata/attestation/`: a valid identity, per-field tampered identities, a tampered-signature identity, a wrong-key identity, and the test-only trust root. Vault task 010 copies these as conformance vectors, and can shell out to `exec-sandbox verify-attestation <trust-root.pub>` (task 021's subcommand) as an executable oracle.

## Consequences

- **Positive:** vault can now verify "minted by this host's trusted exec-sandbox", the property ADR 014 could not supply, by chaining the signature to an operator-published key the identity does not carry. Stdlib-only (F-003 preserved). The attestation also binds `tier` and the profile (via `profile_digest`), which the v1 self-attestation did not, so vault's binding decision sees what ran, not just which sandbox. Try-each-key rotation needs no version field.
- **Negative / accepted:** exec-sandbox now has a persistent host key file to manage (generation, permissions, rotation, distribution of the public half). That is a stricter operational surface than the ephemeral key of ADR 014, mitigated by the fail-closed loader (0600-or-stricter, ed25519-only) and the non-leak discipline carried over from F-002. The private key is a higher-value target than an ephemeral per-run key; it enters none of the result, audit, argv, sandbox env, or stdout, and its file is never among the sandbox mounts.
- **IPC contract:** in host mode `vault.inject` receives the seven-key `sandbox_identity` above (no `attestation_pubkey`); `handle` and `mode` are unchanged, and the JSON-line shape stays parseable. In the transitional (unconfigured) mode it receives the unchanged ADR 014 five-key shape.
- **Scope held:** this ADR is docs only. Workload attestation (payload-hash binding) stays ADR 014 reopening condition (1), untouched. Vault's own freshness/replay policy is the consumer's call. Key escrow, HSM, and CA hierarchies are out of scope.
