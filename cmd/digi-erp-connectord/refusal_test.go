package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

// With the service restarted every 30 min, a refusal that does not clear
// would write an Application event every 30 min. One is written when the
// reason is new, or once a day for the same reason.
func TestRefusalDue(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	due, st := refusalDue("", "dataacl: a symlink … history", now)
	if !due || st == "" {
		t.Fatalf("first refusal: due=%v state=%q; want reported", due, st)
	}
	if due, st2 := refusalDue(st, "dataacl: a symlink … history", now.Add(30*time.Minute)); due || st2 != st {
		t.Errorf("same reason 30 min later: due=%v state=%q; want not reported, state kept", due, st2)
	}
	if due, _ := refusalDue(st, "dataacl: a hard link … linked.json", now.Add(30*time.Minute)); !due {
		t.Error("a new reason was not reported")
	}
	if due, _ := refusalDue(st, "dataacl: a symlink … history", now.Add(24*time.Hour)); !due {
		t.Error("the same reason a day later was not reported again")
	}
	if due, _ := refusalDue("garbage", "x", now); !due {
		t.Error("an unreadable state blocked the report")
	}
}

// The default fallback seam (setLogPathFn = logger.SetFilePath) really
// redirects the bootstrap logger.
func TestSetLogPathFnRedirects(t *testing.T) {
	alt := filepath.Join(t.TempDir(), "server-x.log")
	setLogPathFn(alt)
	t.Cleanup(func() { logger.SetFilePath("") })
	l := logger.NewBootstrap()
	l.Info("redirected")
	_ = l.Close()
	if b, err := os.ReadFile(alt); err != nil || !strings.Contains(string(b), "redirected") {
		t.Errorf("fallback log %q, %v; want the line there", b, err)
	}
}
