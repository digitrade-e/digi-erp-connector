package provision

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

// Plan 087: state lists every counter (read from its store, the index and
// history scanned per range) and every typed folder (read like the pickup
// folder); it writes nothing.
func TestState_CountersAndFolders(t *testing.T) {
	sys, env, orders := countersBox(t)
	herp := mkdir(t, "herp")
	cfg, _ := readConfig(t, sys.dir)
	cfg.Documents.Folders = map[string]config.FolderConfig{
		"herp": {Kind: "orders", Path: herp, NamePattern: "{prefix}IMOVEIN_{n}.doc", TempPrefix: "TMP_", Layout: "legacy"},
	}
	writeConfig(t, sys.dir, cfg)
	for _, name := range []string{"IMOVEIN_1000296.doc", "tmp_imovein_1000297.DOC", "IMOVEIN_1000290.doc.20261116.MASOFON2.BAK", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(herp, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(orders, "history", "1000297"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(sys.dir, "orders", "index", "a.json"), map[string]any{"key": "a", "number": 9_000_001, "state": "retryable", "reservedAt": "x"})

	_, res, _ := runState(t, stateSys{fakeSys: sys, link: autostart.ServiceInfo{}}, env, "{}")
	byKey := map[string]int{}
	for i, c := range res.Counters {
		byKey[c.Key] = i
	}
	if len(res.Counters) != 4 {
		t.Fatalf("counters %+v", res.Counters)
	}
	b2b := res.Counters[byKey["b2b"]]
	if b2b.Last != 1_000_295 || b2b.HighestIssued != 1_000_297 || b2b.Remaining != 1_999_999-1_000_297 || b2b.Store != config.CounterStoreLastOrderNumber || !b2b.StorePresent {
		t.Errorf("b2b %+v", b2b)
	}
	test := res.Counters[byKey["b2b_test"]]
	if test.HighestIssued != 9_000_001 || test.InFlight != 1 || test.StorePresent || test.Error == "" || !test.Test {
		t.Errorf("b2b_test %+v (its store does not exist: an error, not a number)", test)
	}
	if len(res.Folders) != 1 {
		t.Fatalf("folders %+v", res.Folders)
	}
	f := res.Folders[0]
	if f.Key != "herp" || f.Pending.Count != 2 || f.Imported != 1 || f.Layout != "legacy" {
		t.Errorf("herp %+v", f)
	}
	if _, err := os.Stat(filepath.Join(sys.dir, "counters")); !os.IsNotExist(err) {
		t.Error("state created a store")
	}
}

// Today's box: the implicit counters, no folder.
func TestState_ImplicitCounters(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	writeOrderCounter(t, orders, 42)
	_, res, _ := runState(t, stateSys{fakeSys: sys}, testEnv(sys, fdb), "")
	if len(res.Counters) != 1 || res.Counters[0].Key != "orders" || !res.Counters[0].Implicit || res.Counters[0].Last != 42 || res.Counters[0].Max != config.MaxDocumentNumber {
		t.Fatalf("counters %+v", res.Counters)
	}
	if res.Folders == nil || len(res.Folders) != 0 {
		t.Errorf("folders %+v", res.Folders)
	}
}
