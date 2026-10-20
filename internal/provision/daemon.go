package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// daemon is the connector running on this machine, reached through its own
// API with a credential from config.yaml — the only way to see its in-memory
// order queue, and the proof that it listens.
type daemon struct {
	base string
	http *http.Client
}

func (e Env) daemonFor(cfg config.Config) daemon {
	scheme := "http"
	if cfg.TLS.Enabled() {
		scheme = "https"
	}
	return daemon{base: scheme + "://" + e.LocalAddr(cfg.APIListen), http: e.HTTP}
}

var (
	// errUnauthorized: the daemon refused the credential — it runs on
	// another config than the one on disk.
	errUnauthorized = errors.New("the running connector refused the credential in config.yaml")
	// errNoQueueRoute: a connector older than GET /api/sendOrder.
	errNoQueueRoute = errors.New("the running connector has no GET /api/sendOrder")
)

// maxLocalBody bounds what is read from the local daemon.
const maxLocalBody = 64 << 10

func (d daemon) login(ctx context.Context, user, pass string) (string, error) {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.base+"/auth/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST /auth/token answered %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxLocalBody)).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", errors.New("POST /auth/token answered 200 without an access_token")
	}
	return tok.AccessToken, nil
}

// get calls an /api route; out may be nil. The status is returned as well as
// any transport error.
func (d daemon) get(ctx context.Context, path, token string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.base+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := d.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxLocalBody)).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("GET %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

type pingBody struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

func (d daemon) ping(ctx context.Context, token string) (pingBody, error) {
	var p pingBody
	status, err := d.get(ctx, "/api/ping", token, &p)
	if err != nil {
		return p, err
	}
	if status != http.StatusOK || !p.OK {
		return p, fmt.Errorf("GET /api/ping answered %d", status)
	}
	return p, nil
}

// queue is GET /api/sendOrder: accepted orders not written yet.
func (d daemon) queue(ctx context.Context, token string) (queued, running int, err error) {
	var q struct {
		Queued  *int `json:"queued"`
		Running *int `json:"running"`
	}
	status, err := d.get(ctx, "/api/sendOrder", token, &q)
	switch {
	case err != nil:
		return 0, 0, err
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return 0, 0, errNoQueueRoute
	case status != http.StatusOK || q.Queued == nil || q.Running == nil:
		return 0, 0, fmt.Errorf("GET /api/sendOrder answered %d", status)
	}
	return *q.Queued, *q.Running, nil
}
