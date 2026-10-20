package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
  digi-erp-connector-provision discover [--spec -]      read the box (stdin: optional {"db":{...}} to fingerprint databases)
  digi-erp-connector-provision apply --spec -           make the connector match the spec on stdin
  digi-erp-connector-provision verify [--spec -]        check the running connector (stdin: optional expectations)
  digi-erp-connector-provision restore <changeSet> [--assume-idle]
  digi-erp-connector-provision version
Secrets travel on stdin only. The output is one JSON object; exit 0 ok, 1 refused, 2 usage.`

// Run runs one command and returns what to print and the exit code. Every
// byte printed passes Redact over the secrets the input carried and the ones
// born here.
func Run(ctx context.Context, env Env, args []string, stdin io.Reader) ([]byte, int) {
	if len(args) == 0 {
		return []byte(Usage + "\n"), ExitUsage
	}
	cmd, rest := args[0], args[1:]
	env.ensureDefaults()
	if u, ok := env.Sys.(interface{ Unsupported() error }); ok && cmd != "version" {
		return envelope(cmd, nil, refuse(CodeUnsupported, "run it on the Windows machine the connector is installed on", "%v", u.Unsupported()), nil), ExitRefused
	}

	var (
		result  any
		err     error
		secrets []string
	)
	switch cmd {
	case "version":
		if len(rest) != 0 {
			return []byte(Usage + "\n"), ExitUsage
		}
		result = map[string]string{"version": buildinfo.Version}
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
		if ok, u := readSpecFlag(rest); !u || !ok {
			return []byte(Usage + "\n"), ExitUsage
		}
		spec, r := DecodeSpec(io.LimitReader(stdin, MaxInput))
		if r != nil {
			return envelope(cmd, nil, r, nil), ExitRefused
		}
		secrets = append(secrets, spec.secrets()...)
		var res *ApplyResult
		res, err = Apply(ctx, env, spec)
		result = res
		if res != nil {
			secrets = append(secrets, res.secrets...)
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
	code := ExitOK
	if refusal != nil {
		code = ExitRefused
	}
	return envelope(cmd, result, refusal, secrets), code
}

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

func decodeInput(stdin io.Reader, v any) *Refusal {
	if err := decodeStrict(io.LimitReader(stdin, MaxInput), v); err != nil {
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
// strings (redactJSON).
func envelope(cmd string, result any, refusal *Refusal, secrets []string) []byte {
	e := Envelope{Command: cmd, OK: refusal == nil, Tool: buildinfo.Version, Refusal: refusal, Result: result}
	raw, err := json.Marshal(e)
	if err == nil {
		var out []byte
		if out, err = redactJSON(raw, secrets...); err == nil {
			return out
		}
	}
	return []byte(fmt.Sprintf(`{"command":%q,"ok":false,"refusal":{"code":"failed","message":"the result does not encode"}}`+"\n", cmd))
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
}
