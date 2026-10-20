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
// with the login, the database name and the password filled in. None can
// carry SQL: the login matches loginNameRe, the name passes
// hasavshevet.IsSafeDBName and the password is NewSQLPassword's alphabet, all
// checked again here.
func leastPrivilegeBatches(login, database, password string) ([]string, error) {
	if !loginNameRe.MatchString(login) {
		return nil, fmt.Errorf("login name %q is not safe to write into SQL", login)
	}
	if !hasavshevet.IsSafeDBName(database) {
		return nil, fmt.Errorf("database name %q is not safe to write into SQL", database)
	}
	if password == "" || strings.Trim(password, sqlPasswordAlphabet) != "" {
		return nil, errors.New("the login's password is not from the generated alphabet")
	}
	l := login
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

func (s sqlDatabase) LoginExists(ctx context.Context, login string) (bool, error) {
	names, err := scanNames(ctx, s.conn, `SELECT name FROM sys.server_principals WHERE name = @p1`, login)
	return len(names) > 0, err
}

func (s sqlDatabase) EnsureLeastPrivilegeLogin(ctx context.Context, login, database, password string) error {
	batches, err := leastPrivilegeBatches(login, database, password)
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

// openable is the databases this login can open, the system ones left out.
const openable = `database_id > 4 AND state = 0 AND HAS_DBACCESS(name) = 1`

func (s sqlDatabase) CountDatabases(ctx context.Context) (int, error) {
	var n int
	err := s.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM sys.databases WHERE `+openable).Scan(&n)
	return n, err
}

// Fingerprint names the ERP of database by its tables. Only a name that is
// safe to bracket-quote, and that this login can open, is looked into.
func (s sqlDatabase) Fingerprint(ctx context.Context, database string) (DatabaseFact, error) {
	f := DatabaseFact{Name: database}
	if !hasavshevet.IsSafeDBName(database) {
		return f, fmt.Errorf("%q is not a database name this tool opens", database)
	}
	names, err := scanNames(ctx, s.conn, `SELECT name FROM sys.databases WHERE name = @p1 AND `+openable, database)
	if err != nil {
		return f, err
	}
	if len(names) == 0 {
		return f, fmt.Errorf("%s is not a database this login can open", database)
	}
	var placeholders []string
	var args []any
	for i, t := range fingerprintTables {
		placeholders = append(placeholders, fmt.Sprintf("@t%d", i))
		args = append(args, sql.Named(fmt.Sprintf("t%d", i), t))
	}
	q := fmt.Sprintf("SELECT name FROM [%s].sys.tables WHERE name IN (%s)", database, strings.Join(placeholders, ", "))
	tables, err := scanNames(ctx, s.conn, q, args...)
	if err != nil {
		return f, err
	}
	f.ERP = classify(tables)
	return f, nil
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
