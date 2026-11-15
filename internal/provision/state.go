package provision

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/digitrade-e/digi-erp-connector/internal/buildinfo"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/link"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/autostart"
)

// The state command (plan 086 C4): what an operator looks at when something breaks, read on the box and answered as
// one JSON object — the two services, the running daemon, config.yaml against the service start and the model, the
// order and receipt numbers and indexes, the pickup folder, the TLS certificate, link.yaml and the disk. It writes
// nothing: no file, no DACL, no database (the daemon is asked through its own read-only routes). A part that cannot be
// read says so in its own error and the rest goes on. No secret is in it: config.yaml's are only "set"/"missing" in
// discover, and here not even that; link.yaml's proxy loses its user and password.

// StateInput is `state --spec -`'s stdin: nothing yet ({}).
type StateInput struct{}

// StateResult is what state answers.
type StateResult struct {
	Version  string        `json:"version"` // this provision's build: the release installed with the services
	Services StateServices `json:"services"`
	Daemon   StateDaemon   `json:"daemon"`
	Config   *StateConfig  `json:"config"`
	Orders   StateOrders   `json:"orders"`
	Receipts StateReceipts `json:"receipts"`
	Pickup   *StatePickup  `json:"pickup,omitempty"`
	TLS      *StateTLS     `json:"tls,omitempty"`
	Link     StateLink     `json:"link"`
	Disk     StateDisk     `json:"disk"`
	Errors   []string      `json:"errors,omitempty"`
}

// StateServices are both Windows services.
type StateServices struct {
	Daemon autostart.ServiceInfo `json:"daemon"`
	Link   autostart.ServiceInfo `json:"link"`
}

// StateDaemon is the running daemon as its own API answers, with the credential in config.yaml.
type StateDaemon struct {
	Asked   bool   `json:"asked"` // false when the service is not running or there is no config
	Version string `json:"version,omitempty"`
	// Health is /api/health's status: 200 the database answers, 503 it does not.
	Health int `json:"health,omitempty"`
	// Queue is GET /api/sendOrder: accepted orders not written yet (a restart loses them).
	Queue *StateQueue `json:"queue,omitempty"`
	Error string      `json:"error,omitempty"`
}

// StateQueue is the daemon's in-memory order queue.
type StateQueue struct {
	Queued  int `json:"queued"`
	Running int `json:"running"`
}

// StateConfig is config.yaml as a file.
type StateConfig struct {
	Modified time.Time `json:"modified"`
	SHA256   string    `json:"sha256"`
	// ConfigNewerThanStart: written after the daemon's process started — it runs an older config.
	ConfigNewerThanStart bool   `json:"configNewerThanStart"`
	ERP                  string `json:"erp"`
	APIListen            string `json:"apiListen"`
	// MissingKeys are keys the model always writes that the file lacks (a hand-written or old file): their defaults
	// apply. UnknownKeys are keys in the file the model does not know (left from an older build, or misspelt).
	MissingKeys []string `json:"missingKeys"`
	UnknownKeys []string `json:"unknownKeys"`
	Error       string   `json:"error,omitempty"`
}

// StateNumbers is one state's count and up to maxStateNumbers of its numbers, the highest.
type StateNumbers struct {
	Count   int     `json:"count"`
	Numbers []int64 `json:"numbers"`
}

const maxStateNumbers = 50

// StateIndex counts an index's entries by state.
type StateIndex struct {
	Entries    int                      `json:"entries"`
	ByState    map[string]*StateNumbers `json:"byState"`
	Unreadable []string                 `json:"unreadable,omitempty"` // file names, at most maxStateNumbers
	Error      string                   `json:"error,omitempty"`
}

// StateOrders are the order numbers (Hasavshevet).
type StateOrders struct {
	Configured bool `json:"configured"`
	// LastOrderNumber is sendOrderDir\lastOrderNumber.json (0 when absent: the next order is 1).
	LastOrderNumber int64  `json:"lastOrderNumber"`
	CounterPresent  bool   `json:"counterPresent"`
	CounterError    string `json:"counterError,omitempty"`
	// HighestHistory is the highest sendOrderDir\history\<n>: an order written with that number.
	HighestHistory int64       `json:"highestHistory"`
	HistoryError   string      `json:"historyError,omitempty"`
	Index          *StateIndex `json:"index"`
}

// StateReceipts are the receipt numbers (Hasavshevet KUPAIN).
type StateReceipts struct {
	Configured bool `json:"configured"`
	// NextReceiptNumber is the store's (receipts\receiptNumber.json); absent, every receipt answers 503 until it is
	// seeded.
	NextReceiptNumber int64       `json:"nextReceiptNumber"`
	StorePresent      bool        `json:"storePresent"`
	StoreError        string      `json:"storeError,omitempty"`
	HighestHistory    int64       `json:"highestHistory"`
	HistoryError      string      `json:"historyError,omitempty"`
	Index             *StateIndex `json:"index"`
	// ByImport counts the index's entries by what the receipt importer did (model b).
	ByImport map[string]*StateNumbers `json:"byImport,omitempty"`
}

// StatePickup is the pickup folder of model (a) (DIG-1297).
type StatePickup struct {
	Dir string `json:"dir"`
	// Pending are order files waiting for the ERP's scheduler (IMOVEIN_<n>.doc, TMP_IMOVEIN_<n>.doc).
	Pending StateNumbers `json:"pending"`
	// Imported are the files the scheduler renamed (*.MASOFON2.BAK).
	Imported int    `json:"imported"`
	Error    string `json:"error,omitempty"`
}

// StateTLS is the daemon's certificate.
type StateTLS struct {
	CertFile  string    `json:"certFile"`
	Subject   string    `json:"subject,omitempty"`
	DNSNames  []string  `json:"dnsNames,omitempty"`
	IPs       []string  `json:"ips,omitempty"`
	NotBefore time.Time `json:"notBefore,omitzero"`
	NotAfter  time.Time `json:"notAfter,omitzero"`
	DaysLeft  int       `json:"daysLeft"`
	Error     string    `json:"error,omitempty"`
}

// StateLink is link.yaml without secrets.
type StateLink struct {
	Present bool   `json:"present"`
	Relay   string `json:"relay,omitempty"`
	Box     string `json:"box,omitempty"`
	// PairCodeWaiting: a pairing code is stored and not redeemed yet (the code itself is not shown).
	PairCodeWaiting bool `json:"pairCodeWaiting"`
	Admin           bool `json:"admin"`
	// Proxy is link.yaml's proxy, its user and password left out.
	Proxy       string `json:"proxy,omitempty"`
	UnknownKeys bool   `json:"unknownKeys"`
	Error       string `json:"error,omitempty"`
}

// StateDisk is the data directory's volume.
type StateDisk struct {
	Path       string `json:"path"`
	FreeBytes  uint64 `json:"freeBytes"`
	TotalBytes uint64 `json:"totalBytes"`
	Error      string `json:"error,omitempty"`
}

// maxIndexFiles bounds the index entries one state reads (an index holds one file per order or receipt ever made).
const maxIndexFiles = 200_000

// State answers the state command.
func State(ctx context.Context, env Env, _ StateInput) (*StateResult, error) {
	sys := env.Sys
	dir := sys.DataDir()
	res := &StateResult{Version: buildinfo.Version}
	failed := func(what string, err error) { res.Errors = append(res.Errors, what+": "+err.Error()) }

	var err error
	if res.Services.Daemon, err = sys.Service(ServiceName); err != nil {
		failed("the "+ServiceName+" service", err)
	}
	if res.Services.Link, err = sys.Service(link.ServiceName); err != nil {
		failed("the "+link.ServiceName+" service", err)
	}

	var cfg *config.Config
	raw, err := os.ReadFile(filepath.Join(dir, configFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		res.Errors = append(res.Errors, "config.yaml: there is none: the connector is not configured")
	case err != nil:
		failed("config.yaml", err)
	default:
		res.Config = &StateConfig{MissingKeys: []string{}, UnknownKeys: []string{}}
		sum := sha256.Sum256(raw)
		res.Config.SHA256 = hex.EncodeToString(sum[:])
		if fi, err := os.Stat(filepath.Join(dir, configFile)); err == nil {
			res.Config.Modified = fi.ModTime().UTC()
			d := res.Services.Daemon
			res.Config.ConfigNewerThanStart = d.State == "running" && configStale(fi.ModTime(), d.ProcessStart)
		}
		res.Config.MissingKeys, res.Config.UnknownKeys = configKeys(raw)
		if parsed, err := config.Parse(raw); err != nil {
			res.Config.Error = err.Error()
		} else {
			cfg = &parsed
			res.Config.ERP, res.Config.APIListen = string(parsed.ERP), parsed.APIListen
		}
	}

	res.Daemon = askDaemon(ctx, env, cfg, res.Services.Daemon)
	res.Orders = orderState(dir, cfg)
	res.Receipts = receiptState(dir, cfg)
	if cfg != nil && strings.TrimSpace(cfg.SendOrderPickupDir) != "" {
		res.Pickup = pickupState(cfg.SendOrderPickupDir)
	}
	if cfg != nil && cfg.TLS.Enabled() {
		res.TLS = tlsState(cfg.TLS.CertFile, env.Now())
	}
	res.Link = linkState(dir)
	res.Disk = StateDisk{Path: dir}
	if free, total, err := diskSpace(dir); err != nil {
		res.Disk.Error = err.Error()
	} else {
		res.Disk.FreeBytes, res.Disk.TotalBytes = free, total
	}
	return res, nil
}

// askDaemon asks the running daemon for its version, its database's health and its order queue — read-only routes,
// with the credential in config.yaml (which may not be the one it runs: then it says so).
func askDaemon(ctx context.Context, env Env, cfg *config.Config, svc autostart.ServiceInfo) StateDaemon {
	if cfg == nil || svc.State != "running" {
		return StateDaemon{}
	}
	out := StateDaemon{Asked: true}
	d := env.daemonFor(*cfg)
	tok, err := d.login(ctx, cfg.Auth.Username, cfg.Auth.Password)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if p, err := d.ping(ctx, tok); err == nil {
		out.Version = p.Version
	} else {
		out.Error = err.Error()
	}
	if status, err := d.get(ctx, "/api/health", tok, nil); err == nil {
		out.Health = status
	} else if out.Error == "" {
		out.Error = err.Error()
	}
	if q, r, err := d.queue(ctx, tok); err == nil {
		out.Queue = &StateQueue{Queued: q, Running: r}
	} else if out.Error == "" {
		out.Error = err.Error()
	}
	return out
}

// configKeys compares config.yaml's keys with the model's: the keys the model always writes that the file lacks, and
// the file's keys the model does not know.
func configKeys(raw []byte) (missing, unknown []string) {
	missing, unknown = []string{}, []string{}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return missing, unknown
	}
	var walk func(t reflect.Type, m map[string]any, prefix string)
	walk = func(t reflect.Type, m map[string]any, prefix string) {
		known := map[string]bool{}
		for i := range t.NumField() {
			f := t.Field(i)
			name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			known[name] = true
			v, present := m[name]
			if !present {
				if !strings.Contains(opts, "omitempty") {
					missing = append(missing, prefix+name)
				}
				continue
			}
			if f.Type.Kind() == reflect.Struct {
				if sub, ok := v.(map[string]any); ok {
					walk(f.Type, sub, prefix+name+".")
				}
			}
		}
		for k := range m {
			if !known[k] {
				unknown = append(unknown, prefix+k)
			}
		}
	}
	walk(reflect.TypeFor[config.Config](), doc, "")
	slices.Sort(missing)
	slices.Sort(unknown)
	return missing, unknown
}

func orderState(dir string, cfg *config.Config) StateOrders {
	out := StateOrders{}
	out.Index = indexState(hasavshevet.OrderIndexDir(dir), func(b []byte) (int64, string, string, error) {
		var e struct {
			Number int64  `json:"number"`
			State  string `json:"state"`
		}
		err := json.Unmarshal(b, &e)
		return e.Number, e.State, "", err
	}, nil)
	if cfg == nil || !cfg.OrdersConfigured() {
		return out
	}
	out.Configured = true
	counter := filepath.Join(cfg.SendOrderDir, orderNumberFile)
	if _, err := os.Stat(counter); err == nil {
		out.CounterPresent = true
	}
	if next, err := hasavshevet.NewOrderNumberStore(counter).Peek(); err != nil {
		out.CounterError = err.Error()
	} else {
		out.LastOrderNumber = next - 1
	}
	out.HighestHistory, out.HistoryError = highestNumbered(filepath.Join(cfg.SendOrderDir, "history"))
	return out
}

func receiptState(dir string, cfg *config.Config) StateReceipts {
	out := StateReceipts{Configured: cfg != nil && strings.TrimSpace(cfg.SendReceiptDir) != ""}
	byImport := map[string]*StateNumbers{}
	out.Index = indexState(filepath.Join(dir, "receipts", "index"), func(b []byte) (int64, string, string, error) {
		var e struct {
			Number int64  `json:"number"`
			State  string `json:"state"`
			Import string `json:"import"`
		}
		err := json.Unmarshal(b, &e)
		return e.Number, e.State, e.Import, err
	}, byImport)
	if len(byImport) > 0 {
		out.ByImport = byImport
	}
	store := hasavshevet.ReceiptNumberStorePath(dir)
	if _, err := os.Stat(store); err == nil {
		out.StorePresent = true
		if next, err := hasavshevet.NewReceiptNumberStore(store).Peek(); err != nil {
			out.StoreError = err.Error()
		} else {
			out.NextReceiptNumber = next
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		out.StoreError = err.Error()
	}
	out.HighestHistory, out.HistoryError = highestNumbered(filepath.Join(dir, "history", "receipts"))
	return out
}

// indexState reads an index of one JSON file per key: it counts the entries by state (and by the second state when
// second is given), keeping the highest numbers of each. Only number and states are read from an entry.
func indexState(dir string, decode func([]byte) (int64, string, string, error), second map[string]*StateNumbers) *StateIndex {
	ix := &StateIndex{ByState: map[string]*StateNumbers{}}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return ix
	}
	if err != nil {
		ix.Error = err.Error()
		return ix
	}
	add := func(m map[string]*StateNumbers, state string, n int64) {
		if state == "" {
			state = "(none)"
		}
		s := m[state]
		if s == nil {
			s = &StateNumbers{Numbers: []int64{}}
			m[state] = s
		}
		s.Count++
		s.Numbers = append(s.Numbers, n)
	}
	for i, e := range entries {
		if i >= maxIndexFiles {
			ix.Error = fmt.Sprintf("more than %d entries: the first %d were read", maxIndexFiles, maxIndexFiles)
			break
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		ix.Entries++
		b, err := readSmall(filepath.Join(dir, e.Name()), 1<<20)
		var n int64
		var state, other string
		if err == nil {
			n, state, other, err = decode(b)
		}
		if err != nil {
			if len(ix.Unreadable) < maxStateNumbers {
				ix.Unreadable = append(ix.Unreadable, e.Name())
			}
			continue
		}
		add(ix.ByState, state, n)
		if second != nil {
			add(second, other, n)
		}
	}
	for _, m := range []map[string]*StateNumbers{ix.ByState, second} {
		for _, s := range m {
			slices.Sort(s.Numbers)
			if len(s.Numbers) > maxStateNumbers {
				s.Numbers = s.Numbers[len(s.Numbers)-maxStateNumbers:]
			}
		}
	}
	return ix
}

// readSmall reads a regular file of at most max bytes, refusing a bigger one and a link in its place.
func readSmall(path string, max int64) ([]byte, error) {
	if fi, err := os.Lstat(path); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Size() > max {
		return nil, fmt.Errorf("%s is not a file of at most %d bytes", path, max)
	}
	b := make([]byte, fi.Size())
	_, err = f.ReadAt(b, 0)
	return b, err
}

// highestNumbered is the highest all-digits entry name in dir (0 when it has none or does not exist).
func highestNumbered(dir string) (int64, string) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, ""
	}
	if err != nil {
		return 0, err.Error()
	}
	var top int64
	for _, e := range entries {
		if n, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && n > top {
			top = n
		}
	}
	return top, ""
}

// pickupState counts the pickup folder's order files: waiting (IMOVEIN_<n>.doc, TMP_IMOVEIN_<n>.doc, any case) and
// imported (renamed *.MASOFON2.BAK).
func pickupState(dir string) *StatePickup {
	out := &StatePickup{Dir: dir, Pending: StateNumbers{Numbers: []int64{}}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	for _, e := range entries {
		lower := strings.ToLower(e.Name())
		if strings.HasSuffix(lower, ".masofon2.bak") {
			out.Imported++
			continue
		}
		name := strings.TrimPrefix(lower, "tmp_")
		if num, ok := strings.CutPrefix(name, "imovein_"); ok {
			if digits, ok := strings.CutSuffix(num, ".doc"); ok {
				if n, err := strconv.ParseInt(digits, 10, 64); err == nil {
					out.Pending.Count++
					out.Pending.Numbers = append(out.Pending.Numbers, n)
				}
			}
		}
	}
	slices.Sort(out.Pending.Numbers)
	if len(out.Pending.Numbers) > maxStateNumbers {
		out.Pending.Numbers = out.Pending.Numbers[len(out.Pending.Numbers)-maxStateNumbers:]
	}
	return out
}

// tlsState reads the daemon's certificate: whom it names and when it expires.
func tlsState(certFile string, now time.Time) *StateTLS {
	out := &StateTLS{CertFile: certFile}
	b, err := readSmall(certFile, 1<<20)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		out.Error = "the certificate file is not PEM"
		return out
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Subject, out.DNSNames, out.NotBefore, out.NotAfter = c.Subject.String(), c.DNSNames, c.NotBefore.UTC(), c.NotAfter.UTC()
	for _, ip := range c.IPAddresses {
		out.IPs = append(out.IPs, ip.String())
	}
	out.DaysLeft = int(c.NotAfter.Sub(now).Hours() / 24)
	return out
}

// linkState is link.yaml as the link reads it, without the pairing code or the proxy's credential.
func linkState(dir string) StateLink {
	out := StateLink{}
	if _, err := os.Stat(filepath.Join(dir, "link.yaml")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			out.Error = err.Error()
		}
		out.Admin = true
		return out
	}
	out.Present = true
	st, err := (&link.Store{Dir: dir}).LoadState()
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Relay, out.Box, out.PairCodeWaiting, out.Admin, out.UnknownKeys = st.RelayURL(), st.Box, st.PairCode != "", st.AdminOn(), st.UnknownKeys()
	if p := strings.TrimSpace(st.Proxy); p != "" {
		out.Proxy = proxyWithoutCredential(p)
	}
	return out
}

// proxyWithoutCredential is a proxy URL without its user and password; one that does not parse is only named as set.
func proxyWithoutCredential(p string) string {
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return "(set, does not parse)"
	}
	u.User = nil
	return u.Scheme + "://" + u.Host
}

// decodeOptional is decodeInput where an empty stdin means {}.
func decodeOptional(stdin io.Reader, v any) *Refusal {
	b, err := io.ReadAll(io.LimitReader(stdin, MaxInput))
	if err != nil {
		return refuse(CodeSpecInvalid, "send one JSON object with only the documented fields", "the input cannot be read: %v", err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return decodeInput(bytes.NewReader(b), v)
}
