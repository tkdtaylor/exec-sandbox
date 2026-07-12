# Task 020: attestation trust-root ADR (host ed25519 key, published trust root, vault consumer contract)

**Status:** ⬜ backlog
**Branch:** `task/020-attestation-trust-root-adr`
**Spec:** [`docs/tasks/test-specs/020-attestation-trust-root-adr-test-spec.md`](../test-specs/020-attestation-trust-root-adr-test-spec.md)
**ADR:** this task's deliverable IS the ADR. Next available number (017 at time of writing; re-check `ls docs/architecture/decisions/` first). It **amends ADR 014** through ADR 014's own reopening condition (2).

## Readiness

**📋 READY (docs-only, decision task).** The blocker that held task 011 ("an ADR + a vault-consumer contract on the trust root") was resolved once already by ADR 014 (ephemeral self-attestation), and ADR 014 reserved reopening condition (2) for exactly today's situation: *"if vault ... later requires the attestation signature to chain to a key it controls (a host/CA key) ... option (b) must be implemented. The reopening trigger is a documented vault contract change requiring host-key verification."* That trigger has fired: **vault's task 010** (sibling repo, running in parallel) verifies a signed attestation against a **published trust root** instead of treating `sandbox_id` as an opaque string. This task writes the exec-sandbox side of that contract down; task 021 implements it.

Get the history right, it is the trap in this task:

- Task 011 is **completed** (`docs/tasks/completed/011-attestation-signatures.md`), not blocked. Its readiness block is the origin of the "blocked on ADR + vault-consumer contract" phrasing.
- The attestation is **not random bytes anymore**. ADR 014 + `attestation.go` shipped per-run ephemeral ed25519 self-attestation (`mintAttestation`/`verifyAttestation`, shared `attestationPreimage`, domain `"exec-sandbox/attestation/v1\n"`, fields `{sandbox_id, nonce, ts}`, `attestation_pubkey` carried in the identity). Random bytes survive only as the `nonce`.
- `docs/plans/roadmap.md:38` still says item 011 is "⚠️ blocked"; that row is stale, refresh it in this task's commit.

## Problem

Vault currently validates handle binding against `sandbox_id` as an opaque string (SO_PEERCRED socket + single-use handle + first-use binding, per ADR 014's contract reading). Its task 010 upgrades that: vault will **cryptographically verify** the presented attestation against a trust root the operator publishes to it. Ephemeral self-attestation cannot serve that check: the verify key travels inside the attacker-presentable identity, so it proves internal consistency only, never "minted by the host's exec-sandbox" (ADR 014, Consequences, accepted-negative). A **host-held signing key with an out-of-band published public key** is required, which is precisely ADR 014's option (b), deferred behind reopening condition (2).

Nothing pins today: which key, where the private key lives, what bytes are signed, how the public key reaches vault, how keys rotate, or what JSON shape vault verifies. Task 021 (implementation) and vault task 010 (verification) both need those answers fixed in one normative place before either writes code.

## Scope

**Docs only. No code.** One new ADR (next available number, suggested filename `docs/architecture/decisions/017-attestation-trust-root.md`), containing every decision below, byte-precise. The paired test spec's TC-020-02..07 "Expected" blocks contain the exact recommended pins (field tables, preimage bytes, JSON shape, commands, verify algorithm); **copy them into the ADR rather than paraphrasing**. Summary of what gets pinned:

- **Trust root:** one long-lived host ed25519 keypair (operator-owned, per deployment), replacing the per-run ephemeral key for signing. Private key: PEM PKCS#8, created 0600, loader rejects wider perms. Generation: `exec-sandbox keygen <dir>` (task 021) or the documented openssl pair (`genpkey -algorithm ed25519` + `chmod 600` + `pkey -pubout`).
- **Key path into the binary:** new optional **`wiring.attestation_key`** field in the stdin `RunRequest` (string path, `""` = unconfigured). Not an env var, not a config file: `docs/spec/configuration.md` pins "no config files and reads no application environment variables", so wiring is the only compliant channel. Record that rationale.
- **What is signed:** attested fields `sandbox_id`, `tier` (normalized: `""` ⇒ `"bubblewrap"`), `profile_digest` (lowercase-hex sha256 of `json.Marshal(profile)`, nil profile digests the bytes `null`), `created_at` (RFC3339 UTC), `nonce` (16 random bytes, 32 lowercase hex). Canonical preimage: extend ADR 014's length-prefixed scheme under a new domain string, `"exec-sandbox/attestation/v2\n" + LP(sandbox_id) + LP(tier) + LP(profile_digest) + LP(created_at) + LP(nonce)`. Weigh RFC 8785 JCS and record its disposition (rationale in the test spec TC-020-04: no stdlib JCS, Go's JSON HTML-escaping is JCS-incompatible, LP already has house precedent + a shared mint/verify helper).
- **Published trust root for vault:** one file of one-or-more concatenated PEM PKIX/SPKI `PUBLIC KEY` blocks (all ed25519; a non-ed25519 block fails the load). The operator passes the **same file** to both binaries; vault task 010 consumes it as its verification anchor. No other publication channel.
- **Rotation:** append the new public key to the trust-root file, distribute, switch `wiring.attestation_key`, retire the old block after the freshness window; verification is try-each-key so no version field is needed.
- **Fail closed:** configured-but-unreadable/malformed/non-ed25519 key ⇒ hard `{error}` before any side effect (same pre-side-effect ordering as `validateWorkdir`/`validateFileReads`). Never a silent fallback to self-attestation.
- **Transitional path:** `wiring.attestation_key == ""` keeps ADR 014's ephemeral self-attestation byte-for-byte, explicitly labeled **transitional**, with the retirement condition written down.
- **Consumer contract (vault) section:** normative and self-contained: the exact host-signed `sandbox_identity` JSON (with `attestation_format: "host-ed25519/v2"`, **no** `attestation_pubkey`), the 6-step verify algorithm, mode discrimination against the transitional shape, and a pointer to task 021's `testdata/attestation/` fixtures as conformance vectors. Vault task 010 must be able to implement from this section alone.
- **Roadmap touch-up:** refresh the stale `docs/plans/roadmap.md` item-011 row (blocked ⇒ done via ADR 014; host trust root now tracked by tasks 020/021).

**Deviation note (deliberate, keep it):** the caller-level plan wanted "the contract addition to the repo's contract doc" in this task, but `docs/CONTRACT.md` and `docs/spec/*` are current-state snapshots and the repo forbids future-tense spec entries (AGENTS.md Never list). So the contract lives normatively in the ADR now, and `docs/CONTRACT.md`/`docs/spec/*` are rewritten in task 021's feat commit, in the same commit as the code that makes them true.

Out of scope: any `.go` change (task 021); any `docs/spec/` or `docs/CONTRACT.md` edit (task 021's feat commit); workload attestation (payload-hash binding stays ADR 014 reopening condition (1), untouched); vault's own freshness/replay policy (vault task 010 decides; the ADR only notes it is the consumer's call); key escrow/HSM/CA hierarchies.

## Verification plan

- **Highest level achievable: L2-equivalent (inspection).** A docs-only task has no runtime surface; "verified" means the spec-verifier walks TC-020-01..08 against the ADR text and the git diff, per assertion. Flag exactly that in `Verified by` (no fabricated L5/L6).
- **Harness command:** `go test -count=1 ./...` and `gofmt -l .` (must be trivially green: zero code changed), plus `git diff --stat main` showing the docs-only file set of TC-020-08.
- **Runtime observation:** none exists; the observable artifact is the ADR file content matching every literal in TC-020-02..07.

## Definition of done

- The ADR exists at the next free number, `Status: Accepted`, amends ADR 014 via reopening condition (2), cites completed task 011, and never claims the current attestation is random bytes.
- Every pin from the test spec is present byte-precisely: key type + PKCS#8 PEM + 0600 + perm-checking loader + both generation commands; `wiring.attestation_key`; the five attested fields + v2 length-prefixed preimage + JCS disposition; the multi-PEM PKIX trust-root file passed to both binaries; rotation; fail-closed; transitional label + retirement condition.
- The "Consumer contract (vault)" section is complete and self-contained (JSON shape, 6-step verify algorithm, mode discrimination, fixtures pointer): the artifact vault task 010 codes against.
- `docs/plans/roadmap.md` item-011 row refreshed.
- Diff is docs-only (TC-020-08); `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE before promotion to ✅.

## Dependencies

- **Blocks:** task 021 (implementation) must not start until this ADR is committed.
- **Consumed by:** vault task 010 (sibling repo, parallel) implements verification from this ADR's consumer-contract section and the operator-published trust-root file.
- **Amends:** ADR 014 (reopening condition 2). **Supersedes the readiness question of:** task 011 (completed).
