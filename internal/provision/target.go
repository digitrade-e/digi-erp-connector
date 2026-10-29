package provision

import (
	"path/filepath"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
)

// Relative paths under the data directory that Apply writes.
const (
	configFile  = "config.yaml"
	tlsCertFile = "tls/cert.pem"
	tlsKeyFile  = "tls/key.pem"
)

func secretFile(erp config.ERPType) string {
	return filepath.Join("secrets", config.DBPasswordKey(erp)+".bin")
}

// loginKey is the secret store's key for a least-privilege login's password,
// kept apart from the daemon's (secretFile) and out of every pre-image: a
// restore or a rollback never takes it back, since SQL Server knows no older
// password. So a box that restored a config without the login still holds it,
// and the next apply keeps the login instead of taking it over.
func loginKey(login string) string { return "login_" + login }

// buildTarget is the config Apply writes: cur with the spec's fields set and
// everything else — receipts, the signing secret, and every optional spec
// field left out (encrypt, trustServerCertificate, sendOrderDir, the importer,
// each option) — as it was.
// dbUser is the login the connector will use (the least-privilege login, or
// the one the client gave).
func buildTarget(cur config.Config, s Spec, dataDir, dbUser string) config.Config {
	t := cur
	t.ERP = config.ERPType(s.ERP)
	t.APIListen = strings.TrimSpace(s.APIListen)

	server, instance := db.SplitInstance(s.DB.Host)
	t.DB.Driver = config.DBDriverMSSQL
	t.DB.Host = server
	if instance != "" {
		t.DB.Host = server + `\` + instance
	}
	t.DB.Port = s.DB.Port
	t.DB.Database = s.DB.Database
	t.DB.User = dbUser
	if s.DB.Encrypt != nil {
		t.DB.Encrypt = *s.DB.Encrypt
	}
	if s.DB.TrustServerCertificate != nil {
		t.DB.TrustServerCertificate = *s.DB.TrustServerCertificate
	}

	if s.SendOrderDir != nil {
		t.SendOrderDir = strings.TrimSpace(*s.SendOrderDir)
	}
	if imp := s.Importer; imp != nil {
		t.HasExePath, t.HasParamFile, t.HasBatFile = "", "", ""
		switch imp.Mode {
		case "hasExe":
			t.HasExePath = strings.TrimSpace(imp.HasExePath)
			t.HasParamFile = strings.TrimSpace(imp.HasParamFile)
		case "bat":
			t.HasBatFile = strings.TrimSpace(imp.HasBatFile)
		}
	}

	t.Auth.Username = s.Auth.Username
	t.Auth.Password = s.Auth.Password

	if o := s.Options; o != nil {
		if o.ImageFolders != nil {
			t.ImageFolders = make([]string, 0, len(*o.ImageFolders))
			for _, d := range *o.ImageFolders {
				t.ImageFolders = append(t.ImageFolders, filepath.Clean(strings.TrimSpace(d)))
			}
		}
		if o.ERPUser != nil {
			t.ERPUser = strings.TrimSpace(*o.ERPUser)
		}
		if o.Debug != nil {
			t.Debug = *o.Debug
		}
		if o.QueryTimeoutSeconds != nil {
			t.Queries.TimeoutSeconds = *o.QueryTimeoutSeconds
		}
		if o.QueryMaxRows != nil {
			t.Queries.MaxRows = *o.QueryMaxRows
		}
		if o.TokenTTL != nil {
			t.Auth.TokenTTL = strings.TrimSpace(*o.TokenTTL)
		}
	}

	if s.TLS != nil {
		switch s.TLS.Mode {
		case "off":
			t.TLS = config.TLSConfig{}
		case "self-signed":
			t.TLS = config.TLSConfig{
				CertFile: filepath.Join(dataDir, tlsCertFile),
				KeyFile:  filepath.Join(dataDir, tlsKeyFile),
			}
		}
	}
	return t
}
