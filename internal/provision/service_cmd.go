package provision

import (
	"context"
	"io"
	"slices"
	"strings"
)

// ServiceInput is the service command's stdin (plan 086 C3): the daemon
// service's dependencies and recovery, without a full spec — BFL's service
// depends on MSSQLSERVER while its database is MSSQL$WIZSOFT2017, and fixing
// it took RDP.
type ServiceInput struct {
	Version   int    `json:"version"` // 1
	ChangeSet string `json:"changeSet"`
	// DependOn is the whole list of services the daemon depends on: SQL
	// Server services only, each installed on this machine. nil keeps the
	// service's list; [] removes every dependency (a database elsewhere).
	DependOn *[]string `json:"dependOn,omitempty"`
	// Recovery sets the restart-on-failure actions (1, 5, every 30 min, also
	// on non-crash failures); false leaves them as they are.
	Recovery bool `json:"recovery,omitempty"`
}

// MaxDependencies bounds ServiceInput.DependOn.
const MaxDependencies = 8

// DecodeService reads one JSON ServiceInput, strictly like DecodeSpec.
func DecodeService(r io.Reader) (ServiceInput, *Refusal) {
	var in ServiceInput
	if err := decodeStrict(r, &in); err != nil {
		return ServiceInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields",
			"the service input does not decode: %v", err)
	}
	return in, nil
}

// Validate checks the input before anything is read.
func (in ServiceInput) Validate() *Refusal {
	if in.Version != 1 {
		return refuse(CodeSpecInvalid, `send "version": 1`, "version %d is not supported", in.Version)
	}
	if !changeSetRe.MatchString(in.ChangeSet) {
		return refuse(CodeChangeSetID, "use 8 to 64 letters, digits, - or _", "changeSet is not a valid id")
	}
	if in.DependOn == nil && !in.Recovery {
		return refuse(CodeSpecInvalid, "send dependOn, recovery: true, or both", "the input changes nothing")
	}
	if in.DependOn != nil {
		if len(*in.DependOn) > MaxDependencies {
			return refuse(CodeSpecInvalid, "name at most 8 SQL Server services", "dependOn has %d entries", len(*in.DependOn))
		}
		for _, d := range *in.DependOn {
			if !dependencyRe.MatchString(d) {
				return refuse(CodeSpecInvalid, `name a SQL Server service: MSSQLSERVER or MSSQL$<instance>`,
					"dependOn %q is not a SQL Server service name", d)
			}
		}
	}
	return nil
}

// ApplyService sets the daemon service's dependencies and recovery actions
// and nothing else. Every dependency must be a service installed here (one
// that is not would stop the daemon starting at the next boot). The change
// set's pre-image keeps the dependencies before (restore <changeSet> puts
// them back); the recovery actions are not part of a pre-image, as with
// apply. No restart: the service manager reads both at the next start. check
// stops before the first write.
func ApplyService(ctx context.Context, env Env, in ServiceInput, check bool) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if r := in.Validate(); r != nil {
		return res, r
	}
	a := &applier{env: env, s: Spec{Version: in.Version, ChangeSet: in.ChangeSet}, dir: env.Sys.DataDir(), res: res, check: check}
	unlock, err := lockRuns(a.dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	err = a.runService(in)
	a.settle()
	return res, err
}

func (a *applier) runService(in ServiceInput) error {
	var err error
	if a.svc, err = a.env.Sys.Service(ServiceName); err != nil {
		return refuse(CodeServiceFailed, "run it elevated (as an administrator)", "read the %s service: %v", ServiceName, err)
	}
	if !a.svc.Installed {
		return refuse(CodeServiceMissing, "install the connector first (its installer creates the service)", "the %s service is not installed", ServiceName)
	}
	var want []string
	deps := in.DependOn != nil
	if deps {
		want = dedupe(upper(*in.DependOn))
		for _, d := range want {
			st, err := a.env.Sys.Service(d)
			if err != nil {
				return refuse(CodeServiceFailed, "run it elevated (as an administrator)", "read the %s service: %v", d, err)
			}
			if !st.Installed {
				return refuse(CodeDependencyMissing, "name a SQL Server service installed on this machine (discover lists sql.instances[].service)",
					"%s is not a service on this machine: the connector would not start after the next reboot", d)
			}
			// Review m1: a Disabled dependency fails the connector's next start (SCM dependency failure, which
			// recovery does not retry), and this command restarts nothing, so it would never see it.
			if strings.EqualFold(st.StartType, "disabled") {
				return refuse(CodeDependencyDisabled, "set that SQL Server service's start type to Automatic first, or name the instance the database is on",
					"%s is disabled: the connector would not start at its next start or reboot", d)
			}
			if st.State != "running" {
				a.warn("%s is %s (start type %s): the service manager starts it with the connector, and the connector does not start if it cannot",
					d, st.State, st.StartType)
			}
		}
	}
	depsChange := deps && !sameFold(want, a.svc.Dependencies)
	recoveryChange := in.Recovery && !a.svc.RecoveryPlanned
	if a.check {
		a.res.Check = &CheckResult{Config: "unchanged", Dependencies: depsChange, Recovery: recoveryChange}
		return nil
	}
	if !depsChange && !recoveryChange {
		if deps {
			a.step("service_dependencies", false, strings.Join(want, ", "))
		}
		if in.Recovery {
			a.step("service_recovery", false, "")
		}
		return nil
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	var st preState
	if deps {
		st.Service = &preService{Dependencies: slices.Clone(a.svc.Dependencies)}
	}
	if err := a.captureState(nil, st); err != nil {
		return err
	}
	if deps {
		if depsChange {
			if err := a.env.Sys.SetServiceDependencies(ServiceName, want); err != nil {
				return refuse(CodeServiceFailed, "run it elevated (as an administrator)", "set the service's dependencies: %v", err)
			}
		}
		detail := strings.Join(want, ", ")
		if len(want) == 0 {
			detail = "none"
		}
		a.step("service_dependencies", depsChange, detail)
	}
	if in.Recovery {
		changed, err := a.env.Sys.EnsureServiceRecovery(ServiceName)
		if err != nil {
			return refuse(CodeServiceFailed, "run it elevated (as an administrator)", "set the service's recovery: %v", err)
		}
		a.step("service_recovery", changed, "")
	}
	a.step("restart", false, "the service manager reads them at the next start")
	return nil
}

func upper(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToUpper(s))
	}
	return out
}
