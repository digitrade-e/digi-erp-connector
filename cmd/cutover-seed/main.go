// Command cutover-seed writes config.yaml, stores the DB password via DPAPI and
// imports saved queries — using the connector's own config/secrets/queries code
// so the on-disk formats are guaranteed to match what the daemon reads.
//
// Built for the electron-mssql-app → digi-erp-connector cutover and kept for the
// next one: it makes provisioning a replacement install reproducible and
// scriptable. The GUI remains the supported way to configure an installation
// interactively; this is an operator tool, not part of the service.
//
// Note it takes the DB password as a flag, so it lands in shell history and is
// briefly visible in the process list. Acceptable on a box where you are already
// reading the old app's plaintext config; do not use it on shared machines.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

func main() {
	listen := flag.String("listen", "", "apiListen host:port (e.g. [::]:8082)")
	erp := flag.String("erp", "hasavshevet", "erp type")
	dbHost := flag.String("db-host", "", "db host")
	dbPort := flag.Int("db-port", 0, "db port")
	dbUser := flag.String("db-user", "", "db user")
	dbName := flag.String("db-name", "", "db database")
	dbPassword := flag.String("db-password", "", "db password (stored via DPAPI, not in yaml)")
	encrypt := flag.Bool("encrypt", false, "set db.encrypt")
	trust := flag.Bool("trust", false, "set db.trustServerCertificate")
	legacy := flag.Bool("legacy", false, "enable electron-mssql-app compatibility")
	legacySecret := flag.String("legacy-secret", "", "legacy JWT secret")
	legacyUser := flag.String("legacy-user", "", "legacy JWT username")
	legacyPass := flag.String("legacy-pass", "", "legacy JWT password")
	legacyRawSQL := flag.Bool("legacy-raw-sql", false, "expose POST /api/query")
	queriesFrom := flag.String("queries-from", "", "import saved queries from this file")
	flag.Parse()

	cfg, err := config.LoadOrDefault()
	if err != nil {
		fail("load config", err)
	}

	if *listen != "" {
		cfg.APIListen = *listen
	}
	if *erp != "" {
		cfg.ERP = config.ERPType(*erp)
	}
	if *dbHost != "" {
		cfg.DB.Host = *dbHost
	}
	if *dbPort != 0 {
		cfg.DB.Port = *dbPort
	}
	if *dbUser != "" {
		cfg.DB.User = *dbUser
	}
	if *dbName != "" {
		cfg.DB.Database = *dbName
	}
	cfg.DB.Driver = config.DBDriverMSSQL
	cfg.DB.Encrypt = *encrypt
	cfg.DB.TrustServerCertificate = *trust

	if *legacy {
		cfg.LegacyCompat.Enabled = true
		cfg.LegacyCompat.JWTSecret = *legacySecret
		cfg.LegacyCompat.JWTUser = *legacyUser
		cfg.LegacyCompat.JWTPassword = *legacyPass
		cfg.LegacyCompat.AllowRawSQL = *legacyRawSQL
		if cfg.LegacyCompat.JWTExpiryMinutes == 0 {
			cfg.LegacyCompat.JWTExpiryMinutes = 30
		}
	}

	// Never regenerate an existing token: the backend may already hold it.
	if cfg.BearerToken == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			fail("generate bearer token", err)
		}
		cfg.BearerToken = hex.EncodeToString(b)
		fmt.Println("generated a new bearerToken")
	}

	if err := config.Save(cfg); err != nil {
		fail("save config", err)
	}
	cfgPath, _ := paths.ConfigFilePath()
	fmt.Printf("config written: %s\n", cfgPath)
	fmt.Printf("  apiListen=%s erp=%s db=%s:%d/%s user=%s encrypt=%v trust=%v\n",
		cfg.APIListen, cfg.ERP, cfg.DB.Host, cfg.DB.Port, cfg.DB.Database, cfg.DB.User,
		cfg.DB.Encrypt, cfg.DB.TrustServerCertificate)
	fmt.Printf("  legacyCompat.enabled=%v allowRawSQL=%v jwtUser=%s\n",
		cfg.LegacyCompat.Enabled, cfg.LegacyCompat.AllowRawSQL, cfg.LegacyCompat.JWTUser)
	fmt.Printf("  bearerToken=%s\n", cfg.BearerToken)

	if *dbPassword != "" {
		key := "db_password_" + string(cfg.ERP)
		if err := secrets.Set(key, []byte(*dbPassword)); err != nil {
			fail("store db password", err)
		}
		got, err := secrets.Get(key)
		if err != nil {
			fail("verify db password", err)
		}
		if string(got) != *dbPassword {
			fail("verify db password", fmt.Errorf("round-trip mismatch"))
		}
		fmt.Printf("db password stored and verified via DPAPI (key=%s, %d bytes)\n", key, len(got))
	}

	if *queriesFrom != "" {
		target := filepath.Join(paths.DataDir(), "queries.json")
		data, err := os.ReadFile(*queriesFrom)
		if err != nil {
			fail("read source queries", err)
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			fail("write queries.json", err)
		}
		// Parse it back through the real store so a bad import fails here and
		// not at daemon startup.
		store, err := queries.NewStore(target)
		if err != nil {
			fail("validate queries.json", err)
		}
		fmt.Printf("queries imported: %s -> %s (%d queries parsed)\n", *queriesFrom, target, store.Count())
	}
}

func fail(what string, err error) {
	fmt.Fprintf(os.Stderr, "cutover-seed: %s: %v\n", what, err)
	os.Exit(1)
}
