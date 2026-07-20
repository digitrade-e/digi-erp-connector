package pdf

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RemoteFetcher fetches pre-rendered HTML documents from the backend's
// /api/pdf-template/connector/render/:token endpoint. The returned bytes are
// passed unchanged to Generator.GenerateFromHTML.
type RemoteFetcher struct {
	client    *http.Client
	baseURL   string
	userAgent string
}

// NewRemoteFetcher returns a fetcher rooted at baseURL. baseURL is the backend
// origin without any /api suffix (e.g. "https://api.example.com"); the
// /api/pdf-template/... path is appended internally. timeout=0 falls back to
// 15s. userAgent identifies this connector instance for backend logging.
//
// Operators sometimes paste the full URL shown in the admin's "Generate token"
// dialog (which contains placeholders like `?documentNumber={documentNumber}`)
// into the BaseURL field. Without normalization the connector would append
// its own path+query, producing a URL with two `?` segments where Express
// reads `documentNumber={documentNumber}` literally and returns 404. Strip
// any path/query/fragment to scheme+host only.
func NewRemoteFetcher(baseURL string, timeout time.Duration, userAgent string) *RemoteFetcher {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if userAgent == "" {
		userAgent = "digi-erp-connector/unknown"
	}
	return &RemoteFetcher{
		client:    &http.Client{Timeout: timeout},
		baseURL:   normalizeBaseURL(baseURL),
		userAgent: userAgent,
	}
}

// normalizeBaseURL reduces the operator-supplied value to scheme+host. Adds
// `https://` if no scheme is present. Returns "" for empty/malformed input.
func normalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}

// Fetch returns the HTML bytes for the given (token, documentType, documentNumber, userExtId).
// On non-2xx the body's first 200 bytes are included in the error for diagnostics.
// The token is NEVER included in returned errors — callers may log only its last 4 chars.
func (r *RemoteFetcher) Fetch(ctx context.Context, token, documentType, documentNumber, userExtId string) ([]byte, error) {
	if r.baseURL == "" {
		return nil, fmt.Errorf("remote template base URL not configured")
	}
	if token == "" {
		return nil, fmt.Errorf("token required")
	}

	u := fmt.Sprintf("%s/api/pdf-template/connector/render/%s",
		r.baseURL, url.PathEscape(token))
	q := url.Values{}
	q.Set("documentNumber", documentNumber)
	q.Set("userExtId", userExtId)
	if documentType != "" {
		q.Set("documentType", documentType)
	}
	u += "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build remote request: %w", err)
	}
	req.Header.Set("User-Agent", r.userAgent)
	req.Header.Set("Accept", "text/html")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("remote fetch: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview := body
		if len(preview) > 200 {
			preview = preview[:200]
		}
		return nil, fmt.Errorf("remote fetch http %d: %s", resp.StatusCode, string(preview))
	}
	if readErr != nil {
		return nil, fmt.Errorf("remote fetch read body: %w", readErr)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("remote fetch returned empty body")
	}
	return body, nil
}

// MaskToken returns the token's last 4 chars, prefixed with asterisks of equal
// length, suitable for logging.
func MaskToken(token string) string {
	if len(token) <= 4 {
		return strings.Repeat("*", len(token))
	}
	return strings.Repeat("*", len(token)-4) + token[len(token)-4:]
}
