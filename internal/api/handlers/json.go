package handlers

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
)

// maxRequestBody caps every JSON request body. One limit for all routes: they
// all carry small documents (a saved query, an order, a file reference), and a
// single number is easier to reason about than five identical constants.
const maxRequestBody = 1 << 20 // 1 MiB

// decodeJSONBody reads exactly one JSON document from the request body into dst.
//
// It applies the shared size limit, rejects trailing data (so a body like
// `{}{}` or `{} garbage` is an error rather than being silently ignored), and
// decodes numbers as json.Number.
//
// UseNumber is applied uniformly: it only changes anything for fields typed
// `any`/`map[string]any` — the saved-query and legacy raw-SQL parameter maps —
// where it keeps large integers exact instead of degrading them through
// float64 before they are bound as SQL parameters. Struct fields with concrete
// types are unaffected.
//
// The caller decides how to report failure, because the native routes and the
// legacy compatibility routes use different error shapes.
//
// The request body is not closed here: net/http closes it for the handler.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)

	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return ensureEOF(dec)
}

// ensureEOF verifies the body held a single JSON document and nothing more.
func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return errors.New("extra data after JSON document")
}

// badJSONRequest reports an unparseable body in the native error shape.
func badJSONRequest(w http.ResponseWriter) {
	respond.Error(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
}

// constantTimeEqual compares two credential strings without leaking their
// contents through timing. Length differs first, which is unavoidable and not
// sensitive here.
func constantTimeEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
