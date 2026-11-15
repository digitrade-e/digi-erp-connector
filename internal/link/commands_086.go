package link

// Plan 086's commands (builder B2), kept apart from the base set in commands.go so that the commands other work adds
// there merge without touching these. Each is a reviewed addition to the fixed set (TestTheFixedCommandSet).

func init() {
	// C4: read-only diagnostics, run by provision.exe like every other command; the answer is redacted on the box
	// (plan 086 D1).
	commands["logs"] = fixed("logs", "--spec", "-")
	commands["state"] = fixed("state", "--spec", "-")
}
