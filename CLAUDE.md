# exec-sandbox — project instructions

OS execution isolation. Runs untrusted agent-generated code with no network; the only egress
is a credential-injecting proxy on a Unix socket. Go. PolyForm Noncommercial 1.0.0 (private
during bootstrap).

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
Structured current-state snapshot: [`docs/spec/`](docs/spec/). Full contract reference:
[`docs/CONTRACT.md`](docs/CONTRACT.md).

## Project structure

```
main.go       ← CLI entrypoint — `exec-sandbox run` reads a JSON RunRequest from stdin
run.go        ← Run() orchestration: allowlist parse → vault.inject → proxy → bwrap → audit
proxy.go      ← host-side egress proxy (Unix socket, domain allowlist, credential injection)
run_test.go   ← integration + unit tests (sandbox tests skip when bwrap is absent)
docs/         ← spec + planning + history (the source-of-truth side)
  CONTRACT.md     v1 contract reference (mirrors the ecosystem's v1 interface contract §2)
  spec/           authoritative current-state snapshot — SPEC.md, behaviors, architecture, data-model, interfaces, configuration, fitness-functions
  architecture/   narrative overview, diagrams.md, ADRs, agent-rules.md
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

## Conventions

- Single Go `main` package at the repo root; no internal package split yet (the codebase is
  small enough that the four `.go` files are the unit of organization).
- Task files are named `NNN-short-name.md` (zero-padded, sequential across all task states)
- Every task has a paired test spec; no implementation starts without one
- Tasks follow Unix philosophy — one task, one responsibility; break things smaller when in doubt (see Design principles below)
- ADRs live in `docs/architecture/decisions/` — add one whenever a significant design decision is made
- **Spec is updated in the same commit as the code change.** A task that changes externally-visible behavior, the data model, an interface, or configuration is not done until the matching `docs/spec/` file reflects the new state. Stale spec entries are rewritten in place — never appended to. The ADR carries the history; the spec carries the current truth.
- **Diagrams update with the code.** When a component boundary moves or a runtime flow changes, update `docs/architecture/diagrams.md` in the same commit. Use the `architect` agent's drift-audit mode periodically to catch silent drift.

## Design principles

This project follows **Unix philosophy** as its default design approach — favoring **composability over monolithic design**. Complex behavior should emerge from combining small, independent components that communicate through standardized interfaces, not by growing one large one. The full statement lives in `docs/architecture/overview.md` under *Design principles*; the short version is:

Four structural properties to design for:

- **Modularity** — independent units that can be built, understood, and changed on their own
- **Interface standardization** — stable, well-defined contracts between components (typed signatures, versioned APIs, plain-text formats)
- **Maintainability** — changes in one module should not cascade across unrelated ones
- **Reusability** — components should be liftable into another project without entanglement

Derived working rules:

- **One thing, well** — each module, service, and function has a single clear responsibility
- **Small, composable pieces** over large configurable ones
- **Plain text** for configs, intermediate artifacts, and data interchange where possible
- **Explicit over implicit** — surface assumptions in code and types, not in comments
- **Fail fast, crash loudly** on unexpected state — never silently paper over it
- **Test in isolation** — every component runnable without the whole stack
- **Defer premature decisions** — no abstractions until the second or third concrete use case demands them

**Monolithic is a legitimate choice when deliberate** — the Linux kernel itself is monolithic for good reasons (performance, correctness, tight internal coupling that plug-ins would undermine). The same can apply to a hot-path runtime core, a state machine, or a cryptographic primitive. The principle is "prefer composability at user-facing or cross-module boundaries, and document any deviation with an ADR." Accidental monolithic drift is not the same as a deliberate monolithic decision — the architect agent flags the former, accepts the latter.

The `tier` seam (bubblewrap | gvisor | firecracker) is the deliberate composability boundary
here: new isolation backends plug in behind it without changing the `run()` contract.

## Working in this project

Every task lives on its own branch (or worktree under concurrent sessions). Working directly on `main` is blocked by the `no-commit-on-main.py` hook — `scripts/start-task.sh` is how you pick the right isolation for the moment.

> Note: this repo's default branch is `main`. The hooks treat the checked-out default branch
> as protected.

1. Start each session by reading the relevant task file (including its **Verification plan**) and its test spec
2. Check `docs/architecture/overview.md` for system context
3. Write the test spec before any implementation code
4. Use the **task-executor** agent to implement. Its Step 0 runs `scripts/start-task.sh <NNN> <slug>` to set up either:
   - `BRANCH task/NNN-<slug>` (solo session — the common case), or
   - `WORKTREE .claude/worktrees/NNN-<slug>/` (concurrent session detected; the executor `cd`s in)

   The executor commits at status **🟡 (code merged)** on the task branch.
5. After the executor returns, use **spec-verifier** on the task — it returns APPROVE or BLOCK based on per-assertion evidence
6. If spec-verifier APPROVEs **and** the verification plan's L5/L6 evidence is recorded (validation harness output or runtime observation), promote the row to **✅ (verified)** in `coverage-tracker.md` in a **separate commit** titled `verify: confirm task NNN — <evidence>` (still on the task branch)
7. **Merge to the default branch** when ready: `git checkout main && git merge task/NNN-<slug>`. The `auto-cleanup-merge.py` hook then deletes the task branch and removes the worktree (if any) automatically. If the merge introduced conflicts or you want to keep the branch around for reference, the hook surfaces a note and leaves it in place.
8. **Commit after each milestone** — never start the next task without committing the current one first. (No remote is configured; this repo is local-only during bootstrap, so there is nothing to push.)

The separation between the task branch and the default branch is the load-bearing rule for multi-session safety. Two sessions on different `task/*` branches can work in parallel without ever stepping on each other's files; two sessions both editing the default branch cannot. The hook is the floor — the discipline is the goal.

The separation between 🟡 (feat commit) and ✅ (verify commit) is the load-bearing rule: it makes "merged" and "verified" two distinct artifacts in git history, so neither can silently substitute for the other. **Never** mark ✅ in the same commit as the feature work — the verification step must be its own observable event.

## Commit rules

**You must commit after every milestone.** Do not batch multiple tasks into one commit. Do not continue to the next task until the current one is committed. (No remote configured — local-only repo; "push" steps are no-ops here.)

All commits below land on the **task branch** (`task/NNN-<slug>`), never on the default branch directly. The merge to the default branch happens after the verify step, in a separate explicit operation.

| Milestone | What to stage | Message | Branch |
|-----------|--------------|---------|--------|
| ADR written | `docs/architecture/decisions/NNN-*.md`, any superseded spec entries rewritten in `docs/spec/` | `docs: add ADR NNN — <decision title>` | task branch |
| Test spec written | `docs/tasks/test-specs/NNN-*-test-spec.md`, updated `coverage-tracker.md` | `test: add spec for task NNN — <name>` | task branch |
| Task code merged (🟡) | source changes, moved task file, `coverage-tracker.md` row set to **🟡**, **and any affected `docs/spec/` files** | `feat: complete task NNN — <name>` | task branch |
| Task verified (✅) | `coverage-tracker.md` row promoted from 🟡 → ✅ with `Verified by` column filled (harness command + final assertion, or operator observation) | `verify: confirm task NNN — <evidence>` | task branch |
| Diagram updated | `docs/architecture/diagrams.md` (with date bump at top) | `docs: refresh diagrams — <what changed>` | task branch (or `[allow-main]` for standalone doc fixes) |
| Spec rewritten standalone | `docs/spec/<file>.md` | `spec: <what changed and why now>` | task branch (or `[allow-main]` for standalone doc fixes) |
| Merged into default branch | (after `git merge task/NNN-<slug>` on `main`) | (uses the default `Merge branch …` message) | `main` |

After each milestone:
```bash
git add <relevant files>
git commit -m "<message>"
```

## Plan mode

When you exit plan mode, a hook automatically restructures the plan:
- Each step becomes a task file in `docs/tasks/backlog/`
- Test spec stubs are created for each task
- The plan is replaced with a lightweight skeleton to save context tokens
- The full plan is backed up to `docs/plans/`

Use the **task-executor** agent to work through tasks one at a time. Each agent call is ephemeral — it reads the task file, does the work, commits, and reports back without bloating the main conversation.

```
use task-executor — task: docs/tasks/backlog/NNN-name.md, spec: docs/tasks/test-specs/NNN-name-test-spec.md
```

### End handoffs with a resume command

When a response completes a logical milestone that leaves follow-on work (a task planned but not executed, an ADR drafted awaiting implementation, a handoff to another session or agent), end the response with a **fenced code block** containing the exact resume command. Not inline backticks, not a prose description, not a vague pointer — a fenced code block is what renders the copy button in the VSCode chat UI. Inline code does not get that affordance.

**Verify the path exists before writing the resume block.** Glob `docs/tasks/backlog/NNN-*.md` (and the matching `docs/tasks/test-specs/NNN-*-test-spec.md`) and copy the real filenames into the block. Do NOT infer filenames from the plan or from a prior message — the plan-mode hook may rename task files as it writes them out, and a wrong path wastes a whole task-executor round trip when the user or future session blindly pastes it.

If there is genuinely nothing to resume (the work is fully shipped, nothing follows), skip the block. This is a rule for real handoffs, not a ritual at the end of every message.

## Hook profiles

Hooks run automatically and are gated by profile level. Control via environment variables:

```bash
export CLAUDE_HOOK_PROFILE=minimal    # Safety hooks only (secret protection, block-no-verify, config-protection, protect-checkout)
export CLAUDE_HOOK_PROFILE=standard   # + workflow hooks (plan restructuring, compaction, checkpoints) — default
export CLAUDE_HOOK_PROFILE=strict     # + formatting, notifications (batch-format-typecheck, desktop-notify)
export CLAUDE_DISABLED_HOOKS=desktop-notify,batch-format-typecheck  # Disable specific hooks
```

## Boundaries

### Always
- Write the test spec before any implementation code
- Fill in the **Verification plan** section of the task file *before* writing code — the highest verification level achievable, the harness command, the runtime observation
- Commit after every milestone (task completed, spec written, ADR written)
- Read the task file (including its Verification plan) and test spec before starting work on a task
- Create an ADR for significant design decisions
- **Preserve the no-network + proxy-only-egress invariant** — any new isolation backend must enforce it; it is the security model, not a style choice
- **Update `docs/spec/` in the same commit as any code change that alters externally-visible behavior, data model, interfaces, or configuration**
- **Update `docs/architecture/diagrams.md` in the same commit as any code change that moves a component boundary or alters a diagrammed runtime flow**
- **Default new task status to 🟡 on the feat commit; ✅ only after spec-verifier APPROVE + recorded L5/L6 evidence, in a separate `verify:` commit**
- **Run `spec-verifier` on every task** before promoting to ✅ — its APPROVE/BLOCK verdict is the gate, not the executor's self-judgement
- **Start every task on its own branch via `scripts/start-task.sh <NNN> <slug>`** — the script picks branch or worktree based on whether other Claude Code sessions are active. The task-executor runs this as Step 0 automatically.

### Ask first
- Modifying files in `docs/plans/`, `docs/tasks/`, or `docs/architecture/decisions/` — they are planning and historical documents
- Deleting or renaming existing source files (`main.go`, `run.go`, `proxy.go`, `run_test.go`)
- Adding dependencies not already in the tech stack (this project is currently stdlib-only)
- Changing the project structure beyond what a task requires
- Reorganizing `docs/spec/` (splitting files, renaming sections) — the structure is a stable contract; restructure deliberately, not opportunistically

### Never
- Combine unrelated changes in one task or commit
- Skip the test spec — even for "small" changes
- Force push or rewrite published git history
- Add a `Co-Authored-By` line to commits unless explicitly asked
- **Add a `--share-net` flag, a network namespace, or any direct route out of the sandbox.** The only egress is the bind-mounted proxy socket.
- **Let a proxy-mode credential value reach the sandbox** (env, args, stdout). It lives only at the proxy injection edge.
- Run `git checkout -- <path>` (or `git checkout <ref> -- <path>`) over a dirty working tree — it silently overwrites uncommitted work and the reflog cannot recover it. To *compare* to a prior commit, use `git diff <ref> -- <path>`, `git show <ref>:<path>`, or `git worktree add ../baseline <ref>`. To *discard* changes, `git stash` first. A `protect-checkout` hook blocks this automatically, but the rule stands even if the hook is disabled.
- **Append to spec entries instead of rewriting them.** When a decision changes, edit the spec entry to reflect the new truth. The ADR keeps the history — the spec is a snapshot, not a changelog.
- **Add future-tense statements to the spec.** The spec is what *is*, not what *will be*. Planned work goes in `docs/plans/` and `docs/tasks/`.
- **Mark a task ✅ on the same commit as the feature work.** ✅ is reserved for the separate `verify:` commit after spec-verifier APPROVE plus L5/L6 evidence. Merged-equals-verified is the failure mode this rule exists to prevent.
- **Claim a verification level you did not actually reach.** If the binary wasn't run, the row says `pending` or `N/A`, not ✅. If the harness doesn't exist, that's a blocker to flag, not an excuse for ✅ at L4.
- **Commit directly to the default branch.** Every task commit lands on `task/NNN-<slug>`. The `no-commit-on-main.py` hook will block you; the rule stands even without the hook. For genuine default-branch-only commits (e.g. a standalone doc fix, a hotfix), include `[allow-main]` in the commit message — it's self-documenting in `git log`.
- **Forget to `cd` into the worktree.** When `scripts/start-task.sh` returns `WORKTREE <path>`, every subsequent command must run inside that path. Editing the parent repo while believing you're in a worktree is the silent isolation failure.

## Agent rules and retros

Process-level rules, common rationalizations, and project-specific retros all live in `docs/architecture/agent-rules.md`. The `inject-retros.py` SessionStart hook reads that file and surfaces relevant entries at the start of every session, so adding an entry there is how a one-time mistake becomes a permanent guard. The starter file ships with rules covering parallel-dispatch worktree isolation, the `git checkout -- <path>` hazard, smoke-test rationalization, dead-code delegates, and a "Common rationalizations" table.

When dispatching parallel agents in one message, run `scripts/verify-worktree-isolation.sh <agent-id> [<agent-id> ...]` after they complete to confirm none bypassed the worktree flag.

## Recommended tooling

This is a Go OS-isolation block that pulls and runs agent-generated code. The tooling below
maps to that threat model.

### Skills
- **code-scanner** — scan any code/package/deps this block will build on or run before trusting them. Trigger: "scan this repo for malware".
- **code-review** — review diffs before merge, especially anything touching the egress proxy or bwrap argv. Trigger: `/code-review`.
- **deep-research** — when adding an isolation backend (gVisor, Firecracker, Kata), survey prior art / build-vs-adopt first. Trigger: "deep research on <X>".

### External tools
- **dep-scan** — supply-chain CVE scan of Go modules. This block is stdlib-only today, but any new dependency must pass dep-scan before merge (use `gods` for Go). Install: `curl -fsSL https://raw.githubusercontent.com/tkdtaylor/dep-scan/main/install.sh | bash`.
- **gh** — clone/inspect related block repos (vault, audit-trail) and open PRs if/when a remote is added.

### Hooks
- Wired via `.claude/settings.json` (standard profile): `no-commit-on-main`, `protect-secrets`, `block-no-verify`, plan→tasks restructuring, compaction guards, spec-coverage-check, protect-checkout, config-protection. Control with `CLAUDE_HOOK_PROFILE` (minimal/standard/strict).

### Note on MCP
Not needed — `gh` covers repo ops, the vault/audit blocks are driven over Unix-socket IPC, and the provider CLIs (if any) run as subprocesses.
