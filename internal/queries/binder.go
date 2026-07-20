package queries

import (
	"database/sql"
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// forcedStringParams are parameter names that must always bind as strings,
// never coerced to numbers. Ported from electron-mssql-app server.js:
// skuArray is a CSV consumed by STRING_SPLIT; warehouse/sku/syncKey are
// NVARCHAR codes (e.g. SAP B1 WhsCode) that may look numeric.
var forcedStringParams = map[string]struct{}{
	"skuarray":  {},
	"warehouse": {},
	"sku":       {},
	"synckey":   {},
}

// IsForcedString reports whether the parameter must bind as a string.
func IsForcedString(name string) bool {
	_, ok := forcedStringParams[strings.ToLower(strings.TrimPrefix(name, "@"))]
	return ok
}

// BuildNamedArgs converts a params map into sorted sql.Named arguments.
// Integer-coercion hints are derived from TOP/OFFSET/FETCH NEXT usage in the
// query so paging values sent as strings bind as integers.
func BuildNamedArgs(query string, params map[string]any) []any {
	if len(params) == 0 {
		return nil
	}
	hints := detectIntegerParams(query)

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := make([]any, 0, len(keys))
	for _, key := range keys {
		name := strings.TrimPrefix(key, "@")
		if IsForcedString(name) {
			args = append(args, sql.Named(name, toString(params[key])))
			continue
		}
		args = append(args, sql.Named(name, normalizeParamValue(name, params[key], hints)))
	}
	return args
}

// InferStringValue applies the electron-mssql-app type inference to a raw
// query-string value: all-digits → int64, decimal → float64, ISO-ish date →
// time.Time, anything else stays a string.
func InferStringValue(raw string) any {
	if allDigitsRe.MatchString(raw) {
		if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return i
		}
		return raw
	}
	if decimalRe.MatchString(raw) {
		if f, err := strconv.ParseFloat(raw, 64); err == nil {
			return f
		}
		return raw
	}
	if datePrefixRe.MatchString(raw) {
		for _, layout := range []string{
			time.RFC3339,
			"2006-01-02T15:04:05",
			"2006-01-02 15:04:05",
			"2006-01-02",
		} {
			if d, err := time.Parse(layout, raw); err == nil {
				return d
			}
		}
	}
	return raw
}

var (
	allDigitsRe  = regexp.MustCompile(`^\d+$`)
	decimalRe    = regexp.MustCompile(`^\d+\.\d+$`)
	datePrefixRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)
)

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		if math.Trunc(t) == t {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return strings.Trim(string(b), `"`)
	}
}

func normalizeParamValue(name string, v any, intParamHints map[string]struct{}) any {
	switch t := v.(type) {
	case float64:
		if math.Trunc(t) == t {
			return int64(t)
		}
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			if math.Trunc(f) == f {
				return int64(f)
			}
			return f
		}
		return t.String()
	case string:
		if shouldCoerceToInt(name, intParamHints) {
			if i, ok := parseInt64String(t); ok {
				return i
			}
		}
		return t
	default:
		return v
	}
}

func shouldCoerceToInt(name string, intParamHints map[string]struct{}) bool {
	if len(intParamHints) == 0 {
		return false
	}
	_, ok := intParamHints[strings.ToLower(strings.TrimPrefix(name, "@"))]
	return ok
}

func parseInt64String(raw string) (int64, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, false
	}
	for i, ch := range s {
		if i == 0 && (ch == '+' || ch == '-') {
			if len(s) == 1 {
				return 0, false
			}
			continue
		}
		if ch < '0' || ch > '9' {
			return 0, false
		}
	}
	out, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return out, true
}

func detectIntegerParams(query string) map[string]struct{} {
	hints := make(map[string]struct{})
	addIntegerParamHints(hints, query, offsetParamRe)
	addIntegerParamHints(hints, query, fetchNextParamRe)
	addIntegerParamHints(hints, query, topParamRe)
	return hints
}

func addIntegerParamHints(hints map[string]struct{}, query string, re *regexp.Regexp) {
	matches := re.FindAllStringSubmatch(query, -1)
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		hints[strings.ToLower(strings.TrimPrefix(m[1], "@"))] = struct{}{}
	}
}

var (
	offsetParamRe    = regexp.MustCompile(`(?i)\boffset\s+@([a-z_][a-z0-9_]*)\s+rows\b`)
	fetchNextParamRe = regexp.MustCompile(`(?i)\bfetch\s+next\s+@([a-z_][a-z0-9_]*)\s+rows\s+only\b`)
	topParamRe       = regexp.MustCompile(`(?i)\btop\s*\(\s*@([a-z_][a-z0-9_]*)\s*\)`)
)
