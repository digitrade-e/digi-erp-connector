package main

import (
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/link"
)

type memSecrets map[string][]byte

func (m memSecrets) Get(k string) ([]byte, error) {
	if v, ok := m[k]; ok {
		return v, nil
	}
	return nil, link.ErrNoSecret
}
func (m memSecrets) Set(k string, v []byte) error { m[k] = v; return nil }

// Plan 094: a person at the server turns the shell on and off; nothing else writes link\shell.yaml.
func TestShellOnOffAtTheServer(t *testing.T) {
	store := &link.Store{Dir: t.TempDir(), Secrets: memSecrets{}}
	if o, err := store.LoadShellOptIn(); err != nil || o.Enabled {
		t.Fatalf("a new server: %+v %v", o, err)
	}
	if err := shellCommand(store, "on"); err != nil {
		t.Fatal(err)
	}
	o, err := store.LoadShellOptIn()
	if err != nil || !o.Enabled || o.EnabledBy == "" || o.EnabledAt == "" {
		t.Fatalf("after on: %+v %v", o, err)
	}
	if err := shellCommand(store, "status"); err != nil {
		t.Fatal(err)
	}
	if err := shellCommand(store, "off"); err != nil {
		t.Fatal(err)
	}
	if o, _ := store.LoadShellOptIn(); o.Enabled {
		t.Fatal("still on after off")
	}
	if err := shellCommand(store, "maybe"); err == nil {
		t.Fatal("an unknown op passed")
	}
}
