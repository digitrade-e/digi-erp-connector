package config

import (
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"errors"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

var ErrNotFound = errors.New("config not found")

func Load() (Config, error) {
	p, err := paths.ConfigFilePath()
	if err != nil {
		return Config{}, err
	}

	f, err := os.Open(p)

	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, ErrNotFound
		}
		return Config{}, err
	}
	defer f.Close()

	b, err := io.ReadAll(f)
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	errYaml := yaml.Unmarshal(b, &cfg)

	if errYaml != nil {
		return Config{}, errYaml
	}

	return cfg, nil

}

func LoadOrDefault() (Config, error) {
	cfg, err := Load()

	if err == nil {
		return cfg, nil
	}

	if errors.Is(err, ErrNotFound) {
		return Default(), nil
	}

	return Config{}, err
}

func Save(cfg Config) error {
	p, err := paths.ConfigFilePath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(p)
	errDir := os.MkdirAll(dir, 0o755)

	if errDir != nil {
		return errDir
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return err
	}

	tmpName := tmp.Name()

	_ = tmp.Chmod(0o600)

	_, writeErr := tmp.Write(out)

	syncErr := tmp.Sync()

	closeErr := tmp.Close()

	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmpName)
		if writeErr != nil {
			return writeErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}

	_ = os.Remove(p)

	errRename := os.Rename(tmpName, p)

	if errRename != nil {
		_ = os.Remove(tmpName)
		return errRename
	}

	return nil

}
