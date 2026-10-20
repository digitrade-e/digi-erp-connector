package provision

import (
	"bytes"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

// Redacted replaces a secret in output.
const Redacted = "[redacted]"

// redactForms is every form a secret can take in text that reached us: as
// given, and as a URL escapes it — in the userinfo of a DSN (what a driver's
// parse error quotes), in a path, in a query. Longest first, so a form that
// contains another is not left half replaced.
func redactForms(secrets []string) []string {
	seen := map[string]bool{}
	var forms []string
	add := func(f string) {
		if f != "" && !seen[f] {
			seen[f] = true
			forms = append(forms, f)
		}
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		add(s)
		add(strings.TrimPrefix(url.UserPassword("x", s).String(), "x:"))
		add(url.PathEscape(s))
		add(url.QueryEscape(s))
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}

// Redact replaces every form of every secret in s with Redacted.
func Redact(s string, secrets ...string) string {
	for _, f := range redactForms(secrets) {
		s = strings.ReplaceAll(s, f, Redacted)
	}
	return s
}

// redactJSON redacts inside the string values of a JSON document only. Keys,
// numbers, booleans and the document's shape are never touched, so a secret
// such as "1" or "true" cannot break the one-object output.
func redactJSON(b []byte, secrets ...string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	forms := redactForms(secrets)
	var walk func(any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case string:
			for _, f := range forms {
				t = strings.ReplaceAll(t, f, Redacted)
			}
			return t
		case []any:
			for i := range t {
				t[i] = walk(t[i])
			}
		case map[string]any:
			for k := range t {
				t[k] = walk(t[k])
			}
		}
		return v
	}
	v = walk(v)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
