package handlers

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"

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

// decodeJSONBodyStrict is decodeJSONBody that also refuses fields dst does not
// declare (json.Decoder.DisallowUnknownFields). For routes whose body is a
// contract where an unexpected field means the two sides disagree — a receipt
// is a legal document, and a field the connector silently drops is a field
// the caller believes was written.
//
// An unknown field is reported as an error whose text is
// `json: unknown field "<name>"` (unknownJSONField extracts the name); a wrong
// type as *json.UnmarshalTypeError.
//
// Before decoding it walks the document's tokens against dst's type and refuses
// what encoding/json would silently accept: a key given twice (it keeps the
// last) and a key that matches a field only case-insensitively (it fills the
// field anyway). Both come back as *jsonKeyError naming the key's path
// ("account/city"), as does a key dst does not declare at all. The shared size
// limit applies to the whole body as before.
func decodeJSONBodyStrict(w http.ResponseWriter, r *http.Request, dst any) error {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		return err
	}
	if err := checkJSONKeys(body, reflect.TypeOf(dst)); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return ensureEOF(dec)
}

// jsonKeyError is a key decodeJSONBodyStrict refuses. Path is slash-joined
// ("details/reference"; array elements add nothing).
type jsonKeyError struct {
	Path      string
	Duplicate bool
}

func (e *jsonKeyError) Error() string {
	if e.Duplicate {
		return e.Path + ": key given more than once"
	}
	return e.Path + ": not a field (keys are case-sensitive)"
}

// checkJSONKeys returns a *jsonKeyError for the first duplicate or undeclared
// key, else nil. Malformed JSON is left for the decoder to report, so the
// error a caller sees for a syntax fault does not change.
func checkJSONKeys(body []byte, t reflect.Type) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var ke *jsonKeyError
	if err := walkJSONKeys(dec, t, ""); errors.As(err, &ke) {
		return ke
	}
	return nil
}

// walkJSONKeys consumes one JSON value. t is the Go type it decodes into, or
// nil when unknown (a type mismatch the decoder will report, or an any): then
// only duplicates are checked beneath it.
func walkJSONKeys(dec *json.Decoder, t reflect.Type, path string) error {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		var fields map[string]reflect.Type
		var elem reflect.Type
		if t != nil && t.Kind() == reflect.Struct {
			fields = jsonFieldTypes(t)
		} else if t != nil && t.Kind() == reflect.Map {
			elem = t.Elem()
		}
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := kt.(string)
			p := key
			if path != "" {
				p = path + "/" + key
			}
			if seen[key] {
				return &jsonKeyError{Path: p, Duplicate: true}
			}
			seen[key] = true
			next := elem
			if fields != nil {
				ft, declared := fields[key]
				if !declared {
					return &jsonKeyError{Path: p}
				}
				next = ft
			}
			if err := walkJSONKeys(dec, next, p); err != nil {
				return err
			}
		}
	case '[':
		var elem reflect.Type
		if t != nil && (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) {
			elem = t.Elem()
		}
		for dec.More() {
			if err := walkJSONKeys(dec, elem, path); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token() // the closing delimiter
	return err
}

// jsonFieldTypes maps a struct's exact JSON keys to their field types.
func jsonFieldTypes(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

// unknownJSONField returns the field name from a DisallowUnknownFields error,
// or "" when err is not one. encoding/json has no typed error for it; the text
// `json: unknown field "<name>"` is what it has produced since Go 1.10.
func unknownJSONField(err error) string {
	const prefix = `json: unknown field "`
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) || !strings.HasSuffix(msg, `"`) {
		return ""
	}
	return strings.TrimSuffix(strings.TrimPrefix(msg, prefix), `"`)
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
