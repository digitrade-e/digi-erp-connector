package provision

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Redacted replaces a secret in output.
const Redacted = "[redacted]"

// Redact replaces every secret in b with Redacted, in its raw form and in the
// form JSON escapes it to. It is the last line of defence over every result:
// an error from a driver or a URL parser can carry a DSN, password included.
// Longer secrets go first, so one that contains another is not left half
// replaced.
func Redact(b []byte, secrets ...string) []byte {
	forms := make([]string, 0, 2*len(secrets))
	for _, s := range secrets {
		if s == "" {
			continue
		}
		forms = append(forms, s)
		if j, err := json.Marshal(s); err == nil {
			if esc := string(j[1 : len(j)-1]); esc != s {
				forms = append(forms, esc)
			}
		}
	}
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	for _, f := range forms {
		b = bytes.ReplaceAll(b, []byte(f), []byte(Redacted))
	}
	return b
}
