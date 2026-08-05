# digi-erp-connector — Research Summary & Migration Plan

> **Historical.** This is the plan written before the code existed (2026-07-20),
> kept for provenance. Several things were decided differently during
> implementation — notably PDF/print/email were removed entirely, and a legacy
> compatibility layer was added for the production cutover. Where this document
> disagrees with [docs/](docs/README.md) or [.claude/docs/02-decisions.md](.claude/docs/02-decisions.md),
> those are right and this is not.


Goal: create **digi-erp-connector** (Go) as the merge of two existing repos:

- **erp-connector** (Go, new-gen) → provides the architecture, service model, and all ERP features. This is the codebase we replicate.
- **electron-mssql-app** (Node/Electron, legacy) → provides the **SQL access model that overrides** the current one: the backend must stop sending raw SQL and instead execute **named saved queries** stored on the connector.

Rule: **no code changes** in `electron-mssql-app` or `erp-connector`. All new work happens only in this repo.

---

## 1. Research summary

### 1.1 electron-mssql-app (legacy — logic to adopt)

Source: `main.js`, `server.js`, `configStore.js`, `renderer/`.

| Concern | How it works |
|---|---|
| SQL access model | **Named saved queries** stored locally in `custom_sql_data.json` (per-machine). Backend calls queries **by name + params**, never sends SQL text at runtime. |
| Saved-query CRUD | `POST /api/create_custom_sql`, `GET /api/custom_sql`, `GET/PATCH/DELETE /api/custom_sql/:name`. Entry = `{name, description, sql, params}` where `params` are **default values**. |
| Runner | `GET /api/sqlqueries/:name?p1=v1...` — merges defaults with query-string (URL wins), binds every param via `request.input()` (parameterized, no concatenation), executes, returns `{value: rows, rowsAffected}`. Saved queries are **trusted** — any SQL allowed (not just SELECT). |
| Param typing | Special cases first: `skuArray` → `NVARCHAR(MAX)` (used with `STRING_SPLIT`), `warehouse`/`sku`/`syncKey` → `NVARCHAR(50)` (never coerced to number). Otherwise inference: integer regex → Int, decimal → Float, `YYYY-MM-DD…` → DateTime, else NVarChar. |
| Ad-hoc SQL | `POST /api/query` — restricted to `SELECT`/`WITH` prefix only. |
| Auth | JWT bearer: `POST /auth/token` with **hardcoded** creds (`digitrade`/`123456`), hardcoded secret, 30-min expiry. All `/api/*` requires bearer. |
| Misc endpoints | `/api/ping` (health), `/api/test-connection` (try temp MSSQL config), sample `/api/customers`, `/api/orders/:id`. |
| App shell | Electron tray app: minimize-to-tray keeps API alive, single-instance lock, autostart at login, app-level login gate (config `auth.enabled`, default admin/admin) protecting the settings UI, MSSQL connection pool rebuilt on config save. |

### 1.2 erp-connector (Go — architecture & feature base)

Two binaries: `cmd/erp-connectord` (daemon / Windows service "erp-connectord", HTTP API) and `cmd/erp-connector` (Windows-only `lxn/walk` GUI that edits config, tests connection, and starts/stops the service). Installed by Inno Setup (`assets/installer/erp-connector.iss`) with `sc.exe` auto-start service; GitHub Actions auto-tag + Windows release workflows. `CLAUDE.md` is the authoritative doc; `README.md`/parts of `docs/` are stale (Fyne UI, wrong routes/paths).

API (`internal/api/server.go`, static bearer auth + logging middleware, strict timeouts):

| Route | Purpose |
|---|---|
| `GET /api/health` | DB connectivity check (3s) |
| `POST /api/sql` | **Raw SQL from backend** — read-only validated (SELECT/WITH only, no `;`, no comments, keyword blocklist after string-literal stripping), named params with int-coercion hints for `TOP/OFFSET/FETCH`, 1 MiB body, 8s timeout, 10k row cap, multi-recordset support. **This is the endpoint the saved-query model replaces.** |
| `GET /api/folders/list` | List files in allow-listed image folders |
| `POST /api/file` | Stream a file (hardened path resolution: allow-list, symlink re-check, traversal rejection) |
| `POST /api/sendOrder` | Async order intake → single-worker `OrderQueue`, 202 + `jobId` (= reserved order number) |
| `POST /api/priceAndStockHandler` | Routes by `cfg.ERP` to Hasavshevet or SAP price/stock |

ERP logic:
- **Hasavshevet**: price/stock via installed procs `dbo.GPRICE_Bulk` (wraps native `GPRICE`) and `dbo.GetOnHandStockForSkus` (`Ensure*Procedure` = CREATE OR ALTER, invoked from GUI on save); order pipeline builds fixed-width **IMOVEIN** doc/prm files (2891-byte records, Windows-1255 encoding, Hebrew field titles), reserves sequential order numbers (`lastOrderNumber.json`), writes history copies, runs `has.exe` or Masofon `digi.bat`; **single-worker queue is mandatory** (shared IMOVEIN files); post-order PDF hook (fetch remote HTML template from backend by token → headless Chrome PDF → optional print + email).
- **SAP B1**: price/stock implemented as one large CTE query (price lists, OSPP special prices, discount groups, BOM trees, OITW stock).
- **Priority**: selectable in UI, not implemented.

Subsystems: config (YAML at `%PROGRAMDATA%\erp-connector\`, atomic 0600 writes), secrets (DPAPI machine scope on Windows; **plaintext on Unix**), db (mssql DSN, pool, ping), email (go-mail SMTP), pdf (chromedp `file://` navigation — never `data:`), print (engine order: PDFtoPrinter → Acrobat → Sumatra; WSD-port/session-0 validation), files (path hardening), logger (file-first multiwriter), platform paths, Windows service management.

Known gaps worth fixing in the new repo: non-constant-time bearer compare, no rate limiting, job-status map exists but no polling endpoint, SAP `ErrNotImplemented` dead branch, Priority ERP stub, stale docs.

### 1.3 digi-erp-connector (target)

Empty repo, remote `github.com/digitrade-e/digi-erp-connector`, no commits.

---

## 2. Target design

**Base = erp-connector, verbatim architecture.** Same two-binary layout, service model, config/secrets/paths, ERP packages, PDF/print/email, installer, CI. Module path changes to `digi-erp-connector` (or `github.com/digitrade-e/digi-erp-connector`).

**Override = electron app's saved-query model** as the primary data-access contract:

### 2.1 New subsystem: `internal/queries` (saved-query registry)

- Store: `queries.json` in the connector data dir (`%PROGRAMDATA%\digi-erp-connector\`), atomic writes (reuse `config/io.go` pattern), 0600. Format mirrors `custom_sql_data.json`: `name → {description, sql, params}` so existing definitions can be imported as-is.
- Entry validation on create/update: non-empty name (slug pattern), non-empty SQL, params must be a flat object. Name unique (409 on conflict).
- Thread-safe (RWMutex), reload-on-change not required (single process owns it).

### 2.2 New/changed API surface

| Route | Source of logic | Notes |
|---|---|---|
| `POST /api/custom_sql` + `GET /api/custom_sql` + `GET/PATCH/DELETE /api/custom_sql/{name}` | electron CRUD | Go 1.22 method patterns; keep legacy alias `POST /api/create_custom_sql` for backend compatibility. 1 MiB body limit. |
| `GET /api/sqlqueries/{name}` | electron runner | Merge stored default params with query-string (query-string wins), bind all params named, execute, return `{value, rowsAffected}` **plus** erp-connector-style envelope (`status`, `rowCount`, `recordsets`) — final shape agreed with backend. Saved queries are trusted → full SQL allowed (INSERT/UPDATE/EXEC), executed with per-query timeout + row cap. |
| `POST /api/sql` | — | **Deleted.** digi-erp-connector never accepts raw SQL from the backend. The backend must be switched to `/api/sqlqueries/{name}` as part of the rollout. |
| Everything else | erp-connector | `/api/health`, `/api/folders/list`, `/api/file`, `/api/sendOrder`, `/api/priceAndStockHandler` unchanged. |
| `GET /api/sendOrder/{jobId}` | new (gap fix) | Expose the existing `OrderQueue.Status` map. |

### 2.3 Parameter binding (merge of both repos' logic)

Port electron's typing rules into Go on top of erp-connector's `sql.Named` machinery:

1. Special-case names first: `skuArray` → `NVarChar(MAX)` string; `warehouse`, `sku`, `syncKey` → string, never numeric.
2. erp-connector's int-coercion hints for `TOP(@x)` / `OFFSET @x ROWS` / `FETCH NEXT @x ROWS`.
3. Electron-style inference for the rest: all-digits → int64, decimal → float64, `YYYY-MM-DD…` parseable → `time.Time`, else string. Preserve the "leading-zero string stays string" behavior from erp-connector's `normalizeParamValue`.

One shared binder in `internal/queries` (single source of truth, table-driven tests), built from erp-connector's `normalizeParamValue`/`detectIntegerParams` plus electron's special-case rules.

### 2.4 Auth — decision

Keep **erp-connector's static bearer token** (generated by the GUI, stored in config). Do **not** port the electron JWT flow: its value was token expiry, but its hardcoded credentials/secret are a liability, and the backend already provisions per-installation bearer tokens for erp-connector. Improvements: constant-time compare (`crypto/subtle`), simple per-IP rate limiting middleware on all `/api/*`.

If backend later needs short-lived tokens, add `/auth/token` exchanging the static token for a JWT — deferred, not in v1.

### 2.5 Config model

erp-connector's `Config` plus:

```yaml
queries:
  timeoutSeconds: 30     # saved-query execution timeout (writes can be slower than 8s)
  maxRows: 10000
```

GUI gets a "Saved Queries" section only if needed later; v1 manages queries exclusively through the API (the backend is the editor of record, like it was for the electron app).

---

## 3. Repository layout

```
digi-erp-connector/
├── cmd/
│   ├── digi-erp-connector/     # walk GUI  (from cmd/erp-connector)
│   └── digi-erp-connectord/    # daemon/service (from cmd/erp-connectord)
├── internal/
│   ├── api/                    # server, handlers, middleware, dto, utils
│   │   └── handlers/           # + custom_sql.go, sql_queries.go (new)
│   ├── queries/                # NEW: saved-query store + runner + binder
│   ├── config/  ├── db/  ├── secrets/  ├── email/  ├── pdf/  ├── print/
│   ├── files/   ├── logger/    ├── platform/{autostart,paths}
│   └── erp/{hasavshevet,sap}
├── assets/installer/digi-erp-connector.iss
├── .github/workflows/          # auto-tag + release-windows
├── docs/                       # rewritten, accurate (do not copy stale docs)
├── CLAUDE.md                   # port + update hard constraints
└── go.mod                      # module digi-erp-connector
```

Service name: `digi-erp-connectord`; data dir: `%PROGRAMDATA%\digi-erp-connector\`. Installer must not collide with the old `erp-connector` service if both are installed during transition.

---

## 4. Phased plan

**Phase 0 — Bootstrap** (small)
Init Go module, port `platform/paths`, `logger`, `config`, `secrets`, `db` with renamed app name/service name. Port CI workflows + installer skeleton. `go vet` + `go test ./...` green.

**Phase 1 — API core** (small)
Port `internal/api` (server, auth, logging, dto, utils, health) with fixes: constant-time bearer compare, rate-limit middleware. `POST /api/sql` is **not** ported — the param-binding/normalization logic (and its tests) moves into `internal/queries`.

**Phase 2 — Saved-query subsystem** (the actual override — main new work)
`internal/queries`: store (JSON, atomic, mutex), shared param binder (rules in §2.3), runner (timeout, row cap, multi-recordset). Handlers: CRUD + `/api/sqlqueries/{name}` + legacy `POST /api/create_custom_sql` alias. Table-driven tests ported from electron behavior (defaults merge, URL-wins, special-case params, type inference) + Go-side tests for store atomicity and traversal-free names. Import tool/endpoint able to ingest an existing `custom_sql_data.json`.

**Phase 3 — ERP features** (bulk port, low risk)
Port verbatim: `erp/hasavshevet` (imovein, send_order, queue, order_number, gprice_bulk, onhand_stock_proc, pdf_hook, exec), `erp/sap`, `files`, `email`, `pdf`, `print` + all existing tests. Add `GET /api/sendOrder/{jobId}` status endpoint. Clean the SAP dead `ErrNotImplemented` branch; either implement or hide Priority in the UI.

**Phase 4 — GUI + service + installer**
Port walk GUI + pdf_settings dialog + service management; rename service/product; build Inno Setup installer bundling PDFtoPrinter/qpdf/resource.dat; verify session-0 printing path untouched.

**Phase 5 — Cutover**
Backend migration: register its queries on each connector via CRUD and switch all data access to `GET /api/sqlqueries/{name}` (raw SQL is not supported by digi-erp-connector). Side-by-side test against a customer-like MSSQL; verify IMOVEIN byte-compatibility (existing 2891-byte tests) and order-number continuity (can seed `lastOrderNumber.json` from an existing install).

---

## 5. Carry-over constraints (from erp-connector CLAUDE.md — still binding)

- No raw-SQL-from-backend endpoint exists; saved queries are the only SQL entry point, always parameter-bound.
- OrderQueue stays **single-worker** (shared IMOVEIN.doc/prm files).
- PDF generation navigates `file://`, never `data:` URIs.
- Print engine preference PDFtoPrinter → Acrobat → Sumatra; don't trust Sumatra/Adobe exit codes; keep the three bundled binaries in the installer.
- File endpoint path hardening (allow-list + symlink re-resolution) must not be weakened.
- No business logic in `cmd/`; DB password only in secrets, never in YAML.

## 6. Open decisions (defaults chosen, flag if you disagree)

1. **Response shape of `/api/sqlqueries/{name}`** — default: erp-connector envelope (`status/rowCount/rows/recordsets`) with `value`/`rowsAffected` kept as aliases for drop-in backend compatibility.
2. **Write-capable saved queries** — default: allowed (matches electron's trusted-runner semantics); alternative is a per-query `readOnly` flag defaulting to true.
3. **Module path** — default `github.com/digitrade-e/digi-erp-connector`.
4. **JWT layer** — default: skip; static bearer + constant-time compare + rate limit.
