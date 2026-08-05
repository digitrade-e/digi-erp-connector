package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
)

var ErrNotFound = errors.New("config not found")

func Load() (Config, error) {
	p, err := paths.ConfigFilePath()
	if err != nil {
		return Config{}, err
	}

	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, ErrNotFound
		}
		return Config{}, fmt.Errorf("read config %s: %w", p, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", p, err)
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

// Save writes the config atomically with 0600 permissions: it holds the bearer
// token in plaintext, and a half-written config would stop the daemon starting.
func Save(cfg Config) error {
	p, err := paths.ConfigFilePath()
	if err != nil {
		return err
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return atomicfile.Write(p, out, 0o600)
}
