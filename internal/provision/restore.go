package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// RestoreInput names the change set whose pre-image Restore puts back.
type RestoreInput struct {
	ChangeSet  string `json:"changeSet"`
	AssumeIdle bool   `json:"assumeIdle,omitempty"`
}

// Restore puts a change set's pre-image back — the files, the firewall rule,
// the service's dependencies — and restarts the service on them if it was
// running, proving the port with the restored credential. A config.yaml that
// no longer parses is no obstacle (it is what a restore is for), but then the
// order queue cannot be asked: AssumeIdle is required.
//
// SQL Server is left as it is. A least-privilege login Apply created stays
// (a credential only moves forward): when the config before and after the
// restore both log in as it, its current password is kept, since SQL Server
// knows no other.
func Restore(ctx context.Context, env Env, in RestoreInput) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if !changeSetRe.MatchString(in.ChangeSet) {
		return res, refuse(CodeChangeSetID, "use the change set's id", "changeSet is not a valid id")
	}
	dir := env.Sys.DataDir()
	p, err := loadPreimage(preimageDir(dir, in.ChangeSet))
	if errors.Is(err, os.ErrNotExist) {
		return res, refuse(CodePreimageMissing, "discover lists the pre-images on this box (a run that changed nothing keeps none)",
			"there is no pre-image for %s", in.ChangeSet)
	}
	if err != nil {
		return res, refuse(CodePreimageMissing, "restore by hand from the folder", "%v", err)
	}
	unlock, err := lockRuns(dir)
	if err != nil {
		return res, err
	}
	defer unlock()

	a := &applier{env: env, s: Spec{ChangeSet: in.ChangeSet, AssumeIdle: in.AssumeIdle}, dir: dir, res: res}
	if err := a.readConfig(); err != nil {
		if !in.AssumeIdle {
			return res, refuse(CodePendingUnknown, "run restore with --assume-idle once nothing sends orders",
				"config.yaml does not read, so the running connector's order queue cannot be asked: %v", err)
		}
		a.cur = config.Default()
		a.warn("config.yaml did not read (%v); --assume-idle was set", err)
	}
	if a.svc, err = env.Sys.Service(ServiceName); err != nil {
		return res, refuse(CodeServiceFailed, "run it elevated (as an administrator)", "read the %s service: %v", ServiceName, err)
	}
	if !a.svc.Installed {
		return res, refuse(CodeServiceMissing, "install the connector first", "the %s service is not installed", ServiceName)
	}
	a.running = a.svc.State != "stopped"
	if err := a.idle(ctx, "before the restore"); err != nil {
		return res, err
	}
	// The login's current password, if what runs now logs in as it.
	var loginPassword string
	if strings.EqualFold(a.cur.DB.User, LeastPrivilegeLogin) {
		if b, err := env.Sys.SecretGet(config.DBPasswordKey(a.cur.ERP)); err == nil {
			loginPassword = string(b)
			res.secrets = append(res.secrets, loginPassword)
		}
	}

	if err := env.Sys.ProtectDataDir(); err != nil {
		return res, refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	changed, err := putBack(env, p)
	if err == nil && loginPassword != "" {
		err = keepLoginPassword(env, loginPassword)
	}
	a.step("restore", changed, "pre-image of "+p.m.Created.Format("2006-01-02 15:04:05Z"))
	if err != nil {
		return res, refuse(CodeWriteFailed, "restore the rest by hand from provision\\"+in.ChangeSet, "%v", err)
	}
	if err := env.Sys.ProtectDataDir(); err != nil {
		a.warn("the data directory could not be protected after the restore: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, configFile))
	if errors.Is(err, os.ErrNotExist) {
		// Before the change set the connector had no config: it cannot run.
		if err := env.Sys.StopService(ServiceName); err != nil {
			a.warn("stop the service: %v", err)
		}
		a.warn("the pre-image has no config.yaml: the connector is unconfigured again and its service is stopped")
		return res, nil
	}
	restored, perr := config.Parse(b)
	if err != nil || perr != nil {
		return res, refuse(CodeConfigRefused, "fix config.yaml by hand", "the restored config.yaml does not read back")
	}
	if !a.running {
		a.step("restart", false, "the service was stopped before the restore, and stays stopped")
		return res, nil
	}
	if !changed {
		a.step("restart", false, "nothing changed")
		return res, nil
	}
	_ = env.Sys.StopService(ServiceName)
	if err := env.Sys.StartService(ServiceName); err != nil {
		return res, refuse(CodeServiceFailed, "read server.log for why it stopped", "the service did not start on the restored config: %v", err)
	}
	res.Restarted = true
	a.step("restart", true, "")
	proof, err := a.prove(ctx, restored)
	if err != nil {
		return res, refuse(CodePortProofFailed, "read server.log", "%v", err)
	}
	res.Proof = proof
	return res, nil
}
