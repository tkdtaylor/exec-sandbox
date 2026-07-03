# exec-sandbox — Agent briefing (canonical)

This is the **canonical, harness-neutral briefing** for exec-sandbox. It is the
single source of truth for project context, commands, architectural invariants,
the task workflow, verification expectations, commit rules, and the load-bearing
process rules every agent must follow.

Every coding-agent harness loads this file:

- **Codex** auto-loads `AGENTS.md` (this file). The Codex-specific short commands
  are at the end under *Harness notes — Codex*.
- **Antigravity / Gemini** load it via `GEMINI.md` (a symlink to this file).
- **Claude Code** loads `CLAUDE.md`, which imports this file (`@AGENTS.md`) and adds
  the Claude-specific mechanics (skills, subagents, hooks).

Keep this file harness-neutral. Anything that only one harness understands belongs
in that harness's layer (`CLAUDE.md` for Claude Code), not here.

## What this is

exec-sandbox is the **OS execution-isolation block** of the secure-agent ecosystem.
It runs untrusted agent-generated code with **no network**; the only egress is a
credential-injecting proxy on a Unix socket. Go. Apache-2.0.

It is a single-binary Go CLI: `exec-sandbox run` reads a JSON `RunRequest` on stdin
and writes a JSON result on stdout. v0 implements **Tier-1 isolation (bubblewrap)
only**, behind a `tier` seam wired to accept `gvisor` and `firecracker` in later
versions without changing the `run()` contract.

## Invariants

These are load-bearing — violating one breaks the security model, not just style:

- The sandbox has **no network** (`bwrap --unshare-all`). Its only path out is the
  bind-mounted `/proxy.sock`. Never add a `--share-net` or a direct route.
- **exec-sandbox owns** the network boundary + egress proxy + allowlist. **vault owns**
  credential injection into the proxy. In proxy mode the credential value must never
  enter the sandbox (env, args, or stdout).
- exec-sandbox calls `vault.inject(handle, sandbox_identity, mode)` itself at spawn
  (pull-triggered push). The agent passes only opaque handles in `secret_refs`.

## Contract (v1)

`run(payload, profile, tier, secret_refs) -> {stdout, stderr, exit_code, sandbox_status}`.
Authoritative contract reference: [`docs/CONTRACT.md`](docs/CONTRACT.md) (v1).
Validated by the tracer-bullet reference (A1–A3).
Structured current-state snapshot: [`docs/spec/`](docs/spec/).

## Project structure

```
main.go       ← CLI entrypoint — `exec-sandbox run` reads a JSON RunRequest from stdin
run.go        ← Run() orchestration: allowlist parse → snapshot → vault.inject → proxy → backend → audit
proxy.go      ← host-side egress proxy (Unix socket, domain + per-host verb allowlist, credential injection)
gvisor.go     ← Tier-2 gVisor/runsc backend: OCI bundle/spec generation behind the tier seam
limits.go     ← profile.limits parsing + host-side output-cap writer (cpu/mem/pids/disk/timeout/output)
snapshot.go   ← snapshot/restore reset boundary: pristine per-run baseline, teardown, leak-proof restore
*_test.go     ← integration + unit tests (sandbox tests skip when bwrap is absent)
docs/         ← spec + planning + history (the source-of-truth side)
  CONTRACT.md     v1 contract reference (mirrors the ecosystem's v1 interface contract §2)
  spec/           authoritative current-state snapshot — SPEC.md, behaviors, architecture, data-model, interfaces, configuration, fitness-functions
  architecture/   narrative overview, diagrams.md, ADRs
  agent-rules.md  process rules + project retros (the growing log of lessons)
  tasks/          active, backlog, completed task files
    test-specs/   TDD specs — always written before implementation
```

This is a single-binary Go CLI; all source lives at the repo root (idiomatic for a small
`main`-package tool), not under `src/`. `docs/` is the input side (read before you act, and
the artifact that survives a rewrite); the `.go` files at root are the output side.

`docs/spec/` is **dual-natured** — it's the output of every task that changes externally-visible
behavior, *and* the input to onboarding, drift audits, and (in the limit) regenerating the
codebase from scratch. The code is one realization of the spec. Spec and code that disagree
means one of them is wrong; fix it in the same change.

## Tech stack

Go 1.26 (module `github.com/tkdtaylor/exec-sandbox`). Standard library only — no third-party
dependencies (`net/http`, `os/exec`, `encoding/json`, `crypto/rand`). External runtime
dependency: `bwrap` (bubblewrap) for Tier-1 isolation. vault and audit-trail are reached over
Unix-socket JSON-lines IPC.

## Commands

```bash
make build          # go build -o bin/exec-sandbox ./...
make test           # go test ./...   (integration tests skip without bwrap)
make fmt            # go fmt ./...
make clean          # rm -rf bin
go build ./...      # compile everything
go test ./...       # run tests directly
```

## Design principles

This project follows **Unix philosophy** as its default design approach — favoring
**composability over monolithic design**. Complex behavior should emerge from
combining small, independent components that communicate through standardized
interfaces, not by growing one large one. The full statement lives in
[docs/architecture/overview.md](docs/architecture/overview.md) under *Design
principles*; the short version is four structural properties to design for:

- **Modularity** — independent units that can be built, understood, and changed on
  their own
- **Interface standardization** — stable, well-defined contracts between components
  (typed signatures, versioned APIs, plain-text formats)
- **Maintainability** — changes in one module should not cascade across unrelated
  ones
- **Reusability** — components should be liftable into another project without
  entanglement

Derived working rules:

- **One thing, well** — each module, service, and function has a single clear
  responsibility
- **Small, composable pieces** over large configurable ones
- **Plain text** for configs, intermediate artifacts, and data interchange where
  possible
- **Explicit over implicit** — surface assumptions in code and types, not in
  comments
- **Fail fast, crash loudly** on unexpected state — never silently paper over it
- **Test in isolation** — every component runnable without the whole stack
- **Defer premature decisions** — no abstractions until the second or third concrete
  use case demands them

**Monolithic is a legitimate choice when deliberate** — the Linux kernel itself is
monolithic for good reasons (performance, correctness, tight internal coupling that
plug-ins would undermine). The same can apply to a hot-path runtime core, a state
machine, or a cryptographic primitive. The principle is "prefer composability at
user-facing or cross-module boundaries, and document any deviation with an ADR."
Accidental monolithic drift is not the same as a deliberate monolithic decision.

The `tier` seam (bubblewrap | gvisor | firecracker) is the deliberate composability
boundary here: new isolation backends plug in behind it without changing the `run()`
contract.

## Conventions

- Single Go `main` package at the repo root; no internal package split yet (the codebase is
  small enough that the handful of `.go` source files — `main.go`, `run.go`, `proxy.go`,
  `gvisor.go`, `limits.go`, `snapshot.go` — plus their `_test.go` files are the unit of
  organization).
- Task files are named `NNN-short-name.md` (zero-padded, sequential across all task states)
- Every task has a paired test spec; no implementation starts without one
- Tasks follow Unix philosophy — one task, one responsibility; break things smaller when in
  doubt
- ADRs live in `docs/architecture/decisions/` — add one whenever a significant design decision
  is made
- **Spec is updated in the same commit as the code change.** A task that changes
  externally-visible behavior, the data model, an interface, or configuration is not done until
  the matching `docs/spec/` file reflects the new state. Stale spec entries are rewritten in
  place — never appended to. The ADR carries the history; the spec carries the current truth.
- **Diagrams update with the code.** When a component boundary moves or a runtime flow changes,
  update `docs/architecture/diagrams.md` in the same commit.

## Working in this project

Every task lives on its own branch (or worktree under concurrent sessions). Working directly on
`main` is blocked by the `no-commit-on-main` hook — `scripts/start-task.sh` is how you pick the
right isolation for the moment.

> Note: this repo's default branch is `main`. The hooks treat the checked-out default branch
> as protected.

1. Start each session by reading the relevant task file (including its **Verification plan**)
   and its test spec
2. Check [docs/architecture/overview.md](docs/architecture/overview.md) for system context
3. Write the test spec before any implementation code
4. Implement via your harness's task-execution flow. Its Step 0 runs
   `scripts/start-task.sh <NNN> <slug>` to set up either:
   - `BRANCH task/NNN-<slug>` (solo session — the common case), or
   - `WORKTREE .claude/worktrees/NNN-<slug>/` (concurrent session detected; `cd` in)

   Commit at status **🟡 (code merged)** on the task branch.
5. After the executor returns, run the **spec-verifier** role on the task — it returns APPROVE
   or BLOCK based on per-assertion evidence
6. If spec-verifier APPROVEs **and** the verification plan's L5/L6 evidence is recorded
   (validation-harness output or runtime observation), promote the row to **✅ (verified)** in
   `coverage-tracker.md` in a **separate commit** titled `verify: confirm task NNN — <evidence>`
   (still on the task branch)
7. **Merge to the default branch** when ready: `git checkout main && git merge task/NNN-<slug>`.
   The `auto-cleanup-merge` hook then deletes the task branch and removes the worktree
   automatically. If the merge conflicts or you want to keep the branch, the hook surfaces a note
   and leaves it in place.
8. **Commit after each milestone** — never start the next task without committing the current one
   first. (No remote is configured; this repo is local-only during bootstrap, so there is nothing
   to push.)

The separation between the task branch and the default branch is the load-bearing rule for
multi-session safety. Two sessions on different `task/*` branches can work in parallel without
stepping on each other; two sessions both editing the default branch cannot.

The separation between 🟡 (feat commit) and ✅ (verify commit) is the load-bearing rule: it makes
"merged" and "verified" two distinct artifacts in git history, so neither can silently substitute
for the other. **Never** mark ✅ in the same commit as the feature work — the verification step
must be its own observable event.

## Commit rules

**You must commit after every milestone.** Do not batch multiple tasks into one commit. Do not
continue to the next task until the current one is committed. (No remote configured — local-only
repo; "push" steps are no-ops here.)

All commits below land on the **task branch** (`task/NNN-<slug>`), never on the default branch
directly. The merge to the default branch happens after the verify step, in a separate explicit
operation.

| Milestone | What to stage | Message | Branch |
|-----------|--------------|---------|--------|
| ADR written | `docs/architecture/decisions/NNN-*.md`, any superseded spec entries rewritten in `docs/spec/` | `docs: add ADR NNN — <decision title>` | task branch |
| Test spec written | `docs/tasks/test-specs/NNN-*-test-spec.md`, updated `coverage-tracker.md` | `test: add spec for task NNN — <name>` | task branch |
| Task code merged (🟡) | source changes, moved task file, `coverage-tracker.md` row set to **🟡**, **and any affected `docs/spec/` files** | `feat: complete task NNN — <name>` | task branch |
| Task verified (✅) | `coverage-tracker.md` row promoted from 🟡 → ✅ with `Verified by` column filled (harness command + final assertion, or operator observation) | `verify: confirm task NNN — <evidence>` | task branch |
| Diagram updated | `docs/architecture/diagrams.md` (with date bump at top) | `docs: refresh diagrams — <what changed>` | task branch (or `[allow-main]` for standalone doc fixes) |
| Spec rewritten standalone | `docs/spec/<file>.md` | `spec: <what changed and why now>` | task branch (or `[allow-main]` for standalone doc fixes) |
| Merged into default branch | (after `git merge task/NNN-<slug>` on `main`) | (uses the default `Merge branch …` message) | `main` |

Do **not** add a `Co-Authored-By` line to commits unless explicitly asked.

## Load-bearing process rules

These are the rules that exist specifically to stop a preventable mistake. The
**full treatment, with the incident that motivated each, lives in
[docs/agent-rules.md](docs/agent-rules.md)** — read it. The essentials, so they
reach you even without that file loaded:

- **Commit after every milestone — now, not "after the next task too."** Batched
  commits are impossible to untangle. One task, one commit.
- **Test spec before implementation — always.** No "this is too small for a spec."
  The spec defines done; without it you're guessing.
- **Never work directly on the default branch.** First action of any task is
  `scripts/start-task.sh <NNN> <slug>`, which puts you on `task/NNN-<slug>` or in a
  worktree. When it prints `WORKTREE <path>`, your **next command must be `cd
  <path>`** — editing the parent repo while believing you're isolated is the silent
  failure.
- **"Done" means operationally verified, not "code merged."** The verification
  ladder: (1) code merged → (2) unit tests pass → (3) `make test` passes → (4) CI
  → (5) validation harness exercises the live path → (6) live binary observed.
  Levels 1–4 are 🟡; only 5 or 6 flips a row to ✅. Never claim a level you did not
  reach.
- **Trace producer→consumer before declaring done on cross-module state.** A test
  that sets a field by hand proves the gate works *given* the field; it does not
  prove the field is ever set on the live path. Grep the write site and the read
  site and identify the live path.
- **No smoke tests where the spec asks for assertions.** If the spec says "returns
  exit_code 0", the test must verify that, not merely that the call doesn't panic. If
  constructing the state is hard, that's a blocker to report — not a license to
  downgrade the test.
- **No new warnings self-justified away.** A change that adds a linter/typecheck
  warning over baseline must fix the root cause or stop and report. "Acceptable
  false positive" is not a label you apply unilaterally — use an explicit suppression
  with a reason, or escalate.
- **Run it when the change is runtime-visible.** Logging, CLI/exit codes, proxy
  behavior, endpoints, file outputs, side effects — `make test` alone is not
  verification. Run the binary path and quote the output.
- **Never `git checkout -- <path>` over uncommitted work.** It silently overwrites
  and the reflog cannot recover it. Use `git stash`, `git worktree add <ref>`, or
  `git diff <ref> -- <path>` / `git show <ref>:<path>` instead. A `protect-checkout`
  hook blocks this; the rule stands even if the hook is off.
- **Git status must be clean before declaring a task complete.** `git status` must
  report `nothing to commit, working tree clean`. The common miss: `cp` instead of
  `git mv` when moving a task file leaves the original undeleted.

## Boundaries

### Always
- Write the test spec before any implementation code
- Fill in the **Verification plan** section of the task file *before* writing code — the highest
  verification level achievable, the harness command, the runtime observation
- Commit after every milestone (task completed, spec written, ADR written)
- Read the task file (including its Verification plan) and test spec before starting work on a
  task
- Create an ADR for significant design decisions
- **Preserve the no-network + proxy-only-egress invariant** — any new isolation backend must
  enforce it; it is the security model, not a style choice
- **Update `docs/spec/` in the same commit** as any code change that alters externally-visible
  behavior, data model, interfaces, or configuration
- **Update `docs/architecture/diagrams.md` in the same commit** as any code change that moves a
  component boundary or alters a diagrammed runtime flow
- **Default new task status to 🟡 on the feat commit; ✅ only after spec-verifier APPROVE +
  recorded L5/L6 evidence, in a separate `verify:` commit**
- **Run `spec-verifier` on every task** before promoting to ✅ — its APPROVE/BLOCK verdict is the
  gate, not the executor's self-judgement
- **Start every task on its own branch via `scripts/start-task.sh <NNN> <slug>`**

### Ask first
- Modifying files in `docs/plans/`, `docs/tasks/`, or `docs/architecture/decisions/` — they are
  planning and historical documents
- Deleting or renaming existing source files (`main.go`, `run.go`, `proxy.go`, `gvisor.go`,
  `limits.go`, `snapshot.go`, or any `*_test.go`)
- Adding dependencies not already in the tech stack (this project is currently stdlib-only)
- Changing the project structure beyond what a task requires
- Reorganizing `docs/spec/` (splitting files, renaming sections) — the structure is a stable
  contract; restructure deliberately, not opportunistically

### Never
- Combine unrelated changes in one task or commit
- Skip the test spec — even for "small" changes
- Force push or rewrite published git history
- Add a `Co-Authored-By` line to commits unless explicitly asked
- **Add a `--share-net` flag, a network namespace, or any direct route out of the sandbox.** The
  only egress is the bind-mounted proxy socket.
- **Let a proxy-mode credential value reach the sandbox** (env, args, stdout). It lives only at
  the proxy injection edge.
- Run `git checkout -- <path>` over a dirty working tree
- **Append to spec entries instead of rewriting them** (the ADR keeps history; the spec is a
  snapshot)
- **Add future-tense statements to the spec** (the spec is what *is*; planned work goes in
  `docs/plans/` and `docs/tasks/`)
- **Mark a task ✅ on the same commit as the feature work** — ✅ is reserved for the separate
  `verify:` commit after spec-verifier APPROVE plus L5/L6 evidence
- **Claim a verification level you did not actually reach** — if the binary wasn't run, the row
  says `pending` or `N/A`, not ✅
- **Commit directly to the default branch** (use `[allow-main]` in the message for genuine
  default-branch-only fixes — standalone doc fixes, hotfixes)

## External tools

This is a Go OS-isolation block that pulls and runs agent-generated code. The tooling below
maps to that threat model.

- **dep-scan** — supply-chain CVE scan of Go modules. This block is stdlib-only today, but any
  new dependency must pass dep-scan before merge (use `gods` for Go). Install:
  `curl -fsSL https://raw.githubusercontent.com/tkdtaylor/dep-scan/main/install.sh | bash`.
- **code-scanner** — scan any code/package/deps this block will build on or run before trusting
  them.
- **gh** — clone/inspect related block repos (vault, audit-trail) and open PRs if/when a remote
  is added.

MCP is not needed — `gh` covers repo ops, the vault/audit blocks are driven over Unix-socket
IPC, and the provider CLIs (if any) run as subprocesses.

## Harness notes — Codex

Use these short phrases as automatic repo-local workflows. The user should not need
to paste long agent prompts.

- `work task NNN`, `start task NNN`, `continue task NNN`, or `use task-executor on
  task NNN`: locate the matching task file under
  `docs/tasks/{backlog,active,completed}/NNN-*.md` and the paired
  `docs/tasks/test-specs/NNN-*-test-spec.md`; read `.claude/agents/task-executor.md`;
  then follow that workflow. Start with `scripts/start-task.sh <NNN> <slug>` unless
  already on the correct isolated task branch/worktree.
- `verify task NNN`, `spec verify NNN`, or `use spec-verifier on task NNN`: read
  `.claude/agents/spec-verifier.md` and perform its assertion-by-assertion gate
  against the task, test spec, diff, and test output. Do not edit files.
- `review task NNN`, `review current diff`, or `/code-review`: read
  `.claude/agents/code-reviewer.md` and respond in code-review mode, findings first.
- `architect task NNN`, `architecture review`, or `drift audit`: read
  `.claude/agents/architect.md` and apply that role to the requested scope.
- `security audit task NNN` or `security review`: read
  `.claude/agents/security-auditor.md` and apply that role to the requested scope.

When the user explicitly asks to delegate, run parallel agents, or "use" one of the
named executor/reviewer roles as a subagent, spawn subagents using the matching
`.claude/agents/*.md` file as the role prompt. Otherwise execute the workflow locally
in the current session. The `.claude/agents/*.md` files are role prompts — they are
not automatically-available Codex agents; mirror their intent manually.
