package link

import (
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Review m11: the digest a held policy names for the build replaced counts only when the policy verifies at the box's
// epoch floor — a retired key's policy names nothing (since D8 the digest is a cross-check, never an authority).
func TestTheKnownDigestIgnoresARetiredKey(t *testing.T) {
	priv1, kr1 := team(t, 1)
	_, kr2 := team(t, 2)
	store := &Store{Dir: t.TempDir(), Secrets: &memSecrets{}}
	l := New(Config{Store: store, Team: append(kr1, kr2...), Build: "v1.0.29", Log: &testLog{}})
	now := time.Now()
	old := targetPolicy(everyBox(), 1, "1.0.29", strings.Repeat("7", 64), now)
	sp, err := linkproto.SignPolicy(priv1, "team-1", old)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.savePolicies(policyFile{V: 1, Policies: []storedPolicy{{Scope: old.Scope, Seq: 1, Epoch: 1, AcceptedAt: now, Signed: sp}}}); err != nil {
		t.Fatal(err)
	}
	if got := l.knownSetupSHA("1.0.29"); got != strings.Repeat("7", 64) {
		t.Fatalf("at epoch 1: %q", got)
	}
	if err := store.RaiseMinEpoch(2, CompiledMinEpoch); err != nil {
		t.Fatal(err)
	}
	if got := l.knownSetupSHA("1.0.29"); got != "" {
		t.Fatalf("a retired key's policy named %q", got)
	}
	// a floor that cannot be read: nothing counts
	_ = store.Secrets.Set(secretMinEpoch, []byte("not a number"))
	if got := l.knownSetupSHA("1.0.29"); got != "" {
		t.Fatalf("with no floor to judge by: %q", got)
	}
}
