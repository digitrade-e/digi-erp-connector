package link

import (
	"context"
	"crypto/tls"
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

func (l *Link) handleStream(st *yamux.Stream) {
	defer func() { _ = st.Close() }()
	_ = st.SetReadDeadline(time.Now().Add(10 * time.Second))
	var kind [1]byte
	if _, err := io.ReadFull(st, kind[:]); err != nil {
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	// Streams the relay may hold at once: two commands, 64 data calls; more are closed at once (review 2026-11-15:
	// 500 idle command streams held 500 MiB on the client's SQL Server host).
	slots := l.dataSlots
	if kind[0] == linkproto.StreamCommand {
		slots = l.cmdSlots
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		return
	}
	switch kind[0] {
	case linkproto.StreamData:
		l.pipe(st)
	case linkproto.StreamCommand:
		l.command(st)
	}
}

// pipe connects a data stream to the daemon's local API, byte for byte.
func (l *Link) pipe(st net.Conn) {
	api, err := net.DialTimeout("tcp", l.cfg.APIAddr, 5*time.Second)
	if err != nil {
		return // the relay answers 504 forwarded-no-answer, or 503 if nothing was written
	}
	if l.cfg.APITLS {
		// Only the daemon's own certificate, read from its certFile, is trusted — never "skip verification", even on
		// loopback (CodeQL go/disabled-certificate-check). Unreadable, the data stream is not served.
		conf, err := APITLSConfig(l.cfg.APICert)
		if err != nil {
			_ = api.Close()
			l.cfg.Log.Error("link: the connector's TLS certificate", err)
			return
		}
		api = tls.Client(api, conf)
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
	env, keyID, digest, refusal := l.verify(state, envFrame)
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
			if env.Sealed {
				plain, err := linkproto.Open(l.cfg.Keys, stdin)
				if err != nil {
					refusal = "sealed_input_refused"
				}
				stdin = plain
			}
		}
		if refusal == "" {
			l.cfg.Log.Info(fmt.Sprintf("link: command %q change-set %q by %q (key %s)", env.Command, env.ChangeSet, env.By, keyID))
			stdout, ans.Exit = l.run(env, stdin)
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
func (l *Link) verify(state State, envFrame []byte) (env linkproto.Envelope, keyID, digest, refusal string) {
	var s linkproto.Signed
	if err := json.Unmarshal(envFrame, &s); err != nil {
		return env, "", "", linkproto.CodeMalformed
	}
	if state.Box == "" {
		return env, "", "", linkproto.CodeBoxUnknown
	}
	minEpoch, err := l.cfg.Store.MinEpochStrict(CompiledMinEpoch)
	if err != nil {
		return env, "", "", "state_unreadable"
	}
	env, key, err := l.cfg.Team.VerifyEnvelope(s, l.cfg.Now(), state.Box, minEpoch)
	if err != nil {
		return linkproto.Envelope{}, "", "", refusalCode(err)
	}
	digest = linkproto.EnvelopeDigest(s)
	if err := l.cfg.Store.UseNonce(env.Nonce, time.Unix(env.ExpiresAt, 0), l.cfg.Now()); err != nil {
		return env, key.ID, digest, refusalCode(err) // replayed_nonce: this envelope reached the box before
	}
	if err := l.cfg.Store.RaiseMinEpoch(key.Epoch, CompiledMinEpoch); err != nil {
		l.cfg.Log.Error("link: recording the key epoch", err)
	}
	if !state.AdminOn() {
		return env, key.ID, digest, "admin_off" // the client's off switch (link.yaml admin: false)
	}
	if state.UnknownKeys() {
		return env, key.ID, digest, "state_unreadable" // link.yaml names a key we do not know: perhaps a misspelt off switch
	}
	return env, key.ID, digest, ""
}

// checkInput is what needs the stdin: its digest, then the command's argv.
func (l *Link) checkInput(env linkproto.Envelope, stdin []byte) string {
	if linkproto.SHA256Hex(stdin) != env.StdinSHA256 {
		return linkproto.CodeBodyMismatch
	}
	if _, err := l.argv(env); err != nil {
		return refusalCode(err)
	}
	if MustBeSealed(env.Command) && !env.Sealed {
		return CodeInputNotSealed // plan 086: a secret never crosses the relay in clear
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
	if env.Command == CommandHashes {
		if len(env.Args) != 0 {
			return nil, &linkproto.Refusal{Code: linkproto.CodeMalformed}
		}
		return nil, nil
	}
	return Argv(env)
}

func (l *Link) run(env linkproto.Envelope, stdin []byte) ([]byte, int) {
	if env.Command == CommandHashes {
		return Hashes(l.cfg.InstallDir), 0
	}
	argv, _ := Argv(env) // checked already
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
