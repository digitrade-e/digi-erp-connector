package link

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// TestAgainstTheRealRelay runs this link against the relay binary of
// digitrade-e/connector-link (plan 083 P1), with an in-memory registry:
//
//	LINK_E2E_RELAY=/path/to/relay go test -run TestAgainstTheRealRelay -v ./internal/link/
//
// Skipped otherwise (CI has no relay binary).
func TestAgainstTheRealRelay(t *testing.T) {
	relayBin := os.Getenv("LINK_E2E_RELAY")
	if relayBin == "" {
		t.Skip("set LINK_E2E_RELAY to a built connector-link relay")
	}
	teamPub, teamPriv, _ := ed25519.GenerateKey(rand.Reader)
	pub, intl := freePort(t), freePort(t)
	cmd := exec.Command(relayBin)
	cmd.Env = append(os.Environ(),
		"LINK_RELAY_ID=e2e", "LINK_POD_ADDR=127.0.0.1:"+intl, "LINK_LISTEN=127.0.0.1:"+pub, "LINK_INTERNAL_LISTEN=127.0.0.1:"+intl,
		"LINK_INTERNAL_TOKEN="+strings.Repeat("t", 40), "LINK_TEAM_KEYS=team-1:1:"+base64.StdEncoding.EncodeToString(teamPub),
		"LINK_ALLOW_C=127.0.0.0/8", "LINK_DATABASE_URL=", "LINK_DEV_MEMORY=1")
	var relayLog bytes.Buffer
	cmd.Stdout, cmd.Stderr = &relayLog, &relayLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	base := "http://127.0.0.1:" + pub
	waitHTTP(t, base+"/healthz")

	admin := func(op, path string, body any) []byte {
		b, _ := json.Marshal(body)
		now := time.Now()
		s, _ := linkproto.SignAdmin(teamPriv, "team-1", linkproto.AdminRequest{Op: op, Nonce: linkproto.RandomNonce(16),
			IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), By: "e2e"}, b)
		raw, _ := json.Marshal(s)
		req, _ := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
		req.Header.Set("X-Link-Admin", base64.StdEncoding.EncodeToString(raw))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s\nrelay log:\n%s", path, resp.StatusCode, out, relayLog.String())
		}
		return out
	}

	// 1. digi-mcp asks for a pairing code.
	var pc struct{ Code string }
	_ = json.Unmarshal(admin(linkproto.OpPairCreate, "/admin/pair", map[string]string{"tenant": "bflstore", "alias": "bfl-read"}), &pc)

	// 2. The box redeems it (what setup.exe + the service do).
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"path":%q,"auth":%q}`, r.URL.Path, r.Header.Get("Authorization"))
	}))
	defer api.Close()
	store := &Store{Dir: t.TempDir(), Secrets: &memSecrets{}}
	keys, _, _ := store.Keys(func() (linkproto.BoxKeys, error) { return linkproto.NewBoxKeys(rand.Reader) })
	_ = store.SaveState(State{Relay: "ws://127.0.0.1:" + pub + "/link", PairCode: pc.Code})
	runner := &fakeRunner{}
	log := &testLog{}
	l := New(Config{Store: store, Keys: keys, Team: linkproto.Keyring{{ID: "team-1", Epoch: 1, Pub: teamPub}},
		Build: "e2e", Host: "SQLSRV", Domain: "b4l.local", DomainRole: 3, APIAddr: api.Listener.Addr().String(),
		Runner: runner, Log: log, PendingRetry: 200 * time.Millisecond, LiveEvery: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// 3. The human compares the fingerprint and confirms.
	var box string
	deadline := time.Now().Add(10 * time.Second)
	for box == "" && time.Now().Before(deadline) {
		st, _ := store.LoadState()
		box = st.Box
		time.Sleep(50 * time.Millisecond)
	}
	if box != "bfl-read" {
		t.Fatalf("the box id after redemption: %q", box)
	}
	list := string(admin(linkproto.OpBoxesList, "/admin/boxes", map[string]string{}))
	if !strings.Contains(list, keys.Public().Fingerprint()) || !strings.Contains(list, `"status":"pending"`) {
		t.Fatalf("boxes.list: %s", list)
	}
	admin(linkproto.OpPairConfirm, "/admin/pair/confirm", map[string]string{"box": box, "fingerprint": keys.Public().Fingerprint()})

	// 4. The data path: a caller → the relay → the link → the local API.
	deadline = time.Now().Add(10 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/c/"+box+"/api/ping", nil)
		req.Header.Set("Authorization", "Bearer abc")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				body = string(b)
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if body != `{"ok":true,"path":"/api/ping","auth":"Bearer abc"}` {
		t.Fatalf("the data path: %q\nrelay log:\n%s", body, relayLog.String())
	}

	// 5. A signed command through the relay, answered and signed by the box.
	stdin := []byte(`{}`)
	e := linkproto.Envelope{Box: box, Command: "discover", StdinSHA256: linkproto.SHA256Hex(stdin), Nonce: linkproto.RandomNonce(16),
		IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(), By: "e2e"}
	s, _ := linkproto.SignEnvelope(teamPriv, "team-1", e)
	raw, _ := json.Marshal(s)
	var req bytes.Buffer
	_ = linkproto.WriteFrame(&req, raw)
	_ = linkproto.WriteFrame(&req, stdin)
	resp, err := http.Post(base+"/admin/run/"+box, "application/octet-stream", &req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("run: %d %s", resp.StatusCode, out)
	}
	rd := bytes.NewReader(out)
	stdout, _ := linkproto.ReadFrame(rd, linkproto.MaxPayload)
	ansRaw, _ := linkproto.ReadFrame(rd, linkproto.MaxControlFrame)
	var sa linkproto.SignedAnswer
	_ = json.Unmarshal(ansRaw, &sa)
	a, err := linkproto.VerifyAnswer(keys.Public(), sa, box, e.Nonce, linkproto.EnvelopeDigest(s), stdout)
	if err != nil || a.Refusal != "" || runner.count() != 1 {
		t.Fatalf("the command: %v %+v, %d runs", err, a, runner.count())
	}
	t.Logf("paired %s, data path %s, command %s → %s", box, body, e.Command, stdout)
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	_, p, _ := net.SplitHostPort(l.Addr().String())
	return p
}

func waitHTTP(t *testing.T, u string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(u); err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never answered", u)
}
