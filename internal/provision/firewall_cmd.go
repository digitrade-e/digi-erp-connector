package provision

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// FirewallRemoveInput is the firewall-remove command's stdin (plan 086 C7):
// the inbound rules to take away once a box is on the link — its old 8082 /
// 8083 rules, which apply refuses to touch in link mode. Only rules this tool
// made (the connector's own group) are removed; a client's rule of the same
// name is refused, never touched.
type FirewallRemoveInput struct {
	Version   int      `json:"version"` // 1
	ChangeSet string   `json:"changeSet"`
	Names     []string `json:"names"`
}

// MaxFirewallNames bounds FirewallRemoveInput.Names.
const MaxFirewallNames = 16

// DecodeFirewallRemove reads one JSON FirewallRemoveInput, strictly like
// DecodeSpec.
func DecodeFirewallRemove(r io.Reader) (FirewallRemoveInput, *Refusal) {
	var in FirewallRemoveInput
	if err := decodeStrict(r, &in); err != nil {
		return FirewallRemoveInput{}, refuse(CodeSpecInvalid, "send one JSON object with only the documented fields",
			"the firewall-remove input does not decode: %v", err)
	}
	return in, nil
}

// Validate checks the input before anything is read.
func (in FirewallRemoveInput) Validate() *Refusal {
	if in.Version != 1 {
		return refuse(CodeSpecInvalid, `send "version": 1`, "version %d is not supported", in.Version)
	}
	if !changeSetRe.MatchString(in.ChangeSet) {
		return refuse(CodeChangeSetID, "use 8 to 64 letters, digits, - or _", "changeSet is not a valid id")
	}
	if len(in.Names) == 0 || len(in.Names) > MaxFirewallNames {
		return refuse(CodeSpecInvalid, fmt.Sprintf("name 1 to %d rules", MaxFirewallNames), "names has %d entries", len(in.Names))
	}
	seen := map[string]bool{}
	for _, n := range in.Names {
		if !ruleNameRe.MatchString(n) {
			return refuse(CodeSpecInvalid, "use up to 64 letters, digits, spaces, . _ ( ) or -", "names: %q is not a rule name", n)
		}
		if k := strings.ToLower(n); seen[k] {
			return refuse(CodeSpecInvalid, "name each rule once", "names: %q twice", n)
		} else {
			seen[k] = true
		}
	}
	return nil
}

// RemoveFirewallRules removes the named inbound rules this tool made, with
// apply's guarantees: every rule is read before the first removal, and one
// that is not the connector's (its group) refuses the whole run
// (firewall_foreign); the change set's pre-image keeps each rule removed, so
// restore <changeSet> re-creates them. A name with no rule is no error: it is
// already gone. Allowed behind the link — closing the old inbound rules is
// what it is for. No restart: the service does not read the firewall. check
// stops before the first write.
func RemoveFirewallRules(_ context.Context, env Env, in FirewallRemoveInput, check bool) (*ApplyResult, error) {
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
	err = a.runFirewallRemove(in)
	a.settle()
	return res, err
}

func (a *applier) runFirewallRemove(in FirewallRemoveInput) error {
	var present []FirewallRule
	var absent []string
	for _, name := range in.Names {
		rule, err := a.env.Sys.FirewallRule(name)
		if err != nil {
			return refuse(CodeFirewallFailed, "run it elevated (as an administrator)", "read the firewall rule %q: %v", name, err)
		}
		switch {
		case rule == nil:
			absent = append(absent, name)
		case !rule.Ours:
			return refuse(CodeFirewallForeign, "leave it out: a rule this tool did not make is the client's to remove",
				"the firewall rule %q is not the connector's (its group is not %s)", name, firewallGroup)
		default:
			present = append(present, *rule)
		}
	}
	if a.check {
		c := &CheckResult{Config: "unchanged", Firewall: "keep"}
		for _, r := range present {
			c.Rules = append(c.Rules, r.Name)
		}
		if len(present) > 0 {
			c.Firewall = "remove"
		}
		a.res.Check = c
		return nil
	}
	for _, name := range absent {
		a.step("firewall", false, fmt.Sprintf("no rule named %q", name))
	}
	if len(present) == 0 {
		return nil
	}
	if err := a.env.Sys.ProtectDataDir(); err != nil {
		return refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	var st preState
	for _, r := range present {
		rule := r
		st.Firewalls = append(st.Firewalls, preFirewall{Name: r.Name, Rule: &rule})
	}
	if err := a.captureState(nil, st); err != nil {
		return err
	}
	for _, r := range present {
		if err := a.env.Sys.RemoveFirewallRule(r.Name); err != nil {
			return a.failFirewall(refuse(CodeFirewallFailed, "run it elevated (as an administrator)", "remove the firewall rule %q: %v", r.Name, err))
		}
		a.step("firewall", true, fmt.Sprintf("removed %q (TCP %d from %s)", r.Name, r.Port, sourcesText(r.RemoteAddress)))
	}
	return nil
}

// failFirewall puts back the rules this run removed before a removal failed:
// the run's pre-image, as apply's rollback does.
func (a *applier) failFirewall(cause error) error {
	a.res.RolledBack = true
	p, err := loadPreimage(runDir(a.dir, a.s.ChangeSet))
	if err == nil {
		_, err = putBack(a.env, p)
	}
	if err != nil {
		a.warn("the rollback did not finish: %v; restore by hand from provision\\%s\\run", err, a.s.ChangeSet)
	}
	return cause
}

func sourcesText(in []string) string {
	if len(in) == 0 {
		return "any address"
	}
	return strings.Join(in, ", ")
}
