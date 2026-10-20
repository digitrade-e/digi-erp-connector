package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// RestoreInput names the change set whose pre-image Restore puts back.
type RestoreInput struct {
	ChangeSet  string `json:"changeSet"`
	AssumeIdle bool   `json:"assumeIdle,omitempty"`
}

// Restore puts a change set's pre-image back — the files, the firewall rule,
// the service's dependencies — restarts the service on them and proves the
// port with the restored credential. SQL Server is left as it is: a
// least-privilege login Apply created stays (a credential only moves
// forward), and the restored config simply stops using it.
func Restore(ctx context.Context, env Env, in RestoreInput) (*ApplyResult, error) {
	res := &ApplyResult{ChangeSet: in.ChangeSet, Steps: []Step{}}
	if !changeSetRe.MatchString(in.ChangeSet) {
		return res, refuse(CodeChangeSetID, "use the change set's id", "changeSet is not a valid id")
	}
	dir := env.Sys.DataDir()
	m, err := loadPreimage(dir, in.ChangeSet)
	if errors.Is(err, os.ErrNotExist) {
		return res, refuse(CodePreimageMissing, "discover lists the pre-images on this box", "there is no pre-image for %s", in.ChangeSet)
	}
	if err != nil {
		return res, refuse(CodePreimageMissing, "restore by hand from the folder", "%v", err)
	}
	a := &applier{env: env, s: Spec{ChangeSet: in.ChangeSet, AssumeIdle: in.AssumeIdle}, dir: dir, res: res}
	if err := a.readCurrent(); err != nil {
		return res, err
	}
	if err := a.idle(ctx, "before the restore"); err != nil {
		return res, err
	}
	if err := env.Sys.ProtectDataDir(); err != nil {
		return res, refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	changed, err := putBack(env, m)
	a.step("restore", changed, "pre-image of "+m.Created.Format("2006-01-02 15:04:05Z"))
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
	if !changed && a.svc.State == "running" {
		a.step("restart", false, "nothing changed")
		return res, nil
	}
	_ = env.Sys.StopService(ServiceName)
	if err := env.Sys.StartService(ServiceName); err != nil {
		return res, refuse(CodeServiceFailed, "read server.log for why it stopped", "the service did not start on the restored config: %v", err)
	}
	res.Restarted = true
	proof, err := a.prove(ctx, restored)
	if err != nil {
		return res, refuse(CodePortProofFailed, "read server.log", "%v", err)
	}
	res.Proof = proof
	return res, nil
}
