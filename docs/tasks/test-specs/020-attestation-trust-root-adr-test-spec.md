# Test Spec 020: attestation trust-root ADR (host ed25519 key, published trust root, vault consumer contract)

**Linked task:** [`docs/tasks/backlog/020-attestation-trust-root-adr.md`](../backlog/020-attestation-trust-root-adr.md)
**ADR:** this task PRODUCES the ADR. It takes the **next available ADR number** (sequential-by-creation, not bound to the task ID; 017 is next at time of writing, re-check `ls docs/architecture/decisions/` at execution). It **amends ADR 014** via ADR 014's reopening condition (2).
**Written:** 2026-07-11

## Context for the test author

This is a **docs-only decision task**: the deliverable is one ADR file, no code, no `docs/spec/` rewrite. The test cases are therefore **inspection assertions on the ADR's content**, each checking that a specific decision is pinned byte-precisely, plus scope checks that nothing else changed. There is no `go test` surface; the "tests" are the spec-verifier's per-assertion checklist.

History the ADR must reconcile (do not restate it wrong):

- Task 011 (`docs/tasks/completed/011-attestation-signatures.md`) was blocked on exactly this decision ("an ADR + a vault-consumer contract on the trust root") and was **unblocked and completed** by **ADR 014**, which chose **(a) ephemeral per-run ed25519 self-attestation**: `mintAttestation` (`attestation.go`) generates a fresh keypair per run, signs the length-prefixed preimage `"exec-sandbox/attestation/v1\n" + LP(sandbox_id) + LP(nonce) + LP(ts)`, and carries `attestation_pubkey` inside `sandbox_identity`. The attestation is therefore **no longer random bytes** (the pre-011 state); random bytes survive only as the `nonce` component.
- ADR 014 explicitly reserved **reopening condition (2)**: "if vault (or a future consumer) later requires the attestation signature to chain to a key **it** controls (a host/CA key) ... option (b) must be implemented. The reopening trigger is a documented vault contract change requiring host-key verification."
- That trigger has now fired: **vault's task 010** (sibling repo, in flight in parallel) verifies a signed attestation against a **published trust root** instead of treating `sandbox_id` as an opaque string. This ADR is the exec-sandbox side of that contract.
- `docs/plans/roadmap.md:38` still lists task 011 as "⚠️ blocked"; that row is stale and this task refreshes it.

Constraints the ADR must respect (repo facts, not choices):

- **Stdlib only** (AGENTS.md tech stack; F-003 discipline in task 011). `crypto/ed25519`, `crypto/x509`, `encoding/pem`, `crypto/sha256`, `encoding/json` are all stdlib and sufficient.
- **No config files, no application env vars** (`docs/spec/configuration.md`: "all configuration arrives inside the stdin `RunRequest`"). The signing-key location must therefore be a `wiring` field, not an env var or config file.
- **No future tense in `docs/spec/`** (AGENTS.md Never list). The vault-consumable contract shapes live **in the ADR** (ADRs record decisions, including not-yet-implemented ones); `docs/CONTRACT.md` + `docs/spec/*` are rewritten in task 021's feat commit, in the same commit as the code.
- The repo is a **single `main` package** (`docs/spec/interfaces.md`): vault cannot `import` a verify function. The contract must be executable by vault via copied fixtures + a documented byte-level algorithm (and, per task 021, a `verify-attestation` subcommand vault's tests can shell out to).

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-020-01 | The ADR exists at the next available number, is titled as the attestation trust-root decision, carries `Status: Accepted`, names ADR 014 as amended-in-part (reopening condition 2 triggered by vault task 010), and cites task 011 as the blocked predecessor this supersedes | TC-020-01 | ✅ |
| REQ-020-02 | The ADR pins the **trust root**: a host-generated ed25519 keypair; private key file format (PEM PKCS#8, `PRIVATE KEY` block), permissions (0600, loader rejects wider), and generation commands (both the task-021 `keygen` subcommand and the openssl equivalent incl. `chmod 600`) | TC-020-02 | ✅ |
| REQ-020-03 | The ADR pins **how the key path reaches the binary**: a new optional `wiring.attestation_key` field in the stdin `RunRequest` (path to the private key); empty means not configured. It records why not an env var/config file (configuration.md invariant) | TC-020-03 | ✅ |
| REQ-020-04 | The ADR pins **what is signed**: the exact attested fields (`sandbox_id`, `tier` normalized, `profile_digest`, `created_at`, `nonce`) and the **byte-precise canonical preimage encoding**, with the JCS alternative weighed and the choice justified | TC-020-04 | ✅ |
| REQ-020-05 | The ADR pins **the published trust root for vault**: public-key file format (PEM PKIX/SPKI `PUBLIC KEY` block(s)), that one file may carry multiple concatenated keys, and that the operator passes the same file to both binaries | TC-020-05 | ✅ |
| REQ-020-06 | The ADR pins the **rotation story** (multi-key trust root: add new pubkey, distribute, switch signer, retire old key) and the **fail-closed rule** (configured-but-unreadable/malformed key is a hard `{error}` before side effects; unconfigured falls back to ADR 014 self-attestation, explicitly labeled transitional) | TC-020-06 | ✅ |
| REQ-020-07 | The ADR contains a normative **"Consumer contract (vault)"** section: the exact host-signed `sandbox_identity` JSON shape, the verify algorithm step-by-step, and how vault distinguishes host mode from transitional self mode. Complete enough that vault task 010 can implement verification without reading exec-sandbox source | TC-020-07 | ✅ |
| REQ-020-08 | Scope discipline: **no code changes**, no `docs/spec/` or `docs/CONTRACT.md` edits (those are task 021's feat commit); the only other permitted edit is refreshing the stale roadmap row for task 011 | TC-020-08 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected content is specified for each inspection case
- [x] The byte-precise preimage recommendation is written out so the ADR author does not have to invent it
- [x] Every REQ-ID has at least one test case
- [x] Confirmed: task 011 is COMPLETED (not blocked) and ADR 014 exists; this ADR amends it via its own reopening condition, it does not pretend the attestation is still random bytes

---

## Test cases

### TC-020-01: the ADR exists, is numbered next-available, and reconciles the history

- **Requirement:** REQ-020-01
- **Type:** inspection (ADR file)
- **Input:** `ls docs/architecture/decisions/`; read the new ADR.
- **Expected:** exactly one new file `docs/architecture/decisions/0NN-attestation-trust-root.md` where `0NN` is the next free number (017 unless another ADR landed first). Header carries `Status: Accepted`, a date, and an explicit **"Amends: ADR 014 (reopening condition 2)"** line (or equivalent under Supersedes) stating the trigger: vault task 010 now requires host-key verification against a published trust root, which is verbatim ADR 014's documented reopening trigger. The Context section cites task 011's readiness block ("blocked on an ADR + a vault-consumer contract on the trust root") and states that ADR 014 resolved it with ephemeral self-attestation, which this ADR now upgrades. It must NOT claim the current attestation is random bytes.

### TC-020-02: trust root pinned (key type, private-key file format, perms, generation)

- **Requirement:** REQ-020-02
- **Type:** inspection (ADR content)
- **Input:** the ADR's Decision section.
- **Expected:** all of the following are stated, each byte-precise:
  - Key type: **ed25519** (stdlib `crypto/ed25519`), one long-lived **host** keypair (per host / per deployment, operator-owned), replacing the per-run ephemeral keypair for the signing role.
  - Private key file: **PEM, PKCS#8** (`-----BEGIN PRIVATE KEY-----`), parsed with stdlib `crypto/x509.ParsePKCS8PrivateKey` + `encoding/pem`. A non-ed25519 key in the file is an error (fail closed), never a fallback.
  - Permissions: the file is created **0600**; the loader **rejects** a key file with group/other permission bits set (fail closed). The openssl generation doc includes the required `chmod 600`.
  - Generation, both ways:
    - `exec-sandbox keygen <dir>` (implemented in task 021) writing `<dir>/attestation-signing.key` (0600) and `<dir>/attestation-trust-root.pub` (0644), refusing to overwrite existing files;
    - openssl equivalent: `openssl genpkey -algorithm ed25519 -out attestation-signing.key && chmod 600 attestation-signing.key && openssl pkey -in attestation-signing.key -pubout -out attestation-trust-root.pub`.

### TC-020-03: key-path wiring pinned (RunRequest field, not env/config)

- **Requirement:** REQ-020-03
- **Type:** inspection (ADR content)
- **Input:** the ADR's Decision section.
- **Expected:** the ADR names the exact field **`wiring.attestation_key`** (string, host path to the PEM private key, default `""`), states it rides in the stdin `RunRequest` like `wiring.vault_socket` does, and records the rationale: `docs/spec/configuration.md` pins "no config files and reads no application environment variables", so per-request wiring is the only compliant channel. `""` ⇒ host signing not configured (transitional self-attestation path).

### TC-020-04: attested fields + canonical preimage pinned byte-precisely

- **Requirement:** REQ-020-04
- **Type:** inspection (ADR content)
- **Input:** the ADR's Decision section.
- **Expected:** the ADR pins the attested fields and encoding. The **recommended decision** (the ADR may deviate only with written rationale, and whatever it picks must be equally byte-precise):
  - Attested fields, all strings:
    | Field | Source | Notes |
    |-------|--------|-------|
    | `sandbox_id` | the per-run `sbx-<6 hex>` id | binds to this sandbox |
    | `tier` | `run.tier`, **normalized**: `""` ⇒ `"bubblewrap"`, else verbatim | binds the isolation tier |
    | `profile_digest` | lowercase-hex sha256 of `json.Marshal(req.Run.Profile)` (Go marshals map keys sorted, deterministic for a JSON-decoded `map[string]any`; a nil/absent profile marshals to the 4 bytes `null` and is digested as such) | binds the profile without embedding an unbounded object |
    | `created_at` | `time.Now().UTC().Format(time.RFC3339)` | freshness window for the consumer |
    | `nonce` | 16 fresh `crypto/rand` bytes, lowercase hex (32 chars) | replay resistance, carried over from ADR 014 |
  - Preimage (extends ADR 014's proven scheme, new domain string):
    ```
    "exec-sandbox/attestation/v2\n" + LP(sandbox_id) + LP(tier) + LP(profile_digest) + LP(created_at) + LP(nonce)
    ```
    where `LP(s)` is the 4-byte big-endian length of `s` followed by `s`'s UTF-8 bytes, identical to ADR 014's `LP`.
  - The **RFC 8785 JCS alternative is weighed and its disposition recorded**. The honest rationale to record for rejecting it: Go stdlib has no JCS; `encoding/json` HTML-escapes `<`, `>`, `&` (not JCS-compatible string escaping) and JCS number canonicalization is easy to get subtly wrong, whereas the length-prefixed scheme already exists in `attestationPreimage`, is trivially portable, and has a shared mint/verify helper precedent (ADR 014). Note that the profile object itself still enters via sorted-key `json.Marshal` bytes, which is where the ecosystem's audit-trail-style canonical-JSON instinct is honored, but as a digest input, not as the signed preimage syntax.

### TC-020-05: published trust root pinned (format, multi-key, distribution)

- **Requirement:** REQ-020-05
- **Type:** inspection (ADR content)
- **Input:** the ADR's Decision + Consumer contract sections.
- **Expected:** the trust root is a plain file of **one or more concatenated PEM `PUBLIC KEY` (PKIX/SPKI) blocks**, each an ed25519 key (`x509.MarshalPKIXPublicKey` output; parseable by `x509.ParsePKIXPublicKey`). A non-ed25519 block is a load error (fail closed, no skip). The operator generates it with `keygen` (or `openssl pkey -pubout`) and **passes the same file to both binaries**: exec-sandbox only for its own `verify-attestation` helper/tests, vault as the verification trust anchor (vault task 010's input). exec-sandbox does not publish the key anywhere else (no socket handshake, no in-identity pubkey in host mode).

### TC-020-06: rotation + fail-closed rules pinned

- **Requirement:** REQ-020-06
- **Type:** inspection (ADR content)
- **Input:** the ADR's Decision/Consequences sections.
- **Expected:**
  - **Rotation:** generate the new keypair; **append** the new PEM block to the trust-root file and distribute it to vault (vault accepts a signature verifying under **any** listed key); switch `wiring.attestation_key` to the new private key; after the freshness window drains, remove the retired PEM block. No signature versioning field is needed for rotation because verification is try-each-key.
  - **Fail closed:** if `wiring.attestation_key` is non-empty and the file is missing, unreadable, malformed PEM, non-PKCS#8, or not ed25519, `Run()` returns a hard `{error}` **before any side effect** (no proxy start, no vault.inject, no spawn audit), same ordering discipline as `validateWorkdir`/`validateFileReads` (ADR 004/005). There is **no silent fallback** from a configured-but-broken key to self-attestation.
  - **Transitional:** `wiring.attestation_key == ""` keeps the ADR 014 ephemeral self-attestation byte-for-byte (NOT the pre-011 random bytes, which no longer exist), and the ADR explicitly labels this path **transitional** with its retirement condition (vault ecosystem-wide requires host attestation ⇒ flip to fail-closed-when-unconfigured in a follow-on task).

### TC-020-07: the vault consumer contract section is normative and complete

- **Requirement:** REQ-020-07
- **Type:** inspection (ADR content)
- **Input:** the ADR's "Consumer contract (vault)" section.
- **Expected:** the section contains, verbatim and self-contained (vault task 010 implements from this section alone):
  - The host-signed `sandbox_identity` JSON shape:
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
    with **no `attestation_pubkey`** in host mode (the verify key comes from the published trust root, never from the attacker-controllable identity object).
  - The verify algorithm, numbered: (1) `attestation_format` must equal `"host-ed25519/v2"`; (2) the five attested fields must be present as strings; (3) rebuild the v2 preimage exactly as pinned in TC-020-04; (4) hex-decode `attestation`, must be exactly 64 bytes; (5) `ed25519.Verify(pub, preimage, sig)` against **each** trust-root key, accept iff any passes; (6) freshness (`created_at` window) and replay (`nonce` uniqueness) policy is the **consumer's** decision, out of exec-sandbox scope, noted as vault task 010's call.
  - Mode discrimination: an identity carrying `attestation_pubkey` and no `attestation_format` is the **transitional self-attestation** (ADR 014 shape, v1 preimage); vault chooses per its own policy whether to accept it during the transition.
  - A pointer to the fixtures task 021 ships (`testdata/attestation/`: valid, tampered-field, tampered-signature, wrong-key identities + the test-only trust root) as copy-able conformance vectors.

### TC-020-08: docs-only scope held

- **Requirement:** REQ-020-08
- **Type:** inspection (git diff)
- **Input:** `git diff --stat` of the task branch against `main`.
- **Expected:** changed files are exactly: the new ADR, `docs/plans/roadmap.md` (the stale "⚠️ blocked" row for item 011 refreshed to point at ADR 014 + this ADR and tasks 020/021), and the task/coverage bookkeeping files (`docs/tasks/*`, `coverage-tracker.md`). **Zero** `.go` files, **zero** `docs/spec/*` files, **zero** `docs/CONTRACT.md` changes. `go test ./...` and `gofmt -l .` are trivially unchanged/green.

---

## Post-implementation verification

- [ ] TC-020-01: ADR at next free number; Amends ADR 014 (reopening condition 2); cites task 011; no "random bytes today" misstatement
- [ ] TC-020-02: key type, PKCS#8 PEM, 0600 + loader perm check, keygen + openssl commands all pinned
- [ ] TC-020-03: `wiring.attestation_key` pinned with the no-env/no-config rationale
- [ ] TC-020-04: attested fields + v2 length-prefixed preimage pinned byte-precisely; JCS weighed with honest rationale
- [ ] TC-020-05: multi-PEM PKIX trust-root file pinned; same file to both binaries
- [ ] TC-020-06: rotation story + fail-closed + transitional-labeled fallback pinned
- [ ] TC-020-07: consumer contract section complete (shape, 6-step verify algorithm, mode discrimination, fixtures pointer)
- [ ] TC-020-08: docs-only diff (ADR + roadmap row + task bookkeeping)

## Test framework notes

- No Go tests are added by this task; the spec-verifier performs the inspections above directly against the ADR text and the git diff.
- Wording precision matters more than usual: task 021's executor and vault task 010's executor both implement from this ADR without a shared conversation. Every "Expected" above that contains a literal string, byte layout, or command is normative; do not paraphrase them in the ADR, copy them.
