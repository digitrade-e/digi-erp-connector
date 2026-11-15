package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/link"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

// stateSys is fakeSys with the link's service too.
type stateSys struct {
	*fakeSys
	link autostart.ServiceInfo
}

func (s stateSys) Service(name string) (autostart.ServiceInfo, error) {
	if name == link.ServiceName {
		return s.link, nil
	}
	return s.fakeSys.Service(name)
}

func runState(t *testing.T, sys System, env Env, stdin string) (Envelope, *StateResult, []byte) {
	t.Helper()
	env.Sys = sys
	out, _ := Run(context.Background(), env, []string{"state", "--spec", "-"}, strings.NewReader(stdin))
	var e Envelope
	var res StateResult
	e.Result = &res
	if err := json.Unmarshal(out, &e); err != nil {
		t.Fatalf("state printed %s: %v", out, err)
	}
	return e, &res, out
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestState_ABoxInFull(t *testing.T) {
	sys, _, orders := handInstallBox(t)
	dir := sys.dir
	cfg, _ := readConfig(t, dir)
	pickup := filepath.Join(t.TempDir(), "herp")
	_ = os.MkdirAll(pickup, 0o700)
	cfg.SendOrderPickupDir, cfg.SendOrderFormat = pickup, "legacy"
	cfg.SendReceiptDir = filepath.Join(t.TempDir(), "kupain")
	certPEM, keyPEM, err := selfSignedPair([]string{"127.0.0.1", "erp01"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg.TLS.CertFile, cfg.TLS.KeyFile = filepath.Join(dir, "tls", "cert.pem"), filepath.Join(dir, "tls", "key.pem")
	_ = os.MkdirAll(filepath.Join(dir, "tls"), 0o700)
	_ = os.WriteFile(cfg.TLS.CertFile, certPEM, 0o600)
	_ = os.WriteFile(cfg.TLS.KeyFile, keyPEM, 0o600)
	raw := writeConfig(t, dir, cfg)
	// a key the model does not know, and one it always writes taken out
	raw = append(bytes.Replace(raw, []byte("erpUser: \"\"\n"), nil, 1), []byte("bearerToken: old\n")...)
	_ = os.WriteFile(filepath.Join(dir, configFile), raw, 0o600)
	sys.svc.ProcessStart = time.Now().Add(-time.Hour) // config.yaml is newer than the daemon

	// orders: the counter, history copies, an index with each state
	writeJSON(t, filepath.Join(orders, "lastOrderNumber.json"), map[string]int{"lastOrderNumber": 1207})
	for _, n := range []string{"1199", "1205", "notanumber"} {
		_ = os.MkdirAll(filepath.Join(orders, "history", n), 0o700)
	}
	ix := filepath.Join(dir, "orders", "index")
	for i, e := range []struct {
		n     int64
		state string
	}{{1200, "written"}, {1201, "written"}, {1202, "failed"}, {1203, "reserved"}, {1204, "retryable"}} {
		writeJSON(t, filepath.Join(ix, strings.Repeat("a", 63)+string(rune('0'+i))+".json"), map[string]any{"key": "k", "number": e.n, "state": e.state, "requestSha256": "x"})
	}
	_ = os.WriteFile(filepath.Join(ix, "broken.json"), []byte("{"), 0o600)
	// receipts: the store, history, index entries with import states (the request is never read out)
	writeJSON(t, filepath.Join(dir, "receipts", "receiptNumber.json"), map[string]any{"nextReceiptNumber": 5003})
	_ = os.MkdirAll(filepath.Join(dir, "history", "receipts", "5002"), 0o700)
	writeJSON(t, filepath.Join(dir, "receipts", "index", "k1.json"), map[string]any{"number": 5001, "state": "written", "import": "ok", "request": map[string]string{"customerName": "PRIVATE-PERSON"}})
	writeJSON(t, filepath.Join(dir, "receipts", "index", "k2.json"), map[string]any{"number": 5002, "state": "written", "import": "failed"})
	// pickup: two waiting, one imported, one foreign file
	for _, f := range []string{"IMOVEIN_1206.doc", "tmp_imovein_1207.DOC", "IMOVEIN_1100.doc.MASOFON2.BAK", "readme.txt"} {
		_ = os.WriteFile(filepath.Join(pickup, f), []byte("x"), 0o600)
	}
	// link.yaml: a proxy with a credential and a pairing code waiting
	_ = os.WriteFile(filepath.Join(dir, "link.yaml"), []byte("box: bfl-read\npairCode: ABCDE-FGHJK\nadmin: false\nproxy: http://svc:proxy-pass@proxy.b4l.local:3128\n"), 0o600)
	sys.daemon.set(func(d *fakeDaemon) { d.queued, d.running = 2, 1 })

	ss := stateSys{fakeSys: sys, link: autostart.ServiceInfo{Installed: true, State: "running", ProcessStart: time.Now().Add(-2 * time.Hour)}}
	env := testEnv(sys, &fakeDB{})
	env.Now = time.Now
	// TLS on: the fake daemon serves plain HTTP, so the daemon is asked over a client of the test's
	e, res, out := runState(t, ss, env, `{}`)
	if !e.OK {
		t.Fatalf("%s", out)
	}
	for _, secret := range []string{cfg.Auth.Password, cfg.Auth.Secret, testSaPass, "proxy-pass", "ABCDE-FGHJK", "PRIVATE-PERSON", string(keyPEM)} {
		if bytes.Contains(out, []byte(secret)) {
			t.Errorf("%q is in the answer", secret)
		}
	}
	if res.Services.Link.State != "running" || res.Services.Daemon.State != "running" || res.Services.Daemon.ProcessStart.IsZero() {
		t.Errorf("services %+v", res.Services)
	}
	c := res.Config
	if c == nil || !c.ConfigNewerThanStart || !slices.Contains(c.MissingKeys, "erpUser") || !slices.Equal(c.UnknownKeys, []string{"bearerToken"}) || c.ERP != "hasavshevet" {
		t.Errorf("config %+v", c)
	}
	o := res.Orders
	if !o.Configured || o.LastOrderNumber != 1207 || !o.CounterPresent || o.HighestHistory != 1205 || o.Index.Entries != 6 ||
		o.Index.ByState["written"].Count != 2 || !slices.Equal(o.Index.ByState["reserved"].Numbers, []int64{1203}) || len(o.Index.Unreadable) != 1 {
		t.Errorf("orders %+v index %+v", o, o.Index)
	}
	r := res.Receipts
	if !r.Configured || r.NextReceiptNumber != 5003 || !r.StorePresent || r.HighestHistory != 5002 || r.Index.ByState["written"].Count != 2 ||
		!slices.Equal(r.ByImport["failed"].Numbers, []int64{5002}) {
		t.Errorf("receipts %+v", r)
	}
	if p := res.Pickup; p == nil || p.Pending.Count != 2 || !slices.Equal(p.Pending.Numbers, []int64{1206, 1207}) || p.Imported != 1 {
		t.Errorf("pickup %+v", res.Pickup)
	}
	if tl := res.TLS; tl == nil || tl.DaysLeft < 3000 || !slices.Contains(tl.IPs, "127.0.0.1") || !slices.Contains(tl.DNSNames, "erp01") {
		t.Errorf("tls %+v", res.TLS)
	}
	if l := res.Link; !l.Present || l.Box != "bfl-read" || !l.PairCodeWaiting || l.Admin || l.Proxy != "http://proxy.b4l.local:3128" || l.Relay != link.DefaultRelay {
		t.Errorf("link %+v", l)
	}
	if res.Disk.TotalBytes == 0 || res.Disk.Path != dir {
		t.Errorf("disk %+v", res.Disk)
	}
}

func TestState_AsksTheRunningDaemon(t *testing.T) {
	sys, _, _ := handInstallBox(t)
	sys.daemon.set(func(d *fakeDaemon) { d.queued, d.running, d.health = 3, 1, 503 })
	_, res, _ := runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), ``)
	d := res.Daemon
	if !d.Asked || d.Version != "v1.0.99" || d.Health != 503 || d.Queue == nil || d.Queue.Queued != 3 || d.Queue.Running != 1 || d.Error != "" {
		t.Fatalf("daemon %+v", d)
	}
	// a credential the daemon refuses: it runs another config
	sys.daemon.set(func(d *fakeDaemon) { d.pass = "another" })
	_, res, _ = runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), `{}`)
	if !res.Daemon.Asked || res.Daemon.Queue != nil || !strings.Contains(res.Daemon.Error, "refused the credential") {
		t.Fatalf("daemon %+v", res.Daemon)
	}
	// stopped: nothing to ask
	sys.svc.State = "stopped"
	_, res, _ = runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), `{}`)
	if res.Daemon.Asked || res.Config.ConfigNewerThanStart {
		t.Fatalf("a stopped daemon: %+v", res.Daemon)
	}
}

func TestState_AnUnconfiguredBox(t *testing.T) {
	d := newFakeDaemon(t)
	sys := newFakeSys(t, d)
	sys.svc = autostart.ServiceInfo{Installed: true, State: "stopped"}
	e, res, out := runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), `{}`)
	if !e.OK || res.Config != nil || res.Orders.Configured || res.Receipts.StorePresent || res.Link.Present || !res.Link.Admin || len(res.Errors) != 1 {
		t.Fatalf("%s", out)
	}
	e, _, _ = runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), `{"verbose":true}`)
	if e.OK || e.Refusal.Code != CodeSpecInvalid {
		t.Fatalf("an unknown field: %+v", e)
	}
}

func TestConfigKeys(t *testing.T) {
	missing, unknown := configKeys([]byte("erp: hasavshevet\napiListen: 0.0.0.0:8082\ndb:\n  host: x\n  pasword: typo\nlegacyCompat: {}\n"))
	for _, want := range []string{"debug", "db.port", "auth", "queries"} {
		if !slices.Contains(missing, want) {
			t.Errorf("%s not missing: %v", want, missing)
		}
	}
	if slices.Contains(missing, "sendOrderPickupDir") || slices.Contains(missing, "erp") {
		t.Errorf("optional or present keys listed: %v", missing)
	}
	if !slices.Equal(unknown, []string{"db.pasword", "legacyCompat"}) {
		t.Errorf("unknown %v", unknown)
	}
}
