package main

import (
	"fmt"
	"strconv"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

func dbPasswordKey(erp config.ERPType) string {
	return "db_password_" + string(erp)
}

// portText is the DB port as the form shows it: empty for 0, which the form
// reads back as "no port" (parsePort) — a named instance (host\instance)
// found through the SQL Server Browser, or no database at all. Showing "0"
// made such a config impossible to save: parsePort refuses it.
func portText(port int) string {
	if port <= 0 {
		return ""
	}
	return strconv.Itoa(port)
}

func resolveDBPassword(erp config.ERPType, entered string, required bool) (string, error) {
	if entered != "" {
		return entered, nil
	}
	if !required {
		return "", nil
	}
	b, err := secrets.Get(dbPasswordKey(erp))
	if err != nil {
		return "", fmt.Errorf("db password is required to initialize Hasavshevet procedures: %w", err)
	}
	return string(b), nil
}
