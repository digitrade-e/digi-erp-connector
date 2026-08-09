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

	"github.com/digitrade-e/digi-erp-connector/internal/auth"
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
	queriesFrom := flag.String("queries-from", "", "import saved queries from this file")
	authUser := flag.String("auth-user", "", "enable POST /auth/token with this username (see -auth-password)")
	authPassword := flag.String("auth-password", "", "password for the credential exchange; blank generates one and prints it")
	authTTL := flag.String("auth-ttl", "", "issued token lifetime (e.g. 30m); blank uses the default")
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

	// Never regenerate an existing token: the backend may already hold it.
	if cfg.BearerToken == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			fail("generate bearer token", err)
		}
		cfg.BearerToken = hex.EncodeToString(b)
		fmt.Println("generated a new bearerToken")
	}

	// The credential exchange, for backends that authenticate with a username and
	// password rather than a static token. Off unless -auth-user is given.
	//
	// Everything is generated here rather than left for the daemon so the operator
	// walks away with the exact values to hand the calling backend; the daemon
	// would otherwise mint the secret silently on first start.
	if *authUser != "" {
		cfg.Auth.Enabled = true
		cfg.Auth.Username = *authUser
		cfg.Auth.TokenTTL = *authTTL

		cfg.Auth.Password = *authPassword
		if cfg.Auth.Password == "" {
			pw, err := auth.NewPassword()
			if err != nil {
				fail("generate auth password", err)
			}
			cfg.Auth.Password = pw
		}
		// Never regenerate an existing secret: tokens already issued verify
		// against it.
		if cfg.Auth.Secret == "" {
			secret, err := auth.NewSecret()
			if err != nil {
				fail("generate auth signing secret", err)
			}
			cfg.Auth.Secret = secret
			fmt.Println("generated this installation's auth signing secret")
		}
		if err := cfg.Auth.Validate(); err != nil {
			fail("auth settings", err)
		}
		if !cfg.Auth.TokenTTLValid() {
			fail("auth settings", fmt.Errorf("-auth-ttl %q is not a positive duration", *authTTL))
		}
	}

	if err := config.Save(cfg); err != nil {
		fail("save config", err)
	}
	cfgPath, _ := paths.ConfigFilePath()
	fmt.Printf("config written: %s\n", cfgPath)
	fmt.Printf("  apiListen=%s erp=%s db=%s:%d/%s user=%s encrypt=%v trust=%v\n",
		cfg.APIListen, cfg.ERP, cfg.DB.Host, cfg.DB.Port, cfg.DB.Database, cfg.DB.User,
		cfg.DB.Encrypt, cfg.DB.TrustServerCertificate)
	fmt.Printf("  bearerToken=%s\n", cfg.BearerToken)
	if cfg.Auth.Enabled {
		fmt.Println("credential exchange enabled at POST /auth/token — give the calling backend:")
		fmt.Printf("  username=%s\n", cfg.Auth.Username)
		fmt.Printf("  password=%s\n", cfg.Auth.Password)
	}

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
