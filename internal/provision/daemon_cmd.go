package provision

import (
	"context"

	"github.com/digitrade-e/digi-erp-connector/internal/api"
)

// DaemonInput is the daemon command's command line (plan 086 C2):
// daemon <restart|stop|start> [--assume-idle]. It takes no stdin.
type DaemonInput struct {
	Action string // restart | stop | start
	// AssumeIdle is apply's: go on when the running connector's order queue
	// cannot be read (a daemon on another credential than config.yaml's).
	AssumeIdle bool
}

// Daemon actions.
const (
	DaemonRestart = "restart"
	DaemonStop    = "stop"
	DaemonStart   = "start"
)

// ControlDaemon restarts, stops or starts the connector's service — what a
// person does in services.msc after a hand edit or before a reboot — with
// provisioning's guarantees: a stop or a restart of a running service asks
// the order queue first (the queue is in memory: a restart drops it) unless
// AssumeIdle; a start or a restart first checks config.yaml with the daemon's
// own startup check, so a running service is never stopped onto a config it
// will refuse; and both end with the proof — /auth/token with config.yaml's
// credential, /api/ping, /api/health. Nothing is written, so there is no
// pre-image and nothing to roll back: a failed proof is reported, and the
// service is left as the action left it.
func ControlDaemon(ctx context.Context, env Env, in DaemonInput) (*ApplyResult, error) {
	res := &ApplyResult{Steps: []Step{}}
	switch in.Action {
	case DaemonRestart, DaemonStop, DaemonStart:
	default:
		return res, refuse(CodeSpecInvalid, "use restart, stop or start", "daemon %q is not an action", in.Action)
	}
	dir := env.Sys.DataDir()
	unlock, err := lockRuns(dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	a := &applier{env: env, s: Spec{AssumeIdle: in.AssumeIdle}, dir: dir, res: res}
	cfgErr := a.readConfig()
	if a.svc, err = env.Sys.Service(ServiceName); err != nil {
		return res, refuse(CodeServiceFailed, "run it elevated (as an administrator)", "read the %s service: %v", ServiceName, err)
	}
	if !a.svc.Installed {
		return res, refuse(CodeServiceMissing, "install the connector first (its installer creates the service)", "the %s service is not installed", ServiceName)
	}
	a.running = a.svc.State != "stopped"

	if in.Action == DaemonStop {
		return res, a.stopDaemon(ctx, cfgErr)
	}
	// start and restart: the daemon must be able to start on config.yaml.
	switch {
	case cfgErr != nil:
		return res, cfgErr
	case a.curBytes == nil:
		return res, refuse(CodeConfigRefused, "provision the connector with apply first", "there is no config.yaml: the connector would not start")
	}
	if err := api.CheckConfig(a.cur); err != nil {
		return res, refuse(CodeConfigRefused, "fix config.yaml (restore a change set, or apply), then run it again",
			"the connector would refuse to start on config.yaml: %v", err)
	}
	if !a.running {
		return res, a.startDaemon(ctx, "the service was stopped")
	}
	if in.Action == DaemonStart {
		a.step("start", false, "the service is already running")
		return res, a.proveDaemon(ctx)
	}
	if err := a.idle(ctx, "before the restart"); err != nil {
		return res, err
	}
	if err := env.Sys.StopService(ServiceName); err != nil {
		return res, refuse(CodeServiceFailed, "stop it in services.msc and run it again", "stop the service: %v", err)
	}
	a.step("stop", true, "")
	return res, a.startDaemon(ctx, "")
}

// stopDaemon stops a running service once the order queue is empty. A config
// that does not read cannot be asked: only AssumeIdle gets past it.
func (a *applier) stopDaemon(ctx context.Context, cfgErr error) error {
	if !a.running {
		a.step("stop", false, "the service is already stopped")
		return nil
	}
	switch {
	case cfgErr != nil && !a.s.AssumeIdle:
		return refuse(CodePendingUnknown, "run it with --assume-idle once nothing sends orders",
			"config.yaml does not read, so the running connector's order queue cannot be asked: %v", cfgErr)
	case cfgErr != nil:
		a.warn("config.yaml did not read (%v); --assume-idle was set", cfgErr)
	default:
		if err := a.idle(ctx, "before the stop"); err != nil {
			return err
		}
	}
	if err := a.env.Sys.StopService(ServiceName); err != nil {
		return refuse(CodeServiceFailed, "stop it in services.msc", "stop the service: %v", err)
	}
	a.step("stop", true, "")
	return nil
}

func (a *applier) startDaemon(ctx context.Context, detail string) error {
	if err := a.env.Sys.StartService(ServiceName); err != nil {
		return refuse(CodeServiceFailed, "read server.log (the logs command) for why it stopped", "the service did not start: %v", err)
	}
	a.res.Restarted = true
	a.step("start", true, detail)
	return a.proveDaemon(ctx)
}

// proveDaemon proves the service with config.yaml's credential. There is
// nothing to roll back: the refusal says the proof failed.
func (a *applier) proveDaemon(ctx context.Context) error {
	proof, err := a.prove(ctx, a.cur)
	if err != nil {
		return refuse(CodePortProofFailed, "read server.log (the logs command)", "%v", err)
	}
	a.res.Proof = proof
	if proof.Health != "ok" && proof.Health != HealthNoDatabase {
		a.warn("the connector answers but its database does not (/api/health: %s)", proof.Health)
	}
	return nil
}
