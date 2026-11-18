// Command connector-rehearsal checks a connector upgrade or downgrade on a
// test box (DIG-1304: GROOPY, before BFL's install). It only reads.
//
//	connector-rehearsal snapshot -data <DataDir> -send <sendOrderDir> -out <file.json>
//	connector-rehearsal check <before.json> <after.json>
//
// snapshot records the order counter, the order index, the order history,
// the receipts' number store and index and the format floor: numbers, states
// and hashes, never content. check names what an upgrade or a downgrade must
// never do — a counter backwards or missing, an index entry lost or
// renumbered, a number issued twice, the receipt store backwards, the floor
// lowered. Exit 0: nothing found; 1: findings; 2: usage or read error.
// Build for the box: GOOS=windows go build ./cmd/connector-rehearsal.
// The procedure: docs/order-cutover-proof.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/digitrade-e/digi-erp-connector/internal/rehearsal"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `usage:
  connector-rehearsal snapshot -data <DataDir> -send <sendOrderDir> -out <file.json>
  connector-rehearsal check <before.json> <after.json>
`

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "snapshot":
		fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
		fs.SetOutput(stderr)
		data := fs.String("data", "", `the connector's DataDir (C:\ProgramData\digi-erp-connector)`)
		send := fs.String("send", "", "sendOrderDir from config.yaml (where lastOrderNumber.json is)")
		out := fs.String("out", "", "file to write the snapshot to")
		if err := fs.Parse(args[1:]); err != nil || *data == "" || *send == "" || *out == "" || fs.NArg() != 0 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		for _, d := range []string{*data, *send} {
			if st, err := os.Stat(d); err != nil || !st.IsDir() {
				fmt.Fprintf(stderr, "%s is not a folder\n", d)
				return 2
			}
		}
		s, err := rehearsal.Take(*data, *send)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		b, _ := json.MarshalIndent(s, "", "  ")
		if err := os.WriteFile(*out, append(b, '\n'), 0o600); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		fmt.Fprintf(stdout, "snapshot %s: order counter %s, %d order index entries, history %d folders (highest %d), %d receipt index entries, format-floor %q\n",
			*out, num(s.OrderCounter), len(s.Orders), s.HistoryCount, s.HistoryMax, len(s.Receipts), s.FormatFloor)
		return 0
	case "check":
		if len(args) != 3 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		var snaps [2]rehearsal.Snapshot
		for i, p := range args[1:] {
			b, err := os.ReadFile(p)
			if err == nil {
				err = json.Unmarshal(b, &snaps[i])
			}
			if err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", p, err)
				return 2
			}
		}
		fmt.Fprintln(stdout, rehearsal.Summary(snaps[0], snaps[1]))
		findings := rehearsal.Check(snaps[0], snaps[1])
		if len(findings) == 0 {
			fmt.Fprintln(stdout, "OK: nothing lost, nothing issued twice, no counter backwards")
			return 0
		}
		for _, f := range findings {
			fmt.Fprintln(stdout, "FINDING: "+f)
		}
		return 1
	}
	fmt.Fprint(stderr, usage)
	return 2
}

func num(p *int64) string {
	if p == nil {
		return "missing"
	}
	return fmt.Sprint(*p)
}
