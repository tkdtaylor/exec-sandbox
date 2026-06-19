// SPDX-License-Identifier: Apache-2.0
// Command exec-sandbox is the OS execution-isolation block: it runs agent-generated code in
// a bubblewrap sandbox with no network, routing egress through a credential-injecting proxy.
// exec-sandbox owns the network boundary (--network none + host proxy + allowlist); vault
// plugs credential injection into the proxy at spawn (proxy mode: value never enters the
// sandbox).
//
// Contract (interface-contracts.md §2, v1):
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
