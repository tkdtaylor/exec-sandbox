.PHONY: build test fmt clean \
	fitness \
	fitness-no-share-net fitness-cred-not-in-sandbox fitness-no-deps \
	fitness-handle-prefix fitness-limits fitness-only-workdir \
	fitness-fileread-ro fitness-output-cap fitness-verb-allowlist \
	fitness-snapshot-restore

build:
	go build -o bin/exec-sandbox ./...
test:
	go test ./...
fmt:
	go fmt ./...
clean:
	rm -rf bin

# ---------------------------------------------------------------------------
# Fitness functions — F-001..F-010 (task 009)
# TC-009-01: every block-rule fitness-<id> target exists and is invokable
# TC-009-02: fitness-no-deps (F-003, warn) target exists and is invokable
# TC-009-03: the fitness: umbrella runs all 9 block rules and passes on current main
# TC-009-04: the umbrella EXCLUDES F-003 (warn) — fitness-no-deps is NOT a prerequisite
# TC-009-11: F-005..F-010 targets wrap the spec's declared go test -run commands verbatim
# TC-009-12: spec status flipped to active for F-001/F-002/F-004 (see docs/spec/fitness-functions.md)
# ---------------------------------------------------------------------------
# The fitness: umbrella runs EXACTLY the 9 block-severity rules (F-001, F-002,
# F-004..F-010).  F-003 (warn) is intentionally excluded: a warn-severity rule
# must never fail the umbrella.  fitness-no-deps is a standalone target.
# ---------------------------------------------------------------------------

fitness: fitness-no-share-net fitness-cred-not-in-sandbox fitness-handle-prefix \
	fitness-limits fitness-only-workdir fitness-fileread-ro \
	fitness-output-cap fitness-verb-allowlist fitness-snapshot-restore
	@echo "All fitness checks passed."

# F-001 (block): no backend grants the sandbox a network namespace
# TC-009-05/TC-009-06: new check — see TestFitnessNoShareNetPositive/Negative in fitness_test.go
fitness-no-share-net:
	go test -count=1 -run 'FitnessNoShareNet|TestGvisorSpecHasNoSharedNetwork' ./...

# F-002 (block): proxy-mode credential never appears in sandbox env/args/stdout
# TC-009-07/TC-009-08: new check — see TestFitnessCredNotInSandboxPositive/Negative in fitness_test.go
fitness-cred-not-in-sandbox:
	go test -count=1 -run 'FitnessCredNotInSandbox' ./...

# F-003 (warn): stdlib-only — no third-party Go dependencies.
# NOTE: excluded from the fitness: umbrella (warn severity must not fail the umbrella).
fitness-no-deps:
	@echo "F-003 (warn): checking for third-party dependencies..."
	@if grep -q '^require' go.mod 2>/dev/null; then \
		echo "WARN F-003: go.mod has a require block — third-party dependencies present"; exit 1; \
	else \
		echo "F-003 passed: go.mod has no require block (stdlib-only)"; \
	fi

# F-004 (block): secrets_injected exposes only an ≤8-char handle prefix
# TC-009-09/TC-009-10: new check — see TestFitnessHandlePrefixPositive/Negative in fitness_test.go
fitness-handle-prefix:
	go test -count=1 -run 'FitnessHandlePrefix' ./...

# F-005 (block): every profile.limits cap is enforced on every wired tier
fitness-limits:
	go test -count=1 -run 'Limit|Timeout|CPUAffinity|DiskQuota' ./...

# F-006 (block): only run.workdir is writable; system dirs stay ro; netns stays unshared
fitness-only-workdir:
	go test -count=1 -run 'Workdir|OnlyWorkdir' ./...

# F-007 (block): FileRead host mounts are read-only; only /work is writable; netns stays unshared
fitness-fileread-ro:
	go test -count=1 -run 'FileRead' ./...

# F-008 (block): per-run output cap is host-side and tier-independent
fitness-output-cap:
	go test -count=1 -run 'OutputCap|CapWriter|MaxOutputBytes|OutputTruncated|NoOutputCap' ./...

# F-009 (block): per-host verb allowlist narrows egress; a blocked verb makes no outbound connection
fitness-verb-allowlist:
	go test -count=1 -run 'Verb|NetVerb|BlockedByMethod|DisallowedVerb|AllowedVerb|HostCheckPrecedes' ./...

# F-010 (block): a restored sandbox is indistinguishable from a fresh one
fitness-snapshot-restore:
	go test -count=1 -run 'Snapshot|Restore|Baseline|Leak|OneShot|SecondRun' ./...
