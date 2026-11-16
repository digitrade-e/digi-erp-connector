package link

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// The session marker (plan 088 D5): DataDir\link\session.json is the link's connection to the relay as the upgrade
// runner sees it — the runner is another process and cannot ask link-status. The link writes it when a connection
// comes up or goes down and at every liveness answer that says the connection is current, so LiveAt is at most
// LiveEvery old while the box is connected. The runner's health check needs it from every later build too: a build that
// stopped writing it would be rolled back by the build before it.

// SessionMarker is DataDir\link\session.json.
type SessionMarker struct {
	Build       string    `json:"build"` // the link's build
	Connected   bool      `json:"connected"`
	Epoch       int64     `json:"epoch,omitempty"`
	Relay       string    `json:"relay,omitempty"`
	ConnectedAt time.Time `json:"connectedAt,omitzero"`
	LiveAt      time.Time `json:"liveAt,omitzero"` // the relay last said this connection is the one it routes to
}

// The link's own state files, all in DataDir\link (beside nonces.json): written atomically under the data directory's
// protected DACL, read by handle (never through a link).
func linkDir(dataDir string) string       { return filepath.Join(dataDir, "link") }
func sessionPath(dataDir string) string   { return filepath.Join(linkDir(dataDir), "session.json") }
func policyPath(dataDir string) string    { return filepath.Join(linkDir(dataDir), "policy.json") }
func autoStatePath(dataDir string) string { return filepath.Join(linkDir(dataDir), "autoupgrade.json") }

// writeLinkFile writes one of the link's state files: DataDir\link must be a plain folder (no junction in its place)
// and the write is atomic. The file is born inside the data directory's protected tree and inherits its DACL;
// ProtectFile only asserts it again (a no-op off Windows, and when it already matches), so its failure does not undo a
// write that happened.
func writeLinkFile(dataDir, path string, b []byte) error {
	dir := linkDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := dataacl.PlainDir(dir); err != nil {
		return err
	}
	if err := atomicfile.Write(path, b, 0o600); err != nil {
		return err
	}
	_ = dataacl.ProtectFile(path)
	return nil
}

// readLinkFile reads one of the link's state files by handle, at most max bytes; a missing file wraps os.ErrNotExist.
func readLinkFile(dataDir, path string, max int64) ([]byte, error) {
	f, err := dataacl.OpenRead(dataDir, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), max)
	}
	return b, nil
}

// ReadSessionMarker reads DataDir\link\session.json.
func ReadSessionMarker(dataDir string) (SessionMarker, error) {
	b, err := readLinkFile(dataDir, sessionPath(dataDir), 64<<10)
	if err != nil {
		return SessionMarker{}, err
	}
	var m SessionMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return SessionMarker{}, fmt.Errorf("link\\session.json does not parse: %w", err)
	}
	return m, nil
}

// writeSession records the live status in the marker. Writes are serialized and each one writes the status as it is
// then, so a late write never brings back an older state.
func (l *Link) writeSession() {
	if l.cfg.Store == nil || l.cfg.Store.Dir == "" {
		return
	}
	l.sessionMu.Lock()
	defer l.sessionMu.Unlock()
	l.status.mu.Lock()
	m := SessionMarker{Build: l.cfg.Build, Connected: l.status.connected}
	if l.status.connected {
		m.Epoch, m.Relay, m.ConnectedAt, m.LiveAt = l.status.epoch, l.status.relay, l.status.connectedAt.UTC(), l.status.liveAt.UTC()
	}
	l.status.mu.Unlock()
	b, _ := json.Marshal(m)
	if err := writeLinkFile(l.cfg.Store.Dir, sessionPath(l.cfg.Store.Dir), b); err != nil {
		l.logEvery("session", func() {
			l.cfg.Log.Error("link: writing link\\session.json (the upgrade runner's health check reads it)", err)
		})
	}
}
