# Saved Queries — the data-access model

digi-erp-connector deliberately has **no raw-SQL endpoint**. The legacy
`POST /api/sql` from erp-connector was removed; the electron-mssql-app model
replaced it: SQL text lives on the connector, the backend executes by name.

## Lifecycle

1. The backend (or an operator) registers a query:

```http
POST /api/custom_sql
Authorization: Bearer <token>
Content-Type: application/json

{
  "name": "stock_by_sku",
  "description": "on-hand stock for a CSV of SKUs",
  "sql": "SELECT * FROM dbo.Stock WHERE ItemCode IN (SELECT value FROM STRING_SPLIT(@skuArray, ',')) AND WhsCode = @warehouse",
  "params": { "skuArray": "", "warehouse": "10" }
}
```

`params` are **default values**. Duplicate names → `409 NAME_CONFLICT`.
`POST /api/create_custom_sql` is a legacy alias with the same body.

2. The backend executes it by name; query-string parameters override defaults:

```http
GET /api/sqlqueries/stock_by_sku?skuArray=1001,1002&warehouse=12
```

Response (erp-connector envelope + electron-mssql-app compat fields):

```json
{
  "api": "/api/sqlqueries/stock_by_sku",
  "status": "success",
  "name": "stock_by_sku",
  "rowCount": 2,
  "rows": [ ... ],
  "recordsets": [ [ ... ] ],
  "value": [ ... ],
  "rowsAffected": [2]
}
```

3. Manage with `GET /api/custom_sql` (list), `GET/PATCH/DELETE /api/custom_sql/{name}`.

## Parameter binding (internal/queries/binder.go)

Everything binds via `sql.Named()` — never string concatenation.

- **Forced strings** (never coerced to numbers): `skuArray` (CSV for
  `STRING_SPLIT`), `warehouse`, `sku`, `syncKey` (SAP WhsCode/SKU codes can
  look numeric).
- **Query-string inference** (electron compat): `^\d+$` → int64,
  `^\d+\.\d+$` → float64, `YYYY-MM-DD…` parseable → datetime, else string.
- **Integer hints**: `TOP(@x)`, `OFFSET @x ROWS`, `FETCH NEXT @x ROWS ONLY`
  force string values of `@x` to bind as integers.
- JSON default values keep their JSON types (integral floats → int64).

## Execution (internal/queries/runner.go)

- Timeout: `queries.timeoutSeconds` (default 30s) → `504 SQL_TIMEOUT`
- Row cap across all recordsets: `queries.maxRows` (default 10000) → `413 SQL_ROW_LIMIT`
- Multiple recordsets supported (`recordsets` array)
- Plain `INSERT`/`UPDATE`/`DELETE`/`MERGE` without an `OUTPUT` clause run via
  Exec and report the real affected-row count; everything else (including
  `EXEC proc`, `WITH`, `OUTPUT`) runs via Query and returns rows
- DB errors are never leaked: generic `500 DB_ERROR`

## Trust model

Saved queries are **trusted** — they are managed by the authenticated backend
or an operator, so they may contain writes or `EXEC`. The security boundary is
the bearer token + the CRUD store, not a keyword filter. This mirrors the
electron-mssql-app design ("Keep your saved queries trusted").

Consequences:
- Guard the bearer token like a DB credential.
- Keep `apiListen` on `127.0.0.1` unless there is a reason not to.
- Rate limiting (25 rps / burst 50 per IP) applies before auth.

## Storage & migration

`%PROGRAMDATA%\digi-erp-connector\queries.json` — atomic writes, 0600, format
identical to electron-mssql-app `custom_sql_data.json`:

```json
{ "<name>": { "description": "...", "sql": "...", "params": { } } }
```

To migrate an electron installation, copy its `custom_sql_data.json` (from the
Electron userData dir) to the path above and restart the daemon.
