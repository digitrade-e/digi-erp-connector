package provision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode/utf16"
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

// asciiOnly escapes every non-ASCII character of a JSON document as \uXXXX
// (a pair for one beyond the BMP). Windows PowerShell 5.1 reads a file in the
// system code page — Windows-1255 on a Hebrew box — and shows UTF-8 as
// mojibake; escaped, the output reads the same in any code page, and every
// JSON reader decodes it back. Only strings can hold such characters, so the
// escapes are always inside one.
func asciiOnly(b []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(b))
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			out.WriteByte(byte(r))
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			fmt.Fprintf(&out, `\u%04x\u%04x`, hi, lo)
		default:
			fmt.Fprintf(&out, `\u%04x`, r)
		}
	}
	return out.Bytes()
}
