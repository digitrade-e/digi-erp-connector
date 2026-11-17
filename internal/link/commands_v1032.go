package link

// Connector v1.0.32's commands (plan 087 review R6): one unfinished order or
// receipt index entry resolved from afar, each a provision subcommand with
// fixed argv, in its own table so other work merges without touching these
// lines. Not sealed: the input carries no secret. A reviewed addition to the
// fixed set (TestTheFixedCommandSet, TestV1032Commands).
var provisionCommandsV1032 = map[string]func(args []string) ([]string, bool){
	"inflight-check": fixed("inflight", "--check", "--spec", "-"),
	"inflight":       fixed("inflight", "--spec", "-"),
}

func init() {
	for name, f := range provisionCommandsV1032 {
		commands[name] = f
	}
}
