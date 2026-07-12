// SPDX-License-Identifier: Apache-2.0
// Command exec-sandbox is the OS execution-isolation block: it runs agent-generated code in
// a bubblewrap sandbox with no network, routing egress through a credential-injecting proxy.
// exec-sandbox owns the network boundary (--network none + host proxy + allowlist); vault
// plugs credential injection into the proxy at spawn (proxy mode: value never enters the
// sandbox).
//
// Contract (docs/CONTRACT.md, v1):
//
//	run(payload, profile, tier, secret_refs) -> { stdout, stderr, exit_code, sandbox_status }
//
// Usage:
//
//	echo '{"run":{…},"wiring":{…}}' | exec-sandbox run     # JSON request on stdin -> result on stdout
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func main() {
	// fc-launch is the internal in-bwrap launcher for the Firecracker Tier-3 backend (ADR 010
	// A1.Q3): firecrackerBackend.Argv spawns `exec-sandbox fc-launch <bundle>` under bwrap. It drives
	// the firecracker REST API and exits with the GUEST's exit code, so Run()'s host capture maps it
	// unchanged. Not a public contract surface — it is the backend's own spawn target.
	if len(os.Args) >= 2 && os.Args[1] == "fc-launch" {
		os.Exit(fcLaunchMain(os.Args[2:]))
	}
	// Attestation setup + oracle subcommands (ADR 017). keygen generates the host signing keypair;
	// verify-attestation is vault's executable oracle over a trust root + an identity on stdin.
	if len(os.Args) >= 2 && os.Args[1] == "keygen" {
		os.Exit(keygenMain(os.Args[2:]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "verify-attestation" {
		os.Exit(verifyAttestationMain(os.Args[2:]))
	}
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: exec-sandbox run   (JSON RunRequest on stdin)")
		os.Exit(2)
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read stdin:", err)
		os.Exit(1)
	}
	var req RunRequest
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Fprintln(os.Stderr, "parse request:", err)
		os.Exit(1)
	}
	result := Run(req)
	out, _ := json.Marshal(result)
	os.Stdout.Write(out)
}
