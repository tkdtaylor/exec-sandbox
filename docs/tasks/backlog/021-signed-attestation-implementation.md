# Task 021: host-signed sandbox_identity attestation (implement the trust-root ADR)

**Status:** ⬜ backlog
**Branch:** `task/021-signed-attestation-implementation`
**Spec:** [`docs/tasks/test-specs/021-signed-attestation-implementation-test-spec.md`](../test-specs/021-signed-attestation-implementation-test-spec.md)
**ADR:** the task-020 trust-root ADR (next available number, 017 at time of writing), written and committed by task 020 **before** this task starts. This task adds no new ADR; it implements that one. If the committed ADR deviates from any literal below, the ADR wins: reconcile the test spec first, then implement.

## Readiness

**⛔ Blocked on task 020** (the ADR). Once 020 is merged this is **📋 READY**: every decision is pinned, no open questions remain. Vault's task 010 (sibling repo) is being built in parallel against the same ADR; this task's `testdata/attestation/` fixtures and `verify-attestation` subcommand are its conformance vectors, so land them exactly as pinned.

History (get it right): task 011 (`docs/tasks/completed/011-attestation-signatures.md`) already replaced the pre-v1 random-bytes attestation with **ephemeral per-run ed25519 self-attestation** (ADR 014, `attestation.go`). This task implements ADR 014's **option (b)**, host-key signing, per its reopening condition (2), triggered by vault task 010's trust-root verification. The self-attestation path survives only as the documented **transitional** behavior when no key is configured; nothing in this task reintroduces or preserves random-bytes attestations (they no longer exist).

## Problem

`Run()` (`run.go:72-80`) mints `sandbox_identity` via `mintAttestation(sandboxID)`: a fresh keypair per run whose public key travels **inside** the identity. Vault can check internal consistency but can never distinguish "minted by this host's exec-sandbox" from "minted by anyone", because the verify key is attacker-presentable. Vault task 010 verifies against an operator-published trust root, which requires the signature to chain to a **host-held key** the identity does not carry. The identity also attests nothing about `tier` or the profile, both of which vault's binding decision wants pinned.

## Scope

All literals below restate the task-020 ADR pins; the test spec's "Pinned contract literals" section is the single normative copy.

- **`attestation.go` gains the v2 path** (keep the v1 helpers untouched for the transitional path):
  - `attestationPreimageV2(sandboxID, tier, profileDigest, createdAt, nonce string) []byte`: `"exec-sandbox/attestation/v2\n" + LP(sandbox_id) + LP(tier) + LP(profile_digest) + LP(created_at) + LP(nonce)`, reusing the existing 4-byte big-endian `LP` loop. One shared helper for mint and verify, same discipline as v1.
  - `profileDigest(profile map[string]any) string`: lowercase-hex sha256 of `json.Marshal(profile)` (nil map marshals to `null`, digest those 4 bytes).
  - `mintHostAttestation(priv ed25519.PrivateKey, sandboxID, tier string, profile map[string]any) map[string]any`: normalizes tier (`""` ⇒ `"bubblewrap"`), stamps `created_at` (`time.Now().UTC().Format(time.RFC3339)`) and a fresh 16-byte hex `nonce`, signs the v2 preimage, returns the seven-key map `{sandbox_id, tier, profile_digest, created_at, nonce, attestation_format: "host-ed25519/v2", attestation}`. **No `attestation_pubkey`.**
  - `verifyHostAttestation(identity map[string]any, roots []ed25519.PublicKey) bool`: requires `attestation_format == "host-ed25519/v2"` and all five attested fields as strings, rebuilds the v2 preimage, hex-decodes the 64-byte signature, accepts iff `ed25519.Verify` passes under **any** trust-root key. Never consults any in-identity key material.
  - `loadSigningKey(path string) (ed25519.PrivateKey, error)`: PEM ⇒ `x509.ParsePKCS8PrivateKey` ⇒ must be ed25519; **fails closed** on missing/garbage/wrong-algorithm files and on file mode with any group/other bits (0600 or stricter required).
  - `loadTrustRoots(path string) ([]ed25519.PublicKey, error)`: parses every concatenated PEM `PUBLIC KEY` block via `x509.ParsePKIXPublicKey`; any non-ed25519 block fails the whole load (no partial skip).
- **`run.go` wiring:**
  - Add `AttestationKey string \`json:"attestation_key"\`` to `RunRequest.Wiring` (alongside `VaultSocket`, `run.go:34-40`).
  - In `Run()`, branch where `mintAttestation` is called today (`run.go:77`): non-empty `wiring.attestation_key` ⇒ `loadSigningKey`; on error return `{"error": "attestation signing key: <detail>"}` **before** the spawn audit emit, snapshot/proxy start, and any `vaultInject` (same pre-side-effect ordering as `validateWorkdir`/`validateFileReads`); on success ⇒ `mintHostAttestation(priv, sandboxID, req.Run.Tier, req.Run.Profile)`. Empty ⇒ `mintAttestation(sandboxID)` exactly as today (transitional). `vaultInject` (`run.go:529`) is unchanged: the identity map shape is the only thing that varies.
- **`main.go` subcommands** (new file `keygen.go` for the logic is fine):
  - `keygen <dir>`: generate one ed25519 keypair; write `<dir>/attestation-signing.key` (PEM PKCS#8, 0600) and `<dir>/attestation-trust-root.pub` (PEM PKIX, 0644); refuse to overwrite either (exit 1); print exactly `signing_key=<abs path>` and `trust_root=<abs path>`; wrong arg count exit 2.
  - `verify-attestation <trust-root.pub>`: read one identity JSON object on stdin; exit 0 + `ok` on stdout when `verifyHostAttestation` passes; exit 1 + reason on stderr on verification failure or unreadable/unparseable trust root/stdin; exit 2 on usage error. This is vault's executable oracle and the L6 observation surface.
- **Fixtures `testdata/attestation/`** (committed; vault task 010 copies them): `signing.key` (test-only, marked as such in a comment header), `trust-root.pub`, `identity-valid.json`, `identity-tampered-sandbox-id.json`, `identity-tampered-signature.json`, `identity-wrong-key.json`. A test proves each file's verdict matches its name and that `identity-valid.json` is reproducible from `signing.key` (TC-021-12).
- **No secret leak:** the host private key (a persistent file, a stricter surface than 011's ephemeral key) enters none of result/audit/argv/sandbox env/stdout, and the key file is never among the sandbox mounts (TC-021-10). Mirrors the F-002 discipline.
- **Stdlib only:** `crypto/ed25519`, `crypto/x509`, `encoding/pem`, `crypto/sha256`. No new dependency (F-003 preserved).
- **Spec update in the same feat commit** (this is where the contract becomes present-tense true, deferred from task 020 by the no-future-tense rule): `docs/spec/data-model.md` `sandbox_identity` rewritten for both modes (host v2 primary, self-attestation labeled transitional); `docs/spec/configuration.md` wiring table + `wiring.attestation_key` row; `docs/spec/interfaces.md` CLI table + `keygen`/`verify-attestation` rows and exit codes; `docs/CONTRACT.md` vault.inject paragraph; `docs/spec/behaviors.md` inject row (~line 42). Rewritten in place, no appends, no future tense. `diagrams.md` only if a diagram names the attestation flow (none does today; check before skipping).

Out of scope: removing the transitional self-attestation path (a follow-on flips it to fail-closed once vault requires host mode ecosystem-wide; the ADR records the condition); workload attestation (payload hash/binding stays ADR 014 reopening condition (1)); vault's freshness/replay policy (`created_at` window, `nonce` uniqueness are the consumer's checks, vault task 010); key rotation tooling beyond multi-key trust-root support in `loadTrustRoots`; changing the audit-event shape; any change to `proxy.go`/`gvisor.go`/`firecracker.go`/`snapshot.go`/`seccomp.go`.

## Verification plan

- **Highest level achievable: L6 (live binary observed).** The crypto core is pure L2; the live path is L5 against recording stub vault/audit sockets (identity recorded from a real `Run()`); L6 is the built binary driven end to end: `keygen` ⇒ `run` with `wiring.attestation_key` ⇒ pipe the recorded identity into `verify-attestation` ⇒ `ok`, exit 0.
- **Harness commands:**
  - `go test -count=1 -run 'Attestation|Keygen|VerifyAttestation|TrustRoot|HostSigned' ./...`
  - `go test -count=1 ./...` and `gofmt -l .`
- **Runtime observation (L6):** paste the `keygen` stdout lines and the two file modes (`stat -c '%a'`); paste a real run's recorded inject JSON showing the seven-key identity (and no `attestation_pubkey`); paste `verify-attestation` returning `ok`/exit 0 on it and a non-zero exit + stderr reason on a tampered copy; paste the fail-closed run's `{"error":"attestation signing key: ..."}` with the stub sockets confirming zero contacts.
- Record in `Verified by`: which TCs ran un-skipped (bwrap/openssl availability) and the exact commands.

## Definition of done

- All TC-021-01..13 assertions pass as written (per-field tampers, wrong-key, fail-closed ordering, transitional byte-shape, non-leak, CLI exit codes, fixture honesty); no assertion downgraded to a smoke test.
- With a configured key, a real `Run()` presents the pinned seven-key host-signed identity to `vault.inject` and it verifies under the published trust root; with no key, the ADR 014 shape is byte-shape-identical to today.
- A configured-but-unreadable/malformed/too-permissive key aborts the run before any side effect; there is no silent fallback.
- `keygen` + `verify-attestation` behave per the pinned CLI contract; `testdata/attestation/` fixtures committed and proven consistent.
- Stdlib-only preserved; the host private key leaks nowhere; the key file is not sandbox-visible.
- `data-model.md`, `configuration.md`, `interfaces.md`, `CONTRACT.md`, `behaviors.md` rewritten in place in the feat commit.
- `go test -count=1 ./...` green; `gofmt -l .` clean; protected files (`proxy.go`, `gvisor.go`, `firecracker.go`, `snapshot.go`, `seccomp.go`) zero-diff.
- spec-verifier APPROVE before promotion to ✅.

## Dependencies

- **Depends on:** task 020 (the trust-root ADR) merged first. Hard gate: do not start without it.
- **Consumed by:** vault task 010 (sibling repo, parallel): verifies the host-signed identity against the operator-published `attestation-trust-root.pub`, using this task's fixtures + `verify-attestation` as conformance vectors.
- **Amends at runtime:** ADR 014's minting path (option (a) demoted to the transitional unconfigured case); supersedes nothing in task 011's tests, which keep guarding the v1 path.
