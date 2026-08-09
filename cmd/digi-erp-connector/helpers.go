package main

import (
	"fmt"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

func dbPasswordKey(erp config.ERPType) string {
	return "db_password_" + string(erp)
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
