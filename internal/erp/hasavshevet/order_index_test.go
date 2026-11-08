package hasavshevet

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newTestOrderIndex(t *testing.T) orderIndex {
	t.Helper()
	return orderIndex{dir: OrderIndexDir(t.TempDir())}
}

func TestOrderIndex_SaveLoadRoundTrip(t *testing.T) {
	ix := newTestOrderIndex(t)
	key := "4711-3f2c9a1e-7b4d-4c8e-9a51-2d6f0b8e4c17"
	e := &orderIndexEntry{
		Key: key, Number: 1000295, State: orderWritten, RequestHash: strings.Repeat("ab", 32),
		Files:      []string{`C:\digiorders\IMOVEIN.doc`, `C:\digiorders\IMOVEIN.prm`},
		ReservedAt: "2026-11-05T10:00:00Z", FinishedAt: "2026-11-05T10:00:01Z",
	}
	if err := ix.save(e); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, found, err := ix.load(key)
	if err != nil || !found {
		t.Fatalf("load = %v, %v", found, err)
	}
	if !reflect.DeepEqual(got, e) {
		t.Errorf("round trip:\n got %+v\nwant %+v", got, e)
	}
	if _, found, err := ix.load(key + "x"); found || err != nil {
		t.Errorf("an unseen key = found %v, %v; want not found", found, err)
	}
}

// The key is the caller's free text; the file name never is. It is the
// SHA-256 of the key, so no key can choose a path, collide with another's
// spelling on a case-insensitive filesystem, or be too long for a file name.
func TestOrderIndex_FileNameIsTheKeysHash(t *testing.T) {
	ix := newTestOrderIndex(t)
	for _, key := range []string{
		`..\..\Windows\win.ini`, "../../etc/passwd", "Web-1234", "web-1234", "CON", "a:b*c?<>|",
		strings.Repeat("k", 4000), "מפתח עם רווחים",
	} {
		if err := ix.save(&orderIndexEntry{Key: key, Number: 1, State: orderReserved}); err != nil {
			t.Fatalf("save %.40q: %v", key, err)
		}
		sum := sha256.Sum256([]byte(key))
		want := filepath.Join(ix.dir, hex.EncodeToString(sum[:])+".json")
		if _, err := os.Stat(want); err != nil {
			t.Errorf("key %.40q is not stored at its hash: %v", key, err)
		}
	}
	names, _ := os.ReadDir(ix.dir)
	if len(names) != 8 {
		t.Errorf("index holds %d files; want 8 — keys differing only in case are different keys", len(names))
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(ix.dir)), "etc")); err == nil {
		t.Error("a key escaped the index directory")
	}
}

// An entry that cannot be trusted refuses — never reads as "key not seen",
// which would give the key a second order number.
func TestOrderIndex_UnreadableEntries(t *testing.T) {
	const key = "4711-release"
	valid := `{"key":"4711-release","number":7,"state":"written","requestSha256":"","reservedAt":"x"}`
	for name, content := range map[string]string{
		"not json":         `{`,
		"empty file":       ``,
		"unknown field":    `{"key":"4711-release","number":7,"state":"written","reservedAt":"x","extra":1}`,
		"another key":      `{"key":"4712-release","number":7,"state":"written","reservedAt":"x"}`,
		"no key":           `{"number":7,"state":"written","reservedAt":"x"}`,
		"unknown state":    `{"key":"4711-release","number":7,"state":"done","reservedAt":"x"}`,
		"no state":         `{"key":"4711-release","number":7,"reservedAt":"x"}`,
		"zero number":      `{"key":"4711-release","number":0,"state":"written","reservedAt":"x"}`,
		"negative number":  `{"key":"4711-release","number":-7,"state":"written","reservedAt":"x"}`,
		"string number":    `{"key":"4711-release","number":"7","state":"written","reservedAt":"x"}`,
		"fraction number":  `{"key":"4711-release","number":7.5,"state":"written","reservedAt":"x"}`,
		"data after entry": valid + ` {}`,
		"array":            `[` + valid + `]`,
		"negative retries": `{"key":"4711-release","number":7,"state":"retryable","retries":-1,"reservedAt":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ix := newTestOrderIndex(t)
			if err := os.MkdirAll(ix.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(ix.pathFor(orderIndexStem(key)), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if e, found, err := ix.load(key); !errors.Is(err, ErrOrderIndexUnreadable) {
				t.Errorf("load = %+v, %v, %v; want ErrOrderIndexUnreadable", e, found, err)
			}
			// all() reports it and goes on: recovery skips a damaged entry.
			entries, problems, err := ix.all()
			if err != nil || len(entries) != 0 || len(problems) != 1 || !errors.Is(problems[0], ErrOrderIndexUnreadable) {
				t.Errorf("all = %v, %v, %v; want the entry reported as unreadable and skipped", entries, problems, err)
			} else if !strings.Contains(problems[0].Error(), ix.pathFor(orderIndexStem(key))) {
				t.Errorf("the problem %q does not name the file", problems[0])
			}
		})
	}
	// The valid form decodes (the table above differs from it in one thing).
	ix := newTestOrderIndex(t)
	_ = os.MkdirAll(ix.dir, 0o700)
	_ = os.WriteFile(ix.pathFor(orderIndexStem(key)), []byte(valid), 0o600)
	if _, found, err := ix.load(key); err != nil || !found {
		t.Fatalf("the valid entry = %v, %v", found, err)
	}
	// So does a retryable one — the state a person may also set by hand.
	_ = os.WriteFile(ix.pathFor(orderIndexStem(key)), []byte(`{"key":"4711-release","number":7,"state":"retryable","retries":2,"reservedAt":"x"}`), 0o600)
	if e, found, err := ix.load(key); err != nil || !found || e.State != orderRetryable || e.Retries != 2 {
		t.Fatalf("a retryable entry = %+v, %v, %v", e, found, err)
	}
}

// all() is what recovery reads: every readable entry sorted by number,
// atomicfile's leftover temp files and other names skipped, and anything wrong
// — an unreadable entry, two keys holding one number, which no correct run
// produces — reported rather than fatal.
func TestOrderIndex_All(t *testing.T) {
	ix := newTestOrderIndex(t)
	for i, n := range []int64{30, 10, 20} {
		if err := ix.save(&orderIndexEntry{Key: "k" + string(rune('a'+i)), Number: n, State: orderReserved}); err != nil {
			t.Fatal(err)
		}
	}
	for _, junk := range []string{"." + orderIndexStem("kz") + ".json.tmp-123", "README.txt"} {
		if err := os.WriteFile(filepath.Join(ix.dir, junk), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(ix.dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	entries, problems, err := ix.all()
	if err != nil || len(problems) != 0 {
		t.Fatalf("all: %v, %v", problems, err)
	}
	var got []int64
	for _, e := range entries {
		got = append(got, e.Number)
	}
	if !reflect.DeepEqual(got, []int64{10, 20, 30}) {
		t.Errorf("all = %v; want [10 20 30]", got)
	}

	if err := ix.save(&orderIndexEntry{Key: "kd", Number: 20, State: orderWritten}); err != nil {
		t.Fatal(err)
	}
	entries, problems, err = ix.all()
	if err != nil || len(entries) != 4 || len(problems) != 1 || !errors.Is(problems[0], ErrOrderIndexUnreadable) {
		t.Errorf("two keys holding order 20 = %d entries, %v, %v; want it reported", len(entries), problems, err)
	}

	missing := orderIndex{dir: filepath.Join(t.TempDir(), "nothing-here")}
	if entries, problems, err := missing.all(); err != nil || len(entries)+len(problems) != 0 {
		t.Errorf("an index never written = %v, %v, %v; want empty", entries, problems, err)
	}

	// A directory that cannot be listed is an error — but not "this key's
	// entry is unreadable", which the route answers as 500 for one key.
	notADir := orderIndex{dir: filepath.Join(t.TempDir(), "file")}
	if err := os.WriteFile(notADir.dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := notADir.all(); err == nil || errors.Is(err, ErrOrderIndexUnreadable) {
		t.Errorf("an unlistable index = %v; want an error that is not ErrOrderIndexUnreadable", err)
	}
}
