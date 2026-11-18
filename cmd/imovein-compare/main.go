// Command imovein-compare is the byte-compare harness of DIG-1304 (plan 075
// P1-6): it checks that the IMOVEIN file the connector writes for an order is
// byte for byte the legacy Digitrade app's file for the same order.
//
//	imovein-compare diff [-allow 9,10,36] <legacy.doc> <connector.doc>
//	imovein-compare run [-out <dir>] <manifest.json>
//
// diff compares two files already written (e.g. by a connector on GROOPY).
// run renders each case's sendOrder body through the connector's own route
// and writer, offline, and compares it with the case's legacy file.
// Exit 0: every comparison matches; 1: something differs; 2: usage or input
// error. The procedure: docs/order-cutover-proof.md. Real legacy files hold
// customer data: keep them, and this tool's output, out of the repository.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/imovcompare"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `usage:
  imovein-compare diff [-allow 9,10,36] <legacy.doc> <connector.doc>
  imovein-compare run [-out <dir>] <manifest.json>
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "diff":
		fs := flag.NewFlagSet("diff", flag.ContinueOnError)
		fs.SetOutput(stderr)
		allowText := fs.String("allow", "", "comma-separated PRM lines whose differences do not fail (e.g. 9,10,36)")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 2 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		allow, err := parseAllow(*allowText)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		want, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		got, err := os.ReadFile(fs.Arg(1))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		r := imovcompare.Compare(want, got, allow)
		fmt.Fprint(stdout, r.String())
		if !r.Match() {
			return 1
		}
		return 0
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.SetOutput(stderr)
		out := fs.String("out", "", "folder to keep each connector file in, as <case>.<file name>")
		if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		rep, err := imovcompare.RunManifest(context.Background(), fs.Arg(0), *out)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprint(stdout, rep.String())
		if !rep.OK() {
			return 1
		}
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

// parseAllow reads "9,10,36". Which lines exist is the manifest's and
// Compare's business: an unknown line simply never differs.
func parseAllow(s string) (map[int]bool, error) {
	allow := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 2 || n > 86 {
			return nil, fmt.Errorf("-allow: %q is not a PRM line (2-86)", part)
		}
		allow[n] = true
	}
	return allow, nil
}
