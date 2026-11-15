// Command digi-erp-connector-link is the connector's outbound link to
// Digitrade's relay (plan 083). As a Windows service it keeps the box
// connected; from a console it pairs the box and reports its state. The logic
// is internal/link.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/digitrade-e/digi-erp-connector/internal/buildinfo"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/link"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

const usage = `digi-erp-connector-link — the connector's outbound link to Digitrade

  digi-erp-connector-link pair <CODE>   store a pairing code from Digitrade; prints the fingerprint to read back to them
  digi-erp-connector-link fingerprint   print this server's key fingerprint
  digi-erp-connector-link status        print the relay, the box id and whether commands are on
  digi-erp-connector-link version       print the build
  digi-erp-connector-link run           run in the foreground (the service does this itself)

link.yaml in the data directory holds the state; "admin: false" there turns remote commands off.`

func main() {
	if runAsService() {
		return
	}
	m := machine(nil)
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "version":
		fmt.Println(buildinfo.Version)
	case "fingerprint":
		k, err := m.Keys()
		exitOn(err)
		fmt.Println(k.Public().Fingerprint())
	case "pair":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, usage)
			os.Exit(2)
		}
		fp, err := m.Pair(args[1])
		exitOn(err)
		fmt.Printf("Pairing code stored. Read this fingerprint to Digitrade:\n\n    %s\n\n", fp)
	case "status":
		st, err := m.Store().LoadState()
		exitOn(err)
		k, err := m.Keys()
		exitOn(err)
		box := st.Box
		switch {
		case box == "" && st.PairCode != "":
			box = "(a pairing code is waiting to be redeemed)"
		case box == "":
			box = "(not paired)"
		}
		fmt.Printf("relay:       %s\nbox:         %s\ncommands:    %s\nfingerprint: %s\n", st.RelayURL(), box, onOff(st.AdminOn()), k.Public().Fingerprint())
	case "run":
		log, err := link.OpenFileLog(filepath.Join(paths.DataDir(), "link.log"))
		exitOn(err)
		app := &link.App{Machine: machine(log), Log: log}
		exitOn(app.Start())
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		app.Stop(context.Background())
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
}

func machine(log link.Logger) link.Machine {
	exe, _ := os.Executable()
	return link.Machine{
		DataDir: paths.DataDir(), InstallDir: filepath.Dir(exe), Secrets: secretStore{}, LoadDaemon: config.Load,
		Build: buildinfo.Version, Log: log, HostFacts: link.MachineFacts,
	}
}

// secretStore adapts internal/secrets (DPAPI on Windows) to link.Secrets.
type secretStore struct{}

func (secretStore) Get(k string) ([]byte, error) {
	b, err := secrets.Get(k)
	if errors.Is(err, secrets.ErrNotFound) {
		return nil, link.ErrNoSecret
	}
	return b, err
}

func (secretStore) Set(k string, v []byte) error { return secrets.Set(k, v) }

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off (link.yaml admin: false)"
}

func exitOn(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
