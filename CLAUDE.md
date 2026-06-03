# exec-sandbox — project instructions

OS execution isolation. Runs untrusted agent-generated code with no network; the only egress
is a credential-injecting proxy on a Unix socket. Go. PolyForm Noncommercial 1.0.0.

## Invariants

- The sandbox has **no network** (`bwrap --unshare-all`). Its only path out is the
  bind-mounted `/proxy.sock`. Never add a `--share-net` or a direct route.
- **exec-sandbox owns** the network boundary + egress proxy + allowlist. **vault owns**
  credential injection into the proxy. In proxy mode the credential value must never enter
  the sandbox (env, args, or stdout).
- exec-sandbox calls `vault.inject(handle, sandbox_identity, mode)` itself at spawn
  (pull-triggered push). The agent passes only opaque handles in `secret_refs`.

## Contract (v1)

`run(payload, profile, tier, secret_refs) -> {stdout, stderr, exit_code, sandbox_status}`.
Authoritative spec: `exec-sandbox.md` +
`interface-contracts.md` (v1). Validated by the tracer-bullet reference (A1–A3).

## Conventions

- `go build ./...` / `go test ./...` stay green. Integration tests skip if `bwrap` is absent.
- v0 = bubblewrap only. Add gVisor/Firecracker behind the OCI seam in v1 without changing
  the run() contract. Error shape `{error:{code,message,retryable}}`.
