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
		// The daemon's own self-signed pair, on loopback: the identity is the address.
		t := tls.Client(api, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec // loopback only
		api = t
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

// command runs one signed envelope. Everything is checked before anything
// runs, and every refusal is itself a signed answer, so digi-mcp can tell the
// box refused from the relay failed.
func (l *Link) command(st net.Conn) {
	_ = st.SetDeadline(time.Now().Add(CommandDeadline + time.Minute))
	envFrame, err := linkproto.ReadFrame(st, linkproto.MaxControlFrame)
	if err != nil {
		return
	}
	stdin, err := linkproto.ReadFrame(st, linkproto.MaxPayload)
	if err != nil {
		return
	}
	state, _ := l.cfg.Store.LoadState()
	ans := linkproto.Answer{Box: state.Box, Started: l.cfg.Now().Unix()}
	var stdout []byte

	env, keyID, refusal := l.check(state, envFrame, stdin)
	ans.Nonce = env.Nonce
	switch {
	case refusal != "":
		ans.Refusal = refusal
		l.cfg.Log.Warn(fmt.Sprintf("link: command refused (%s): %q change-set %q by %q", refusal, env.Command, env.ChangeSet, env.By))
	default:
		if env.Sealed {
			plain, err := linkproto.Open(l.cfg.Keys, stdin)
			if err != nil {
				ans.Refusal = "sealed_input_refused"
				break
			}
			stdin = plain
		}
		l.cfg.Log.Info(fmt.Sprintf("link: command %q change-set %q by %q (key %s)", env.Command, env.ChangeSet, env.By, keyID))
		stdout, ans.Exit = l.run(env, stdin)
		l.cfg.Log.Info(fmt.Sprintf("link: command %q change-set %q ended, exit %d, %d bytes", env.Command, env.ChangeSet, ans.Exit, len(stdout)))
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

// check runs every refusal in order; it returns the envelope (for the log
// and the answer's nonce), the key that signed it, and a refusal code or "".
func (l *Link) check(state State, envFrame, stdin []byte) (linkproto.Envelope, string, string) {
	var s linkproto.Signed
	if err := json.Unmarshal(envFrame, &s); err != nil {
		return linkproto.Envelope{}, "", linkproto.CodeMalformed
	}
	if !state.AdminOn() {
		// The client's off switch (link.yaml admin: false). Read the envelope only to log what was refused.
		var e linkproto.Envelope
		_ = json.Unmarshal(s.Body, &e)
		return e, "", "admin_off"
	}
	if state.Box == "" {
		return linkproto.Envelope{}, "", linkproto.CodeBoxUnknown
	}
	minEpoch := l.cfg.Store.MinEpoch(CompiledMinEpoch)
	env, key, err := l.cfg.Team.VerifyEnvelope(s, l.cfg.Now(), state.Box, minEpoch)
	if err != nil {
		var e linkproto.Envelope
		_ = json.Unmarshal(s.Body, &e) // for the log only: nothing from it is trusted
		return e, "", refusalCode(err)
	}
	if linkproto.SHA256Hex(stdin) != env.StdinSHA256 {
		return env, key.ID, linkproto.CodeBodyMismatch
	}
	if _, err := l.argv(env); err != nil {
		return env, key.ID, refusalCode(err)
	}
	if err := l.cfg.Store.UseNonce(env.Nonce, time.Unix(env.ExpiresAt, 0), l.cfg.Now()); err != nil {
		return env, key.ID, refusalCode(err)
	}
	if err := l.cfg.Store.RaiseMinEpoch(key.Epoch, CompiledMinEpoch); err != nil {
		l.cfg.Log.Error("link: recording the key epoch", err)
	}
	return env, key.ID, ""
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
