package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

// T4-1: SetFilePath (the daemon's fallback when a foreign server.log cannot
// be moved aside) really redirects both the bootstrap logger and New, and the
// default server.log is not touched.
func TestSetFilePath_RedirectsBothLoggers(t *testing.T) {
	t.Setenv("PROGRAMDATA", t.TempDir()) // Windows: the default path, in a temp dir
	def, err := paths.LoggerFilePath()
	if err != nil {
		t.Fatal(err)
	}
	before, beforeErr := os.Stat(def)

	alt := filepath.Join(t.TempDir(), "server-20261018T000000Z.log")
	SetFilePath(alt)
	t.Cleanup(func() { SetFilePath("") })

	b := NewBootstrap()
	b.Info("from the bootstrap logger")
	_ = b.Close()
	l, err := New(config.Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.Info("from New")
	_ = l.Close()

	got, err := os.ReadFile(alt)
	if err != nil {
		t.Fatalf("the fallback log was not written: %v", err)
	}
	if !strings.Contains(string(got), "from the bootstrap logger") || !strings.Contains(string(got), "from New") {
		t.Errorf("fallback log holds %q; want both loggers' lines", got)
	}
	after, afterErr := os.Stat(def)
	switch {
	case os.IsNotExist(beforeErr) && !os.IsNotExist(afterErr):
		t.Errorf("the default server.log %s was created", def)
	case beforeErr == nil && (afterErr != nil || after.Size() != before.Size()):
		t.Errorf("the default server.log %s was written to", def)
	}
}

// Replayed lines keep their time; IsStderr tells the fallback apart.
func TestWriteAtAndIsStderr(t *testing.T) {
	if !IsStderr(NewStderr()) {
		t.Error("IsStderr(NewStderr()) = false")
	}
	alt := filepath.Join(t.TempDir(), "x.log")
	SetFilePath(alt)
	t.Cleanup(func() { SetFilePath("") })
	b := NewBootstrap()
	if IsStderr(b) {
		t.Fatal("the bootstrap file logger reported as stderr")
	}
	tw, ok := b.(TimedWriter)
	if !ok {
		t.Fatal("the file logger is not a TimedWriter")
	}
	at, _ := timeParse("2001/02/03 04:05:06")
	tw.WriteAt(at, "WARN", "early line")
	_ = b.Close()
	if got, _ := os.ReadFile(alt); !strings.Contains(string(got), "2001/02/03 04:05:06 [WARN] early line") {
		t.Errorf("log holds %q; want the line with its original time", got)
	}
}

func timeParse(s string) (time.Time, error) {
	return time.ParseInLocation("2006/01/02 15:04:05", s, time.Local)
}
