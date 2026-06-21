// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// errSeccompTestInjected is the synthetic loader failure TC-019-03 injects through the
// loadTier1SeccompFn seam to prove the backend FAILS the run rather than spawning bwrap unfiltered.
var errSeccompTestInjected = errors.New("seccomp: injected test load failure")

// keyctlProbeC is a tiny self-contained probe: it issues the keyctl(2) syscall directly (no
// libkeyutils needed) and prints "errno=<n>" so the test can read the kernel's verdict. Under the
// Tier-1 default-deny seccomp profile the call is denied with EPERM (errno 1) before it reaches
// the kernel's keyring code. Compiled statically in the test so it runs inside the minimal
// sandbox rootfs without a dynamic-loader dependency surprise.
const keyctlProbeC = `
#include <stdio.h>
#include <errno.h>
#include <unistd.h>
#include <sys/syscall.h>
#ifndef SYS_keyctl
#define SYS_keyctl 250
#endif
int main(void) {
    /* KEYCTL_GET_KEYRING_ID = 0; arg2 = special keyring id; create=0 */
    long r = syscall(SYS_keyctl, 0 /*GET_KEYRING_ID*/, -1 /*SESSION*/, 0);
    if (r == -1) {
        printf("errno=%d\n", errno);
    } else {
        printf("errno=0\n");
    }
    return 0;
}
`

// buildStaticProbe compiles src to a static binary at <dir>/<name> and returns its path. Skips the
// whole test when no working C compiler / static libc is available (mirrors the requireBwrap skip
// idiom — the integration test never silently passes without actually probing).
func buildStaticProbe(t *testing.T, dir, name, src string) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler; skipping seccomp probe integration test")
	}
	csrc := filepath.Join(dir, name+".c")
	if err := os.WriteFile(csrc, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, name)
	out, err := exec.Command(cc, "-static", "-O2", "-o", bin, csrc).CombinedOutput()
	if err != nil {
		t.Skipf("static compile unavailable (%v): %s", err, out)
	}
	return bin
}

// seccompProbeRequest builds a Tier-1 RunRequest that bind-mounts the host probe binary read-only
// (FileRead) at its own absolute path and execs it. No NetConnect capability (this exercises the
// syscall filter, not egress).
func seccompProbeRequest(probePath, payload string) RunRequest {
	var req RunRequest
	req.Run.Payload = payload
	req.Run.Tier = "bubblewrap"
	req.Run.Profile = map[string]any{
		"capabilities": []any{
			map[string]any{"type": "FileRead", "paths": []any{probePath}},
		},
	}
	req.Wiring.RequestID = "seccomp-test"
	return req
}

// ---------------------------------------------------------------------------
// TC-019-01: bwrapArgv carries --seccomp <fd>; --unshare-all kept, no --share-net
// ---------------------------------------------------------------------------

func TestBwrapArgvCarriesSeccomp(t *testing.T) {
	argv := bwrapArgv("/tmp/payload.sh", "/tmp/proxy.sock", "", nil, nil, 0,
		[]string{"/usr/bin/sh", "/payload.sh"}, -1, 3)

	// --seccomp must be immediately followed by the fd token we passed.
	if !argvHasPair(argv, "--seccomp", "3") {
		t.Fatalf("argv missing `--seccomp 3` pair: %v", argv)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--unshare-all") {
		t.Fatal("argv lost --unshare-all — the no-network invariant must survive the new flag")
	}
	if strings.Contains(joined, "--share-net") {
		t.Fatal("argv gained --share-net — the no-network invariant is broken")
	}
}

// TC-019-01 (continued): the live backend threads the seccomp *os.File into ExtraFiles so the
// child's fd number matches the argv token. ExtraFiles[0] is child fd 3 = the seccomp blob.
func TestBubblewrapBackendThreadsSeccompFD(t *testing.T) {
	argv, cleanup, _, extraFiles, err := bubblewrapBackend{}.Argv(
		"/tmp/payload.sh", "/tmp/proxy.sock", "", nil, nil, nil, Limits{})
	if cleanup != nil {
		defer cleanup()
	}
	defer func() {
		for _, f := range extraFiles {
			_ = f.Close()
		}
	}()
	if err != nil {
		t.Fatalf("Argv error: %v", err)
	}
	if len(extraFiles) < 1 {
		t.Fatalf("expected at least one extraFile (the seccomp blob), got %d", len(extraFiles))
	}
	// ExtraFiles[0] → child fd 3. The argv must name fd 3.
	if !argvHasPair(argv, "--seccomp", "3") {
		t.Fatalf("argv must name the seccomp fd 3 (ExtraFiles[0]); argv=%v", argv)
	}
	// The first extraFile must actually be the seccomp blob: its bytes equal the verified blob.
	b := make([]byte, len(tier1SeccompBlob))
	if _, err := extraFiles[0].ReadAt(b, 0); err != nil {
		t.Fatalf("reading seccomp extraFile: %v", err)
	}
	if hex.EncodeToString(sha256Of(b)) != hex.EncodeToString(sha256Of(tier1SeccompBlob)) {
		t.Fatal("ExtraFiles[0] is not the verified Tier-1 seccomp blob")
	}
}

func sha256Of(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// ---------------------------------------------------------------------------
// TC-019-02: sha256 loader accepts the matching committed blob
// ---------------------------------------------------------------------------

func TestSeccompLoaderAcceptsMatchingBlob(t *testing.T) {
	f, err := loadTier1Seccomp()
	if err != nil {
		t.Fatalf("loadTier1Seccomp on the committed blob errored: %v", err)
	}
	defer f.Close()
	// The returned file must hold exactly the embedded blob bytes.
	b := make([]byte, len(tier1SeccompBlob))
	if _, err := f.ReadAt(b, 0); err != nil {
		t.Fatalf("reading loaded seccomp file: %v", err)
	}
	if hex.EncodeToString(sha256Of(b)) != hex.EncodeToString(sha256Of(tier1SeccompBlob)) {
		t.Fatal("loaded file bytes differ from the embedded blob")
	}
	// The committed pin file on disk must agree with the embedded blob (proves the pin is honest).
	pinBytes, err := os.ReadFile("seccomp/tier1.bpf.sha256")
	if err != nil {
		t.Fatalf("reading committed pin: %v", err)
	}
	if err := verifyTier1SeccompBytes(tier1SeccompBlob, string(pinBytes)); err != nil {
		t.Fatalf("committed pin does not verify the embedded blob: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TC-019-03: loader fails fast on a mismatched blob — no unfiltered fall-back
// ---------------------------------------------------------------------------

func TestSeccompLoaderFailsFastOnMismatch(t *testing.T) {
	// A tampered/truncated blob must NOT verify against the committed pin.
	tampered := append([]byte(nil), tier1SeccompBlob...)
	if len(tampered) > 0 {
		tampered[0] ^= 0xFF // flip a byte
	} else {
		tampered = []byte{0x00}
	}
	if err := verifyTier1SeccompBytes(tampered, tier1SeccompPin); err == nil {
		t.Fatal("verifyTier1SeccompBytes accepted a tampered blob — fail-fast is broken")
	}

	// A truncated blob also fails.
	if err := verifyTier1SeccompBytes(tier1SeccompBlob[:len(tier1SeccompBlob)/2], tier1SeccompPin); err == nil {
		t.Fatal("verifyTier1SeccompBytes accepted a truncated blob")
	}

	// A malformed pin (not 64 hex chars) is a hard error, never a pass.
	if err := verifyTier1SeccompBytes(tier1SeccompBlob, "deadbeef  tier1.bpf"); err == nil {
		t.Fatal("verifyTier1SeccompBytes accepted a malformed (short) pin")
	}
}

// TC-019-03 (continued): a backend that cannot load a verified profile FAILS the run rather than
// spawning bwrap unfiltered. We drive this through the seam by pointing the loader at a bad blob
// via a swapped package-level verifier is not possible (embed is const), so we assert the
// contract at the loader boundary: loadTier1Seccomp returns (file,nil) only when the pin matches,
// and bubblewrapBackend.Argv propagates a loader error as a hard err (no argv, no extraFiles).
func TestBackendPropagatesSeccompLoadFailure(t *testing.T) {
	// Save and restore the embedded-blob-backed loader seam.
	orig := loadTier1SeccompFn
	t.Cleanup(func() { loadTier1SeccompFn = orig })
	loadTier1SeccompFn = func() (*os.File, error) {
		return nil, errSeccompTestInjected
	}
	argv, cleanup, _, extraFiles, err := bubblewrapBackend{}.Argv(
		"/tmp/payload.sh", "/tmp/proxy.sock", "", nil, nil, nil, Limits{})
	if cleanup != nil {
		cleanup()
	}
	for _, f := range extraFiles {
		_ = f.Close()
	}
	if err == nil {
		t.Fatal("backend must FAIL when the seccomp profile cannot be loaded — no unfiltered fall-back")
	}
	if argv != nil {
		t.Fatalf("backend returned a usable argv despite the load failure: %v", argv)
	}
}

// ---------------------------------------------------------------------------
// TC-019-07: gvisor.go is untouched — Tier-2 does NOT get --seccomp
// ---------------------------------------------------------------------------

// The seccomp profile is Tier-1 only. The gVisor backend's argv must carry no --seccomp flag (its
// sentry already filters every syscall — ADR 016). This is the behavioral half of TC-019-07; the
// zero-diff half is verified by `git diff gvisor.go` (empty) at review time.
func TestGvisorBackendHasNoSeccompFlag(t *testing.T) {
	argv, cleanup, _, extraFiles, err := gvisorBackend{}.Argv(
		"/tmp/payload.sh", "/tmp/proxy.sock", "", nil, nil, nil, Limits{})
	if cleanup != nil {
		defer cleanup()
	}
	defer func() {
		for _, f := range extraFiles {
			_ = f.Close()
		}
	}()
	if err != nil {
		// gVisor backend may legitimately fail to prepare without runsc; only the argv shape matters
		// here, so a prep error that still produced no argv is acceptable — assert on whatever argv exists.
		if argv == nil {
			t.Skipf("gVisor backend could not prepare an argv on this host: %v", err)
		}
	}
	for _, a := range argv {
		if a == "--seccomp" {
			t.Fatalf("gVisor (Tier-2) argv must NOT carry --seccomp (ADR 016): %v", argv)
		}
	}
}

// ---------------------------------------------------------------------------
// TC-019-11: spec rewritten in place, present tense
// ---------------------------------------------------------------------------

func TestTier1SeccompSpecUpdated(t *testing.T) {
	type check struct {
		file        string
		mustHave    []string
		mustNotHave []string
	}
	checks := []check{
		{
			file:     "docs/spec/SPEC.md",
			mustHave: []string{"default-deny seccomp profile", "--seccomp", "F-011"},
		},
		{
			file:     "docs/spec/behaviors.md",
			mustHave: []string{"B-013", "default-deny", "--seccomp", "keyctl", "EPERM"},
		},
		{
			file:     "docs/spec/fitness-functions.md",
			mustHave: []string{"F-011", "fitness-tier1-seccomp", "--seccomp"},
		},
		{
			file:     "docs/spec/configuration.md",
			mustHave: []string{"seccomp/tier1.bpf", "tier1.bpf.sha256", "build-time"},
		},
	}
	// Present-tense discipline: the spec must not describe this as future/planned work.
	futureTense := []string{"will pass --seccomp", "will install", "planned seccomp", "TODO: seccomp"}
	for _, c := range checks {
		b, err := os.ReadFile(c.file)
		if err != nil {
			t.Fatalf("reading %s: %v", c.file, err)
		}
		s := string(b)
		for _, want := range c.mustHave {
			if !strings.Contains(s, want) {
				t.Errorf("%s: missing required phrase %q", c.file, want)
			}
		}
		for _, bad := range c.mustNotHave {
			if strings.Contains(s, bad) {
				t.Errorf("%s: contains forbidden phrase %q", c.file, bad)
			}
		}
		for _, ft := range futureTense {
			if strings.Contains(s, ft) {
				t.Errorf("%s: future-tense seccomp language %q (spec is present-tense)", c.file, ft)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// TC-019-05: the deny set is present in the plain-text policy
// ---------------------------------------------------------------------------

func TestPolicyDenySetCoversDangerousFamily(t *testing.T) {
	policy, err := os.ReadFile("seccomp/tier1-policy.json")
	if err != nil {
		t.Fatalf("reading policy: %v", err)
	}
	denySection := extractJSONArray(t, string(policy), "deny")
	allowSection := extractJSONArray(t, string(policy), "allow")

	required := []string{
		"keyctl", "add_key", "request_key",
		"ptrace", "process_vm_readv", "process_vm_writev",
		"userfaultfd", "bpf", "perf_event_open",
		"mount", "umount2", "pivot_root",
		"kexec_load", "kexec_file_load",
		"init_module", "finit_module", "delete_module",
	}
	for _, name := range required {
		if !containsToken(denySection, name) {
			t.Errorf("deny set missing required dangerous syscall %q", name)
		}
		// A denied syscall must NOT also be in the allowlist (that would re-permit it).
		if containsToken(allowSection, name) {
			t.Errorf("syscall %q is in BOTH allow and deny — allow would override the default-deny", name)
		}
	}
}

// ---------------------------------------------------------------------------
// TC-019-08: the pinned blob is honestly built from the policy (skips w/o libseccomp)
// ---------------------------------------------------------------------------

func TestSeccompBlobReproducibleFromPolicy(t *testing.T) {
	if _, err := exec.LookPath("pkg-config"); err != nil {
		t.Skip("pkg-config absent; cannot verify seccomp build reproducibility")
	}
	if out, err := exec.Command("pkg-config", "--exists", "libseccomp").CombinedOutput(); err != nil {
		t.Skipf("libseccomp dev tooling absent (%v): %s", err, out)
	}

	// Build into a scratch dir so we never disturb the committed artifacts.
	scratch := t.TempDir()
	// Copy gen.c + policy into scratch and run the compile+export the way build.sh does.
	genSrc, err := os.ReadFile("seccomp/gen.c")
	if err != nil {
		t.Fatal(err)
	}
	policy, err := os.ReadFile("seccomp/tier1-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	genC := filepath.Join(scratch, "gen.c")
	policyJSON := filepath.Join(scratch, "tier1-policy.json")
	if err := os.WriteFile(genC, genSrc, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(policyJSON, policy, 0o600); err != nil {
		t.Fatal(err)
	}
	cflags := strings.Fields(mustCmd(t, "pkg-config", "--cflags", "libseccomp"))
	libs := strings.Fields(mustCmd(t, "pkg-config", "--libs", "libseccomp"))
	bin := filepath.Join(scratch, "gen")
	args := append([]string{"-O2", "-o", bin}, cflags...)
	args = append(args, genC)
	args = append(args, libs...)
	if out, err := exec.Command("cc", args...).CombinedOutput(); err != nil {
		t.Skipf("cannot compile gen.c (%v): %s", err, out)
	}
	cmd := exec.Command(bin, policyJSON)
	blob, err := cmd.Output()
	if err != nil {
		t.Fatalf("running gen: %v", err)
	}
	got := hex.EncodeToString(sha256Of(blob))

	committed, err := os.ReadFile("seccomp/tier1.bpf.sha256")
	if err != nil {
		t.Fatal(err)
	}
	want, err := parseSeccompPin(string(committed))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("rebuilt blob sha256 %s != committed pin %s — the committed blob is stale or hand-edited", got, want)
	}
}

func mustCmd(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------------------
// TC-019-09 / TC-019-10: fitness rule positive + negative (the check bites)
// ---------------------------------------------------------------------------

func TestFitnessTier1SeccompPositive(t *testing.T) {
	argv := bwrapArgv("/tmp/payload.sh", "/tmp/proxy.sock", "", nil, nil, 0,
		[]string{"/usr/bin/sh", "/payload.sh"}, -1, 3)
	if err := assertSeccompInArgv(argv); err != nil {
		t.Fatal(err)
	}
}

func TestFitnessTier1SeccompNegative(t *testing.T) {
	// Construct a Tier-1 argv with --seccomp stripped (a simulated regression). The check must bite.
	full := bwrapArgv("/tmp/payload.sh", "/tmp/proxy.sock", "", nil, nil, 0,
		[]string{"/usr/bin/sh", "/payload.sh"}, -1, 3)
	stripped := make([]string, 0, len(full))
	for i := 0; i < len(full); i++ {
		if full[i] == "--seccomp" {
			i++ // skip the fd token too
			continue
		}
		stripped = append(stripped, full[i])
	}
	if err := assertSeccompInArgv(stripped); err == nil {
		t.Fatal("F-011: assertSeccompInArgv must reject an argv with --seccomp removed (the check would be a no-op otherwise)")
	}

	// Sanity: the stripped argv still has --unshare-all, so the rejection is specifically about the
	// missing --seccomp, not an unrelated failure.
	if !containsArg(stripped, "--unshare-all") {
		t.Fatal("test setup: stripped argv unexpectedly lost --unshare-all")
	}
}

// ---------------------------------------------------------------------------
// TC-019-04: a blocked syscall returns EPERM under Tier-1 (the crux, L6)
// ---------------------------------------------------------------------------

func TestKeyctlBlockedReturnsEPERM_Bwrap(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	probe := buildStaticProbe(t, dir, "keyctl_probe", keyctlProbeC)

	res := Run(seccompProbeRequest(probe, probe+"\n"))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("probe exit_code = %v, stderr=%q stdout=%q", res["exit_code"], res["stderr"], res["stdout"])
	}
	out := strings.TrimSpace(res["stdout"].(string))
	// EPERM == 1. The default-deny profile denies keyctl with EPERM before the kernel keyring code.
	if out != "errno="+strconv.Itoa(int(ePERM)) {
		t.Fatalf("keyctl under Tier-1 seccomp: got %q, want errno=%d (EPERM) — the filter is not biting", out, ePERM)
	}
}

const ePERM = 1

// ---------------------------------------------------------------------------
// TC-019-06: a common-case payload still runs to a normal exit under the profile
// ---------------------------------------------------------------------------

func TestCommonCasePayloadStillRuns_Bwrap(t *testing.T) {
	requireBwrap(t)
	dir := t.TempDir()
	// Ordinary payload: write a file to /work, read it back, list a dir, exec a tool — all of which
	// rely on syscalls the allowlist must keep open. The profile narrows; it must not brick this.
	payload := "echo hello > /work/out.txt; cat /work/out.txt; ls /usr/bin >/dev/null; echo done\n"
	res := Run(workdirRequest("bubblewrap", dir, payload))
	if res["exit_code"].(int) != 0 {
		t.Fatalf("common-case payload exit_code = %v, stderr=%q", res["exit_code"], res["stderr"])
	}
	got := strings.TrimSpace(res["stdout"].(string))
	if !strings.Contains(got, "hello") || !strings.Contains(got, "done") {
		t.Fatalf("common-case payload stdout = %q, want it to contain hello + done", got)
	}
	// The /work write must persist (the allowlist keeps open(2)/write(2) for the writable surface).
	if b, err := os.ReadFile(filepath.Join(dir, "out.txt")); err != nil || strings.TrimSpace(string(b)) != "hello" {
		t.Fatalf("/work write did not persist under the profile: %v (%q)", err, b)
	}
}

// TC-019-06 (continued): the common-case payload also reaches an allowlisted origin through the
// proxy under the seccomp profile — socket/connect/sendto/recvfrom stay allowed.
func TestCommonCaseProxyReachStillWorks_Bwrap(t *testing.T) {
	requireBwrap(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent; skipping proxy-reach-under-seccomp test")
	}
	// Reuse the run_test.go proxy harness shape via newRunRequest is host-coupled; instead assert the
	// simpler property already covered by run_test.go's proxy test now runs WITH the profile applied.
	// (That test, TestSandboxReachesAllowlistedHostViaProxy, exercises the same path and now runs
	// under the seccomp profile — this case documents the coupling without duplicating the server.)
	t.Skip("covered by TestSandboxReachesAllowlistedHostViaProxy, which now runs under the seccomp profile")
}

// ---------------------------------------------------------------------------
// small JSON/array helpers (no third-party dependency)
// ---------------------------------------------------------------------------

// extractJSONArray returns the substring between the '[' that follows "<key>" and its closing ']'.
func extractJSONArray(t *testing.T, json, key string) string {
	t.Helper()
	idx := strings.Index(json, "\""+key+"\"")
	if idx < 0 {
		t.Fatalf("policy: key %q not found", key)
	}
	open := strings.Index(json[idx:], "[")
	if open < 0 {
		t.Fatalf("policy: key %q has no '['", key)
	}
	open += idx
	close := strings.Index(json[open:], "]")
	if close < 0 {
		t.Fatalf("policy: key %q has no ']'", key)
	}
	return json[open : open+close+1]
}

// containsToken reports whether section contains the exact quoted token "name".
func containsToken(section, name string) bool {
	return strings.Contains(section, "\""+name+"\"")
}

func containsArg(argv []string, s string) bool {
	for _, a := range argv {
		if a == s {
			return true
		}
	}
	return false
}
