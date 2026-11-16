package formatfloor

import (
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRaiseNeverLowersTheFloor(t *testing.T) {
	dir := t.TempDir()
	if got, err := Read(dir); err != nil || got != "" {
		t.Fatalf("none yet: %q %v", got, err)
	}
	// a first start records the build's floor
	if got, err := Raise(dir, "1.0.30"); err != nil || got != "1.0.30" {
		t.Fatalf("first: %q %v", got, err)
	}
	if b, _ := os.ReadFile(Path(dir)); string(b) != "1.0.30\n" {
		t.Fatalf("file %q", b)
	}
	// a later build that changed a format raises it
	if got, err := Raise(dir, "1.0.35"); err != nil || got != "1.0.35" {
		t.Fatalf("raised: %q %v", got, err)
	}
	// the build rolled back to (or an older one installed by hand) never lowers it
	for _, older := range []string{"1.0.31", "1.0.30", "1.0.9"} {
		if got, err := Raise(dir, older); err != nil || got != "1.0.35" {
			t.Fatalf("%s lowered it: %q %v", older, got, err)
		}
	}
	if got, _ := Read(dir); got != "1.0.35" {
		t.Fatalf("read %q", got)
	}
	if _, err := Raise(dir, "dev"); err == nil {
		t.Fatal("a floor that is not a release number was recorded")
	}
}

// A recorded floor that cannot be read is never overwritten (it may be higher than this build's), and it reads as an
// error: the runner then refuses a rollback.
func TestAnUnreadableFloorIsLeftAlone(t *testing.T) {
	for _, body := range []string{"garbage", "", "1.0", strings.Repeat("1", 100), "1.0.30 1.0.31"} {
		dir := t.TempDir()
		_ = os.WriteFile(Path(dir), []byte(body), 0o600)
		if _, err := Read(dir); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%q: read %v", body, err)
		}
		if _, err := Raise(dir, "1.0.30"); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%q: raise %v", body, err)
		}
		if b, _ := os.ReadFile(Path(dir)); string(b) != body {
			t.Errorf("%q was overwritten with %q", body, b)
		}
	}
	// a directory in its place is no floor either
	dir := t.TempDir()
	_ = os.Mkdir(Path(dir), 0o700)
	if _, err := Read(dir); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("a directory: %v", err)
	}
}

func TestBelowAndMax(t *testing.T) {
	for _, c := range []struct {
		version, floor string
		below          bool
	}{
		{"1.0.29", "1.0.30", true},
		{"1.0.30", "1.0.30", false},
		{"v1.0.31", "1.0.30", false},
		{"1.0.300", "1.0.31", false},
		{"0.9.99", "1.0.0", true},
		{"dev", "1.0.30", true}, // not a release: below every floor
		{"1.0.29", "", false},   // no floor
		{"1.0.29", "junk", true},
	} {
		if got := Below(c.version, c.floor); got != c.below {
			t.Errorf("Below(%q, %q) = %v", c.version, c.floor, got)
		}
	}
	for _, c := range [][3]string{{"1.0.30", "1.0.35", "1.0.35"}, {"1.0.35", "1.0.30", "1.0.35"}, {"", "1.0.30", "1.0.30"}, {"1.0.30", "", "1.0.30"}, {"", "", ""}} {
		if got := Max(c[0], c[1]); got != c[2] {
			t.Errorf("Max(%q, %q) = %q", c[0], c[1], got)
		}
	}
}
