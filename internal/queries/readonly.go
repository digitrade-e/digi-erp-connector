package queries

import (
	"errors"
	"regexp"
	"strings"
)

// Read-only validation for the legacy POST /api/query route only.
//
// Saved queries are trusted and deliberately unrestricted (see Runner). This
// validator exists solely so the electron-mssql-app compatibility endpoint,
// which accepts SQL text from the caller, cannot be used to write. It is ported
// from erp-connector's deleted handlers/sql.go with two compatibility
// relaxations noted below, because the old Node endpoint only checked that the
// statement started with SELECT/WITH and we must not reject queries it accepted
// unless they are genuinely unsafe.
var (
	ErrSQLRequired       = errors.New("query sql is required")
	ErrSQLNotReadOnly    = errors.New("query is not read-only")
	ErrSQLMultiStatement = errors.New("multiple statements are not allowed")
	ErrSQLComments       = errors.New("sql comments are not allowed")
)

// ValidateReadOnly rejects anything that is not a single read-only statement.
func ValidateReadOnly(query string) error {
	q := strings.TrimSpace(query)
	if q == "" {
		return ErrSQLRequired
	}

	// Strip literals first so a semicolon, comment marker or keyword inside a
	// string value cannot trip any check below. (erp-connector tested for ';'
	// on the raw text, which rejected legitimate queries such as
	// WHERE note = 'a;b' — the old Node endpoint accepted those.)
	stripped := stripStringLiterals(q)

	// A single trailing semicolon is idiomatic and was accepted by the old
	// endpoint; only genuine statement chaining is refused.
	stripped = strings.TrimRight(strings.TrimSpace(stripped), ";")
	if strings.Contains(stripped, ";") {
		return ErrSQLMultiStatement
	}

	lower := strings.ToLower(stripped)
	if strings.Contains(lower, "--") || strings.Contains(lower, "/*") || strings.Contains(lower, "*/") {
		return ErrSQLComments
	}

	if !startsWithSelectOrWith(lower) {
		return ErrSQLNotReadOnly
	}

	for _, re := range disallowedKeywordRegex {
		if re.MatchString(lower) {
			return ErrSQLNotReadOnly
		}
	}

	return nil
}

func startsWithSelectOrWith(lower string) bool {
	trimmed := strings.TrimSpace(lower)
	switch {
	case trimmed == "select", trimmed == "with":
		return true
	}
	for _, prefix := range []string{"select ", "select\n", "select\t", "select\r", "select(",
		"with ", "with\n", "with\t", "with\r"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

var disallowedKeywordRegex = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\binsert\b`),
	regexp.MustCompile(`(?i)\bupdate\b`),
	regexp.MustCompile(`(?i)\bdelete\b`),
	regexp.MustCompile(`(?i)\bmerge\b`),
	regexp.MustCompile(`(?i)\btruncate\b`),
	regexp.MustCompile(`(?i)\bdrop\b`),
	regexp.MustCompile(`(?i)\balter\b`),
	regexp.MustCompile(`(?i)\bcreate\b`),
	regexp.MustCompile(`(?i)\bexec\b`),
	regexp.MustCompile(`(?i)\bexecute\b`),
	regexp.MustCompile(`(?i)\bgrant\b`),
	regexp.MustCompile(`(?i)\brevoke\b`),
}
