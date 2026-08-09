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
    main.go             ← entry point only
    form.go             ← widget declaration + per-widget helpers
    form_config.go      ← form ↔ config.Config mapping, persistence
    actions.go          ← configuration button handlers
    service_control.go  ← service start/stop/restart, daemon discovery
  digi-erp-connectord/  ← Background daemon/service
  cutover-seed/         ← operator tool: seed config/secret/queries for a
                          replacement install (not part of the service)

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
    respond/            ← JSON / Error — the one error envelope (was "utils")
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
  legacyauth/           ← HS256 JWT for the electron compat exchange
  platform/
    autostart/          ← Windows service registration/control
    paths/              ← data dir + config path (PROGRAMDATA-based)
    atomicfile/         ← THE way to write a file: temp + sync + rename
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

### Legacy compatibility routes (only when `legacyCompat.enabled`)

Off by default. Present so a backend written against the old Node connector works
unchanged during migration; all reply in the **old app's error shape**
(`{"error":"snake_case"}`). Full detail: `docs/legacy-compat.md`.

The live backend is **erp-manager**, and it authenticates through `/auth/token`
rather than the static bearer token — so `legacyCompat.enabled: false` cuts it off.
What it sends, the six routes it calls, the response keys it depends on (`value` on
saved queries, a bare array on `custom_sql`) and its 401-triggered re-login are
documented in `docs/erp-manager-integration.md`. Read that before changing any
response shape.

| Route | Method | What it does |
|-------|--------|--------------|
| `/auth/token` | POST | Credentials → HS256 JWT (unauthenticated, rate-limited). Auth then accepts that JWT **or** the static bearer token everywhere. |
| `/api/ping` | GET | `{"ok":true,"ts":<ms>}`, no DB touch |
| `/api/test-connection` | POST | Try supplied `{mssql:{...}}` settings |
| `/api/customers` | GET | Old sample route over `dbo.Items` |
| `/api/orders/{id}` | GET | Old sample route over `dbo.Items` |
| `/api/query` | POST | Ad-hoc read-only SQL — **only** with `allowRawSQL: true` |

## Saved-query model — hard constraints, NEVER bypass

- **There is NO raw-SQL endpoint by default.** The legacy `POST /api/sql` was deliberately deleted; never add an endpoint that accepts SQL text for immediate execution. SQL enters the system only through the CRUD store. The **one** exception is `POST /api/query`, which exists solely for electron-mssql-app compatibility, is absent unless `legacyCompat.allowRawSQL` is true, is read-only validated (`queries.ValidateReadOnly`), fully parameter-bound and logged on every call — see `docs/legacy-compat.md`. Do not widen it, and do not add a second one.
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
3. The queue supports optional PostOrderHook implementations (none registered — the PDF/print/email hook was removed 2026-07-20; hook errors must never fail the order)

**Single-worker constraint:** never make the order queue concurrent — IMOVEIN files are shared.

## Tests

`go test ./...` — table-driven, stdlib only, `t.TempDir()` for FS, `httptest` for HTTP.
Key suites: `internal/queries` (binder inference, forced strings, store CRUD + electron format compat, DML detection), `handlers/send_order_test.go`, `files_test.go`, `imovein_test.go` (2891-byte layout), `order_number_test.go`, `cmd/digi-erp-connector/form_config_test.go` (the GUI's save-time guards — `//go:build windows`, so it runs in CI and on a Windows box, not on Linux).

## Build

```bash
go build -o digi-erp-connectord ./cmd/digi-erp-connectord   # daemon (cross-platform)
go build -o digi-erp-connector.exe ./cmd/digi-erp-connector # GUI (Windows only)
```

## Wire-compatibility constraints (a live backend depends on these)

`internal/queries/runner.go` deliberately reshapes scanned values to match the old
Node driver's JSON. These are not stylistic choices — changing them breaks a
production backend. Covered by `queries/normalize_test.go`:

- datetimes render as `2026-03-08T00:00:00.000Z` (UTC, three fractional digits)
- `DECIMAL`/`NUMERIC`/`MONEY`/`SMALLMONEY` render as JSON **numbers** in shortest
  form (`13085`, never `"13085.00"`)
- the saved-query response carries both envelopes: native
  `api/status/rowCount/rows/recordsets` **and** legacy `value`/`rowsAffected`
- `queries.maxRows` is a real functional limit, not just a safety valve — the b4l
  box needs 100000 because a production query returns 16183 rows

## House rules (introduced by the 2026-08-05 refactor)

- **Never hand-roll a file write.** `internal/platform/atomicfile.Write` is the
  only way config.yaml, queries.json and secrets are written. Three separate
  temp+rename implementations had drifted apart, and one of them reported
  success when the rename failed.
- **Decode request bodies with `decodeJSONBody`** (handlers/json.go): it applies
  the shared 1 MiB limit, rejects trailing data and keeps numbers exact. Do not
  reintroduce per-handler decoders or per-handler size constants.
- **`respond.JSON` / `respond.Error` only** — no direct `json.NewEncoder(w)` in
  handlers. Legacy compat routes are the documented exception and use
  `writeLegacyError`.
- **`readFormConfig` must start from `f.cfg`**, never a zero `config.Config`.
  Settings with no widget (queries limits, db.encrypt, hasExePath) would
  otherwise be wiped by a GUI save. `legacyCompat` was the reason this rule
  exists; it now has widgets of its own, and `readLegacyCompat` starts from the
  loaded block for the same reason.
- **The GUI must not be able to save a config the daemon refuses to start on.**
  `validateLegacyCompat` mirrors the `api.NewServer` precondition (enabled needs
  jwtSecret + jwtUser + jwtPassword) at save time, and `confirmLegacyDisable`
  prompts before the compat layer is switched off, because that single click cuts
  erp-manager off on the next restart. Any new startup precondition that a widget
  can violate needs the same treatment.
- **Keep the tree gofmt-clean.** `.gitattributes` pins `.go` to LF; if
  `gofmt -l ./cmd ./internal` prints anything, fix it rather than adding to it.

## Prohibited (zero exceptions)

- Adding any endpoint that executes SQL text straight from a request body (the
  config-gated, read-only-validated `POST /api/query` compat route is the sole
  pre-existing exception — do not add another, do not widen it)
- Storing secrets in logs (DB password, bearer token, credentials)
- Disabling auth or rate limiting "for testing" on any route
- Returning raw DB driver errors to API clients
- Absolute-path `fileName` values in the file endpoint
- Making the OrderQueue worker concurrent
- Business logic in `cmd/` — belongs in `internal/`

## Removed features (do not resurrect casually)

The PDF/print/email subsystem (`internal/pdf`, `internal/print`,
`internal/email`, the post-order PDF hook, the GUI "PDF & Email Settings"
dialog, `PDFConfig`/`SMTPConfig`, PDFtoPrinter installer bundling) was
deleted on 2026-07-20 by user decision. If it ever comes back, recover it
from git history AND read erp-connector's `docs/printing.md` first — the
print path had hard-won session-0/WSD-port constraints.

## Known AI Failure Patterns (inherited from erp-connector — do not repeat)

### SQL safety
- ❌ Re-introducing a raw-SQL execution endpoint "for debugging"
- ❌ String-concatenating user input into SQL — always `sql.Named()` via the shared binder

### File path security
- ❌ `filepath.Join` on user paths without canonical checks — use `ResolveFilePath()`

## Migration notes

- Legacy electron-mssql-app installations: import their `custom_sql_data.json` by copying it to `%PROGRAMDATA%\digi-erp-connector\queries.json`.
- Legacy erp-connector installations can run side-by-side (different service name, data dir, installer AppId); seed `lastOrderNumber.json` from the old install to keep order-number continuity.
- The backend must call `GET /api/sqlqueries/{name}` — `POST /api/sql` does not exist here.
