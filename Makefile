.PHONY: build test fmt clean \
	fitness \
	fitness-no-share-net fitness-cred-not-in-sandbox fitness-no-deps \
	fitness-handle-prefix fitness-limits fitness-only-workdir \
	fitness-fileread-ro fitness-output-cap fitness-verb-allowlist \
	fitness-snapshot-restore fitness-tier1-seccomp \
	fitness-no-nic fitness-cred-not-in-guest fitness-constraints-ge-jailer

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
	fitness-output-cap fitness-verb-allowlist fitness-snapshot-restore \
	fitness-tier1-seccomp \
	fitness-no-nic fitness-cred-not-in-guest fitness-constraints-ge-jailer
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

# F-011 (block, task 019): Tier-1 runs under a default-deny seccomp profile.
# The bwrap argv carries --seccomp <fd>; a blocked syscall (keyctl) returns EPERM under a real
# bwrap run; the loader fails fast on a sha256 mismatch (no unfiltered fall-back); gvisor.go is
# untouched. Negative case: a --seccomp-stripped argv is rejected (TestFitnessTier1SeccompNegative).
# Registered into the fitness: umbrella above (coordinates with task 009), the same way the other
# block rules do. The keyctl probe skips without bwrap/cc but is the load-bearing L6 evidence when present.
fitness-tier1-seccomp:
	go test -count=1 -run 'Seccomp|Tier1Seccomp|BwrapArgvCarries|BubblewrapBackendThreads|BackendPropagates|PolicyDeny|Keyctl|CommonCasePayloadStillRuns' ./...

# ---------------------------------------------------------------------------
# Firecracker microVM fitness rules — F-001/F-002 microVM enforcement points +
# constraints-≥-jailer (task 018). Joined into the fitness: umbrella above
# (coordinates with task 009 — the umbrella rule list is the one inspectable place).
# Each rule reuses an earlier epic task's helper and has a positive + a proven-biting negative.
# ---------------------------------------------------------------------------

# F-001 (microVM, block): the generated Firecracker config carries no network-interface key (no-NIC
# by omission, ADR 010 D2) — the microVM enforcement point of F-001 alongside bwrapArgv/gvisorOCISpec.
# Reuses configHasNoNIC (task 013). Negative: a constructed NIC config is rejected (TestFitnessNoNICNegative).
fitness-no-nic:
	go test -count=1 -run 'FitnessNoNIC|FirecrackerMount_NoNIC|RESTSequenceNeverEmitsNIC' ./...

# F-002 (microVM, block): the credential never crosses the vsock into the guest (guest env/args/stdout)
# — injected host-side after the vsock hop. Reuses assertCredNotInGuest (task 014). Negative: a
# constructed guest leak is caught (TestFitnessCredNotInGuestNegative).
fitness-cred-not-in-guest:
	go test -count=1 -run 'FitnessCredNotInGuest|CredNotInGuest' ./...

# microVM (block, ADR-010 Amendment 1 A1.Q3): the Tier-3 launch's effective constraints are ≥ jailer
# — non-host uid, all namespaces unshared, chroot/pivot_root in effect, /dev/kvm the only device, NO
# jailer binary. Reuses assertConstraintsGEJailerArgv/assertConstraintsGEJailer (task 015). Negative:
# a weakened launch/child (shared namespace / host uid / regained caps / no pivot_root) is rejected.
# The live-process half (TestFirecrackerConstraintsGEJailer_Live) skip-guards on /dev/kvm.
fitness-constraints-ge-jailer:
	go test -count=1 -run 'FitnessConstraintsGeJailer|FirecrackerConstraints' ./...
