// Package formatfloor is DataDir\format-floor (Digitrade plan 088 D11, review M3): the oldest release that still reads
// every file format the builds on this box have written — the order index, the counters, the receipts' store, the
// link's state.
//
// An older build may decode strictly what a newer one wrote, or recover a counter from it the wrong way (plan 087
// review m12: a rollback from v1.0.30 to v1.0.29 pushed production counters into test ranges and ended in
// COUNTER_EXHAUSTED). So every build from v1.0.31 raises the floor to its compiled RollbackFloor when it starts —
// never lowers it — and the upgrade runner never rolls back to a build below it.
//
// A build that changes a persisted format sets RollbackFloor to its own version in the same change.
package formatfloor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// RollbackFloor is the oldest release that reads every format this build writes. v1.0.30 (plan 087) changed the order
// index and the counter recovery; nothing since has changed a format.
const RollbackFloor = "1.0.30"

// FileName is the floor's file in the data directory.
const FileName = "format-floor"

// ErrUnreadable: the recorded floor cannot be read or is not a release number. Raise leaves such a file as it is (it
// may hold a higher floor than this build's) and the runner refuses a rollback (fail closed).
var ErrUnreadable = errors.New("the format floor cannot be read")

var release = regexp.MustCompile(`^v?(\d{1,4})\.(\d{1,4})\.(\d{1,6})$`)

// Path is DataDir\format-floor.
func Path(dataDir string) string { return filepath.Join(dataDir, FileName) }

func parse(s string) ([3]int, bool) {
	m := release.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

// Below reports whether version is a release below floor. A version that is not a release number is below every
// floor; an empty floor is no floor.
func Below(version, floor string) bool {
	if strings.TrimSpace(floor) == "" {
		return false
	}
	f, ok := parse(floor)
	if !ok {
		return true
	}
	v, ok := parse(version)
	return !ok || slices.Compare(v[:], f[:]) < 0
}

// Max is the higher of two floors ("" is none).
func Max(a, b string) string {
	if Below(a, b) {
		return b
	}
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a
}

// Read is the floor recorded in dataDir: "" when there is none, ErrUnreadable when the file cannot be read or holds
// no release number. Read by handle (no reparse point, no hard link, the final path under the data directory).
func Read(dataDir string) (string, error) {
	f, err := dataacl.OpenRead(dataDir, Path(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 65))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	s := strings.TrimSpace(string(b))
	if _, ok := parse(s); !ok || len(b) > 64 {
		return "", fmt.Errorf("%w: %s does not hold a release number like 1.0.30", ErrUnreadable, FileName)
	}
	return strings.TrimPrefix(s, "v"), nil
}

// Raise records floor in dataDir unless the recorded one is as high or higher, and returns the floor in force. An
// unreadable recorded floor is left as it is and returned as ErrUnreadable; a failed write is another error (the
// daemon then refuses to start: it would otherwise write formats a rollback cannot see).
func Raise(dataDir, floor string) (string, error) {
	if _, ok := parse(floor); !ok {
		return "", fmt.Errorf("format floor %q is not a release number", floor)
	}
	recorded, err := Read(dataDir)
	if err != nil {
		return "", err
	}
	if recorded != "" && !Below(recorded, floor) {
		return recorded, nil
	}
	if err := dataacl.PlainDir(dataDir); err != nil {
		return recorded, err
	}
	if err := atomicfile.Write(Path(dataDir), []byte(floor+"\n"), 0o600); err != nil {
		return recorded, err
	}
	_ = dataacl.ProtectFile(Path(dataDir)) // born inside the protected tree; only asserted again
	return floor, nil
}
