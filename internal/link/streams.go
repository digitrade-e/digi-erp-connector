package link

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// CommandDeadline bounds one command: provision's worst case (a stop, a slow
// start, the proof and a rollback) is about 25 minutes.
const CommandDeadline = 30 * time.Minute

func (l *Link) handleStream(c *conn, st *yamux.Stream) {
	defer func() { _ = st.Close() }()
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	var kind [1]byte
	if _, err := io.ReadFull(st, kind[:]); err != nil {
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	// Streams the relay may hold at once: two commands, 64 data calls, one policy delivery (plan 088); more are closed
	// at once (review 2026-11-15: 500 idle command streams held 500 MiB on the client's SQL Server host).
	slots := l.dataSlots
	switch kind[0] {
	case linkproto.StreamCommand:
		slots = l.cmdSlots
	case linkproto.StreamPolicy:
		slots = l.policySlots
	}
	select {
	case slots <- struct{}{}:
	default:
		return
	}
	released := false
	release := func() { // the policy stream lets its slot go before it reports: the relay sends its next delivery then
		if !released {
			released = true
			<-slots
		}
	}
	defer release()
	switch kind[0] {
	case linkproto.StreamData:
		l.pipe(st)
	case linkproto.StreamCommand:
		l.command(st)
	case linkproto.StreamPolicy:
		l.policyStream(c, st, release)
	}
}

// pipe connects a data stream to the daemon's local API, byte for byte. The address is read per stream (plan 086
// C5: daemonTargets), so an apply that moves the port or turns TLS on does not strand the data path.
func (l *Link) pipe(st net.Conn) {
	api := l.dialTargets()
	if api == nil {
		return // the relay answers 504 forwarded-no-answer, or 503 if nothing was written
	}
	defer func() { _ = api.Close() }()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(api, st)
		if cw, ok := api.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()
	_, _ = io.Copy(st, api)
	_ = st.Close()
	wg.Wait()
}

// dialTargets connects to the first target that answers (review B1): an unusable certificate ends the search — no other
// address, never plaintext — and a target whose far end is not the connector's process is skipped. The current
// target answering retires the previous one (DaemonWatch.Answered). nil when none answered.
func (l *Link) dialTargets() net.Conn {
	var owner func(net.Conn) error
	if l.cfg.Daemon != nil {
		owner = l.cfg.Daemon.Owner
	}
	for i, t := range l.daemonTargets() {
		api, err := dialDaemon(t, owner)
		switch {
		case err == nil:
			if i == 0 && l.cfg.Daemon != nil {
				l.cfg.Daemon.Answered(t)
			}
			return api
		case errors.Is(err, errDaemonCert):
			l.logEvery("cert", func() { l.cfg.Log.Error("link: the connector's TLS certificate; no data stream is served", err) })
			return nil
		case errors.Is(err, errDaemonOwner):
			l.logEvery("owner", func() { l.cfg.Log.Warn(fmt.Sprintf("link: %s refused for data streams: %v", t.Addr, err)) })
		}
	}
	return nil
}

// logEvery runs log at most once a minute per kind: a refusal repeated on every data stream (a squatter answering
// for minutes) must not flood link.log and the Event Log.
func (l *Link) logEvery(kind string, log func()) {
	l.logMu.Lock()
	now := l.cfg.Now()
	if last, ok := l.loggedAt[kind]; ok && now.Sub(last) < time.Minute {
		l.logMu.Unlock()
		return
	}
	if l.loggedAt == nil {
		l.loggedAt = map[string]time.Time{}
	}
	l.loggedAt[kind] = now
	l.logMu.Unlock()
	log()
}

// frameWait bounds the wait for each frame of a command: a relay that announces a frame and stalls holds the stream no
// longer than this (review 2026-11-15).
const frameWait = 30 * time.Second

// command runs one signed envelope. The envelope is verified before its stdin
// is read, and its nonce spent at once: an answer to a verified envelope names
// its nonce and its digest (linkproto.EnvelopeDigest), so a relay cannot pass
// off the answer to one delivery as another's. A refusal before verification
// names neither, and says nothing about any command (review 2026-11-15).
func (l *Link) command(st net.Conn) {
	_ = st.SetDeadline(time.Now().Add(CommandDeadline + time.Minute))
	_ = st.SetReadDeadline(time.Now().Add(frameWait))
	envFrame, err := linkproto.ReadFrame(st, linkproto.MaxControlFrame)
	if err != nil {
		return
	}
	state, _ := l.cfg.Store.LoadState()
	ans := linkproto.Answer{Box: state.Box, Started: l.cfg.Now().Unix()}
	var stdout []byte
	env, keyID, cert, digest, refusal := l.verify(state, envFrame)
	if cert != nil {
		env.By = "installer " + cert.Name // the certificate's name, which a team key signed; never the envelope's own by
	}
	if digest != "" {
		ans.Nonce, ans.Env = env.Nonce, digest
	}
	if refusal == "" {
		_ = st.SetReadDeadline(time.Now().Add(frameWait))
		stdin, err := linkproto.ReadFrame(st, linkproto.MaxPayload)
		if err != nil {
			l.cfg.Log.Warn(fmt.Sprintf("link: command %q change-set %q: its input did not arrive; not run", env.Command, env.ChangeSet))
			return
		}
		_ = st.SetDeadline(time.Now().Add(CommandDeadline + time.Minute))
		refusal = l.checkInput(env, stdin)
		if refusal == "" {
			refusal = l.checkInstaller(cert, env, stdin)
		}
		if refusal == "" {
			refusal = l.checkUpgrade(env)
		}
		if refusal == "" {
			if env.Sealed {
				plain, err := linkproto.Open(l.cfg.Keys, stdin)
				if err != nil {
					refusal = "sealed_input_refused"
				}
				stdin = plain
			}
		}
		// An installer's apply marks the first setup done BEFORE it runs, and only a failed one takes the mark back
		// (plan 090 review): a mark that cannot be written stops the apply, and one that cannot be taken back leaves the
		// box closed to installers, never open.
		installerApply := cert != nil && env.Command == "apply"
		if refusal == "" && installerApply {
			if err := l.cfg.Store.MarkInstallerSetupDone(); err != nil {
				l.cfg.Log.Error("link: recording the installer's first setup before it runs", err)
				refusal = "state_unreadable"
			}
		}
		if refusal == "" {
			l.cfg.Log.Info(fmt.Sprintf("link: command %q change-set %q by %q (key %s)", env.Command, env.ChangeSet, env.By, keyID))
			stdout, ans.Exit = l.run(env, stdin, cert != nil)
			if installerApply && ans.Exit != 0 {
				if err := l.cfg.Store.ReopenInstallerSetup(); err != nil {
					l.cfg.Log.Error("link: the installer's first setup failed, and the box stays closed to installers", err)
				}
			}
			l.cfg.Log.Info(fmt.Sprintf("link: command %q change-set %q ended, exit %d, %d bytes", env.Command, env.ChangeSet, ans.Exit, len(stdout)))
		}
	}
	if refusal != "" {
		ans.Refusal = refusal
		if digest != "" {
			l.cfg.Log.Warn(fmt.Sprintf("link: command %q change-set %q by %q refused: %s", env.Command, env.ChangeSet, env.By, refusal))
		} else {
			l.refusedUnverified(refusal)
		}
	}
	ans.Finished = l.cfg.Now().Unix()
	ans.StdoutSHA256, ans.StdoutLen = linkproto.SHA256Hex(stdout), int64(len(stdout))
	signed, err := linkproto.SignAnswer(l.cfg.Keys, ans)
	if err != nil {
		return
	}
	raw, _ := json.Marshal(signed)
	if linkproto.WriteFrame(st, stdout) == nil {
		_ = linkproto.WriteFrame(st, raw)
	}
}

// verify checks the envelope alone, in order, and spends its nonce. digest is
// set once the signature verified: refusals from there on are about this
// envelope. Nothing from an unverified envelope is returned or logged.
//
// An envelope carrying a certificate is an installer's (plan 090 D1): the
// certificate must verify with the compiled-in team keys, and the envelope with
// the certified key. cert is then set, keyID is the installer's id, and the
// caller applies checkInstaller. Without one it is a team key's, as before.
func (l *Link) verify(state State, envFrame []byte) (env linkproto.Envelope, keyID string, cert *linkproto.InstallerCert, digest, refusal string) {
	var s linkproto.Signed
	if err := json.Unmarshal(envFrame, &s); err != nil {
		return env, "", nil, "", linkproto.CodeMalformed
	}
	if state.Box == "" {
		return env, "", nil, "", linkproto.CodeBoxUnknown
	}
	minEpoch, err := l.cfg.Store.MinEpochStrict(CompiledMinEpoch)
	if err != nil {
		return env, "", nil, "", "state_unreadable"
	}
	var key linkproto.TeamKey
	if s.Cert != nil {
		var c linkproto.InstallerCert
		env, c, key, err = l.cfg.Team.VerifyInstallerEnvelope(s, l.cfg.Now(), state.Box, minEpoch)
		if err == nil {
			cert, keyID = &c, c.ID
		}
	} else {
		env, key, err = l.cfg.Team.VerifyEnvelope(s, l.cfg.Now(), state.Box, minEpoch)
		keyID = key.ID
	}
	if err != nil {
		return linkproto.Envelope{}, "", nil, "", refusalCode(err)
	}
	digest = linkproto.EnvelopeDigest(s)
	if err := l.cfg.Store.UseNonce(env.Nonce, time.Unix(env.ExpiresAt, 0), l.cfg.Now()); err != nil {
		return env, keyID, cert, digest, refusalCode(err) // replayed_nonce: this envelope reached the box before
	}
	if err := l.cfg.Store.RaiseMinEpoch(key.Epoch, CompiledMinEpoch); err != nil { // an installer's: its certifier's
		l.cfg.Log.Error("link: recording the key epoch", err)
	}
	if cert != nil {
		if r := l.installerBinding(cert); r != "" {
			return env, keyID, cert, digest, r // before admin_off and the rest: nothing about another's box
		}
	}
	if !state.AdminOn() {
		return env, keyID, cert, digest, "admin_off" // the client's off switch (link.yaml admin: false)
	}
	if state.UnknownKeys() {
		return env, keyID, cert, digest, "state_unreadable" // link.yaml names a key we do not know: perhaps a misspelt off switch
	}
	return env, keyID, cert, digest, ""
}

// checkInput is what needs the stdin: its digest, then the command's argv.
func (l *Link) checkInput(env linkproto.Envelope, stdin []byte) string {
	if linkproto.SHA256Hex(stdin) != env.StdinSHA256 {
		return linkproto.CodeBodyMismatch
	}
	if _, err := l.argv(env); err != nil {
		return refusalCode(err)
	}
	if !env.Sealed && MustBeSealedInput(env.Command, stdin) {
		// plan 086, review m5: refused, never run — a sender's mistake; the stdin has crossed the relay already
		return CodeInputNotSealed
	}
	return ""
}

// checkUpgrade refuses a provision command while an upgrade is pending (review M1): its setup stops the services and
// replaces provision.exe, which a running command would hold open. The link's own commands still answer
// (upgrade-result above all).
func (l *Link) checkUpgrade(env linkproto.Envelope) string {
	if _, self := selfCommands[env.Command]; self {
		return ""
	}
	if busy, _ := runningTicket(l.upgradeDataDir(), l.cfg.Now()); busy {
		return CodeUpgradeRunning
	}
	return ""
}

// refusedUnverified logs refusals of envelopes that did not verify as one
// summary a minute: a relay (or anyone it relays) cannot flood link.log and the
// client's Application log, or choose the text written there.
func (l *Link) refusedUnverified(code string) {
	l.unverMu.Lock()
	defer l.unverMu.Unlock()
	l.unverCount++
	if now := l.cfg.Now(); now.Sub(l.unverAt) >= time.Minute {
		l.cfg.Log.Warn(fmt.Sprintf("link: %d envelope(s) refused before verification in the last minute (last: %s)", l.unverCount, code))
		l.unverAt, l.unverCount = now, 0
	}
}

func (l *Link) argv(env linkproto.Envelope) ([]string, error) {
	if _, self := selfCommands[env.Command]; self {
		if len(env.Args) != 0 {
			return nil, &linkproto.Refusal{Code: linkproto.CodeMalformed}
		}
		return nil, nil
	}
	return Argv(env)
}

func (l *Link) run(env linkproto.Envelope, stdin []byte, installer bool) ([]byte, int) {
	if self, ok := selfCommands[env.Command]; ok {
		return self(l, env, stdin)
	}
	argv, _ := Argv(env) // checked already
	if installer && installerWrites[env.Command] {
		argv = append(argv, "--installer") // provision's installer rules (plan 090 D3); never from the envelope
	}
	ctx, cancel := context.WithTimeout(context.Background(), CommandDeadline)
	defer cancel()
	out, exit, err := l.cfg.Runner.Run(ctx, argv, stdin, linkproto.MaxPayload)
	if err != nil {
		l.cfg.Log.Error(fmt.Sprintf("link: running %q", env.Command), err)
		return nil, -1
	}
	return out, exit
}

func refusalCode(err error) string {
	var r *linkproto.Refusal
	if errors.As(err, &r) {
		return r.Code
	}
	return "internal"
}
