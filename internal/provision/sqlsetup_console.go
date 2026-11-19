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
func sqlSetupConsole(ctx context.Context, env Env) (out []byte, code int) {
	con := env.Console
	if con == nil {
		return envelope("sql-setup", nil, refuse(CodeSQLSetupConsoleOnly,
			"run it yourself, as an administrator, in a console on this server",
			"sql-setup asks for an SQL Server administrator's password at this machine's console, and there is none here"), nil), ExitRefused
	}
	var secrets []string // grows as the run learns them; every line said passes Redact over them
	say := func(format string, args ...any) {
		con.Say(string(asciiOnly([]byte(Redact(fmt.Sprintf(format, args...), secrets...)))))
	}
	defer func() {
		if r := recover(); r != nil { // never the panic's text: it can quote what the run held
			say("Not done: sql-setup stopped on an internal error. Nothing is kept from a run that stopped; run it again, or ask Digitrade.")
			out, code = nil, ExitRefused
		}
	}()
	// Who can run it, before a password is asked: an elevated console, on a paired server.
	login, err := sqlSetupBox(env.Sys.DataDir())
	if err != nil {
		var r *Refusal
		if errors.As(err, &r) {
			say("Not done (%s): %s", r.Code, r.Message)
			say("Fix: %s", r.Fix)
		}
		return nil, ExitRefused
	}
	say("This creates the connector's own SQL Server login, with read rights only.")
	say("The administrator login you type is used once, here on this server, and is not kept. Digitrade never sees its password.")
	say("")

	def := ""
	if insts, err := env.Sys.SQLInstances(); err == nil && len(insts) > 0 {
		say("SQL Server instances on this server:")
		for _, in := range insts {
			say("  %s%s", instanceAddress(in), instanceNotes(in))
		}
		def = instanceAddress(insts[0])
	}
	// On a box in service the only answer sql-setup takes is the connector's own server: offer that.
	if cur, inService, err := connectorUses(env.Sys.DataDir(), login); err == nil && inService {
		def = serverAddress(cur.DB.Host, cur.DB.Port)
		say("The connector already uses %s on %s, database %s: a run now only puts the procedures back.", login, def, cur.DB.Database)
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
	var server string
	server, err = ask(`SQL Server (host\instance, or host:port)`, def)
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
	secrets = append(secrets, in.AdminPassword)
	res, err := SQLSetup(ctx, env, in)
	in.AdminPassword = ""
	if res != nil {
		secrets = append(secrets, res.secrets...)
	}
	if err != nil {
		var r *Refusal
		if !errors.As(err, &r) {
			r = &Refusal{Code: "failed", Message: err.Error()}
		}
		say("Not done (%s): %s", r.Code, r.Message)
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

// instanceAddress is how sql-setup offers an instance: host:port when it has a static port (no SQL Server Browser
// needed), else host\name for a named instance (the Browser finds its port), else the default instance's port.
func instanceAddress(in SQLInstance) string {
	switch {
	case in.StaticPort > 0:
		return "127.0.0.1:" + strconv.Itoa(in.StaticPort)
	case in.Name != "MSSQLSERVER":
		return `127.0.0.1\` + in.Name
	case in.DynamicPort > 0:
		return "127.0.0.1:" + strconv.Itoa(in.DynamicPort)
	}
	return "127.0.0.1:1433"
}

// instanceNotes says what will stop the connector's login on an instance, before anyone types a password for it.
func instanceNotes(in SQLInstance) string {
	var n []string
	if !in.TCPEnabled {
		n = append(n, "TCP/IP is off")
	}
	if !in.MixedAuth {
		n = append(n, "SQL logins are off (Windows authentication only)")
	}
	if in.StaticPort == 0 && in.Name == "MSSQLSERVER" && in.DynamicPort > 0 {
		n = append(n, "a dynamic port, which can change at a restart")
	}
	if len(n) == 0 {
		return ""
	}
	return "  (" + strings.Join(n, "; ") + ")"
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
