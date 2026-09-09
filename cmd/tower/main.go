// Command airlift-tower hosts an airlift session on the operator's laptop:
// it receives relayed QR frames from scanners, reassembles and verifies the
// payload, unpacks repobundles, and serves the dashboard and downloads over
// the LAN. Phase 2 fills this in; see docs/BUILD-PLAN.md.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "airlift-tower: not implemented yet (Phase 2)")
	os.Exit(2)
}
