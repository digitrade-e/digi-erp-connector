//go:build windows

package autostart

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/sys/windows/svc"
)

type progressApp struct{ progress func() }

func (a *progressApp) SetStartProgress(f func()) { a.progress = f }
func (a *progressApp) Start() error {
	a.progress()
	a.progress()
	return errors.New("stop here")
}
func (*progressApp) Stop(context.Context) {}
func (*progressApp) Errors() <-chan error { return nil }
func (*progressApp) Logger() Logger       { return nil }

// While Start runs, the service reports START_PENDING with a wait hint and a
// new checkpoint at each progress call (the data-directory walk).
func TestExecute_ReportsStartCheckpoints(t *testing.T) {
	status := make(chan svc.Status, 10)
	h := &serviceHandler{app: &progressApp{}}
	if _, code := h.Execute(nil, make(chan svc.ChangeRequest), status); code != 1 {
		t.Fatalf("exit code %d; want 1 (Start failed)", code)
	}
	close(status)
	var got []svc.Status
	for s := range status {
		got = append(got, s)
	}
	if len(got) != 4 {
		t.Fatalf("statuses %+v; want StartPending ×3 then Stopped", got)
	}
	for i, s := range got[:3] {
		if s.State != svc.StartPending || s.WaitHint == 0 || s.CheckPoint != uint32(i) {
			t.Errorf("status %d = %+v; want StartPending, a wait hint, checkpoint %d", i, s, i)
		}
	}
	if got[3].State != svc.Stopped {
		t.Errorf("last status %+v; want Stopped", got[3])
	}
}

type codedErr struct{}

func (codedErr) Error() string    { return "refused" }
func (codedErr) ExitCode() uint32 { return ExitDataDirRefused }

type refusingApp struct{ progressApp }

func (*refusingApp) Start() error { return codedErr{} }

// A Start error carrying an exit code is reported as the service-specific
// exit code, which `sc query` and the GUI show.
func TestExecute_ReportsAServiceSpecificExitCode(t *testing.T) {
	status := make(chan svc.Status, 10)
	h := &serviceHandler{app: &refusingApp{}}
	specific, code := h.Execute(nil, make(chan svc.ChangeRequest), status)
	if !specific || code != ExitDataDirRefused {
		t.Errorf("Execute = %v, %d; want a service-specific %d", specific, code, ExitDataDirRefused)
	}
}
