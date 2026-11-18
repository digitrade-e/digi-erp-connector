package provision

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Console is the person at this machine's keyboard (sql-setup only, plan 090 D5). Env.Console is nil when the input
// is not an interactive console — WinRM, the link, a pipe — and sql-setup then refuses: the administrator's password
// is typed here and nowhere else.
type Console interface {
	Ask(prompt string) (string, error)       // one line, shown as it is typed
	AskSecret(prompt string) (string, error) // one line, not shown
	Say(line string)
}

// sqlSetupConsole asks the person, runs SQLSetup and says what happened. Its output is for that person (ASCII text,
// redacted), not an envelope.
func sqlSetupConsole(ctx context.Context, env Env) ([]byte, int) {
	con := env.Console
	if con == nil {
		return envelope("sql-setup", nil, refuse(CodeSQLSetupConsoleOnly,
			"run it yourself, as an administrator, in a console on this server",
			"sql-setup asks for an SQL Server administrator's password at this machine's console, and there is none here"), nil), ExitRefused
	}
	say := func(format string, args ...any) { con.Say(string(asciiOnly([]byte(fmt.Sprintf(format, args...))))) }
	say("This creates the connector's own SQL Server login, with read rights only.")
	say("The administrator login you type is used once, here on this server, and is not kept. Digitrade never sees its password.")
	say("")

	def := ""
	if insts, err := env.Sys.SQLInstances(); err == nil && len(insts) > 0 {
		say("SQL Server instances on this server:")
		for _, in := range insts {
			say("  %s", instanceAddress(in))
		}
		def = instanceAddress(insts[0])
	}
	ask := func(prompt, dflt string) (string, error) {
		p := prompt
		if dflt != "" {
			p += " [" + dflt + "]"
		}
		v, err := con.Ask(p + ": ")
		v = strings.TrimSpace(v)
		if v == "" {
			v = dflt
		}
		return v, err
	}
	yes := func(prompt string, dflt bool) (bool, error) {
		d := "y/N"
		if dflt {
			d = "Y/n"
		}
		v, err := con.Ask(prompt + " [" + d + "]: ")
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "y", "yes":
			return true, err
		case "n", "no":
			return false, err
		}
		return dflt, err
	}

	var in SQLSetupInput
	server, err := ask(`SQL Server (host\instance, or host:port)`, def)
	if err == nil {
		in.Host, in.Port = splitHostPort(server)
		in.Database, err = ask("Database (the Hashavshevet company's)", "")
	}
	if err == nil {
		in.AdminUser, err = ask("Administrator login (sysadmin, or securityadmin with db_owner on that database)", "")
	}
	if err == nil {
		in.AdminPassword, err = con.AskSecret("Its password (not shown): ")
	}
	if err == nil {
		in.Encrypt, err = yes("Encrypt the connection? Only if this SQL Server requires it", false)
	}
	if err == nil && in.Encrypt {
		in.TrustServerCertificate, err = yes("Trust its certificate (a self-signed one)?", true)
	}
	if err != nil {
		say("Not done: the answers could not be read (%v).", err)
		return nil, ExitRefused
	}
	secrets := []string{in.AdminPassword}
	res, err := SQLSetup(ctx, env, in)
	in.AdminPassword = ""
	if err != nil {
		var r *Refusal
		if !errors.As(err, &r) {
			r = &Refusal{Code: "failed", Message: err.Error()}
		}
		say("Not done (%s): %s", r.Code, Redact(r.Message, secrets...))
		if r.Fix != "" {
			say("Fix: %s", r.Fix)
		}
		say("Nothing is kept from a run that stopped; run it again once that is fixed.")
		return nil, ExitRefused
	}
	say("")
	say("Done. Login %s: %s. Procedures: %s.", res.Login, res.LoginDone, res.Procedures)
	say("The administrator login is not kept. If it was made only for this, you may disable it now.")
	say("Tell Digitrade it is done: they finish setting up the connector from here.")
	return nil, ExitOK
}

// instanceAddress is how sql-setup offers an instance: host\name, or host:port for the default instance.
func instanceAddress(in SQLInstance) string {
	if in.Name != "MSSQLSERVER" {
		return `127.0.0.1\` + in.Name
	}
	port := 1433
	switch {
	case in.StaticPort > 0:
		port = in.StaticPort
	case in.DynamicPort > 0:
		port = in.DynamicPort
	}
	return "127.0.0.1:" + strconv.Itoa(port)
}

// splitHostPort reads host:port (the port a number); anything else is a host or host\instance with no port.
func splitHostPort(s string) (string, int) {
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s, `\`) {
		if p, err := strconv.Atoi(s[i+1:]); err == nil {
			return s[:i], p
		}
	}
	return s, 0
}
