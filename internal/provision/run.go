package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/buildinfo"
)

// MaxInput bounds what is read from stdin.
const MaxInput = 1 << 20

// Envelope is what every command prints: exactly one JSON object.
type Envelope struct {
	Command string   `json:"command"`
	OK      bool     `json:"ok"`
	Tool    string   `json:"tool"`
	Refusal *Refusal `json:"refusal,omitempty"`
	Result  any      `json:"result,omitempty"`
}

// Exit codes.
const (
	ExitOK      = 0
	ExitRefused = 1 // the envelope says why
	ExitUsage   = 2
)

// Usage is the command line, for a human.
const Usage = `usage:
  digi-erp-connector-provision discover [--spec -]      read the box (stdin: optional {"db":{...}} to fingerprint the named database)
  digi-erp-connector-provision apply [--check] --spec - make the connector match the spec on stdin; --check stops before the first write
  digi-erp-connector-provision options [--check] --spec - set config.yaml options only (image folders, query limits, ...); no credential on stdin
  digi-erp-connector-provision auth [--check] --spec -    set the API credential only (and, with rotateSecret, a new signing secret); no DB credential
  digi-erp-connector-provision daemon <restart|stop|start> [--assume-idle]   control the service (the order queue is asked first; start/restart end with the proof)
  digi-erp-connector-provision service [--check] --spec - set the service's SQL Server dependencies and recovery only; no restart
  digi-erp-connector-provision db [--check] --spec -     change the database login and settings only (or turn the database off); no API credential
  digi-erp-connector-provision firewall-remove [--check] --spec -   remove inbound rules this tool made (the connector's group); no restart
  digi-erp-connector-provision receipt-seed <N>         create the receipt number store with N as the next receipt (write-if-absent)
  digi-erp-connector-provision receipts [--check] --spec - set the receipt settings only (sendReceiptDir, model (b) importer, card names)
  digi-erp-connector-provision orders [--check] --spec -   set the order settings only (sendOrderDir, importer, model (a) pickup folder and layout)
  digi-erp-connector-provision counters [--check] --spec - create a named counter, set its range, move it forward (start-from), or retire it (plan 087)
  digi-erp-connector-provision documents [--check] --spec - set the typed folders documentRule names, and the receipts' folder and counter (plan 087)
  digi-erp-connector-provision inflight [--check] --spec -   resolve one unfinished order or receipt index entry as failed or done (the check shows it)
  digi-erp-connector-provision folder-acl [--check] --spec - lock down a document folder (only administrators, SYSTEM and trusted writers write), or restore what a lock-down recorded
  digi-erp-connector-provision verify [--spec -]        check the running connector (stdin: optional expectations)
  digi-erp-connector-provision restore <changeSet> [--assume-idle] [--accept-unreadable=N]
  digi-erp-connector-provision state [--spec -]         read the box's state: services, order queue, counters, indexes, TLS, link.yaml, disk (read-only; stdin {} or empty)
  digi-erp-connector-provision logs --spec -            read a log's tail, redacted (stdin: {"source":"server|link|ui|events-daemon|events-link",...})
  digi-erp-connector-provision version
Secrets travel on stdin only. The output is one JSON object; exit 0 ok, 1 refused, 2 usage.`

// Run runs one command and returns what to print and the exit code. Every
// byte printed passes Redact over the secrets the input carried and the ones
// born here.
func Run(ctx context.Context, env Env, args []string, stdin io.Reader) (out []byte, code int) {
	if len(args) == 0 {
		return []byte(Usage + "\n"), ExitUsage
	}
	cmd, rest := args[0], args[1:]
	var secrets []string
	defer func() {
		// A panic answers like everything else: one envelope, redacted, since
		// its text can quote what the run held.
		if r := recover(); r != nil {
			out, code = envelope(cmd, nil, refuse(CodeInternal, "run verify to see the box's state, and report it",
				"provision stopped on an internal error: %v", r), secrets), ExitRefused
		}
	}()
	env.ensureDefaults()
	if u, ok := env.Sys.(interface{ Unsupported() error }); ok && cmd != "version" {
		return envelope(cmd, nil, refuse(CodeUnsupported, "run it on the Windows machine the connector is installed on", "%v", u.Unsupported()), nil), ExitRefused
	}

	var (
		result any
		err    error
	)
	switch cmd {
	case "version":
		if len(rest) != 0 {
			return []byte(Usage + "\n"), ExitUsage
		}
		result = map[string]string{"version": buildinfo.Version}
	// plan 086 C4: read-only diagnostics (logs.go, state.go)
	case "logs":
		var in LogsInput
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		if r := decodeInput(stdin, &in); r != nil {
			return envelope(cmd, nil, r, nil), ExitRefused
		}
		var res *LogsResult
		res, err = Logs(env, in)
		result = res
		if res != nil {
			secrets = append(secrets, res.secrets...)
		}
	case "state":
		var in StateInput
		if ok, u := readSpecFlag(rest); !u {
			return []byte(Usage + "\n"), ExitUsage
		} else if ok {
			if r := decodeOptional(stdin, &in); r != nil {
				return envelope(cmd, nil, r, nil), ExitRefused
			}
		}
		result, err = State(ctx, env, in)
	case "discover":
		var in DiscoverInput
		if ok, u := readSpecFlag(rest); !u {
			return []byte(Usage + "\n"), ExitUsage
		} else if ok {
			if r := decodeInput(stdin, &in); r != nil {
				return envelope(cmd, nil, r, nil), ExitRefused
			}
		}
		if in.DB != nil {
			secrets = append(secrets, in.DB.Password)
			if r := in.DB.Validate(); r != nil {
				return envelope(cmd, nil, r, secrets), ExitRefused
			}
		}
		result, err = Discover(ctx, env, in)
	case "apply":
		check := len(rest) > 0 && rest[0] == "--check"
		if check {
			rest = rest[1:]
		}
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		spec, r := DecodeSpec(io.LimitReader(stdin, MaxInput))
		if r != nil {
			return envelope(cmd, nil, r, nil), ExitRefused
		}
		secrets = append(secrets, spec.secrets()...)
		var res *ApplyResult
		if check {
			res, err = ApplyCheck(ctx, env, spec)
		} else {
			res, err = Apply(ctx, env, spec)
		}
		result = res
		if res != nil {
			secrets = append(secrets, res.secrets...)
		}
	case "options":
		check := len(rest) > 0 && rest[0] == "--check"
		if check {
			rest = rest[1:]
		}
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		in, r := DecodeOptions(io.LimitReader(stdin, MaxInput))
		if r != nil {
			return envelope(cmd, nil, r, nil), ExitRefused
		}
		result, err = ApplyOptions(ctx, env, in, check)
	case "auth":
		check := len(rest) > 0 && rest[0] == "--check"
		if check {
			rest = rest[1:]
		}
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		in, r := DecodeAuth(io.LimitReader(stdin, MaxInput))
		if r != nil {
			return envelope(cmd, nil, r, nil), ExitRefused
		}
		secrets = append(secrets, in.secrets()...)
		var res *ApplyResult
		res, err = ApplyAuth(ctx, env, in, check)
		result = res
		if res != nil {
			secrets = append(secrets, res.secrets...)
		}
	case "daemon":
		in := DaemonInput{}
		for i, a := range rest {
			switch {
			case i == 0 && (a == DaemonRestart || a == DaemonStop || a == DaemonStart):
				in.Action = a
			case i == 1 && a == "--assume-idle":
				in.AssumeIdle = true
			default:
				return []byte(Usage + "\n"), ExitUsage
			}
		}
		if in.Action == "" {
			return []byte(Usage + "\n"), ExitUsage
		}
		var res *ApplyResult
		res, err = ControlDaemon(ctx, env, in)
		result = res
	case "service":
		check := len(rest) > 0 && rest[0] == "--check"
		if check {
			rest = rest[1:]
		}
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		in, r := DecodeService(io.LimitReader(stdin, MaxInput))
		if r != nil {
			return envelope(cmd, nil, r, nil), ExitRefused
		}
		result, err = ApplyService(ctx, env, in, check)
	case "db":
		check := len(rest) > 0 && rest[0] == "--check"
		if check {
			rest = rest[1:]
		}
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		in, r := DecodeDB(io.LimitReader(stdin, MaxInput))
		if r != nil {
			return envelope(cmd, nil, r, nil), ExitRefused
		}
		secrets = append(secrets, in.secrets()...)
		var res *ApplyResult
		res, err = ApplyDB(ctx, env, in, check)
		result = res
		if res != nil {
			secrets = append(secrets, res.secrets...)
		}
	case "firewall-remove":
		check := len(rest) > 0 && rest[0] == "--check"
		if check {
			rest = rest[1:]
		}
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		in, r := DecodeFirewallRemove(io.LimitReader(stdin, MaxInput))
		if r != nil {
			return envelope(cmd, nil, r, nil), ExitRefused
		}
		result, err = RemoveFirewallRules(ctx, env, in, check)
	case "receipt-seed":
		if len(rest) != 1 || !receiptSeedArg.MatchString(rest[0]) {
			return []byte(Usage + "\n"), ExitUsage
		}
		n, _ := strconv.ParseInt(rest[0], 10, 64) // nine digits at most: it parses
		var res *ApplyResult
		res, err = SeedReceipts(ctx, env, n)
		result = res
	case "receipts", "orders":
		check := len(rest) > 0 && rest[0] == "--check"
		if check {
			rest = rest[1:]
		}
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		if cmd == "receipts" {
			in, r := DecodeReceipts(io.LimitReader(stdin, MaxInput))
			if r != nil {
				return envelope(cmd, nil, r, nil), ExitRefused
			}
			result, err = ApplyReceipts(ctx, env, in, check)
		} else {
			in, r := DecodeOrders(io.LimitReader(stdin, MaxInput))
			if r != nil {
				return envelope(cmd, nil, r, nil), ExitRefused
			}
			result, err = ApplyOrders(ctx, env, in, check)
		}
	// plan 087: named counters and typed folders
	// plan 087 review R6 (v1.0.32): one unfinished index entry resolved
	case "inflight":
		check := len(rest) > 0 && rest[0] == "--check"
		if check {
			rest = rest[1:]
		}
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		in, r := DecodeInflight(io.LimitReader(stdin, MaxInput))
		if r != nil {
			return envelope(cmd, nil, r, nil), ExitRefused
		}
		result, err = ApplyInflight(ctx, env, in, check)
	case "counters", "documents", "folder-acl":
		check := len(rest) > 0 && rest[0] == "--check"
		if check {
			rest = rest[1:]
		}
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		if cmd == "folder-acl" { // plan 087 review R1c
			in, r := DecodeFolderACL(io.LimitReader(stdin, MaxInput))
			if r != nil {
				return envelope(cmd, nil, r, nil), ExitRefused
			}
			result, err = ApplyFolderACL(ctx, env, in, check)
		} else if cmd == "counters" {
			in, r := DecodeCounters(io.LimitReader(stdin, MaxInput))
			if r != nil {
				return envelope(cmd, nil, r, nil), ExitRefused
			}
			result, err = ApplyCounters(ctx, env, in, check)
		} else {
			in, r := DecodeDocuments(io.LimitReader(stdin, MaxInput))
			if r != nil {
				return envelope(cmd, nil, r, nil), ExitRefused
			}
			result, err = ApplyDocuments(ctx, env, in, check)
		}
	case "verify":
		var in VerifyInput
		if ok, u := readSpecFlag(rest); !u {
			return []byte(Usage + "\n"), ExitUsage
		} else if ok {
			if r := decodeInput(stdin, &in); r != nil {
				return envelope(cmd, nil, r, nil), ExitRefused
			}
		}
		var res *VerifyResult
		res, err = Verify(ctx, env, in)
		result = res
		if err == nil && res != nil && !res.OK {
			err = refuse("checks_failed", "read the failed checks", "%d check(s) failed", failedChecks(res))
		}
	case "restore":
		in := RestoreInput{}
		for _, a := range rest {
			switch {
			case a == "--assume-idle":
				in.AssumeIdle = true
			case strings.HasPrefix(a, "--accept-unreadable="): // review R7
				n, err := strconv.Atoi(strings.TrimPrefix(a, "--accept-unreadable="))
				if err != nil || in.AcceptUnreadable != nil {
					return []byte(Usage + "\n"), ExitUsage
				}
				in.AcceptUnreadable = &n
			case strings.HasPrefix(a, "-") || in.ChangeSet != "":
				return []byte(Usage + "\n"), ExitUsage
			default:
				in.ChangeSet = a
			}
		}
		var res *ApplyResult
		res, err = Restore(ctx, env, in)
		result = res
		if res != nil {
			secrets = append(secrets, res.secrets...)
		}
	default:
		return []byte(Usage + "\n"), ExitUsage
	}

	var refusal *Refusal
	if err != nil && !errors.As(err, &refusal) {
		refusal = &Refusal{Code: "failed", Message: err.Error()}
	}
	code = ExitOK
	if refusal != nil {
		code = ExitRefused
	}
	return envelope(cmd, result, refusal, secrets), code
}

// receiptSeedArg is receipt-seed's N: 1 to 9 digits, no sign, no leading zero.
var receiptSeedArg = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// readSpecFlag accepts nothing or exactly "--spec -". ok: input comes on
// stdin; usageOK: the flags were understood.
func readSpecFlag(args []string) (ok, usageOK bool) {
	switch {
	case len(args) == 0:
		return false, true
	case len(args) == 2 && args[0] == "--spec" && args[1] == "-":
		return true, true
	}
	return false, false
}

// decodeInput reads discover's and verify's optional input. An empty stdin
// is {} (plan 086 C10): over the link a command always has a stdin, and one
// with nothing to say should not have to send a body.
func decodeInput(stdin io.Reader, v any) *Refusal {
	b, err := io.ReadAll(io.LimitReader(stdin, MaxInput))
	if err == nil && len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err == nil {
		err = decodeStrict(bytes.NewReader(b), v)
	}
	if err != nil {
		return refuse(CodeSpecInvalid, "send one JSON object with only the documented fields", "the input does not decode: %v", err)
	}
	return nil
}

func failedChecks(res *VerifyResult) int {
	n := 0
	for _, c := range res.Checks {
		if !c.OK {
			n++
		}
	}
	return n
}

// envelope encodes the one object a command prints, redacted inside its
// strings (redactJSON) and in ASCII only (asciiOnly).
func envelope(cmd string, result any, refusal *Refusal, secrets []string) []byte {
	e := Envelope{Command: cmd, OK: refusal == nil, Tool: buildinfo.Version, Refusal: refusal, Result: result}
	raw, err := json.Marshal(e)
	if err == nil {
		var out []byte
		if out, err = redactJSON(raw, secrets...); err == nil {
			return asciiOnly(out)
		}
	}
	b, _ := json.Marshal(Envelope{Command: cmd, Tool: buildinfo.Version,
		Refusal: &Refusal{Code: "failed", Message: "the result does not encode"}}) // a string and a struct always marshal
	return asciiOnly(append(b, '\n'))
}

// ensureDefaults fills what a caller left out with the real box's.
func (e *Env) ensureDefaults() {
	d := NewEnv(e.Sys)
	if e.OpenDB == nil {
		e.OpenDB = d.OpenDB
	}
	if e.HTTP == nil {
		e.HTTP = d.HTTP
	}
	if e.LocalAddr == nil {
		e.LocalAddr = d.LocalAddr
	}
	if e.Now == nil {
		e.Now = d.Now
	}
	if e.Sleep == nil {
		e.Sleep = d.Sleep
	}
	if e.ProofTimeout <= 0 {
		e.ProofTimeout = d.ProofTimeout
	}
	if e.LookupIP == nil {
		e.LookupIP = d.LookupIP
	}
}
