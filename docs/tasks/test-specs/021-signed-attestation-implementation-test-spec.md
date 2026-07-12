# Test Spec 021: host-signed sandbox_identity attestation (implementation of the trust-root ADR)

**Linked task:** [`docs/tasks/backlog/021-signed-attestation-implementation.md`](../backlog/021-signed-attestation-implementation.md)
**ADR:** the task-020 trust-root ADR (next available number, 017 at time of writing) **must be committed before this implementation starts**. This spec restates its pins so the tests are concrete; if the committed ADR deviated from a pin, the ADR wins and the affected literals below are updated in the test-spec commit, never silently in the code.
**Written:** 2026-07-11

## Context for the test author

State of the code today (verified against source, do not trust older docs):

- `Run()` (`run.go:72-80`) mints `sandboxID := "sbx-" + randHex(6)` then `sandboxIdentity, err := mintAttestation(sandboxID)`. That is the **ADR 014 ephemeral self-attestation**, not random bytes: `attestation.go` holds `attestationPreimage(sandboxID, nonce, ts)` (domain `"exec-sandbox/attestation/v1\n"`, 4-byte big-endian length-prefixed fields), `mintAttestation` (fresh per-run ed25519 keypair, returns `{sandbox_id, nonce, ts, attestation_pubkey, attestation}`, all hex), and `verifyAttestation(identity)` (verifies against the **in-identity** pubkey). Task 011 shipped it; the pre-011 random-bytes attestation exists nowhere anymore.
- The identity flows to vault at `run.go:106-108`: `vaultInject(req.Wiring.VaultSocket, handle, sandboxIdentity, req.Wiring.InjectionMode)`, which `ipcCall`s `{"op":"inject","handle":…,"sandbox_identity":…,"mode":…}` (`run.go:529-531`).
- `RunRequest.Wiring` (`main.go`/`run.go:34-40`) has `VaultSocket`, `AuditSocket`, `OriginMap`, `RequestID`, `InjectionMode`. No key field yet.
- `main.go` dispatches subcommands `run` and internal `fc-launch`; exit codes 0/1/2 per `docs/spec/interfaces.md`.

This task implements the task-020 ADR: a **host-held ed25519 signing key** (path in a new `wiring.attestation_key` field) signs a v2 preimage over `{sandbox_id, tier, profile_digest, created_at, nonce}`; the identity presented to `vault.inject` carries the signature and **no pubkey** (vault verifies against the operator-published trust root, its task 010). Unconfigured (`""`) keeps ADR 014 self-attestation byte-for-byte (transitional). Configured-but-broken fails closed before side effects.

### Pinned contract literals (from the task-020 ADR; normative for every test below)

- **v2 preimage:** `"exec-sandbox/attestation/v2\n" + LP(sandbox_id) + LP(tier) + LP(profile_digest) + LP(created_at) + LP(nonce)`, `LP(s)` = 4-byte big-endian `len(s)` + UTF-8 bytes (same `LP` as v1).
- **Attested fields:** `tier` normalized (`""` ⇒ `"bubblewrap"`, else the request value verbatim); `profile_digest` = lowercase-hex sha256 of `json.Marshal(req.Run.Profile)` (nil/absent profile ⇒ digest of the 4 bytes `null`); `created_at` = `time.Now().UTC().Format(time.RFC3339)`; `nonce` = 16 fresh `crypto/rand` bytes as 32 lowercase hex.
- **Host-signed identity JSON** (the map handed to `vaultInject`):
  ```json
  {
    "sandbox_id": "sbx-<6 hex>",
    "tier": "bubblewrap" | "gvisor" | "firecracker",
    "profile_digest": "<64 lowercase hex>",
    "created_at": "<RFC3339 UTC>",
    "nonce": "<32 lowercase hex>",
    "attestation_format": "host-ed25519/v2",
    "attestation": "<128 lowercase hex>"
  }
  ```
  No `attestation_pubkey` key present at all in host mode.
- **Key files:** private = PEM PKCS#8 `PRIVATE KEY` block, ed25519, file mode must be 0600 or stricter (loader rejects group/other bits); trust root = one or more concatenated PEM PKIX/SPKI `PUBLIC KEY` blocks, all ed25519 (any non-ed25519 block fails the whole load).
- **CLI:** `exec-sandbox keygen <dir>` writes `<dir>/attestation-signing.key` (0600) + `<dir>/attestation-trust-root.pub` (0644), prints exactly two lines `signing_key=<abs path>` and `trust_root=<abs path>`, refuses to overwrite (exit 1); missing/extra args exit 2. `exec-sandbox verify-attestation <trust-root.pub>` reads one identity JSON object on stdin, exit 0 + `ok` on stdout when it verifies, exit 1 + reason on stderr when it does not (or trust root/stdin unparseable), exit 2 on usage error.
- **Fail closed:** non-empty `wiring.attestation_key` that is missing/unreadable/malformed/non-ed25519/too-permissive ⇒ `Run()` returns `{"error": "attestation signing key: <detail>"}` **before** proxy start, vault.inject, or the spawn audit emit (same pre-side-effect ordering as `validateWorkdir`).

## Requirements coverage

| Req ID | Requirement | Test cases | Covered? |
|--------|-------------|-----------|----------|
| REQ-021-01 | `keygen` writes a working PKCS#8/PKIX ed25519 pair with pinned filenames, modes (0600/0644), output lines, and no-overwrite refusal; the openssl-generated equivalent loads interchangeably | TC-021-01, TC-021-02 | ✅ |
| REQ-021-02 | The signing-key loader accepts a good key and **fails closed** on: missing file, malformed PEM, non-ed25519 key, permissions wider than 0600. The trust-root loader parses multi-PEM files and fails on a non-ed25519 block | TC-021-03 | ✅ |
| REQ-021-03 | With `wiring.attestation_key` set, `Run()` presents to `vault.inject` a host-signed identity of exactly the pinned v2 shape (all seven keys, no `attestation_pubkey`), whose signature verifies under the trust root over the reconstructed v2 preimage; `handle`/`mode` unchanged; JSON-line IPC intact | TC-021-04, TC-021-05 | ✅ |
| REQ-021-04 | **Negative verification bites:** tampering with each attested field (`sandbox_id`, `tier`, `profile_digest`, `created_at`, `nonce`), the signature bytes, or the format string fails verification; a signature minted under a **different key** fails against the trust root; verification never falls back to any in-identity key material | TC-021-06, TC-021-07 | ✅ |
| REQ-021-05 | Fail-closed on the live path: a configured-but-broken key aborts the run with the pinned `{error}` **before** proxy/vault/audit side effects | TC-021-08 | ✅ |
| REQ-021-06 | Transitional path: `wiring.attestation_key == ""` yields the ADR 014 self-attestation **byte-shape-identical** to today (`{sandbox_id, nonce, ts, attestation_pubkey, attestation}`, `verifyAttestation` passes, no `attestation_format`/`tier`/`profile_digest`/`created_at` keys) | TC-021-09 | ✅ |
| REQ-021-07 | The host **private key never leaks**: its PKCS#8/seed bytes (raw, hex, base64) appear in none of the result, audit events, spawn argv, sandbox env, or stdout; the sandbox cannot read the key file (not among the mounts) | TC-021-10 | ✅ |
| REQ-021-08 | `verify-attestation` subcommand: exit 0/`ok` on a valid identity, exit 1 + stderr reason on each tampered/wrong-key fixture, exit 2 on usage error, per the pinned CLI contract | TC-021-11 | ✅ |
| REQ-021-09 | Copy-able fixtures exist in `testdata/attestation/` (test-only trust root + signing key, valid identity, per-field tampered identities, wrong-key identity) and a test proves each fixture's verdict matches its filename (vault task 010 consumes these as conformance vectors) | TC-021-12 | ✅ |
| REQ-021-10 | Spec rewritten in the same commit: `data-model.md` `sandbox_identity` (both modes, transitional labeled), `configuration.md` wiring table (+`attestation_key`), `interfaces.md` CLI table (+`keygen`, +`verify-attestation`), `CONTRACT.md` vault.inject paragraph, `behaviors.md` inject row. Present tense, rewritten in place | TC-021-13 | ✅ |

## Pre-implementation checklist

- [x] All test cases below are defined
- [x] Expected inputs and outputs are specified for each case
- [x] Tamper coverage is per-field (five fields + signature + format), not a single smoke tamper
- [x] Wrong-key (trust-root mismatch) negative is covered separately from tamper
- [x] Fail-closed is asserted as ordering (no side effect), not just as an error string
- [x] Private-key non-leak is covered for the persistent host key (a stricter surface than 011's ephemeral key)
- [x] Every REQ-ID has at least one test case
- [ ] **BLOCKER:** the task-020 ADR is committed; any deviation from the pinned literals above is reconciled into this spec first

---

## Test cases

### TC-021-01: `keygen` writes the pinned pair

- **Requirement:** REQ-021-01
- **Type:** integration (Go test running the built binary, or unit on the keygen function + a thin CLI test)
- **Input:** `exec-sandbox keygen <t.TempDir()>`.
- **Expected:** exit 0; stdout is exactly the two lines `signing_key=<dir>/attestation-signing.key` and `trust_root=<dir>/attestation-trust-root.pub`. The key file stat mode is `0600`, the pub file `0644`. The key file PEM-decodes to one `PRIVATE KEY` block parsing via `x509.ParsePKCS8PrivateKey` into an `ed25519.PrivateKey`; the pub file to one `PUBLIC KEY` block parsing via `x509.ParsePKIXPublicKey` into the matching `ed25519.PublicKey` (assert `priv.Public()` equals it). Running `keygen` again on the same dir exits 1 with an "exists" message and leaves both files byte-identical. `exec-sandbox keygen` with no dir exits 2.

### TC-021-02: openssl-generated keys interoperate

- **Requirement:** REQ-021-01
- **Type:** integration (skips if `openssl` absent, mirroring the bwrap-skip idiom)
- **Input:** `openssl genpkey -algorithm ed25519 -out k.pem && chmod 600 k.pem && openssl pkey -in k.pem -pubout -out p.pem`; load both with the task's loaders; mint and verify one attestation.
- **Expected:** both load without error; an identity minted under `k.pem` verifies against a trust root containing `p.pem`. This proves the "documented openssl generation" path in the ADR is real, not aspirational.

### TC-021-03: loaders fail closed on every bad-key variant

- **Requirement:** REQ-021-02
- **Type:** unit (negative, table-driven)
- **Input/Expected:** the signing-key loader (`loadSigningKey(path)`) returns a non-nil error and a nil key for each of: (a) nonexistent path; (b) a file containing garbage bytes (no PEM); (c) a valid PEM PKCS#8 **RSA or ECDSA** key (generate in-test); (d) a correct ed25519 key file whose mode is `0644` (assert the error message mentions permissions). For (e) a correct 0600 ed25519 key it returns the key and nil error. The trust-root loader (`loadTrustRoots(path)`) on (f) a file with two concatenated valid ed25519 `PUBLIC KEY` blocks returns both keys; on (g) a file where the second block is an RSA public key it returns an error and **no** keys (fail closed, no partial skip).

### TC-021-04: the live Run() path presents the pinned host-signed shape to vault

- **Requirement:** REQ-021-03
- **Type:** integration (recording stub vault socket; skips without bwrap only for the spawn tail, the inject happens pre-spawn so no skip is needed if the run is allowed to fail after inject; prefer a full bwrap run when available)
- **Input:** `keygen` into a temp dir; a `RunRequest` with `run.tier: ""`, a small profile (e.g. one `NetConnect` entry), one `secret_refs` handle, `wiring.vault_socket` pointed at a recording stub (the task-011 pattern: a Unix listener decoding one JSON line), `wiring.attestation_key` set to the generated key.
- **Expected:** the recorded inject request has `op:"inject"`, the original `handle`, the original `mode`, and `sandbox_identity` with **exactly** the seven pinned keys. Assert: `sandbox_id` matches `^sbx-[0-9a-f]{6}$`; `tier == "bubblewrap"` (normalization of `""`); `profile_digest` equals the test's own lowercase-hex sha256 of `json.Marshal` of the same profile map; `created_at` parses as RFC3339 UTC; `nonce` matches `^[0-9a-f]{32}$`; `attestation_format == "host-ed25519/v2"`; `attestation` matches `^[0-9a-f]{128}$`. Assert the string `attestation_pubkey` is absent from the identity map. This is the producer→consumer live-path proof: the assertion runs against what `Run()` actually sent, not against a hand-built map.

### TC-021-05: the recorded signature verifies under the trust root (and only via the v2 preimage)

- **Requirement:** REQ-021-03
- **Type:** integration (continuation of TC-021-04's recorded identity)
- **Input:** the identity recorded in TC-021-04; the trust root generated alongside the key.
- **Expected:** `verifyHostAttestation(identity, roots)` returns true. Reconstruct the preimage manually in the test (own code, not the production helper): `"exec-sandbox/attestation/v2\n"` + LP of the five fields in pinned order; `ed25519.Verify(root, preimage, sig)` is true. This double-derivation pins the byte layout: if the implementation and the spec disagree on ordering or LP, this test fails.

### TC-021-06: per-field tamper breaks verification

- **Requirement:** REQ-021-04
- **Type:** unit (negative, table-driven over a freshly minted valid identity)
- **Input:** seven variants, each mutating exactly one thing then re-running `verifyHostAttestation` with the correct trust root: (a) `sandbox_id` byte flipped; (b) `tier` changed `bubblewrap`→`gvisor`; (c) one hex digit of `profile_digest` changed; (d) `created_at` shifted by one second (re-formatted RFC3339); (e) one hex digit of `nonce` changed; (f) one hex digit of `attestation` changed; (g) `attestation_format` changed to `"host-ed25519/v1"`.
- **Expected:** **false for every variant**, and true for the untampered control identity asserted in the same test (guards against a vacuously-false verifier, per the vacuous-test retro). Also: a variant (h) that **adds** an `attestation_pubkey` of an attacker keypair which validly signs the tampered preimage must still return false, proving host-mode verification never consults in-identity key material.

### TC-021-07: a wrong signing key fails against the trust root

- **Requirement:** REQ-021-04
- **Type:** unit (negative)
- **Input:** mint a structurally perfect v2 identity under keypair B; verify against a trust root containing only keypair A's public key. Then verify the same identity against a trust root containing **both** A and B.
- **Expected:** false against A-only (wrong key is rejected even though the identity is internally consistent); true against A+B (the try-each-key rotation semantics work). Both assertions in one test so the negative is provably not a formatting artifact.

### TC-021-08: configured-but-broken key fails closed before side effects

- **Requirement:** REQ-021-05
- **Type:** integration (negative; recording stub vault + recording stub audit sockets)
- **Input:** a `RunRequest` with one `secret_refs` handle, both stub sockets wired, and `wiring.attestation_key` set to (a) a nonexistent path, then (b) a 0644-mode valid key.
- **Expected:** for both variants, `Run()` returns a map whose only key is `error`, with the value prefixed `attestation signing key: `; the vault stub recorded **zero** connections, the audit stub recorded **zero** events (no `spawn` emit), and no per-run proxy socket / work dir was created. No fallback mint occurs: nothing that looks like an identity ever reaches the stubs.

### TC-021-09: unconfigured keeps ADR 014 self-attestation byte-shape-identical (transitional)

- **Requirement:** REQ-021-06
- **Type:** integration (recording stub vault socket)
- **Input:** the TC-021-04 request with `wiring.attestation_key` omitted entirely (and again with explicit `""`).
- **Expected:** the recorded `sandbox_identity` has exactly the five ADR 014 keys `{sandbox_id, nonce, ts, attestation_pubkey, attestation}`; `verifyAttestation(identity)` (the v1 helper) returns true; none of `attestation_format`/`tier`/`profile_digest`/`created_at` is present. This is the backward-compat proof vault relies on during the transition window.

### TC-021-10: the host private key never leaks

- **Requirement:** REQ-021-07
- **Type:** integration (sentinel-based, mirrors TC-011-05 but for a persistent key)
- **Input:** a full run (bwrap present; skip otherwise) with `wiring.attestation_key` set; capture the result map, every audit event, the spawn argv (via the `spawnArgvFn` seam, `run.go:47`), and stdout. Derive the leak needles from the key file: the raw PKCS#8 DER bytes, their lowercase hex, their base64, and the 32-byte ed25519 seed's hex.
- **Expected:** no needle appears in any captured surface, and the key file's path is not among the bwrap argv's `--bind`/`--ro-bind` sources (the sandbox cannot read the key). Only the signature and the public trust root are ever externally visible.

### TC-021-11: `verify-attestation` subcommand is a working oracle

- **Requirement:** REQ-021-08
- **Type:** integration (built binary)
- **Input/Expected:** with the fixtures of TC-021-12: `exec-sandbox verify-attestation testdata/attestation/trust-root.pub < identity-valid.json` exits 0 and prints `ok`; the same invocation with each of `identity-tampered-*.json` and `identity-wrong-key.json` exits 1 with a non-empty stderr line; `exec-sandbox verify-attestation` (no arg) exits 2; a nonexistent trust-root path exits 1 with a stderr reason; garbage on stdin exits 1. Exit codes must not be conflated (assert the exact code, not just non-zero).

### TC-021-12: fixtures are committed, self-describing, and honest

- **Requirement:** REQ-021-09
- **Type:** unit (table-driven over the fixture files)
- **Input:** `testdata/attestation/` containing at minimum: `signing.key` (test-only, its comment header says so), `trust-root.pub`, `identity-valid.json`, `identity-tampered-sandbox-id.json`, `identity-tampered-signature.json`, `identity-wrong-key.json`.
- **Expected:** a test loads `trust-root.pub` and asserts `verifyHostAttestation` returns **true** for `identity-valid.json` and **false** for each `identity-tampered-*.json`/`identity-wrong-key.json`; a second assertion regenerates `identity-valid.json`'s signature from `signing.key` and the file's own attested fields and gets the identical 128-hex `attestation` (ed25519 is deterministic), proving the fixture set is internally consistent, not hand-edited. These files are the conformance vectors vault task 010 copies.

### TC-021-13: spec rewritten in place, same commit

- **Requirement:** REQ-021-10
- **Type:** inspection (spec)
- **Input:** the feat commit's versions of `docs/spec/data-model.md`, `docs/spec/configuration.md`, `docs/spec/interfaces.md`, `docs/CONTRACT.md`, `docs/spec/behaviors.md`.
- **Expected:** `data-model.md` `sandbox_identity` documents **both** modes (host-signed v2 as primary with the full seven-key shape + preimage; self-attestation explicitly labeled transitional/unconfigured-only) with no future tense; `configuration.md`'s wiring table gains the `wiring.attestation_key` row (type, default `""`, fail-closed effect); `interfaces.md`'s CLI table gains `keygen` and `verify-attestation` rows with their exit codes; `CONTRACT.md`'s vault.inject paragraph mentions the host-signed identity and the trust-root file; `behaviors.md`'s inject behavior row (~line 42, currently `{sandbox_id, attestation}`) reflects the new shape. All rewritten in place, none appended.

---

## Post-implementation verification

- [ ] TC-021-01: keygen pair, modes, output lines, no-overwrite, usage exit
- [ ] TC-021-02: openssl interop (or recorded skip)
- [ ] TC-021-03: all loader fail-closed variants incl. permissions and multi-PEM
- [ ] TC-021-04: live Run() presents the exact seven-key host shape, no `attestation_pubkey`
- [ ] TC-021-05: recorded signature verifies; test-side preimage re-derivation matches
- [ ] TC-021-06: all seven tamper variants false + control true + in-identity-key variant false
- [ ] TC-021-07: wrong key false; two-key trust root true
- [ ] TC-021-08: fail closed before proxy/vault/audit side effects, both broken-key variants
- [ ] TC-021-09: unconfigured path byte-shape-identical to ADR 014
- [ ] TC-021-10: no key-material needle on any surface; key file not mounted
- [ ] TC-021-11: verify-attestation exit codes 0/1/2 exactly
- [ ] TC-021-12: fixture verdicts match filenames; valid fixture reproducible from signing.key
- [ ] TC-021-13: five docs rewritten in place, present tense

## Test framework notes

- Standard Go `testing`, stdlib only (`crypto/ed25519`, `crypto/x509`, `encoding/pem`, `crypto/sha256`): no new dependency, F-003 preserved.
- Reuse the recording stub vault socket pattern from `attestation_test.go`/task 011 for TC-021-04/05/08/09; add a recording stub audit socket for TC-021-08 (same JSON-line listener).
- Keep mint and verify on one shared `attestationPreimageV2` helper, but TC-021-05 must **re-derive the preimage independently in test code**; otherwise a byte-layout bug passes both sides (the dead-wire/shared-helper blind spot).
- TC-021-04/09 assert on the identity **recorded at the socket during a real `Run()`**, never on a directly-constructed map: the constructor-wiring retro (correct component built, live path still on the old one) is the exact failure mode this guards.
- CLI TCs build the binary once via `go build -o` into `t.TempDir()` (or reuse the repo's existing binary-driving test idiom from `fclaunch_testmain_test.go`).
- Suggested new files: `attestation_v2_test.go` (or extend `attestation_test.go`), `keygen.go` + `keygen_test.go`, fixtures under `testdata/attestation/`. `attestation.go` gains the v2 helpers; `run.go` gains only the wiring field + the branch; `main.go` gains the two subcommand dispatches.
