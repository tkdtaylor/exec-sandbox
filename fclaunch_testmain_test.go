// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"testing"
)

// TestMain routes the internal `fc-launch` subcommand in the TEST binary exactly as main() does in
// the production binary. The Firecracker backend spawns os.Executable() with `fc-launch <bundle>`
// (under bwrap); under `go test` os.Executable() is the TEST binary, so without this routing the
// re-exec would re-run the whole test suite inside the microVM launcher instead of driving
// firecracker. Routing it here makes the L5 boot TCs exercise the real launch path against the same
// fcLaunchMain the production binary uses — no separate built binary required.
func TestMain(m *testing.M) {
	if len(os.Args) >= 2 && os.Args[1] == "fc-launch" {
		os.Exit(fcLaunchMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}
