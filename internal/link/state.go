// Package link is the connector's outbound link to the relay (Digitrade plan
// 083): the box dials out on 443, proves its key, and the relay sends it data
// streams (to the daemon's local API) and signed commands (run as
// digi-erp-connector-provision.exe, nothing else). Nothing listens on the
// network for it.
package link

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// AutoUpgrade is the client's off switch for managed auto-update (plan 088): false = the box never upgrades on
	// its own, whatever policy it holds. nil = on. Set by hand only, like admin: the link never writes it.
	AutoUpgrade *bool `yaml:"autoUpgrade,omitempty"`

	unknownKeys bool // link.yaml names a key the link does not know: commands are refused (UnknownKeys)
}

// UnknownKeys says link.yaml names a key the link does not know (a misspelt off switch): every command is refused.
func (s State) UnknownKeys() bool { return s.unknownKeys }

// RelayURL is Relay or the default.
func (s State) RelayURL() string {
	if s.Relay == "" {
		return DefaultRelay
	}
	return s.Relay
}

// AdminOn reports whether signed commands may run.
func (s State) AdminOn() bool { return s.Admin == nil || *s.Admin }

// AutoUpgradeOn reports whether the box may upgrade itself under a policy (plan 088).
func (s State) AutoUpgradeOn() bool { return s.AutoUpgrade == nil || *s.AutoUpgrade }

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

	policyMu sync.Mutex // link\policy.json (plan 088)
	autoMu   sync.Mutex // link\autoupgrade.json (plan 088)
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
	// Strict: a key the link does not know (a misspelt "Admin: false") is an error, not a silent "commands on"; the
	// caller then refuses commands (review 2026-11-15).
	var st State
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&st); err != nil && !errors.Is(err, io.EOF) {
		var lenient State
		if yaml.Unmarshal(b, &lenient) != nil {
			return State{}, fmt.Errorf("link.yaml does not parse: %w", err)
		}
		// a key the link does not know: the link connects (the data path stays) and refuses every command
		lenient.unknownKeys = true
		return lenient, nil
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
	n, err := s.MinEpochStrict(compiled)
	if err != nil {
		return compiled
	}
	return n
}

// MinEpochStrict is MinEpoch that fails closed: a floor that cannot be read is an error, never the compiled one —
// a downgrade could otherwise bring a retired key back (review 2026-11-15). No floor stored yet is the compiled one.
func (s *Store) MinEpochStrict(compiled int) (int, error) {
	b, err := s.Secrets.Get(secretMinEpoch)
	if errors.Is(err, ErrNoSecret) {
		return compiled, nil
	}
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscan(string(b), &n); err != nil {
		return 0, fmt.Errorf("the stored key-epoch floor does not parse: %w", err)
	}
	return max(n, compiled), nil
}

// RaiseMinEpoch records epoch if it is higher than the stored one.
func (s *Store) RaiseMinEpoch(epoch, compiled int) error {
	if epoch <= s.MinEpoch(compiled) {
		return nil
	}
	return s.Secrets.Set(secretMinEpoch, []byte(fmt.Sprint(epoch)))
}

// nonceKeep is how long past its expiry a nonce is remembered, in seconds.
const nonceKeep = 24 * 60 * 60

// UseNonce records nonce until expires and refuses one seen before. The file
// survives a restart, so a restart does not reopen the replay window.
func (s *Store) UseNonce(nonce string, expires, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.nonces == nil {
		nonces := map[string]int64{}
		b, err := os.ReadFile(s.noncePath())
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return &linkproto.Refusal{Code: "state_unreadable"} // fail closed: never start over from nothing
		default:
			if err := json.Unmarshal(b, &nonces); err != nil {
				return &linkproto.Refusal{Code: "state_unreadable"}
			}
		}
		s.nonces = nonces
	}
	// Kept a day past their expiry: a clock set back (NTP spoofed, a VM restored) does not reopen the window.
	for n, exp := range s.nonces {
		if exp+nonceKeep <= now.Unix() {
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
