# Agent Guidelines — digi-erp-connector

> Project memory lives in `.claude/docs/` — origin story, decision log,
> implementation history, operations runbook, and migration records. Read
> `.claude/docs/README.md` first when picking up this project.

## What this is

A Go application providing a local HTTP REST API gateway between the main app and ERP systems. It is the merge of two predecessor repos:

- **erp-connector** (Go) — supplied the architecture and all ERP features.
- **electron-mssql-app** (Node/Electron, legacy) — supplied the SQL access model: **named saved queries**. The backend never sends raw SQL; it registers queries by name via CRUD and executes them with parameters.

Two binaries:
- **`digi-erp-connectord`** — background daemon / Windows service; runs the REST API server
- **`digi-erp-connector`** — Windows-only native UI (`walk` library) for config management and service control

**Supported ERPs:** Hasavshevet (complete), SAP (price/stock query implemented), Priority (not implemented)
**Primary target OS:** Windows. Linux supported for daemon only.

## Project structure

```
cmd/
  digi-erp-connector/   ← Windows GUI (walk library)
  digi-erp-connectord/  ← Background daemon/service

internal/
  api/
    server.go           ← HTTP mux, route registration, rate limiter, timeouts
    handlers/           ← One handler per endpoint
      health.go         ← GET /api/health
      custom_sql.go     ← Saved-query CRUD (/api/custom_sql...)
      sql_queries.go    ← GET /api/sqlqueries/{name} (saved-query runner)
      folders.go        ← GET /api/folders/list
      file.go           ← POST /api/file (path-safe file streaming)
      price_stock.go    ← POST /api/priceAndStockHandler
      send_order.go     ← POST /api/sendOrder (async queue)
      send_order_status.go ← GET /api/sendOrder/{jobId}
    middleware/
      auth.go           ← Bearer token validation (constant-time compare)
      ratelimit.go      ← Per-IP token bucket (429 RATE_LIMITED)
      logging.go        ← Request/response logging (no secrets)
    utils/responses.go  ← WriteJSON / WriteError helpers
    dto/                ← Request/response structs per endpoint
  queries/              ← Saved-query subsystem (THE data-access model)
    store.go            ← JSON registry (queries.json, atomic writes, mutex)
    binder.go           ← Param binding: forced strings, type inference, int hints
    runner.go           ← Execution: timeout, row cap, multi-recordset, DML exec
  config/               ← YAML config (atomic write, 0o600)
  db/                   ← MSSQL pool
  erp/hasavshevet/      ← Complete order pipeline (IMOVEIN, queue, GPRICE)
  erp/sap/              ← SAP B1 price/stock query
  files/                ← Path traversal prevention
  logger/               ← LoggerService interface
  pdf/                  ← headless-Chrome PDF (file:// temp HTML)
  print/                ← PDFtoPrinter → Acrobat → Sumatra engine chain
  email/                ← SMTP sender
  platform/             ← autostart (Windows service), paths
  secrets/              ← Windows DPAPI (machine scope)
```

Data dir: `%PROGRAMDATA%\digi-erp-connector\` (config.yaml, queries.json, server.log, ui.log, secrets/). Linux: `/etc/digi-erp-connector/`.

## API endpoints (all require `Authorization: Bearer <token>`; all rate-limited)

| Route | Method | What it does |
|-------|--------|--------------|
| `/api/health` | GET | Pings DB; `{"status":"ok"}` or 503 |
| `/api/custom_sql` | POST | Create saved query `{name, description, sql, params}` (409 on duplicate) |
| `/api/create_custom_sql` | POST | Legacy alias of the above (electron-mssql-app compat) |
| `/api/custom_sql` | GET | List saved queries |
| `/api/custom_sql/{name}` | GET / PATCH / DELETE | Fetch / partial-update / delete one |
| `/api/sqlqueries/{name}` | GET | **Execute saved query.** Stored default params merged with query-string (URL wins), typed binding, returns rows + recordsets + legacy `value`/`rowsAffected` |
| `/api/folders/list` | GET | Configured image folders with file lists |
| `/api/file` | POST | Path-validated binary file streaming |
| `/api/priceAndStockHandler` | POST | Hasavshevet or SAP price/stock fetch |
| `/api/sendOrder` | POST | Validates order, enqueues, `202 + jobId` |
| `/api/sendOrder/{jobId}` | GET | Poll job status (queued/running/done/failed) |

## Saved-query model — hard constraints, NEVER bypass

- **There is NO raw-SQL endpoint.** The legacy `POST /api/sql` was deliberately deleted; never add an endpoint that accepts SQL text for immediate execution. SQL enters the system only through the CRUD store.
- **Param binding only** — every request value binds via `sql.Named()` in `internal/queries/binder.go`; no string concatenation into SQL, ever.
- **Forced-string params:** `skuArray`, `warehouse`, `sku`, `syncKey` always bind as strings (never coerced to numbers) — SAP WhsCodes and SKUs can look numeric.
- **Type inference** for query-string values (electron compat): all-digits → int64, decimal → float64, `YYYY-MM-DD…` → datetime, else string. Integer hints from `TOP(@x)`/`OFFSET @x ROWS`/`FETCH NEXT @x ROWS`.
- Saved queries are trusted (operator/backend-managed) and MAY contain writes/EXEC — that is by design; the trust boundary is the bearer token + CRUD store, not a keyword filter.
- **Row limit** default 10,000; **execution timeout** default 30s (config `queries.maxRows` / `queries.timeoutSeconds`); CRUD body limit 1 MiB.
- `queries.json` format stays drop-in compatible with electron-mssql-app `custom_sql_data.json` (import = copy the file).

## File endpoint hard constraints — NEVER bypass

- `folderPath` must exactly match (after canonicalization) a configured `imageFolders` entry
- `fileName` must not contain `.`, `..`, or absolute paths
- Final path re-validated with `filepath.Rel` and symlink re-resolution — `ResolveFilePath()` in `internal/files/files.go`

## Authentication

All routes: `middleware/auth.go` validates `Authorization: Bearer <token>` against config `BearerToken` using `subtle.ConstantTimeCompare`. Token never logged. Rate limiting (`middleware/ratelimit.go`) runs before auth.

## Hasavshevet send-order flow

1. `OrderQueue.Submit(req)` reserves the order number (`lastOrderNumber.json`, mutex) → 202 + jobId (= order number)
2. Single worker builds IMOVEIN.doc/.prm (fixed-width 2891-byte records, Windows-1255), writes history copies, runs `has.exe` or `digi.bat`
3. Post-order hooks: remote-template PDF via headless Chrome → optional print + email (never fail the order)

**Single-worker constraint:** never make the order queue concurrent — IMOVEIN files are shared.

## Tests

`go test ./...` — table-driven, stdlib only, `t.TempDir()` for FS, `httptest` for HTTP.
Key suites: `internal/queries` (binder inference, forced strings, store CRUD + electron format compat, DML detection), `handlers/send_order_test.go`, `files_test.go`, `imovein_test.go` (2891-byte layout), `order_number_test.go`, `pdf/remote_test.go`.

## Build

```bash
go build -o digi-erp-connectord ./cmd/digi-erp-connectord   # daemon (cross-platform)
go build -o digi-erp-connector.exe ./cmd/digi-erp-connector # GUI (Windows only)
```

## Prohibited (zero exceptions)

- Adding any endpoint that executes SQL text straight from a request body
- Storing secrets in logs (DB password, bearer token, credentials)
- Disabling auth or rate limiting "for testing" on any route
- Returning raw DB driver errors to API clients
- Absolute-path `fileName` values in the file endpoint
- Making the OrderQueue worker concurrent
- Business logic in `cmd/` — belongs in `internal/`

## Known AI Failure Patterns (inherited from erp-connector — do not repeat)

### PDF generation
- ❌ `data:text/html;base64` navigation URLs — Chrome blocks embedded data: images; always write a temp file and navigate `file://`
- ❌ Typing a `data:` URI as `string` in html/template — becomes `#ZgotmplZ`; must be `template.URL`

### PDF printing (read `docs/printing.md` first)
- ❌ Trusting SumatraPDF `-silent` exit code — returns 0 even when nothing prints
- ❌ Adobe Reader `/t` from a service — hangs in session 0
- ❌ Printers on `WSD-*` ports from the daemon — jobs vanish; require Standard TCP/IP ports
- ❌ Removing `PDFtoPrinter.exe` / `qpdf29.dll` / `resource.dat` from installer or release workflow

### SQL safety
- ❌ Re-introducing a raw-SQL execution endpoint "for debugging"
- ❌ String-concatenating user input into SQL — always `sql.Named()` via the shared binder

### File path security
- ❌ `filepath.Join` on user paths without canonical checks — use `ResolveFilePath()`

## Migration notes

- Legacy electron-mssql-app installations: import their `custom_sql_data.json` by copying it to `%PROGRAMDATA%\digi-erp-connector\queries.json`.
- Legacy erp-connector installations can run side-by-side (different service name, data dir, installer AppId); seed `lastOrderNumber.json` from the old install to keep order-number continuity.
- The backend must call `GET /api/sqlqueries/{name}` — `POST /api/sql` does not exist here.
