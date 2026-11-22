package link

import (
	"bytes"
	"context"
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
	"github.com/digitrade-e/digi-erp-connector/internal/platform/filelock"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// The remote shell (plan 094): a PowerShell script, signed by a shell key (never a team key), run as the link's own
// account (LocalSystem) where the client's administrator opted in, never on a domain controller. Its script arrives
// sealed to this box and its whole answer leaves sealed to the one-shot key the envelope names, so the relay sees
// neither. Every run is journalled before it starts.
const (
	// MaxShellScript bounds the script.
	MaxShellScript = 64 << 10
	// ShellTimeout bounds one run; its process tree is ended then.
	ShellTimeout = 5 * time.Minute
	// ShellOutputCap bounds stdout and stderr together: the sealed JSON answer (base64 bytes, the seal's overhead)
	// must fit one frame (linkproto.MaxPayload).
	ShellOutputCap = 640 << 10
)

// The shell's refusals.
const (
	CodeShellKeyRequired     = "shell_key_required"      // a team key signed a shell command
	CodeShellKeyWrongCommand = "shell_key_wrong_command" // a shell key signed something else
	CodeShellOff             = "shell_off"               // the client has not opted in (link\shell.yaml)
	CodeShellStateUnreadable = "shell_state_unreadable"  // link\shell.yaml does not parse: off
	CodeShellRefusedDC       = "shell_refused_dc"        // this server is a domain controller
	CodeShellNotSupported    = "not_supported"           // no shell on this build's platform
	CodeShellNoReplyKey      = "shell_no_reply_key"      // the answer would leave in clear
	CodeShellScriptTooLarge  = "shell_script_too_large"
	CodeShellBusy            = "shell_busy" // a provision run (or an upgrade) holds the lock
)

// ShellOutput is the shell's answer, sealed to the envelope's reply key.
type ShellOutput struct {
	Stdout    []byte `json:"stdout"`
	Stderr    []byte `json:"stderr"`
	ExitCode  int    `json:"exitCode"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timedOut,omitempty"`
	Error     string `json:"error,omitempty"` // the script could not be started
}

// ShellExecFunc runs one script in workDir (a folder only SYSTEM and the administrators can write) and returns what
// it printed. It ends the script's whole process tree when ctx ends.
type ShellExecFunc func(ctx context.Context, workDir string, script []byte) ShellOutput

// ShellOptIn is link\shell.yaml: the client's administrator turning the shell on for this server (plan 094 §3 item 2).
// Its own file, never link.yaml: a build before the shell decodes link.yaml strictly, and a key it does not know there
// would refuse every command after a rollback. Absent = off.
type ShellOptIn struct {
	Enabled   bool   `yaml:"enabled"`
	EnabledBy string `yaml:"enabledBy,omitempty"` // the Windows account that turned it on
	EnabledAt string `yaml:"enabledAt,omitempty"` // RFC 3339
}

func (s *Store) shellPath() string    { return filepath.Join(s.Dir, "link", "shell.yaml") }
func (s *Store) shellLogPath() string { return filepath.Join(s.Dir, "link", "shell.log") }
func (s *Store) shellWorkDir() string { return filepath.Join(s.Dir, "link", "shell") }

// LoadShellOptIn reads link\shell.yaml strictly: a key it does not know (a misspelt "enabled") is an error, and the
// caller treats an error as off.
func (s *Store) LoadShellOptIn() (ShellOptIn, error) {
	b, err := os.ReadFile(s.shellPath())
	if errors.Is(err, os.ErrNotExist) {
		return ShellOptIn{}, nil
	}
	if err != nil {
		return ShellOptIn{}, err
	}
	var o ShellOptIn
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&o); err != nil && !errors.Is(err, io.EOF) {
		return ShellOptIn{}, fmt.Errorf("link\\shell.yaml does not parse: %w", err)
	}
	return o, nil
}

// SaveShellOptIn writes link\shell.yaml. Only a person at the server does (digi-erp-connector-link shell on|off): no
// command over the link writes it.
func (s *Store) SaveShellOptIn(o ShellOptIn) error {
	b, err := yaml.Marshal(o)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.shellPath()), 0o700); err != nil {
		return err
	}
	return atomicfile.Write(s.shellPath(), b, 0o600)
}

// checkShell is what a shell command needs before its script is opened, in order: not a domain controller, a platform
// with a shell, the client's opt-in, a reply key, no upgrade pending.
func (l *Link) checkShell(env linkproto.Envelope) string {
	if l.cfg.DomainRole >= 4 { // Win32_ComputerSystem.DomainRole 4 and 5: LocalSystem here is the whole domain
		return CodeShellRefusedDC
	}
	if l.cfg.ShellExec == nil {
		return CodeShellNotSupported
	}
	o, err := l.cfg.Store.LoadShellOptIn()
	if err != nil {
		return CodeShellStateUnreadable
	}
	if !o.Enabled {
		return CodeShellOff
	}
	if env.ReplyKey == "" {
		return CodeShellNoReplyKey
	}
	if busy, _ := runningTicket(l.upgradeDataDir(), l.cfg.Now()); busy {
		return CodeUpgradeRunning
	}
	return ""
}

// lockShell takes provision's lock for the run: a script never runs beside a provision run or an upgrade.
func (l *Link) lockShell() (func(), string) {
	if err := os.MkdirAll(filepath.Dir(ProvisionLockPath(l.cfg.Store.Dir)), 0o700); err != nil {
		return nil, CodeShellBusy
	}
	h, err := filelock.Lock(ProvisionLockPath(l.cfg.Store.Dir))
	if err != nil {
		return nil, CodeShellBusy
	}
	return func() { _ = h.Unlock() }, ""
}

var shellLogMu sync.Mutex

// journal appends one JSON line to link\shell.log. The script's text is never written: its digest and size are.
func (l *Link) journal(line map[string]any) {
	line["at"] = l.cfg.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	shellLogMu.Lock()
	defer shellLogMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(l.cfg.Store.shellLogPath()), 0o700); err != nil {
		l.cfg.Log.Error("link: the shell journal", err)
		return
	}
	f, err := os.OpenFile(l.cfg.Store.shellLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		l.cfg.Log.Error("link: the shell journal", err)
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(append(b, '\n'))
}

// runShell runs the opened script and returns its answer sealed to the envelope's reply key. The journal's start line
// is written before the script runs, and the Event Log line (command, change-set, signer) was written by command().
func (l *Link) runShell(env linkproto.Envelope, keyID string, script []byte) ([]byte, int) {
	reply, err := linkproto.ParseReplyKey(env.ReplyKey)
	if err != nil {
		return nil, -1
	}
	l.journal(map[string]any{"phase": "start", "nonce": env.Nonce, "changeSet": env.ChangeSet, "by": env.By, "key": keyID,
		"scriptSha256": linkproto.SHA256Hex(script), "scriptBytes": len(script)})
	if err := os.MkdirAll(l.cfg.Store.shellWorkDir(), 0o700); err != nil {
		l.cfg.Log.Error("link: the shell's work folder", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShellTimeout)
	out := l.cfg.ShellExec(ctx, l.cfg.Store.shellWorkDir(), script)
	if ctx.Err() == context.DeadlineExceeded {
		out.TimedOut = true
	}
	cancel()
	capOutput(&out)
	plain, _ := json.Marshal(out)
	l.journal(map[string]any{"phase": "end", "nonce": env.Nonce, "exit": out.ExitCode, "truncated": out.Truncated,
		"timedOut": out.TimedOut, "outputSha256": linkproto.SHA256Hex(plain), "outputBytes": len(out.Stdout) + len(out.Stderr)})
	sealed, err := linkproto.Seal(reply, plain)
	if err != nil {
		l.cfg.Log.Error("link: sealing the shell's answer", err)
		return nil, -1
	}
	return sealed, out.ExitCode
}

// capOutput keeps stdout and stderr within ShellOutputCap together, stdout first.
func capOutput(o *ShellOutput) {
	if len(o.Stdout) > ShellOutputCap {
		o.Stdout, o.Truncated = o.Stdout[:ShellOutputCap], true
	}
	if room := ShellOutputCap - len(o.Stdout); len(o.Stderr) > room {
		o.Stderr, o.Truncated = o.Stderr[:room], true
	}
}
