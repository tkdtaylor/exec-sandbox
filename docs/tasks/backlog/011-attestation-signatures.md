# Task 011: signed sandbox_identity attestation

**Status:** ⬜ backlog
**Branch:** `task/011-attestation-signatures`
**Spec:** [`docs/tasks/test-specs/011-attestation-signatures-test-spec.md`](../test-specs/011-attestation-signatures-test-spec.md)
**ADR:** ADR 011 — **REQUIRED BEFORE IMPLEMENTATION** (see Readiness).

## Readiness — BLOCKED ON ADR + CROSS-BLOCK CONTRACT CHECK

**This task is not ready to execute as pure implementation.** Two preconditions must be resolved
first, in ADR 011:

1. **Trust root / signing key (open decision).** Pick the signing key and trust model:
   - **(a) Ephemeral per-process ed25519 self-attestation** — stdlib `crypto/ed25519`, no
     key-management infra, the public key travels in `sandbox_identity` (recommended for v1 *iff*
     it satisfies vault). Simplest; preserves stdlib-only (F-003).
   - **(b) Host-provided key** — a longer-lived trust root vault already trusts (a host/CA key).
     Required if vault's trust decision must chain to a key **it** controls.
2. **Vault consumer contract check (load-bearing).** The attestation is handed to **vault** via
   `vault.inject(handle, sandbox_identity, mode)`. **Confirm how vault consumes `attestation`
   before choosing.** If vault expects to verify the signature against a key it already trusts,
   ephemeral self-attestation is a no-op for vault's trust decision and option (b) is mandatory. If
   the vault consumer contract **cannot be confirmed** from the available block contracts
   (`interface-contracts.md`, the vault block repo), ADR 011 records
   the assumption explicitly and **this task stays blocked** until the contract is confirmed — do
   not paper over the unknown by shipping self-attestation and hoping.

**Verdict for the planner/operator:** ready to execute **only after** ADR 011 is written and the
vault-consumer contract is confirmed (or the assumption is explicitly recorded and accepted). If
the operator wants to unblock with the recommended path, the fast route is: confirm vault treats
`attestation` as opaque/self-verifying (no host-key requirement) → ADR 011 picks ephemeral ed25519
self-attestation → implement.

## Problem

`docs/spec/data-model.md` (~line 196) flags: *"the attestation is currently random bytes, not a
signed attestation; v1 adds signatures per the README."* `Run()` mints
`{"sandbox_id": sandboxID, "attestation": randHex(16)}` (`run.go:67`) — 16 random bytes that prove
nothing and verify against nothing. The README lists *"sandbox_identity attestation signatures"* as
deferred v1 work. A real attestation lets the consumer (vault) verify that the identity presented at
`vault.inject` was produced by exec-sandbox and binds to a specific `sandbox_id`.

## Scope

(Implementation begins **after** ADR 011 resolves the trust root and the vault contract.)

- **Replace the random-bytes attestation** (`run.go:67`, the `randHex(16)` for `attestation`) with a
  **signed attestation**: a signature over the documented attested fields (at minimum `sandbox_id`;
  likely a timestamp/nonce for replay resistance — fixed in ADR 011) using the chosen key, in a
  canonical encoding the verifier can reconstruct.
- **Provide a verify helper** (`verifyAttestation(identity, pubkey) bool`) and, for self-attestation,
  carry the public key in `sandbox_identity` so vault can verify internal consistency.
- **Mint per run**, alongside `sandbox_id`. Keep the minting in one place so the attested-fields
  preimage is constructed identically by mint and verify (a shared helper).
- **Preserve the IPC contract.** `vault.inject` still receives
  `sandbox_identity:{sandbox_id, attestation, …}` (plus any ADR-011-added field like the public
  key); `handle`/`mode` unchanged; the JSON-line shape stays parseable by vault.
- **No secret leak.** The signing **private** key (if ephemeral) never enters the result, the audit
  events, the sandbox env/args, the payload, or stdout — only the public key + signature are
  externally visible (mirrors the F-002 credential discipline).
- **Stdlib only.** Use `crypto/ed25519` — no third-party dependency (preserves F-003).
- **Spec update in the same commit:** `docs/spec/data-model.md` `sandbox_identity` section — document
  the signed attestation (shape, attested fields, verify-key source) and **remove the TODO**; update
  the `vault.inject` request shape if a field (e.g. the public key) is added. Rewrite in place, no
  future tense.

Out of scope: a persistent / CA-backed key hierarchy beyond what ADR 011 chooses; attesting the
payload contents or the tier (attest the identity, not the workload — note as ADR 011's reopening
condition); changing the audit-event shape.

## Verification plan

- **Highest level achievable: L2 (unit) — the attestation mint/verify/tamper logic is pure and
  fully unit-testable; vault-presentation is observable against a stub vault socket (L5).** No
  `bwrap` is needed for the core property (sign → verify → tamper-fails). Flag in `Verified by` that
  the cryptographic property is unit-proven and the IPC-presentation is stub-observed.
- **Harness command:** `go test -count=1 ./...`.
- **Runtime observation (L2/L5):** the mint produces a signature that `verifyAttestation` accepts;
  three tamper variants (mutated `sandbox_id`, mutated signature, mutated nonce) all fail
  verification; a run with a `secret_ref` presents a well-formed `sandbox_identity` to a recording
  stub vault socket; the private key appears in none of result/audit/argv/stdout.
- **ADR 011 written FIRST:** settles the trust root (ephemeral ed25519 self-attestation vs host
  key), the attested fields + canonical encoding, the vault-consumer contract confirmation (or the
  recorded assumption + blocked status), and the reopening condition (workload attestation).

## Definition of done

- ADR 011 written; vault-consumer contract confirmed (or assumption recorded + the task was
  unblocked by the operator).
- `sandbox_identity.attestation` is a verifiable signature over the documented attested fields;
  tampering with any attested field or the signature breaks verification.
- `vault.inject` receives a well-formed `sandbox_identity`; the IPC contract is intact.
- The signing private key (if ephemeral) leaks into none of result/audit/sandbox/stdout; stdlib-only
  (`crypto/ed25519`), F-003 preserved.
- `docs/spec/data-model.md` updated, **TODO removed**, vault.inject shape reflects any added field —
  in the feat commit, rewritten in place.
- `go test -count=1 ./...` green; `gofmt -l .` clean.
- spec-verifier APPROVE before promotion to ✅.
