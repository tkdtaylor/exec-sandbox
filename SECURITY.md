# Security Policy

## Supported versions

exec-sandbox has not yet cut a tagged release. Until a `v1.0.0` ships, only the
current `main` branch receives security fixes. This table will be filled in once
releases begin.

| Version | Security fixes |
|---------|---------------|
| `main` (pre-release) | ✅ Yes |

## Reporting a vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**
A public report exposes the flaw to everyone before a fix is available.

### Option 1 — GitHub private vulnerability reporting (preferred)

Use GitHub's built-in private advisory flow:
<https://github.com/tkdtaylor/exec-sandbox/security/advisories/new>

GitHub keeps the report confidential and notifies only maintainers.

### Option 2 — Email

Send a report to <tools@taylorguard.me> with:

- A concise description of the vulnerability
- Reproduction steps (payload, profile, tier, command line)
- The commit or `main` state you observed it on
- Your assessment of severity (CVSS or plain English is fine)
- Any suggested mitigations

Encrypt with PGP if you prefer — open an issue requesting a public key and
we will publish one.

## Response expectations

- **Acknowledgement:** within 7 days of receipt.
- **Status update:** within 30 days (triaged, confirmed, or declined with
  reasoning).
- **Fix shipped:** within 90 days for confirmed vulnerabilities. Critical
  issues (CVSS ≥ 9.0) target a 14-day patch window. If more time is needed
  we will coordinate a disclosure date with the reporter.

## Scope

A sandbox-escape or isolation bypass is the highest-severity class of bug here.

**In scope:**

- Any escape from the sandbox to the host: breaking out of the rootless-Podman
  container, the `--cap-drop=all` / non-root / `no-new-privileges` posture, or the
  tiered runtime (`runc` → gVisor `runsc` → Kata/Firecracker)
- Network isolation bypass: reaching the network despite "no network", or
  defeating the host-side egress proxy's domain allowlist (direct-IP, DNS
  rebinding, proxy bypass)
- Secret exposure: a payload reading injected credentials it should not see,
  especially defeating proxy-mode injection (where the secret never enters the
  sandbox)
- Attestation/signature forgery over the `run` result (`sandbox_status`,
  stdout/stderr integrity) where signing is wired

**Out of scope:**

- Vulnerabilities in the ecosystem blocks consumed over their contracts
  (`vault`, `policy-engine`, `audit-trail`) — report those to their repositories
- Bugs in the underlying container runtimes (Podman, runc, gVisor, Kata) or the
  host kernel — report upstream (we will help coordinate)
- Resource-exhaustion of the host by a payload within its configured limits
- Findings that require an already-compromised host or operator-supplied
  malicious profile

## Recognition

Reporters are credited in the changelog and release notes unless they
request anonymity. We do not currently offer a bug bounty.

## Maintainer note

After merging this file, enable **Settings → Code security and analysis →
Private vulnerability reporting** in the GitHub repository settings so the
"Report a vulnerability" button is visible on the repo page.
