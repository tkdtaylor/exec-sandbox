# exec-sandbox v1 contract

Mirrors `interface-contracts.md §2`, validated by the
tracer-bullet (A1/A2/A3).

## run(payload, profile, tier, secret_refs) -> result
```
profile = { capabilities:[ {type:NetConnect, allowlist:["host:443"]},
                            {type:FileRead, paths:[…]}, … ],
            limits:{ cpu_count, memory_mb, disk_mb, timeout_sec } }
tier    = bubblewrap | gvisor | firecracker        # v0 = bubblewrap
secret_refs = [ handle ]                            # opaque; exec-sandbox calls vault.inject

result = { stdout, stderr, exit_code,
           sandbox_status:{ sandbox_id, tier, duration_ms, secrets_injected:[…], status } }
```

## vault.inject (called by exec-sandbox at spawn)
Pull-triggered push: present `{handle, sandbox_identity}`. In proxy mode vault returns
`{credential, binding:{host,header,scheme}}`; exec-sandbox loads it into the egress proxy
(never the sandbox). See vault's contract.

## Network boundary
`bwrap --unshare-all` → no network namespace. Bind-mounted `/proxy.sock` is the only egress.
The proxy enforces the domain allowlist (from `profile.NetConnect`) and injects credentials
into allowlisted requests. v0 = HTTP over the Unix socket; v1 adds TLS-terminating + SOCKS5.
