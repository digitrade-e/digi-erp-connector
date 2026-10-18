package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

func stubProtect(t *testing.T, rep dataacl.Report, treeErr, fileErr error) (*[]string, *[]string) {
	t.Helper()
	var trees, files []string
	oldTree, oldFile := protectTreeFn, protectFileFn
	protectTreeFn = func(p string) (dataacl.Report, error) { trees = append(trees, p); return rep, treeErr }
	protectFileFn = func(p string) error { files = append(files, p); return fileErr }
	t.Cleanup(func() { protectTreeFn, protectFileFn = oldTree, oldFile })
	return &trees, &files
}

// The data directory's protected DACL is a startup precondition, set before
// the config is even read (a first install's service start has no config yet,
// and secrets\ may already be there): a daemon that could not set it does not
// serve. A failed owner change is logged, not fatal.
func TestProtectDataDir(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		trees, _ := stubProtect(t, dataacl.Report{Entries: 3}, nil, nil)
		log := &quietLogger{}
		if err := protectDataDir(`C:\ProgramData\digi-erp-connector`, log); err != nil {
			t.Fatalf("protectDataDir = %v", err)
		}
		if len(*trees) != 1 || (*trees)[0] != `C:\ProgramData\digi-erp-connector` {
			t.Errorf("ProtectTree called with %v", *trees)
		}
		if len(log.errors) != 0 {
			t.Errorf("logged %v", log.errors)
		}
	})
	t.Run("the tree fails: fail closed", func(t *testing.T) {
		stubProtect(t, dataacl.Report{}, dataacl.ErrReparsePoint, nil)
		log := &quietLogger{}
		err := protectDataDir("/data", log)
		if !errors.Is(err, dataacl.ErrReparsePoint) {
			t.Fatalf("protectDataDir = %v; want the tree's error", err)
		}
		if len(log.errors) != 1 || !strings.Contains(log.errors[0], "data directory") {
			t.Errorf("logged %v; want one error naming the data directory", log.errors)
		}
	})
	t.Run("the owner change fails: logged, start continues", func(t *testing.T) {
		stubProtect(t, dataacl.Report{OwnerErr: errors.New("no WRITE_OWNER")}, nil, nil)
		log := &quietLogger{}
		if err := protectDataDir("/data", log); err != nil {
			t.Fatalf("protectDataDir = %v; want nil", err)
		}
		if len(log.errors) != 1 || !strings.Contains(log.errors[0], "owner") {
			t.Errorf("logged %v; want the owner error", log.errors)
		}
	})
}

// config.yaml is protected after it loads; a failure there is logged only.
func TestProtectConfigFile(t *testing.T) {
	_, files := stubProtect(t, dataacl.Report{}, nil, errors.New("not the owner"))
	log := &quietLogger{}
	protectConfigFile("/data/config.yaml", log)
	if len(*files) != 1 || (*files)[0] != "/data/config.yaml" {
		t.Errorf("ProtectFile called with %v", *files)
	}
	if len(log.errors) != 1 || !strings.Contains(log.errors[0], "config.yaml") {
		t.Errorf("logged %v; want one error naming config.yaml", log.errors)
	}
}
