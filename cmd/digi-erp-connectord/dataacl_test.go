package main

import (
	"errors"
	"strings"
	"testing"
)

// The data directory's protected DACL is a startup precondition: a daemon that
// could not set it does not serve (the receipt index and config.yaml would stay
// readable by every local user). config.yaml is protected too, but a failure
// there is logged and the daemon starts: the file inherits the directory's
// DACL anyway, and it may be one an operator owns and locked down by hand.
func TestProtectDataDir(t *testing.T) {
	stub := func(t *testing.T, dirErr, fileErr error) (*[]string, *[]string) {
		var dirs, files []string
		oldDir, oldFile := protectDirFn, protectFileFn
		protectDirFn = func(p string) error { dirs = append(dirs, p); return dirErr }
		protectFileFn = func(p string) error { files = append(files, p); return fileErr }
		t.Cleanup(func() { protectDirFn, protectFileFn = oldDir, oldFile })
		return &dirs, &files
	}

	t.Run("both set", func(t *testing.T) {
		dirs, files := stub(t, nil, nil)
		log := &quietLogger{}
		if err := protectDataDir(`C:\ProgramData\digi-erp-connector`, `C:\ProgramData\digi-erp-connector\config.yaml`, log); err != nil {
			t.Fatalf("protectDataDir = %v", err)
		}
		if len(*dirs) != 1 || (*dirs)[0] != `C:\ProgramData\digi-erp-connector` {
			t.Errorf("ProtectDir called with %v", *dirs)
		}
		if len(*files) != 1 || (*files)[0] != `C:\ProgramData\digi-erp-connector\config.yaml` {
			t.Errorf("ProtectFile called with %v", *files)
		}
		if len(log.errors) != 0 {
			t.Errorf("logged %v", log.errors)
		}
	})

	t.Run("the directory fails: fail closed", func(t *testing.T) {
		_, files := stub(t, errors.New("access denied"), nil)
		log := &quietLogger{}
		err := protectDataDir("/data", "/data/config.yaml", log)
		if err == nil || !strings.Contains(err.Error(), "access denied") {
			t.Fatalf("protectDataDir = %v; want the directory's error", err)
		}
		if len(log.errors) != 1 || !strings.Contains(log.errors[0], "data directory") {
			t.Errorf("logged %v; want one error naming the data directory", log.errors)
		}
		if len(*files) != 0 {
			t.Errorf("config.yaml was touched after the directory failed: %v", *files)
		}
	})

	t.Run("config.yaml fails: logged, start continues", func(t *testing.T) {
		stub(t, nil, errors.New("not the owner"))
		log := &quietLogger{}
		if err := protectDataDir("/data", "/data/config.yaml", log); err != nil {
			t.Fatalf("protectDataDir = %v; want nil", err)
		}
		if len(log.errors) != 1 || !strings.Contains(log.errors[0], "config.yaml") {
			t.Errorf("logged %v; want one error naming config.yaml", log.errors)
		}
	})
}
