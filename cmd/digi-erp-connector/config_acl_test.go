package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

type recLogger struct {
	mu     sync.Mutex
	errors []string
}

func (*recLogger) Info(string) {}
func (l *recLogger) Error(msg string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, fmt.Sprintf("%s: %v", msg, err))
}
func (*recLogger) Warn(string)    {}
func (*recLogger) Success(string) {}
func (*recLogger) Close() error   { return nil }

// Before a save that creates config.yaml, the data directory is protected
// (R2-2: so secrets\*.bin and the atomic-write temps are created inside the
// protected tree); after every save config.yaml gets the inherited DACL. A
// failure is logged and becomes a "Saved, but…" warning — the daemon re-applies
// the DACL at its next start, failing closed on the directory.
func TestProtectBeforeAndAfterSave(t *testing.T) {
	dir := t.TempDir()

	t.Run("ok", func(t *testing.T) {
		dirs := stubGUIProtect(t, nil)
		log := &recLogger{}
		if _, err := protectDirBeforeSave(dir, log); err != nil {
			t.Errorf("protectDirBeforeSave = %v", err)
		}
		if w := protectAfterSave(dir, log); w != "" {
			t.Errorf("after-save warning %q", w)
		}
		if len(*dirs) != 2 || (*dirs)[0] != dir || (*dirs)[1] != dir {
			t.Errorf("ProtectTree %v; want the directory before and after", *dirs)
		}
	})
	// R3-2: the save fails closed. Whatever the walk refused — a link, an
	// untrusted owner, an access error — nothing may be written into it.
	for name, walkErr := range map[string]error{
		"a reparse point": dataacl.ErrReparsePoint, "an untrusted owner": dataacl.ErrUntrustedOwner,
		"a hard link": dataacl.ErrHardLink, "any other error": errors.New("access denied"),
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			stubGUIProtect(t, walkErr)
			log := &recLogger{}
			_, err := protectDirBeforeSave(dir, log)
			if !errors.Is(err, walkErr) || !strings.Contains(err.Error(), "Not saved") || !strings.Contains(err.Error(), "nothing was written") {
				t.Errorf("protectDirBeforeSave = %v; want a refusal wrapping %v that says nothing was written", err, walkErr)
			}
			if len(log.errors) != 1 {
				t.Errorf("logged %v", log.errors)
			}
		})
	}
	t.Run("after the save fails", func(t *testing.T) {
		stubGUIProtect(t, errors.New("not the owner"))
		log := &recLogger{}
		if w := protectAfterSave(dir, log); !strings.HasPrefix(w, "Saved, but") || !strings.Contains(w, "not the owner") {
			t.Errorf("warning %q; want a Saved-but warning with the reason", w)
		}
	})
}

// stubGUIProtect replaces the GUI's walk and records its paths; it also checks
// the GUI always hands what it created to Administrators.
func stubGUIProtect(t *testing.T, walkErr error) *[]string {
	t.Helper()
	var dirs []string
	old := guiProtectTreeFn
	guiProtectTreeFn = func(p string, opt dataacl.TreeOptions) (dataacl.Report, error) {
		if !opt.ClaimForAdministrators {
			t.Error("the GUI's walk does not hand what it created to Administrators")
		}
		dirs = append(dirs, p)
		return dataacl.Report{}, walkErr
	}
	t.Cleanup(func() { guiProtectTreeFn = old })
	return &dirs
}
