package linkproto

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func code(err error) string {
	var r *Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	if err != nil {
		return "other: " + err.Error()
	}
	return ""
}

func mustKeys(t *testing.T) BoxKeys {
	t.Helper()
	k, err := NewBoxKeys(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestBoxKeysRoundTrip(t *testing.T) {
	k := mustKeys(t)
	back, err := ParseBoxKeys(k.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if !back.Public().Equal(k.Public()) {
		t.Fatal("parsed keys have other public halves")
	}
	if fp := k.Public().Fingerprint(); len(fp) != 24 || strings.Count(fp, "-") != 4 {
		t.Fatalf("fingerprint %q is not five groups of four", fp)
	}
	if k.Public().Fingerprint() == mustKeys(t).Public().Fingerprint() {
		t.Fatal("two boxes share a fingerprint")
	}
	if _, err := ParseBoxKeys([]byte("short")); err == nil {
		t.Fatal("a short key blob parsed")
	}
}

func TestHello(t *testing.T) {
	k := mustKeys(t)
	ch, err := NewChallenge(rand.Reader, "relay-a")
	if err != nil {
		t.Fatal(err)
	}
	good := Hello{Box: "groopy-tsdc", Build: "v1.1.0", Host: "TSDC", DomainRole: 5}
	SignHello(k, &good, ch)
	pub, err := VerifyHello(good, ch)
	if err != nil || !pub.Equal(k.Public()) {
		t.Fatalf("a good hello: %v", err)
	}

	other, _ := NewChallenge(rand.Reader, "relay-a")
	cases := []struct {
		name   string
		edit   func(*Hello)
		ch     Challenge
		wanted string
	}{
		{"another challenge", func(*Hello) {}, other, CodeBadSignature},
		{"another relay instance", func(*Hello) {}, Challenge{V: 1, Nonce: ch.Nonce, Relay: "relay-b"}, CodeBadSignature},
		{"another box id", func(h *Hello) { h.Box = "other-box" }, ch, CodeBadSignature},
		{"a changed domain role", func(h *Hello) { h.DomainRole = 3 }, ch, CodeBadSignature},
		{"another key named", func(h *Hello) { h.Sign = mustKeys(t).Public().SignB64() }, ch, CodeBadSignature},
		{"neither box nor code", func(h *Hello) { h.Box = "" }, ch, CodeMalformed},
		{"both box and code", func(h *Hello) { h.PairCode = "ABCDE-FGHJK" }, ch, CodeMalformed},
		{"a box id that is not a path segment", func(h *Hello) { h.Box = "../x" }, ch, CodeMalformed},
		{"another version", func(h *Hello) { h.V = 2 }, ch, CodeVersion},
		{"a broken key", func(h *Hello) { h.Seal = "AAAA" }, ch, CodeBadKey},
	}
	for _, c := range cases {
		h := good
		c.edit(&h)
		if _, err := VerifyHello(h, c.ch); code(err) != c.wanted {
			t.Errorf("%s: got %q, want %q", c.name, code(err), c.wanted)
		}
	}
}

func teamKey(t *testing.T, id string, epoch int) (ed25519.PrivateKey, TeamKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, TeamKey{ID: id, Epoch: epoch, Pub: pub}
}

func TestEnvelope(t *testing.T) {
	priv, k1 := teamKey(t, "team-1", 1)
	oldPriv, k0 := teamKey(t, "team-0", 0)
	kr := Keyring{k0, k1}
	now := time.Unix(1_800_000_000, 0)
	base := Envelope{Box: "groopy-tsdc", Command: "discover", StdinSHA256: SHA256Hex(nil), Nonce: RandomNonce(16),
		IssuedAt: now.Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix(), By: "ilya"}

	s, err := SignEnvelope(priv, "team-1", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := kr.VerifyEnvelope(s, now, "groopy-tsdc", 1); err != nil {
		t.Fatalf("a good envelope: %v", err)
	}

	tampered := s
	tampered.Body = bytes.Replace(s.Body, []byte(`"discover"`), []byte(`"restore"`), 1)
	unknownField := s
	unknownField.Body = bytes.Replace(s.Body, []byte(`{`), []byte(`{"shell":"cmd",`), 1)

	sealedOld, _ := SignEnvelope(oldPriv, "team-0", base)
	cases := []struct {
		name   string
		mutate func() (Signed, Envelope)
		box    string
		now    time.Time
		wanted string
	}{
		{"a changed command", func() (Signed, Envelope) { return tampered, base }, "groopy-tsdc", now, CodeBadSignature},
		{"another box", func() (Signed, Envelope) { return s, base }, "bfl-node1", now, CodeWrongBox},
		{"expired", func() (Signed, Envelope) { return s, base }, "groopy-tsdc", now.Add(5 * time.Minute), CodeExpired},
		{"from the future", func() (Signed, Envelope) { return s, base }, "groopy-tsdc", now.Add(-3 * time.Minute), CodeFromFuture},
		{"a key below the box's epoch", func() (Signed, Envelope) { return sealedOld, base }, "groopy-tsdc", now, CodeKeyRevoked},
		{"an unknown key", func() (Signed, Envelope) { x := s; x.Key = "team-9"; return x, base }, "groopy-tsdc", now, CodeKeyUnknown},
		{"an unknown field (re-signed)", func() (Signed, Envelope) {
			x := unknownField
			x.Sig = resign(t, priv, domainEnvelope, x.Body)
			return x, base
		}, "groopy-tsdc", now, CodeMalformed},
		{"a lifetime over five minutes", func() (Signed, Envelope) {
			e := base
			e.ExpiresAt = now.Add(6 * time.Minute).Unix()
			x, _ := SignEnvelope(priv, "team-1", e)
			return x, e
		}, "groopy-tsdc", now, CodeLifetime},
		{"a command that is not a name", func() (Signed, Envelope) {
			e := base
			e.Command = "discover; whoami"
			x, _ := SignEnvelope(priv, "team-1", e)
			return x, e
		}, "groopy-tsdc", now, CodeMalformed},
		{"an admin signature used as an envelope", func() (Signed, Envelope) {
			x, _ := SignAdmin(priv, "team-1", AdminRequest{Op: OpBoxesList, Nonce: RandomNonce(16), IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}, nil)
			return x, base
		}, "groopy-tsdc", now, CodeBadSignature},
	}
	for _, c := range cases {
		sg, _ := c.mutate()
		if _, _, err := kr.VerifyEnvelope(sg, c.now, c.box, 1); code(err) != c.wanted {
			t.Errorf("%s: got %q, want %q", c.name, code(err), c.wanted)
		}
	}
}

func resign(t *testing.T, priv ed25519.PrivateKey, domain string, body []byte) string {
	t.Helper()
	x, err := sign(priv, "team-1", domain, json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	return x.Sig
}

func TestAdmin(t *testing.T) {
	priv, k := teamKey(t, "team-1", 1)
	kr := Keyring{k}
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"alias":"groopy-tsdc","tenant":"groopy"}`)
	s, err := SignAdmin(priv, "team-1", AdminRequest{Op: OpPairCreate, Nonce: RandomNonce(16), IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix()}, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := kr.VerifyAdmin(s, now, OpPairCreate, body, 1); err != nil {
		t.Fatalf("a good request: %v", err)
	}
	if _, _, err := kr.VerifyAdmin(s, now, OpPairConfirm, body, 1); code(err) != CodeWrongOp {
		t.Errorf("another op: %q", code(err))
	}
	if _, _, err := kr.VerifyAdmin(s, now, OpPairCreate, []byte(`{"alias":"bfl"}`), 1); code(err) != CodeBodyMismatch {
		t.Errorf("another body: %q", code(err))
	}
}

func TestAnswer(t *testing.T) {
	k := mustKeys(t)
	stdout := []byte(`{"command":"discover","ok":true}`)
	nonce := RandomNonce(16)
	env := SHA256Hex([]byte(`{"the":"envelope"}`))
	sa, err := SignAnswer(k, Answer{Box: "groopy-tsdc", Nonce: nonce, Env: env, Exit: 0, StdoutSHA256: SHA256Hex(stdout), StdoutLen: int64(len(stdout))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyAnswer(k.Public(), sa, "groopy-tsdc", nonce, env, stdout); err != nil {
		t.Fatalf("a good answer: %v", err)
	}
	cases := []struct {
		name   string
		pub    BoxPublic
		box    string
		nonce  string
		env    string
		stdout []byte
		wanted string
	}{
		{"another box's key (a relay forging a green verify)", mustKeys(t).Public(), "groopy-tsdc", nonce, env, stdout, CodeBadSignature},
		{"an answer to another command", k.Public(), "groopy-tsdc", RandomNonce(16), env, stdout, CodeWrongNonce},
		{"an answer to another delivery of the same nonce", k.Public(), "groopy-tsdc", nonce, SHA256Hex([]byte("other")), stdout, CodeWrongNonce},
		{"a changed stdout", k.Public(), "groopy-tsdc", nonce, env, []byte(`{"command":"discover","ok":false}`), CodeBodyMismatch},
		{"another box id", k.Public(), "bfl-node1", nonce, env, stdout, CodeWrongBox},
	}
	for _, c := range cases {
		if _, err := VerifyAnswer(c.pub, sa, c.box, c.nonce, c.env, c.stdout); code(err) != c.wanted {
			t.Errorf("%s: got %q, want %q", c.name, code(err), c.wanted)
		}
	}
	// a refusal the box signed before it verified an envelope names neither nonce nor envelope: it answers nothing
	pre, _ := SignAnswer(k, Answer{Box: "groopy-tsdc", Refusal: CodeBadSignature, StdoutSHA256: SHA256Hex(nil)})
	if _, err := VerifyAnswer(k.Public(), pre, "groopy-tsdc", nonce, env, nil); code(err) != CodeWrongNonce {
		t.Errorf("a pre-verification refusal passed: %q", code(err))
	}
}

// A low-order X25519 key opens to anyone; a hello's fields have shapes (review 2026-11-15).
func TestBoxKeyAndHelloShapes(t *testing.T) {
	k := mustKeys(t)
	p := k.Public()
	if _, err := ParseBoxPublic(p.SignB64(), base64.StdEncoding.EncodeToString(make([]byte, 32))); code(err) != CodeBadKey {
		t.Errorf("the all-zero X25519 key: %q", code(err))
	}
	if _, err := ParseBoxPublic(p.SignB64(), p.SealB64()); err != nil {
		t.Errorf("a real key: %v", err)
	}
	ch, _ := NewChallenge(rand.Reader, "r1")
	for name, edit := range map[string]func(*Hello){
		"a newline in the build":  func(h *Hello) { h.Build = "v1\nINFO forged" },
		"a NUL in the host":       func(h *Hello) { h.Host = "SQL\x00SRV" },
		"a domain role of 260":    func(h *Hello) { h.DomainRole = 260 },
		"an escape in the domain": func(h *Hello) { h.Domain = "b4l\x1b[2K.local" },
	} {
		h := Hello{Box: "bfl-read", Build: "v1.0.28", Host: "SQLSRV", Domain: "b4l.local", DomainRole: 3}
		edit(&h)
		SignHello(k, &h, ch)
		if _, err := VerifyHello(h, ch); code(err) != CodeMalformed {
			t.Errorf("%s: %q", name, code(err))
		}
	}
	h := Hello{Box: "bfl-read", Build: "v1.0.28-link-580e364", Host: "SQLSRV", Domain: "b4l.local", DomainRole: 3}
	SignHello(k, &h, ch)
	if _, err := VerifyHello(h, ch); err != nil {
		t.Errorf("a good hello: %v", err)
	}
}

// A peer that announces a frame and stalls holds no more memory than it sent (review 2026-11-15).
func TestReadFrameGrowsAsBytesArrive(t *testing.T) {
	var head [4]byte
	binary.BigEndian.PutUint32(head[:], MaxPayload)
	r := io.MultiReader(bytes.NewReader(head[:]), bytes.NewReader([]byte("only a little")))
	if _, err := ReadFrame(r, MaxPayload); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("a short frame: %v", err)
	}
}

func TestSeal(t *testing.T) {
	k := mustKeys(t)
	secret := []byte(`{"db":{"user":"sa","password":"correct horse"}}`)
	sealed, err := Seal(k.Public().Seal, secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("correct horse")) {
		t.Fatal("the secret is visible in the sealed bytes")
	}
	out, err := Open(k, sealed)
	if err != nil || !bytes.Equal(out, secret) {
		t.Fatalf("open: %v", err)
	}
	if _, err := Open(mustKeys(t), sealed); err == nil {
		t.Fatal("another box opened it (a relay that swapped the key could read the password)")
	}
}

func TestPairCode(t *testing.T) {
	c, err := NewPairCode(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(c) != 11 || c[5] != '-' {
		t.Fatalf("code %q is not XXXXX-XXXXX", c)
	}
	n, err := NormalizePairCode(strings.ToLower(c))
	if err != nil || PairCodeHash(n) != PairCodeHash(strings.ReplaceAll(c, "-", "")) {
		t.Fatalf("normalize %q: %v", c, err)
	}
	if a, _ := NormalizePairCode("o1il0-ABCDE"); a != "01110ABCDE" {
		t.Errorf("look-alikes: %q", a)
	}
	for _, bad := range []string{"", "ABCDE", "ABCDE-FGHJKL", "ABCDE-FGHJU", "ABCDE_FGHJK"} {
		if _, err := NormalizePairCode(bad); code(err) != CodeBadPairCode {
			t.Errorf("%q: %q", bad, code(err))
		}
	}
	if strings.Contains(PairCodeHash("01110ABCDE"), "01110ABCDE") {
		t.Fatal("the hash shows the code")
	}
}

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSONFrame(&buf, Control{T: CtlLiveQuery, Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	var c Control
	if err := ReadJSONFrame(&buf, MaxControlFrame, &c); err != nil || c.T != CtlLiveQuery || c.Epoch != 7 {
		t.Fatalf("round trip: %v %+v", err, c)
	}
	buf.Reset()
	_ = WriteFrame(&buf, make([]byte, 100))
	if _, err := ReadFrame(&buf, 99); code(err) != CodeFrameTooLarge {
		t.Fatalf("an oversize frame: %q", code(err))
	}
	buf.Reset()
	_ = WriteFrame(&buf, []byte(`{"t":"live"} {"t":"goaway"}`))
	if err := ReadJSONFrame(&buf, MaxControlFrame, &c); code(err) != CodeMalformed {
		t.Fatalf("trailing data: %q", code(err))
	}
}
