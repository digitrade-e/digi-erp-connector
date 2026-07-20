package secrets

import (
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

func secretFilePath(key string) (string, error) {
	cfgPath, err := paths.ConfigFilePath()
	if err != nil {
		return "", err
	}

	baseDir := filepath.Dir(cfgPath)

	safe := sanitizeKey(key)

	return filepath.Join(baseDir, "secrets", safe+".bin"), nil

}

func Set(key string, value []byte) error {
	p, err := secretFilePath(key)
	if err != nil {
		return err
	}

	errFi := os.MkdirAll(filepath.Dir(p), 0o755)

	if errFi != nil {
		return errFi
	}

	enc, err := encrypt(value)

	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), "secret-*.tmp")

	if err != nil {
		return err
	}

	tmpName := tmp.Name()
	_ = tmp.Chmod(0o600)

	_, errWrite := tmp.Write(enc)

	if errWrite != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return errWrite
	}

	errSync := tmp.Sync()

	if errSync != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return errSync
	}

	errC := tmp.Close()

	if errC != nil {
		_ = os.Remove(tmpName)
		return errC
	}

	_ = os.Remove(p)

	errRename := os.Rename(tmpName, p)

	if errRename != nil {
		_ = os.Remove(tmpName)
		return err
	}

	return nil
}

func Get(key string) ([]byte, error) {
	p, err := secretFilePath(key)

	if err != nil {
		return nil, err
	}

	b, err := os.ReadFile(p)

	if err != nil {
		return nil, err
	}

	dec, err := decrypt(b)

	if err != nil {
		return nil, err
	}

	return dec, nil
}
