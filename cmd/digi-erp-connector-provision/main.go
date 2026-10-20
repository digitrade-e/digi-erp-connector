// Command digi-erp-connector-provision configures this machine's connector
// without the GUI: discover, apply, verify, restore (internal/provision).
// digi-mcp runs it over WinRM with fixed command lines (plan 068); a human can
// run it the same way. Secrets come on stdin only, and it prints one JSON
// object.
package main

import (
	"context"
	"os"

	"github.com/digitrade-e/digi-erp-connector/internal/provision"
)

func main() {
	out, code := provision.Run(context.Background(), provision.NewEnv(provision.NewSystem()), os.Args[1:], os.Stdin)
	_, _ = os.Stdout.Write(out)
	os.Exit(code)
}
