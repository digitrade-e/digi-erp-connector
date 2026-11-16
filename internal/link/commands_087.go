package link

// Plan 087's commands (DIG-1563, builder C1): named counters and typed
// folders, each a provision subcommand with fixed argv, in its own table so
// other work merges without touching these lines. Not sealed: neither input
// carries a secret. A reviewed addition to the fixed set
// (TestTheFixedCommandSet, TestPlan087Commands).
var provisionCommands087 = map[string]func(args []string) ([]string, bool){
	"counters-check":  fixed("counters", "--check", "--spec", "-"),
	"counters":        fixed("counters", "--spec", "-"),
	"documents-check": fixed("documents", "--check", "--spec", "-"),
	"documents":       fixed("documents", "--spec", "-"),
}

func init() {
	for name, f := range provisionCommands087 {
		commands[name] = f
	}
}
