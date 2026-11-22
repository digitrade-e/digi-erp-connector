package linkproto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"golang.org/x/crypto/nacl/box"
)

// Plan 094: the shell's envelope carries a one-shot X25519 key the box seals its whole answer to.
func shellKeyring(t *testing.T) (ed25519.PrivateKey, Keyring) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return priv, Keyring{{ID: "shell-1", Epoch: 1, Pub: pub}}
}

func TestAReplyKeyIsOnlyForTheShell(t *testing.T) {
	priv, kr := shellKeyring(t)
	now := time.Now()
	reply, _, _ := box.GenerateKey(rand.Reader)
	base := Envelope{Box: "b1", StdinSHA256: SHA256Hex(nil), Nonce: RandomNonce(16), IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
		ReplyKey: base64.StdEncoding.EncodeToString(reply[:])}

	shell := base
	shell.Command = CommandShell
	s, _ := SignEnvelope(priv, "shell-1", shell)
	if _, _, err := kr.VerifyEnvelope(s, now, "b1", 1); err != nil {
		t.Fatalf("a shell envelope with a reply key: %v", err)
	}

	other := base
	other.Command = "discover"
	s, _ = SignEnvelope(priv, "shell-1", other)
	if _, _, err := kr.VerifyEnvelope(s, now, "b1", 1); err == nil {
		t.Fatal("a reply key on another command was accepted")
	}

	bad := base
	bad.Command, bad.ReplyKey = CommandShell, "not-a-key"
	s, _ = SignEnvelope(priv, "shell-1", bad)
	if _, _, err := kr.VerifyEnvelope(s, now, "b1", 1); err == nil {
		t.Fatal("a malformed reply key was accepted")
	}
}

func TestKeyringHas(t *testing.T) {
	_, kr := shellKeyring(t)
	if !kr.Has("shell-1") || kr.Has("team-1") || Keyring(nil).Has("shell-1") {
		t.Fatal("Has")
	}
}

func TestReplyKeyParses(t *testing.T) {
	pub, priv, _ := box.GenerateKey(rand.Reader)
	k, err := ParseReplyKey(base64.StdEncoding.EncodeToString(pub[:]))
	if err != nil || k != *pub {
		t.Fatalf("parse: %v", err)
	}
	sealed, err := Seal(k, []byte("answer"))
	if err != nil {
		t.Fatal(err)
	}
	if out, ok := box.OpenAnonymous(nil, sealed, pub, priv); !ok || string(out) != "answer" {
		t.Fatal("the sealed answer does not open with the one-shot key")
	}
	if _, err := ParseReplyKey("AAAA"); err == nil {
		t.Fatal("a short key parsed")
	}
}
