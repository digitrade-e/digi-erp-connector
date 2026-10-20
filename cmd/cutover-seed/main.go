// Command cutover-seed writes config.yaml, stores the DB password via DPAPI and
// imports saved queries — using the connector's own config/secrets/queries code
// so the on-disk formats are guaranteed to match what the daemon reads.
//
// Built for the electron-mssql-app → digi-erp-connector cutover and kept for the
// next one: it makes provisioning a replacement install reproducible and
// scriptable. The GUI remains the supported way to configure an installation
// interactively; this is an operator tool, not part of the service.
//
// -receipt-seed N is a separate mode: it only creates the receipt number store
// (DataDir/receipts/receiptNumber.json) with N as the next receipt number, and
// refuses to run alongside any other flag.
//
// Note it takes the DB password as a flag, so it lands in shell history and is
// briefly visible in the process list. Acceptable on a box where you are already
// reading the old app's plaintext config; do not use it on shared machines.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/auth"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/paths"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
	"github.com/digitrade-e/digi-erp-connector/internal/secrets"
)

func main() {
	e := env{
		dataDir:    paths.DataDir(),
		loadConfig: config.LoadOrDefault,
		saveConfig: config.Save,
		out:        os.Stdout,
	}
	if err := run(os.Args[1:], e); err != nil {
		fmt.Fprintf(os.Stderr, "cutover-seed: %v\n", err)
		os.Exit(1)
	}
}

// env is what run touches outside its arguments, so a test can point it at a
// temp directory and see whether config.yaml would have been written.
type env struct {
	dataDir    string
	loadConfig func() (config.Config, error)
	saveConfig func(config.Config) error
	out        io.Writer
}

func run(args []string, e env) error {
	fs := flag.NewFlagSet("cutover-seed", flag.ContinueOnError)
	listen := fs.String("listen", "", "apiListen host:port (e.g. [::]:8082)")
	erp := fs.String("erp", "hasavshevet", "erp type")
	dbHost := fs.String("db-host", "", "db host")
	dbPort := fs.Int("db-port", 0, "db port")
	dbUser := fs.String("db-user", "", "db user")
	dbName := fs.String("db-name", "", "db database")
	dbPassword := fs.String("db-password", "", "db password (stored via DPAPI, not in yaml)")
	encrypt := fs.Bool("encrypt", false, "set db.encrypt")
	trust := fs.Bool("trust", false, "set db.trustServerCertificate")
	queriesFrom := fs.String("queries-from", "", "import saved queries from this file")
	authUser := fs.String("auth-user", "", "username the calling backend posts to /auth/token")
	authPassword := fs.String("auth-password", "", "its password; blank generates one and prints it")
	authTTL := fs.String("auth-ttl", "", "issued token lifetime (e.g. 30m); blank uses the default")
	receiptSeed := fs.Int64("receipt-seed", 0,
		"create the receipt number store with N as the NEXT receipt number (N > 0); only when no store exists. "+
			"A mode of its own: touches nothing else")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The receipt seed is its own mode. Run with the provisioning flags it
	// would also re-save config.yaml with their defaults (db.encrypt=false,
	// …) on a box that only wanted a seed.
	//
	// The mode is chosen by whether the flag was GIVEN, not by its value:
	// "-receipt-seed 0" (an empty script variable) must be refused as a seed,
	// not fall through to provisioning and rewrite config.yaml.
	if flagGiven(fs, "receipt-seed") {
		if others := otherFlagsSet(fs, "receipt-seed"); len(others) > 0 {
			return fmt.Errorf("receipt seed: -receipt-seed runs alone; also given: -%s", strings.Join(others, ", -"))
		}
		if err := seedReceipts(e.dataDir, *receiptSeed); err != nil {
			return fmt.Errorf("receipt seed: %w", err)
		}
		fmt.Fprintf(e.out, "receipt number store created: %s — the next receipt is %d\n",
			hasavshevet.ReceiptNumberStorePath(e.dataDir), *receiptSeed)
		return nil
	}

	cfg, err := e.loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
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

	// The installation's only credential. Everything is generated here rather
	// than left for the daemon so the operator walks away with the exact values
	// to hand the calling backend; the daemon would otherwise mint the secret
	// silently on first start.
	if *authUser != "" {
		cfg.Auth.Username = *authUser
	}
	if *authTTL != "" {
		cfg.Auth.TokenTTL = *authTTL
	}
	if *authPassword != "" {
		cfg.Auth.Password = *authPassword
	}
	if cfg.Auth.Password == "" {
		pw, err := auth.NewPassword()
		if err != nil {
			return fmt.Errorf("generate auth password: %w", err)
		}
		cfg.Auth.Password = pw
	}
	// Never regenerate an existing secret: tokens already issued verify against it.
	if cfg.Auth.Secret == "" {
		secret, err := auth.NewSecret()
		if err != nil {
			return fmt.Errorf("generate auth signing secret: %w", err)
		}
		cfg.Auth.Secret = secret
		fmt.Fprintln(e.out, "generated this installation's signing secret")
	}
	if err := cfg.Auth.Validate(); err != nil {
		return fmt.Errorf("auth settings: %w — pass -auth-user", err)
	}
	if !cfg.Auth.TokenTTLValid() {
		return fmt.Errorf("auth settings: -auth-ttl %q is not a positive duration", *authTTL)
	}

	if err := e.saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	cfgPath, _ := paths.ConfigFilePath()
	fmt.Fprintf(e.out, "config written: %s\n", cfgPath)
	fmt.Fprintf(e.out, "  apiListen=%s erp=%s db=%s:%d/%s user=%s encrypt=%v trust=%v\n",
		cfg.APIListen, cfg.ERP, cfg.DB.Host, cfg.DB.Port, cfg.DB.Database, cfg.DB.User,
		cfg.DB.Encrypt, cfg.DB.TrustServerCertificate)
	fmt.Fprintln(e.out, "give the calling backend these — it posts them to /auth/token:")
	fmt.Fprintf(e.out, "  username=%s\n", cfg.Auth.Username)
	fmt.Fprintf(e.out, "  password=%s\n", cfg.Auth.Password)

	if *dbPassword != "" {
		key := "db_password_" + string(cfg.ERP)
		if err := secrets.Set(key, []byte(*dbPassword)); err != nil {
			return fmt.Errorf("store db password: %w", err)
		}
		got, err := secrets.Get(key)
		if err != nil {
			return fmt.Errorf("verify db password: %w", err)
		}
		if string(got) != *dbPassword {
			return errors.New("verify db password: round-trip mismatch")
		}
		fmt.Fprintf(e.out, "db password stored and verified via DPAPI (key=%s, %d bytes)\n", key, len(got))
	}

	if *queriesFrom != "" {
		target := filepath.Join(e.dataDir, "queries.json")
		data, err := os.ReadFile(*queriesFrom)
		if err != nil {
			return fmt.Errorf("read source queries: %w", err)
		}
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return fmt.Errorf("write queries.json: %w", err)
		}
		// Parse it back through the real store so a bad import fails here and
		// not at daemon startup.
		store, err := queries.NewStore(target)
		if err != nil {
			return fmt.Errorf("validate queries.json: %w", err)
		}
		fmt.Fprintf(e.out, "queries imported: %s -> %s (%d queries parsed)\n", *queriesFrom, target, store.Count())
	}
	return nil
}

// seedReceipts creates the receipt number store with next as the first
// receipt number. The store writes only when no store file exists, so a seed
// can never lower the numbering or paper over a damaged store: an existing
// one is an error, left exactly as it was.
//
// Pick N in a range the legacy writer cannot reach (plan 005 Q-J6), and seed
// before the first receipt — an unseeded connector answers sendReceipt 503
// RECEIPT_NUMBER_STORE_UNAVAILABLE rather than start at 1.
func seedReceipts(dataDir string, next int64) error {
	if next <= 0 {
		return fmt.Errorf("-receipt-seed must be greater than 0, got %d", next)
	}
	return hasavshevet.NewReceiptNumberStore(hasavshevet.ReceiptNumberStorePath(dataDir)).Seed(next)
}

// flagGiven reports whether name was given on the command line, whatever its
// value.
func flagGiven(fs *flag.FlagSet, name string) bool {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
}

// otherFlagsSet lists the flags given on the command line other than except.
func otherFlagsSet(fs *flag.FlagSet, except string) []string {
	var out []string
	fs.Visit(func(f *flag.Flag) {
		if f.Name != except {
			out = append(out, f.Name)
		}
	})
	return out
}
