package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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

// The elevated GUI gives what it saves the daemon's protected DACL: always
// config.yaml, and the data directory too when this save created it. A
// failure is logged and surfaced as a warning — config.yaml is saved by then,
// and the daemon re-applies the DACL at its next start, failing closed on the
// directory.
func TestProtectSavedConfig(t *testing.T) {
	const dir, cfg = `C:\ProgramData\digi-erp-connector`, `C:\ProgramData\digi-erp-connector\config.yaml`
	stub := func(t *testing.T, dirErr, fileErr error) (*[]string, *[]string) {
		var dirs, files []string
		oldDir, oldFile := guiProtectDirFn, guiProtectFileFn
		guiProtectDirFn = func(p string) error { dirs = append(dirs, p); return dirErr }
		guiProtectFileFn = func(p string) error { files = append(files, p); return fileErr }
		t.Cleanup(func() { guiProtectDirFn, guiProtectFileFn = oldDir, oldFile })
		return &dirs, &files
	}

	t.Run("existing directory: config.yaml only", func(t *testing.T) {
		dirs, files := stub(t, nil, nil)
		log := &recLogger{}
		if w := protectSavedConfig(dir, cfg, false, log); w != "" {
			t.Errorf("warning %q; want none", w)
		}
		if len(*dirs) != 0 || len(*files) != 1 || (*files)[0] != cfg {
			t.Errorf("ProtectDir %v, ProtectFile %v; want only config.yaml", *dirs, *files)
		}
		if len(log.errors) != 0 {
			t.Errorf("logged %v", log.errors)
		}
	})

	t.Run("directory created by this save: both", func(t *testing.T) {
		dirs, files := stub(t, nil, nil)
		if w := protectSavedConfig(dir, cfg, true, &recLogger{}); w != "" {
			t.Errorf("warning %q; want none", w)
		}
		if len(*dirs) != 1 || (*dirs)[0] != dir || len(*files) != 1 {
			t.Errorf("ProtectDir %v, ProtectFile %v; want both", *dirs, *files)
		}
	})

	t.Run("directory fails: logged, warned, config.yaml still protected", func(t *testing.T) {
		_, files := stub(t, errors.New("access denied"), nil)
		log := &recLogger{}
		w := protectSavedConfig(dir, cfg, true, log)
		if !strings.Contains(w, "Saved") || !strings.Contains(w, "data directory") {
			t.Errorf("warning %q; want a saved-but warning naming the data directory", w)
		}
		if len(log.errors) != 1 || !strings.Contains(log.errors[0], "access denied") {
			t.Errorf("logged %v; want the directory's error", log.errors)
		}
		if len(*files) != 1 {
			t.Errorf("config.yaml was not protected after the directory failed: %v", *files)
		}
	})

	t.Run("config.yaml fails: logged and warned", func(t *testing.T) {
		stub(t, nil, errors.New("not the owner"))
		log := &recLogger{}
		w := protectSavedConfig(dir, cfg, false, log)
		if !strings.Contains(w, "config.yaml") {
			t.Errorf("warning %q; want one naming config.yaml", w)
		}
		if len(log.errors) != 1 || !strings.Contains(log.errors[0], "not the owner") {
			t.Errorf("logged %v", log.errors)
		}
	})
}

func TestDataDirMissing(t *testing.T) {
	if dataDirMissing(t.TempDir()) {
		t.Error("an existing directory reported missing")
	}
	if !dataDirMissing(filepath.Join(t.TempDir(), "nope")) {
		t.Error("a missing directory reported present")
	}
}
