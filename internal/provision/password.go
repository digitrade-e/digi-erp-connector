package provision

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

const (
	sqlPasswordLength   = 32
	sqlPasswordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
)

// NewSQLPassword returns the least-privilege login's password. It is born on
// the box and kept only in DPAPI and SQL Server.
//
// Letters and digits only: CREATE LOGIN takes no parameter, so the value is
// written into the statement, and this alphabet needs no quoting at all. At
// least one upper-case letter, one lower-case letter and one digit satisfies
// SQL Server's CHECK_POLICY complexity (three of four classes).
func NewSQLPassword() (string, error) {
	max := big.NewInt(int64(len(sqlPasswordAlphabet)))
	for {
		var b strings.Builder
		for i := 0; i < sqlPasswordLength; i++ {
			n, err := rand.Int(rand.Reader, max)
			if err != nil {
				return "", fmt.Errorf("generate a SQL password: %w", err)
			}
			b.WriteByte(sqlPasswordAlphabet[n.Int64()])
		}
		if p := b.String(); hasEveryClass(p) {
			return p, nil
		}
	}
}

func hasEveryClass(p string) bool {
	var upper, lower, digit bool
	for _, c := range p {
		switch {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	return upper && lower && digit
}
