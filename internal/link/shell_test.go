package link

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	nacl "golang.org/x/crypto/nacl/box"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Plan 094: the remote shell. Signed by a shell key only, run only where the client opted in, never on a domain
// controller, its script sealed in and its whole answer sealed out to a one-shot key.

type fakeShell struct {
	mu      sync.Mutex
	scripts [][]byte
	out     ShellOutput
}

func (f *fakeShell) Exec(_ context.Context, _ string, script []byte) ShellOutput {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, append([]byte(nil), script...))
	return f.out
}

func (f *fakeShell) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.scripts) }

type shellBox struct {
	*box
	rc        *relayConn
	shell     *fakeShell
	teamPriv  ed25519.PrivateKey
	shellPriv ed25519.PrivateKey
}

func newShellBox(t *testing.T, role int, optIn bool) shellBox {
	t.Helper()
	teamPriv, teamKR := team(t, 1)
	pub, shellPriv, _ := ed25519.GenerateKey(rand.Reader)
	fs := &fakeShell{out: ShellOutput{Stdout: []byte("BAT contents"), Stderr: []byte(""), ExitCode: 0}}
	r := newRelay(t)
	b, rc := pairedBox(t, r, "bfl-read", teamKR, "127.0.0.1:1", func(c *Config) {
		c.Shell = linkproto.Keyring{{ID: "shell-1", Epoch: 1, Pub: pub}}
		c.ShellExec = fs.Exec
		c.DomainRole = role
	})
	if optIn {
		if err := b.store.SaveShellOptIn(ShellOptIn{Enabled: true, EnabledBy: `B4L\admin`, EnabledAt: "2026-11-22T10:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	return shellBox{box: b, rc: rc, shell: fs, teamPriv: teamPriv, shellPriv: shellPriv}
}

// shellEnvelope signs a shell command whose script is sealed to the box and whose answer comes back sealed to a fresh
// one-shot key, which it returns.
func (sb shellBox) shellEnvelope(t *testing.T, priv ed25519.PrivateKey, keyID string, script []byte) (raw, stdin []byte, replyPriv *[32]byte) {
	t.Helper()
	replyPub, replyPriv, _ := nacl.GenerateKey(rand.Reader)
	sealed, err := linkproto.Seal(sb.keys.Public().Seal, script)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e := linkproto.Envelope{Box: "bfl-read", Command: linkproto.CommandShell, StdinSHA256: linkproto.SHA256Hex(sealed), Sealed: true,
		Nonce: linkproto.RandomNonce(16), IssuedAt: now.Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix(), By: "ilya",
		ReplyKey: base64.StdEncoding.EncodeToString(replyPub[:])}
	s, _ := linkproto.SignEnvelope(priv, keyID, e)
	raw, _ = json.Marshal(s)
	return raw, sealed, replyPriv
}

func openShellAnswer(t *testing.T, sealed []byte, replyPriv *[32]byte) ShellOutput {
	t.Helper()
	pub := new([32]byte)
	// the one-shot public key is derived again from its private half
	pk, err := linkproto.X25519Public(replyPriv)
	if err != nil {
		t.Fatal(err)
	}
	*pub = pk
	plain, ok := nacl.OpenAnonymous(nil, sealed, pub, replyPriv)
	if !ok {
		t.Fatal("the shell's answer does not open with the one-shot key")
	}
	var out ShellOutput
	if err := json.Unmarshal(plain, &out); err != nil {
		t.Fatalf("the answer is not a ShellOutput: %v", err)
	}
	return out
}

func TestTheShellRunsASealedScriptAndSealsItsAnswer(t *testing.T) {
	sb := newShellBox(t, 3, true)
	raw, stdin, replyPriv := sb.shellEnvelope(t, sb.shellPriv, "shell-1", []byte("Get-Content C:\\Hash7\\digitrade.bat"))
	sealed, sa := send(t, sb.rc, raw, stdin)
	a := answerOf(sa)
	if a.Refusal != "" || a.Exit != 0 {
		t.Fatalf("refused %q exit %d", a.Refusal, a.Exit)
	}
	if a.StdoutSHA256 != linkproto.SHA256Hex(sealed) {
		t.Fatal("the signed answer does not carry the sealed blob's digest")
	}
	if strings.Contains(string(sealed), "BAT contents") {
		t.Fatal("the output crossed the relay in clear")
	}
	out := openShellAnswer(t, sealed, replyPriv)
	if string(out.Stdout) != "BAT contents" || out.ExitCode != 0 {
		t.Fatalf("answer %+v", out)
	}
	if sb.shell.count() != 1 || string(sb.shell.scripts[0]) != "Get-Content C:\\Hash7\\digitrade.bat" {
		t.Fatalf("the shell ran %q", sb.shell.scripts)
	}
	// opened by the wrong key: nothing
	_, other, _ := nacl.GenerateKey(rand.Reader)
	otherPub, _ := linkproto.X25519Public(other)
	if _, ok := nacl.OpenAnonymous(nil, sealed, &otherPub, other); ok {
		t.Fatal("the answer opened with another key")
	}
}

func TestTheShellJournalsBeforeAndAfter(t *testing.T) {
	sb := newShellBox(t, 3, true)
	raw, stdin, _ := sb.shellEnvelope(t, sb.shellPriv, "shell-1", []byte("hostname"))
	send(t, sb.rc, raw, stdin)
	b, err := os.ReadFile(filepath.Join(sb.store.Dir, "link", "shell.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"phase":"start"`) || !strings.Contains(lines[1], `"phase":"end"`) {
		t.Fatalf("journal %q", lines)
	}
	if !strings.Contains(lines[0], linkproto.SHA256Hex([]byte("hostname"))) || !strings.Contains(lines[0], `"key":"shell-1"`) {
		t.Fatalf("the start line names neither the script's digest nor the signer: %s", lines[0])
	}
	if strings.Contains(string(b), "hostname") && !strings.Contains(string(b), linkproto.SHA256Hex([]byte("hostname"))) {
		t.Fatal("the journal holds the script text")
	}
}

func TestATeamKeyCannotRunTheShell(t *testing.T) {
	sb := newShellBox(t, 3, true)
	raw, stdin, _ := sb.shellEnvelope(t, sb.teamPriv, "team-1", []byte("hostname"))
	_, sa := send(t, sb.rc, raw, stdin)
	if a := answerOf(sa); a.Refusal != CodeShellKeyRequired || sb.shell.count() != 0 {
		t.Fatalf("refusal %q, ran %d", a.Refusal, sb.shell.count())
	}
}

func TestAShellKeyRunsNothingElse(t *testing.T) {
	sb := newShellBox(t, 3, true)
	raw, _ := envelope(sb.shellPriv, "shell-1", "bfl-read", "discover", nil, []byte("{}"), time.Now())
	_, sa := send(t, sb.rc, raw, []byte("{}"))
	if a := answerOf(sa); a.Refusal != CodeShellKeyWrongCommand || sb.runner.count() != 0 {
		t.Fatalf("refusal %q, provision ran %d", a.Refusal, sb.runner.count())
	}
}

func TestNoShellWithoutTheClientsOptIn(t *testing.T) {
	sb := newShellBox(t, 3, false)
	raw, stdin, _ := sb.shellEnvelope(t, sb.shellPriv, "shell-1", []byte("hostname"))
	_, sa := send(t, sb.rc, raw, stdin)
	if a := answerOf(sa); a.Refusal != CodeShellOff || sb.shell.count() != 0 {
		t.Fatalf("refusal %q, ran %d", a.Refusal, sb.shell.count())
	}
}

func TestAnOptInThatDoesNotParseIsOff(t *testing.T) {
	sb := newShellBox(t, 3, false)
	if err := os.MkdirAll(filepath.Join(sb.store.Dir, "link"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sb.store.Dir, "link", "shell.yaml"), []byte("enabled: true\nenabeld: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, stdin, _ := sb.shellEnvelope(t, sb.shellPriv, "shell-1", []byte("hostname"))
	_, sa := send(t, sb.rc, raw, stdin)
	if a := answerOf(sa); a.Refusal != CodeShellStateUnreadable || sb.shell.count() != 0 {
		t.Fatalf("refusal %q, ran %d", a.Refusal, sb.shell.count())
	}
}

func TestNoShellOnADomainController(t *testing.T) {
	for _, role := range []int{4, 5} {
		sb := newShellBox(t, role, true)
		raw, stdin, _ := sb.shellEnvelope(t, sb.shellPriv, "shell-1", []byte("hostname"))
		_, sa := send(t, sb.rc, raw, stdin)
		if a := answerOf(sa); a.Refusal != CodeShellRefusedDC || sb.shell.count() != 0 {
			t.Fatalf("role %d: refusal %q, ran %d", role, a.Refusal, sb.shell.count())
		}
	}
}

func TestTheShellsScriptMustBeSealed(t *testing.T) {
	sb := newShellBox(t, 3, true)
	script := []byte("hostname")
	replyPub, _, _ := nacl.GenerateKey(rand.Reader)
	now := time.Now()
	e := linkproto.Envelope{Box: "bfl-read", Command: linkproto.CommandShell, StdinSHA256: linkproto.SHA256Hex(script),
		Nonce: linkproto.RandomNonce(16), IssuedAt: now.Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix(),
		ReplyKey: base64.StdEncoding.EncodeToString(replyPub[:])}
	s, _ := linkproto.SignEnvelope(sb.shellPriv, "shell-1", e)
	raw, _ := json.Marshal(s)
	_, sa := send(t, sb.rc, raw, script)
	if a := answerOf(sa); a.Refusal != CodeInputNotSealed || sb.shell.count() != 0 {
		t.Fatalf("refusal %q, ran %d", a.Refusal, sb.shell.count())
	}
}

func TestTheShellNeedsAReplyKey(t *testing.T) {
	sb := newShellBox(t, 3, true)
	sealed, _ := linkproto.Seal(sb.keys.Public().Seal, []byte("hostname"))
	now := time.Now()
	e := linkproto.Envelope{Box: "bfl-read", Command: linkproto.CommandShell, StdinSHA256: linkproto.SHA256Hex(sealed), Sealed: true,
		Nonce: linkproto.RandomNonce(16), IssuedAt: now.Unix(), ExpiresAt: now.Add(4 * time.Minute).Unix()}
	s, _ := linkproto.SignEnvelope(sb.shellPriv, "shell-1", e)
	raw, _ := json.Marshal(s)
	_, sa := send(t, sb.rc, raw, sealed)
	if a := answerOf(sa); a.Refusal != CodeShellNoReplyKey || sb.shell.count() != 0 {
		t.Fatalf("refusal %q, ran %d", a.Refusal, sb.shell.count())
	}
}

func TestAShellCommandIsNeverReplayed(t *testing.T) {
	sb := newShellBox(t, 3, true)
	raw, stdin, _ := sb.shellEnvelope(t, sb.shellPriv, "shell-1", []byte("hostname"))
	send(t, sb.rc, raw, stdin)
	_, sa := send(t, sb.rc, raw, stdin)
	if a := answerOf(sa); a.Refusal != linkproto.CodeReplay || sb.shell.count() != 1 {
		t.Fatalf("refusal %q, ran %d", a.Refusal, sb.shell.count())
	}
}

func TestAnOversizeScriptIsRefused(t *testing.T) {
	sb := newShellBox(t, 3, true)
	raw, stdin, _ := sb.shellEnvelope(t, sb.shellPriv, "shell-1", []byte(strings.Repeat("x", MaxShellScript+1)))
	_, sa := send(t, sb.rc, raw, stdin)
	if a := answerOf(sa); a.Refusal != CodeShellScriptTooLarge || sb.shell.count() != 0 {
		t.Fatalf("refusal %q, ran %d", a.Refusal, sb.shell.count())
	}
}

func TestTheShellWaitsForAProvisionRun(t *testing.T) {
	sb := newShellBox(t, 3, true)
	if err := os.MkdirAll(filepath.Dir(ProvisionLockPath(sb.store.Dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	h, err := filelock.Lock(ProvisionLockPath(sb.store.Dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Unlock() }()
	raw, stdin, _ := sb.shellEnvelope(t, sb.shellPriv, "shell-1", []byte("hostname"))
	_, sa := send(t, sb.rc, raw, stdin)
	if a := answerOf(sa); a.Refusal != CodeShellBusy || sb.shell.count() != 0 {
		t.Fatalf("refusal %q, ran %d", a.Refusal, sb.shell.count())
	}
}

func TestAShellKeyBelowItsFloorIsRefused(t *testing.T) {
	sb := newShellBox(t, 3, true)
	if err := sb.store.RaiseMinShellEpoch(2, CompiledMinShellEpoch); err != nil {
		t.Fatal(err)
	}
	raw, stdin, _ := sb.shellEnvelope(t, sb.shellPriv, "shell-1", []byte("hostname"))
	_, sa := send(t, sb.rc, raw, stdin)
	if a := answerOf(sa); a.Refusal != linkproto.CodeKeyRevoked || sb.shell.count() != 0 {
		t.Fatalf("refusal %q, ran %d", a.Refusal, sb.shell.count())
	}
	// the team key's floor is apart: it did not move
	if n, _ := sb.store.MinEpochStrict(CompiledMinEpoch); n != CompiledMinEpoch {
		t.Fatalf("the team floor moved to %d", n)
	}
}

// Rollback safety (plan 094 §3 item 2): the opt-in lives in its own file, so a build before the shell — which decodes
// link.yaml strictly — still reads link.yaml as it always did.
func TestTheOptInNeverTouchesLinkYAML(t *testing.T) {
	store := &Store{Dir: t.TempDir(), Secrets: &memSecrets{}}
	if err := store.SaveState(State{Box: "bfl-read"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShellOptIn(ShellOptIn{Enabled: true, EnabledBy: "admin", EnabledAt: "2026-11-22T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	st, err := store.LoadState()
	if err != nil || st.UnknownKeys() || st.Box != "bfl-read" {
		t.Fatalf("link.yaml: %+v %v", st, err)
	}
	b, _ := os.ReadFile(filepath.Join(store.Dir, "link.yaml"))
	if strings.Contains(string(b), "shell") || strings.Contains(string(b), "enabled") {
		t.Fatalf("link.yaml holds the opt-in: %s", b)
	}
}

func TestTheCompiledShellKeysAreUnusableUntilSet(t *testing.T) {
	kr, err := ShellKeyring()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kr {
		if len(k.Pub) != ed25519.PublicKeySize {
			t.Fatalf("shell key %s is not a key", k.ID)
		}
		if !strings.HasPrefix(k.ID, "shell-") {
			t.Fatalf("a shell key named %s", k.ID)
		}
	}
}

func TestNoShellWhereThePlatformHasNone(t *testing.T) {
	teamPriv, teamKR := team(t, 1)
	_ = teamPriv
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := newRelay(t)
	b, rc := pairedBox(t, r, "bfl-read", teamKR, "127.0.0.1:1", func(c *Config) {
		c.Shell = linkproto.Keyring{{ID: "shell-1", Epoch: 1, Pub: pub}}
		c.ShellExec = nil
	})
	_ = b.store.SaveShellOptIn(ShellOptIn{Enabled: true})
	sb := shellBox{box: b, rc: rc}
	raw, stdin, _ := sb.shellEnvelope(t, priv, "shell-1", []byte("hostname"))
	_, sa := send(t, rc, raw, stdin)
	if a := answerOf(sa); a.Refusal != CodeShellNotSupported {
		t.Fatalf("refusal %q", a.Refusal)
	}
}
