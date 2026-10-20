package provision

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/db"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// leastPrivilegeBatches is scripts/sql/least-privilege-read-node.sql made
// runnable by a program: no GO, no USE — the connection's database is the
// target (the DSN sets it), and each database-scoped batch first checks it —
// with the database name and the password filled in. Neither can carry SQL:
// the name passes hasavshevet.IsSafeDBName and the password is NewSQLPassword's
// alphabet, both checked again here.
func leastPrivilegeBatches(database, password string) ([]string, error) {
	if !hasavshevet.IsSafeDBName(database) {
		return nil, fmt.Errorf("database name %q is not safe to write into SQL", database)
	}
	if password == "" || strings.Trim(password, sqlPasswordAlphabet) != "" {
		return nil, errors.New("the login's password is not from the generated alphabet")
	}
	l := LeastPrivilegeLogin
	inDB := fmt.Sprintf("IF DB_NAME() <> N'%s' THROW 51000, N'digi-erp-connector-provision: connected to the wrong database', 1;\n", database)
	return []string{
		// 1. The login: created with the password, or given it. CHECK_POLICY
		// as in the script; the password satisfies its complexity rule.
		fmt.Sprintf(`IF NOT EXISTS (SELECT 1 FROM sys.server_principals WHERE name = N'%[1]s')
    CREATE LOGIN [%[1]s] WITH PASSWORD = N'%[2]s', CHECK_POLICY = ON, DEFAULT_DATABASE = [%[3]s];
ELSE
    ALTER LOGIN [%[1]s] WITH PASSWORD = N'%[2]s' UNLOCK;
ALTER LOGIN [%[1]s] ENABLE;`, l, password, database),
		// 2. The database user, mapped to the login again when it outlived an
		// earlier login of the same name.
		inDB + fmt.Sprintf(`IF NOT EXISTS (SELECT 1 FROM sys.database_principals WHERE name = N'%[1]s')
    CREATE USER [%[1]s] FOR LOGIN [%[1]s];
ELSE
    ALTER USER [%[1]s] WITH LOGIN = [%[1]s];`, l),
		// 3. Read access: SELECT on every table, nothing else.
		inDB + fmt.Sprintf(`ALTER ROLE [db_datareader] ADD MEMBER [%s];`, l),
		// 4. The price & stock procedures run with their own rights.
		inDB + fmt.Sprintf(`IF OBJECT_ID(N'dbo.GPRICE_Bulk', N'P') IS NOT NULL GRANT EXECUTE ON [dbo].[GPRICE_Bulk] TO [%[1]s];
IF OBJECT_ID(N'dbo.GPRICE_BulkJson', N'P') IS NOT NULL GRANT EXECUTE ON [dbo].[GPRICE_BulkJson] TO [%[1]s];
IF OBJECT_ID(N'dbo.GetOnHandStockForSkus', N'P') IS NOT NULL GRANT EXECUTE ON [dbo].[GetOnHandStockForSkus] TO [%[1]s];`, l),
		// 5. Writes denied outright, so a later role membership cannot add them.
		inDB + fmt.Sprintf(`DENY INSERT, UPDATE, DELETE, ALTER, CREATE TABLE TO [%s];`, l),
	}, nil
}

// fingerprintTables are the tables Databases looks for.
var fingerprintTables = []string{"Accounts", "Stock", "Rates", "OITM", "OCRD"}

// maxFingerprinted bounds how many databases Databases opens.
const maxFingerprinted = 50

// classify names the ERP a database's tables point to.
func classify(tables []string) string {
	has := map[string]bool{}
	for _, t := range tables {
		has[strings.ToLower(t)] = true
	}
	switch {
	case has["accounts"] && (has["stock"] || has["rates"]):
		return "hasavshevet"
	case has["oitm"] && has["ocrd"]:
		return "sap"
	}
	return ""
}

// sqlDatabase is Database over database/sql, one connection at a time: the
// database-scoped batches all run in the session the DSN opened.
type sqlDatabase struct {
	conn *sql.DB
}

// OpenDatabase opens cfg's database with password and checks it answers
// (db.Open).
func OpenDatabase(cfg config.Config, password string) (Database, error) {
	conn, err := db.Open(cfg, password, db.DefaultOptions())
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	return sqlDatabase{conn: conn}, nil
}

func (s sqlDatabase) Ping(ctx context.Context) error { return db.Ping(ctx, s.conn, 0) }

func (s sqlDatabase) Close() error { return s.conn.Close() }

func (s sqlDatabase) EnsureProcedures(ctx context.Context) ([]hasavshevet.ProcedureResult, error) {
	return hasavshevet.EnsureProcedures(ctx, s.conn)
}

func (s sqlDatabase) LoginExists(ctx context.Context) (bool, error) {
	names, err := scanNames(ctx, s.conn, `SELECT name FROM sys.server_principals WHERE name = @p1`, LeastPrivilegeLogin)
	return len(names) > 0, err
}

func (s sqlDatabase) EnsureLeastPrivilegeLogin(ctx context.Context, database, password string) error {
	batches, err := leastPrivilegeBatches(database, password)
	if err != nil {
		return err
	}
	for i, b := range batches {
		if _, err := s.conn.ExecContext(ctx, b); err != nil {
			return fmt.Errorf("least-privilege step %d of %d: %w", i+1, len(batches), err)
		}
	}
	return nil
}

func (s sqlDatabase) Databases(ctx context.Context) ([]DatabaseFact, error) {
	rows, err := s.conn.QueryContext(ctx,
		`SELECT name FROM sys.databases WHERE database_id > 4 AND state = 0 AND HAS_DBACCESS(name) = 1 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(names) > maxFingerprinted {
		names = names[:maxFingerprinted]
	}

	var placeholders []string
	var args []any
	for i, t := range fingerprintTables {
		placeholders = append(placeholders, fmt.Sprintf("@t%d", i))
		args = append(args, sql.Named(fmt.Sprintf("t%d", i), t))
	}
	out := make([]DatabaseFact, 0, len(names))
	for _, n := range names {
		f := DatabaseFact{Name: n}
		if strings.EqualFold(n, "SBO-COMMON") {
			f.ERP = "sap-common"
		}
		// Only a name that is safe to bracket-quote is opened; others are
		// listed as they are.
		if hasavshevet.IsSafeDBName(n) {
			q := fmt.Sprintf("SELECT name FROM [%s].sys.tables WHERE name IN (%s)", n, strings.Join(placeholders, ", "))
			if tables, err := scanNames(ctx, s.conn, q, args...); err == nil {
				f.ERP = classify(tables)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

func scanNames(ctx context.Context, conn *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := conn.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
