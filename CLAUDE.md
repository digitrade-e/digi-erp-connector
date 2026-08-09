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
      auth.go           ← POST /auth/token (the credential check), GET /api/ping
      custom_sql.go     ← Saved-query CRUD (/api/custom_sql...)
      sql_queries.go    ← GET /api/sqlqueries/{name} (saved-query runner)
      folders.go        ← GET /api/folders/list
      file.go           ← POST /api/file (path-safe file streaming)
      price_stock.go    ← POST /api/priceAndStockHandler
      send_order.go     ← POST /api/sendOrder (async queue)
      send_order_status.go ← GET /api/sendOrder/{jobId}
    middleware/
      auth.go           ← verifies the issued token on every route
      ratelimit.go      ← Per-IP token bucket (429 RATE_LIMITED)
      logging.go        ← Request/response logging (no secrets)
    respond/            ← JSON / Error — the one error envelope
    dto/                ← Request/response structs per endpoint
  queries/              ← Saved-query subsystem (THE data-access model)
    store.go            ← JSON registry (queries.json, atomic writes, mutex)
    binder.go           ← Param binding: forced strings, type inference, int hints
    runner.go           ← Execution: timeout, row cap, multi-recordset, DML exec
  auth/                 ← HS256 sign/verify + secret/password generation (no deps)
  config/               ← YAML config (atomic write, 0o600)
  db/                   ← MSSQL pool
  erp/hasavshevet/      ← Complete order pipeline (IMOVEIN, queue, GPRICE)
  erp/sap/              ← SAP B1 price/stock query
  files/                ← Path traversal prevention
  logger/               ← LoggerService interface
  platform/
    autostart/          ← Windows service registration/control
    paths/              ← data dir + config path (PROGRAMDATA-based)
    atomicfile/         ← THE way to write a file: temp + sync + rename
  secrets/              ← Windows DPAPI (machine scope)
```

Data dir: `%PROGRAMDATA%\digi-erp-connector\` (config.yaml, queries.json, server.log, ui.log, secrets/). Linux: `/etc/digi-erp-connector/`.

## API endpoints (all `/api/*` require `Authorization: Bearer <token from /auth/token>`; all rate-limited)

| Route | Method | What it does |
|-------|--------|--------------|
| `/auth/token` | POST | **Unauthenticated** (it is the credential check). Username+password → short-lived token. The only way in. |
| `/api/health` | GET | Pings DB; `{"status":"ok"}` or 503 |
| `/api/ping` | GET | Liveness + credential check; **no DB touch**. `{"ok":true,"ts":<epoch ms>}` |
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

- **There is NO raw-SQL endpoint.** The legacy `POST /api/sql` was deleted before the first release, and the electron-compatibility `POST /api/query` was deleted on 2026-08-09. Never add an endpoint that accepts SQL text for immediate execution. SQL enters the system only through the CRUD store.
- **Param binding only** — every request value binds via `sql.Named()` in `internal/queries/binder.go`; no string concatenation into SQL, ever.
- **Forced-string params:** `skuArray`, `warehouse`, `sku`, `syncKey` always bind as strings (never coerced to numbers) — SAP WhsCodes and SKUs can look numeric.
- **Type inference** for query-string values (electron compat): all-digits → int64, decimal → float64, `YYYY-MM-DD…` → datetime, else string. Integer hints from `TOP(@x)`/`OFFSET @x ROWS`/`FETCH NEXT @x ROWS`.
- Saved queries are trusted (operator/backend-managed) and MAY contain writes/EXEC — that is by design; the trust boundary is the credential + CRUD store, not a keyword filter.
- **Row limit** default 10,000; **execution timeout** default 30s (config `queries.maxRows` / `queries.timeoutSeconds`); CRUD body limit 1 MiB.
- `queries.json` format stays drop-in compatible with electron-mssql-app `custom_sql_data.json` (import = copy the file).

## File endpoint hard constraints — NEVER bypass

- `folderPath` must exactly match (after canonicalization) a configured `imageFolders` entry
- `fileName` must not contain `.`, `..`, or absolute paths
- Final path re-validated with `filepath.Rel` and symlink re-resolution — `ResolveFilePath()` in `internal/files/files.go`

## Authentication

**One credential, one scheme.** A caller posts the configured username and password to `POST /auth/token`, gets a short-lived HS256 token, and sends it as `Authorization: Bearer <token>` on every `/api/*` route. `middleware/auth.go` verifies the signature and nothing else; every failure is a flat 401. Nothing is ever logged. Rate limiting (`middleware/ratelimit.go`) runs before auth.

**There is no static API token.** `bearerToken` was removed from the config and the code on 2026-08-09 at the operator's instruction: two credentials meant two things to rotate and two ways in, and erp-manager — the caller that matters — only ever used the exchange. **Do not reintroduce one.**

Hard rules:

- **No credential may ever have a default.** Username and password are operator-set; the signing secret is 32 random bytes generated on first run and saved to config.yaml. The Node app this replaced shipped `digitrade`/`123456` and a secret in its source — a test asserts those exact credentials are rejected.
- **Rejections are exactly 401**, never 403 and never 500. erp-manager re-authenticates on 401 and on nothing else; any other status turns a self-healing hiccup into a dead connection.
- **Success is exactly 200 with `access_token` present.** A caller treats anything else as failure, and stores a missing `access_token` as null.
- A blank username or password **stops the daemon and is refused by the GUI** (`config.AuthConfig.Validate`, called from both). There is no fallback credential, so an exchange accepting blanks would be an open door.

Full details: `docs/authentication.md`. The contract is pinned by `internal/api/auth_exchange_test.go` — those tests exist because a live backend breaks in production when any of it drifts.

## Hasavshevet send-order flow

1. `OrderQueue.Submit(req)` reserves the order number (`lastOrderNumber.json`, mutex) → 202 + jobId (= order number)
2. Single worker builds IMOVEIN.doc/.prm (fixed-width 2891-byte records, Windows-1255), writes history copies, runs `has.exe` or `digi.bat`
3. The queue supports optional PostOrderHook implementations (none registered — the PDF/print/email hook was removed 2026-07-20; hook errors must never fail the order)

**Single-worker constraint:** never make the order queue concurrent — IMOVEIN files are shared.

## Tests

`go test ./...` — table-driven, stdlib only, `t.TempDir()` for FS, `httptest` for HTTP.
Key suites: `internal/queries` (binder inference, forced strings, store CRUD + electron format compat, DML detection), `handlers/send_order_test.go`, `files_test.go`, `imovein_test.go` (2891-byte layout), `order_number_test.go`, `internal/api/auth_exchange_test.go` (the credential contract: 200-with-access_token, only-an-issued-token-authenticates, exactly-401, shipped defaults rejected, ping, and that `/api/query` stays 404), `internal/config/auth_test.go` (the shared save-time/startup guard).

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
  handlers. There is exactly one error envelope; do not add a second shape.
- **`readFormConfig` must start from `f.cfg`**, never a zero `config.Config`.
  Settings with no widget (queries limits, db.encrypt, hasExePath, tls) would
  otherwise be wiped by a GUI save.
- **The GUI must not be able to save a config the daemon refuses to start on.**
  Any startup precondition in `api.NewServer` that a widget can violate needs a
  matching check at save time, so the GUI cannot leave a box holding a config its
  own service will not start on.
- **Keep the tree gofmt-clean.** `.gitattributes` pins `.go` to LF; if
  `gofmt -l ./cmd ./internal` prints anything, fix it rather than adding to it.

## Prohibited (zero exceptions)

- Adding any endpoint that executes SQL text straight from a request body. There
  is no longer an exception: `POST /api/query` was deleted on 2026-08-09 and the
  backend team explicitly asked that it not come back. A test asserts it 404s.
- Shipping a default for any credential — username, password, or signing secret.
  Generate them; never compile one in.
- Reintroducing a static API token as a second way to authenticate. One scheme.
- Storing secrets in logs (DB password, auth password, signing secret, token
  contents)
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
