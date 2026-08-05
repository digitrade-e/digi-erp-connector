package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

var ErrNotFound = errors.New("secret not found")
var numR = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func sanitizeKey(key string) string {
	key = strings.TrimSpace(key)
	key = numR.ReplaceAllString(key, "_")
	if key == "" {
		return "empty"
	}
	return key
}

// secretFilePath maps a key to secrets/<key>.bin next to the config file.
func secretFilePath(key string) (string, error) {
	cfgPath, err := paths.ConfigFilePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), "secrets", sanitizeKey(key)+".bin"), nil
}

// Set encrypts value with the OS keystore and stores it atomically.
//
// Note the previous implementation returned the (nil) encrypt error when the
// final rename failed, so a failed secret write reported success and the daemon
// later started with a stale or absent DB password.
func Set(key string, value []byte) error {
	p, err := secretFilePath(key)
	if err != nil {
		return err
	}

	enc, err := encrypt(value)
	if err != nil {
		return fmt.Errorf("encrypt secret %q: %w", key, err)
	}
	return atomicfile.Write(p, enc, 0o600)
}

// Get returns the decrypted secret, or ErrNotFound if it was never stored.
func Get(key string) ([]byte, error) {
	p, err := secretFilePath(key)
	if err != nil {
		return nil, err
	}

	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("secret %q: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("read secret %q: %w", key, err)
	}

	dec, err := decrypt(b)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret %q: %w", key, err)
	}
	return dec, nil
}
