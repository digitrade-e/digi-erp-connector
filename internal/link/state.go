// Package link is the connector's outbound link to the relay (Digitrade plan
// 083): the box dials out on 443, proves its key, and the relay sends it data
// streams (to the daemon's local API) and signed commands (run as
// digi-erp-connector-provision.exe, nothing else). Nothing listens on the
// network for it.
package link

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// DefaultRelay is the relay's /link URL (plan 083 D6).
const DefaultRelay = "wss://link.digitrade-admin.org/link"

// State is link.yaml in the data directory: what the client's IT may read and
// change. Admin is their off switch for the command path (plan 083 §3.2); the
// data path stays up when it is off.
type State struct {
	Relay    string `yaml:"relay,omitempty"`
	Box      string `yaml:"box,omitempty"`      // set once a pairing code is redeemed
	PairCode string `yaml:"pairCode,omitempty"` // a code waiting to be redeemed; cleared after
	Admin    *bool  `yaml:"admin,omitempty"`    // nil = on
	Proxy    string `yaml:"proxy,omitempty"`    // an HTTP proxy URL; empty = the machine's
}

// RelayURL is Relay or the default.
func (s State) RelayURL() string {
	if s.Relay == "" {
		return DefaultRelay
	}
	return s.Relay
}

// AdminOn reports whether signed commands may run.
func (s State) AdminOn() bool { return s.Admin == nil || *s.Admin }

// Secrets is the box's secret store (DPAPI on Windows: internal/secrets).
type Secrets interface {
	Get(key string) ([]byte, error)
	Set(key string, value []byte) error
}

// ErrNoSecret is what a Secrets returns for a key never stored.
var ErrNoSecret = errors.New("link: secret not found")

const (
	secretKeys     = "link_box_keys"
	secretMinEpoch = "link_min_key_epoch"
)

// Store keeps the link's state under one directory.
type Store struct {
	Dir     string // the data directory
	Secrets Secrets

	mu     sync.Mutex
	nonces map[string]int64 // nonce → expiry (unix); loaded lazily
}

func (s *Store) statePath() string { return filepath.Join(s.Dir, "link.yaml") }
func (s *Store) noncePath() string { return filepath.Join(s.Dir, "link", "nonces.json") }

// LoadState reads link.yaml; a missing file is the zero State.
func (s *Store) LoadState() (State, error) {
	b, err := os.ReadFile(s.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var st State
	if err := yaml.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("link.yaml does not parse: %w", err)
	}
	return st, nil
}

// SaveState writes link.yaml atomically.
func (s *Store) SaveState(st State) error {
	b, err := yaml.Marshal(st)
	if err != nil {
		return err
	}
	return atomicfile.Write(s.statePath(), b, 0o600)
}

// Keys returns the box's keys, making and storing them on first use.
func (s *Store) Keys(newKeys func() (linkproto.BoxKeys, error)) (linkproto.BoxKeys, bool, error) {
	b, err := s.Secrets.Get(secretKeys)
	if err == nil {
		k, err := linkproto.ParseBoxKeys(b)
		return k, false, err
	}
	if !errors.Is(err, ErrNoSecret) {
		return linkproto.BoxKeys{}, false, err
	}
	k, err := newKeys()
	if err != nil {
		return linkproto.BoxKeys{}, false, err
	}
	if err := s.Secrets.Set(secretKeys, k.Marshal()); err != nil {
		return linkproto.BoxKeys{}, false, err
	}
	// Read it back: a key the store cannot return would pair a box that can never reconnect.
	back, err := s.Secrets.Get(secretKeys)
	if err != nil {
		return linkproto.BoxKeys{}, false, fmt.Errorf("link: the box key did not read back: %w", err)
	}
	k2, err := linkproto.ParseBoxKeys(back)
	if err != nil || !k2.Public().Equal(k.Public()) {
		return linkproto.BoxKeys{}, false, errors.New("link: the box key did not read back intact")
	}
	return k, true, nil
}

// MinEpoch is the highest team-key epoch this box has accepted (plan 083 §8
// A17): a key below it is refused, so a downgrade cannot bring a revoked key back.
func (s *Store) MinEpoch(compiled int) int {
	b, err := s.Secrets.Get(secretMinEpoch)
	if err != nil {
		return compiled
	}
	var n int
	if _, err := fmt.Sscan(string(b), &n); err != nil || n < compiled {
		return compiled
	}
	return n
}

// RaiseMinEpoch records epoch if it is higher than the stored one.
func (s *Store) RaiseMinEpoch(epoch, compiled int) error {
	if epoch <= s.MinEpoch(compiled) {
		return nil
	}
	return s.Secrets.Set(secretMinEpoch, []byte(fmt.Sprint(epoch)))
}

// UseNonce records nonce until expires and refuses one seen before. The file
// survives a restart, so a restart does not reopen the replay window.
func (s *Store) UseNonce(nonce string, expires, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nonces == nil {
		s.nonces = map[string]int64{}
		if b, err := os.ReadFile(s.noncePath()); err == nil {
			_ = json.Unmarshal(b, &s.nonces)
		}
	}
	for n, exp := range s.nonces {
		if exp <= now.Unix() {
			delete(s.nonces, n)
		}
	}
	if _, seen := s.nonces[nonce]; seen {
		return &linkproto.Refusal{Code: linkproto.CodeReplay}
	}
	s.nonces[nonce] = expires.Unix()
	b, _ := json.Marshal(s.nonces)
	if err := os.MkdirAll(filepath.Dir(s.noncePath()), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(s.noncePath(), b, 0o600)
}
