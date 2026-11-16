package provision

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// preimageConfig is the config.yaml a pre-image holds, parsed; ok is false
// when it holds none (it removes config.yaml, or has no file at all).
func preimageConfig(p preimage) (cfg config.Config, ok bool, err error) {
	for _, f := range p.m.Files {
		if f.Path != configFile || !f.Present {
			continue
		}
		b, err := os.ReadFile(filepath.Join(p.dir, "files", configFile))
		if err != nil {
			return cfg, false, fmt.Errorf("read the pre-image of %s: %w", configFile, err)
		}
		if cfg, err = config.Parse(b); err != nil {
			return cfg, false, fmt.Errorf("the pre-image of %s does not parse: %w", configFile, err)
		}
		return cfg, true, nil
	}
	return cfg, false, nil
}

// RestoreInput names the change set whose pre-image Restore puts back.
type RestoreInput struct {
	ChangeSet  string `json:"changeSet"`
	AssumeIdle bool   `json:"assumeIdle,omitempty"`
	// AcceptUnreadable (review R7; --accept-unreadable=N) is the number of
	// unreadable index entries a human checked: the counters and documents
	// guards refuse them otherwise.
	AcceptUnreadable *int `json:"acceptUnreadable,omitempty"`
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
	if r := validAcceptUnreadable(in.AcceptUnreadable); r != nil {
		return res, r
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

	a := &applier{env: env, s: Spec{ChangeSet: in.ChangeSet, AssumeIdle: in.AssumeIdle}, dir: dir, res: res, acceptUnreadable: in.AcceptUnreadable}
	// A pre-image of no file (the service or firewall-remove commands) changes
	// nothing the service reads: no restart, so no queue to ask.
	files := len(p.m.Files) > 0
	if err := a.readConfig(); err != nil && files {
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
	if files {
		if err := a.idle(ctx, "before the restore"); err != nil {
			return res, err
		}
	}
	// Plan 087 review m9: a restore that changes the documents block runs the
	// counters and documents commands' guards first, so it can neither strand
	// unfinished work nor leave a counter that refuses every document.
	if restored, ok, err := preimageConfig(p); err != nil {
		return res, refuse(CodePreimageMissing, "restore by hand from the folder", "%v", err)
	} else if ok {
		if a.curBytes == nil {
			a.warn("config.yaml did not read: the counters and documents guards were not run (--assume-idle)")
		} else {
			// Review m3: both guards note what they cannot read; the one total is
			// settled after them.
			a.unreadableDeferred = true
			if err := a.documentsIdle(a.cur, restored); err != nil {
				return res, err
			}
			if err := a.countersRestorable(a.cur, restored); err != nil {
				return res, err
			}
			a.unreadableDeferred = false
			if r := a.unreadableSettled(); r != nil {
				return res, r
			}
		}
	}
	// The DB secret in use now: a login both configs use keeps it
	// (keepLoginPassword).
	var current []byte
	if b, err := env.Sys.SecretGet(config.DBPasswordKey(a.cur.ERP)); err == nil {
		current = b
		res.secrets = append(res.secrets, string(b))
	}

	if err := env.Sys.ProtectDataDir(); err != nil {
		return res, refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	changed, err := putBack(env, p)
	if err == nil && files { // no file put back: the DB secret is not this restore's
		err = keepLoginPassword(env, a.cur.DB.User, current)
	}
	a.step("restore", changed.any(), "pre-image of "+p.m.Created.Format("2006-01-02 15:04:05Z"))
	if err != nil {
		return res, refuse(CodeWriteFailed, "restore the rest by hand from provision\\"+in.ChangeSet, "%v", err)
	}
	if err := env.Sys.ProtectDataDir(); err != nil {
		a.warn("the data directory could not be protected after the restore: %v", err)
	}

	if !changed.files {
		a.step("restart", false, "nothing changed that the service reads")
		return res, nil
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
	if a.running {
		_ = env.Sys.StopService(ServiceName)
	}
	// Stopped: the order folder the restore leaves may have a stale counter.
	if err := a.carryOrderCounter(a.cur.SendOrderDir, restored.SendOrderDir); err != nil {
		return res, refuse(CodeOrderNumberConflict, "copy "+orderNumberFile+" to the restored order folder by hand, then start the service",
			"the service is left stopped: %v", err)
	}
	if !a.running {
		a.step("restart", false, "the service was stopped before the restore, and stays stopped")
		return res, nil
	}
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
	if proof.Health != "ok" && proof.Health != HealthNoDatabase {
		a.warn("the connector answers but its database does not (/api/health: %s): the restored credential may be out of date", proof.Health)
	}
	return res, nil
}
