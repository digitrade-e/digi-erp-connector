package hasavshevet

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// pickupSender is a model (a) sender: own folder for the counter and history,
// a separate pickup folder holding the scheduler's static imovein.prm, and
// importer seams that fail the test if anything runs. hasExePath is set (it
// is allowed: receipts may use it) to prove it is not run for orders.
func pickupSender(t *testing.T, format string) (s *Sender, own, pickup string) {
	t.Helper()
	own, pickup = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(pickup, "imovein.prm"), []byte("the scheduler's own PRM\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		ERP: config.ERPHasavshevet, SendOrderDir: own, SendOrderPickupDir: pickup, SendOrderFormat: format,
		HasExePath: filepath.Join(own, "has.exe"),
	}
	if err := cfg.ValidateOrders(); err != nil {
		t.Fatalf("the test config is refused: %v", err)
	}
	s = NewSender(nil, cfg, NewOrderNumberStore(filepath.Join(own, "lastOrderNumber.json")), noopLogger{})
	s.runBat = func(context.Context, string) (int, string, error) {
		t.Error("model (a) ran the BAT importer")
		return 0, "", nil
	}
	s.runHasExe = func(context.Context, string, string, string) (int, string, error) {
		t.Error("model (a) ran has.exe")
		return 0, "", nil
	}
	return s, own, pickup
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// Model (a) with the legacy layout: the pickup folder gains exactly the file
// the legacy app would have written for the order — its name, its bytes — and
// nothing else; the scheduler's PRM is untouched; no importer runs; the
// connector's own folder holds the history copies and no IMOVEIN.doc/.prm.
func TestPickup_PublishesTheLegacyFileAndNothingElse(t *testing.T) {
	names := legacyNames(t)
	prmGolden := readFile(t, legacyGoldenPath("IMOVEIN.prm"))
	for _, tc := range legacyRequestCases() {
		t.Run(tc.name, func(t *testing.T) {
			s, own, pickup := pickupSender(t, config.OrderFormatLegacy)
			res, err := s.processOrderWithNumber(context.Background(), tc.req, tc.number)
			if err != nil {
				t.Fatalf("processOrderWithNumber: %v", err)
			}
			want := names[tc.name]
			if got := dirNames(t, pickup); strings.Join(got, ",") != strings.Join([]string{want, "imovein.prm"}, ",") &&
				strings.Join(got, ",") != strings.Join([]string{"imovein.prm", want}, ",") {
				t.Fatalf("pickup folder = %v; want only the scheduler's imovein.prm and %s", got, want)
			}
			if got := readFile(t, filepath.Join(pickup, want)); !bytes.Equal(got, readFile(t, legacyGoldenPath(tc.name+".doc"))) {
				t.Errorf("%s is not the legacy writer's bytes", want)
			}
			if got := string(readFile(t, filepath.Join(pickup, "imovein.prm"))); got != "the scheduler's own PRM\n" {
				t.Errorf("the scheduler's imovein.prm was changed: %q", got)
			}
			for _, fixed := range []string{"IMOVEIN.doc", "IMOVEIN.prm"} {
				if _, err := os.Stat(filepath.Join(own, fixed)); !os.IsNotExist(err) {
					t.Errorf("model (a) wrote %s in sendOrderDir (err %v)", fixed, err)
				}
			}
			n := strconv.FormatInt(tc.number, 10)
			hist := filepath.Join(own, "history", n)
			if got := readFile(t, filepath.Join(hist, "IMOVEIN_"+n+".doc")); !bytes.Equal(got, readFile(t, legacyGoldenPath(tc.name+".doc"))) {
				t.Error("the history copy is not the published file")
			}
			if got := readFile(t, filepath.Join(hist, "IMOVEIN_"+n+".prm")); !bytes.Equal(got, prmGolden) {
				t.Error("the history PRM is not the legacy layout's")
			}
			if len(res.WrittenFiles) == 0 || res.WrittenFiles[0] != filepath.Join(pickup, want) {
				t.Errorf("WrittenFiles = %v; want the published file first", res.WrittenFiles)
			}
			if res.OrderNumber != tc.number {
				t.Errorf("OrderNumber = %d; want %d", res.OrderNumber, tc.number)
			}
		})
	}
}

// The model and the layout are separate settings: model (a) with the official
// layout publishes today's official bytes under the pickup name.
func TestPickup_OfficialLayout(t *testing.T) {
	tc := imoveinGoldenCases()[0]
	s, _, pickup := pickupSender(t, config.OrderFormatOfficial)
	if _, err := s.processOrderWithNumber(context.Background(), tc.req, tc.number); err != nil {
		t.Fatal(err)
	}
	name := "IMOVEIN_" + strconv.FormatInt(tc.number, 10) + ".doc"
	if got := readFile(t, filepath.Join(pickup, name)); !bytes.Equal(got, readFile(t, goldenPath(tc.name+".doc"))) {
		t.Error("model (a) with the official layout changed the official bytes")
	}
}

// A number another writer already used in the pickup folder — waiting,
// its .prm, or already imported and renamed by Masofon — fails the order for
// good (not ErrOrderNotWritten: a retry under the same number would write a
// second document under one reference), and nothing at all is written. A
// longer number that merely starts with the same digits is not a clash.
func TestPickup_ANumberAlreadyUsedFailsAndWritesNothing(t *testing.T) {
	tc := legacyRequestCases()[0] // order 100001
	for _, existing := range []string{
		"IMOVEIN_100001.doc",
		"imovein_100001.doc",
		"IMOVEIN_100001.prm",
		"TMP_IMOVEIN_100001.doc",
		"IMOVEIN_100001.doc.20261108-0912.MASOFON2.BAK",
	} {
		t.Run(existing, func(t *testing.T) {
			s, own, pickup := pickupSender(t, config.OrderFormatLegacy)
			if err := os.WriteFile(filepath.Join(pickup, existing), []byte("legacy"), 0o644); err != nil {
				t.Fatal(err)
			}
			before := dirNames(t, pickup)
			_, err := s.processOrderWithNumber(context.Background(), tc.req, tc.number)
			if err == nil || !strings.Contains(err.Error(), "already used") {
				t.Fatalf("processOrderWithNumber = %v; want refused: the number is already used", err)
			}
			if errors.Is(err, ErrOrderNotWritten) {
				t.Fatal("a used number is marked retryable; it must be final")
			}
			if got := dirNames(t, pickup); strings.Join(got, ",") != strings.Join(before, ",") {
				t.Errorf("pickup folder changed: %v → %v", before, got)
			}
			if got := string(readFile(t, filepath.Join(pickup, existing))); got != "legacy" {
				t.Errorf("%s was overwritten", existing)
			}
			if _, err := os.Stat(filepath.Join(own, "history")); !os.IsNotExist(err) {
				t.Errorf("history was written for a refused order (err %v)", err)
			}
		})
	}
	t.Run("a longer number is not a clash", func(t *testing.T) {
		s, _, pickup := pickupSender(t, config.OrderFormatLegacy)
		for _, other := range []string{"IMOVEIN_1000011.doc", "TMP_IMOVEIN_1000010.doc.x.MASOFON2.BAK", "XIMOVEIN_100001.doc"} {
			if err := os.WriteFile(filepath.Join(pickup, other), []byte("other"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.processOrderWithNumber(context.Background(), tc.req, tc.number); err != nil {
			t.Fatalf("processOrderWithNumber = %v; want written", err)
		}
	})
}

// A pickup folder that cannot be read (missing: a typo, a share not mounted)
// is an error before anything is written: retryable under the same number.
func TestPickup_UnreadableFolderIsRetryable(t *testing.T) {
	s, own, pickup := pickupSender(t, config.OrderFormatLegacy)
	if err := os.RemoveAll(pickup); err != nil {
		t.Fatal(err)
	}
	_, err := s.processOrderWithNumber(context.Background(), legacyRequestCases()[0].req, 5)
	if err == nil || !errors.Is(err, ErrOrderNotWritten) {
		t.Fatalf("processOrderWithNumber = %v; want an ErrOrderNotWritten", err)
	}
	if _, statErr := os.Stat(filepath.Join(own, "history")); !os.IsNotExist(statErr) {
		t.Errorf("history was written (err %v)", statErr)
	}
	if _, statErr := os.Stat(pickup); !os.IsNotExist(statErr) {
		t.Error("the pickup folder was created; it is the scheduler's, a missing one must stay an error")
	}
}

// A pickup folder that takes no file (read-only, a write the ACL denies) is
// found before the history copies are written, so the order stays retryable
// under its number instead of failing for good with only history on disk.
func TestPickup_FolderThatTakesNoFileIsRetryable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod cannot make a folder unwritable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only folder")
	}
	s, own, pickup := pickupSender(t, config.OrderFormatLegacy)
	if err := os.Chmod(pickup, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(pickup, 0o755) })
	_, err := s.processOrderWithNumber(context.Background(), legacyRequestCases()[0].req, 5)
	if err == nil || !errors.Is(err, ErrOrderNotWritten) {
		t.Fatalf("processOrderWithNumber = %v; want an ErrOrderNotWritten", err)
	}
	if _, statErr := os.Stat(filepath.Join(own, "history")); !os.IsNotExist(statErr) {
		t.Errorf("history was written before the folder was found unwritable (err %v)", statErr)
	}
}

func TestPickupFileName(t *testing.T) {
	for _, tc := range []struct {
		docType string
		want    string
	}{
		{"ORDER", "IMOVEIN_42.doc"},
		{"QUOATE", "IMOVEIN_42.doc"},
		{"RETURN", "TMP_IMOVEIN_42.doc"},
	} {
		if got := PickupFileName(tc.docType, 42); got != tc.want {
			t.Errorf("PickupFileName(%s) = %q; want %q", tc.docType, got, tc.want)
		}
	}
}

// Through the queue and the key index: the order is written once; a repeat
// of its historyId answers the first job and leaves exactly one file.
func TestPickup_ThroughTheQueue_OneFilePerKey(t *testing.T) {
	s, own, pickup := pickupSender(t, config.OrderFormatLegacy)
	q := NewOrderQueue(s, noopLogger{})
	q.UseKeyIndex(filepath.Join(own, "data"))
	if err := q.Recover(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); <-q.Done() }()
	q.Start(ctx)

	req := legacyRequestCases()[0].req
	first, err := q.Accept(req)
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, q, first.JobID)
	again, err := q.Accept(req)
	if err != nil {
		t.Fatal(err)
	}
	if again.JobID != first.JobID || !again.Replay {
		t.Fatalf("repeat = %+v; want a replay of job %s", again, first.JobID)
	}
	var orders []string
	for _, n := range dirNames(t, pickup) {
		if strings.HasPrefix(n, "IMOVEIN_") {
			orders = append(orders, n)
		}
	}
	if len(orders) != 1 || orders[0] != "IMOVEIN_"+first.JobID+".doc" {
		t.Fatalf("pickup orders = %v; want one IMOVEIN_%s.doc", orders, first.JobID)
	}
}

func waitJob(t *testing.T, q *OrderQueue, jobID string) {
	t.Helper()
	for i := 0; i < 2000; i++ {
		if r, ok := q.Status(jobID); ok && (r.Status == JobStatusDone || r.Status == JobStatusFailed) {
			if r.Status != JobStatusDone {
				t.Fatalf("job %s failed: %v", jobID, r.Err)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", jobID)
}
