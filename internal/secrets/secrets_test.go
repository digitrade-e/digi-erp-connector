package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func redirectDataDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("the data directory is only relocatable via PROGRAMDATA on Windows")
	}
	dir := t.TempDir()
	t.Setenv("PROGRAMDATA", dir)
	return dir
}

func TestSetGetRoundTrip(t *testing.T) {
	redirectDataDir(t)

	const key = "db_password_hasavshevet"
	want := []byte("wizsoft")

	if err := Set(key, want); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := Get(key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Get = %q, want %q", got, want)
	}
}

// The stored bytes must not be the plaintext: this is the whole point of the
// package (DPAPI machine scope on Windows).
func TestStoredValueIsEncrypted(t *testing.T) {
	dir := redirectDataDir(t)

	const secret = "a-recognisable-plaintext-password"
	if err := Set("db_password_hasavshevet", []byte(secret)); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "digi-erp-connector", "secrets", "db_password_hasavshevet.bin"))
	if err != nil {
		t.Fatalf("read secret file: %v", err)
	}
	if string(raw) == secret {
		t.Error("secret is stored as plaintext on disk")
	}
}

func TestSetOverwrites(t *testing.T) {
	redirectDataDir(t)

	const key = "db_password_sap"
	if err := Set(key, []byte("first-and-much-longer-password")); err != nil {
		t.Fatalf("first Set: %v", err)
	}
	if err := Set(key, []byte("second")); err != nil {
		t.Fatalf("second Set: %v", err)
	}

	got, err := Get(key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("Get = %q, want %q", got, "second")
	}
}

func TestGetMissingSecret(t *testing.T) {
	redirectDataDir(t)

	_, err := Get("never_stored")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a missing secret = %v, want ErrNotFound", err)
	}
}

// A rename failure used to be reported as success, which meant the daemon
// started with the wrong DB password and failed at db.Open with no clue why.
func TestSetReportsFailure(t *testing.T) {
	dir := redirectDataDir(t)

	// Occupy the secret's path with a directory so the write cannot succeed.
	blocked := filepath.Join(dir, "digi-erp-connector", "secrets", "blocked.bin")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := Set("blocked", []byte("value")); err == nil {
		t.Error("Set returned nil although the secret could not be written")
	}
}

func TestSanitizeKey(t *testing.T) {
	// Dots are permitted, path separators are not: each run of disallowed
	// characters collapses to a single underscore. ".." therefore survives but
	// can never traverse, since the separator it would need is gone.
	tests := map[string]string{
		"db_password_hasavshevet": "db_password_hasavshevet",
		"db password":             "db_password",
		"../../escape":            ".._.._escape",
		`sub\dir`:                 "sub_dir",
		"sub/dir":                 "sub_dir",
		"":                        "empty",
		"   ":                     "empty",
	}

	for in, want := range tests {
		if got := sanitizeKey(in); got != want {
			t.Errorf("sanitizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// Keys must never be able to escape the secrets directory.
func TestSecretPathStaysInsideSecretsDir(t *testing.T) {
	dir := redirectDataDir(t)

	secretsDir := filepath.Join(dir, "digi-erp-connector", "secrets")
	for _, key := range []string{"../escape", `..\escape`, "a/b/c", "normal"} {
		p, err := secretFilePath(key)
		if err != nil {
			t.Fatalf("secretFilePath(%q): %v", key, err)
		}
		rel, err := filepath.Rel(secretsDir, p)
		if err != nil {
			t.Fatalf("Rel: %v", err)
		}
		if filepath.IsAbs(rel) || rel == ".." || len(rel) > 2 && rel[:3] == ".."+string(filepath.Separator) {
			t.Errorf("key %q escapes the secrets dir: %s", key, p)
		}
	}
}
