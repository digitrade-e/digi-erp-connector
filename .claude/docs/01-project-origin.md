# Project Origin — why digi-erp-connector exists

Created 2026-07-20. This repo is the merge of two predecessor repos into one
Go application. Neither predecessor was modified — they remain as reference.

## The two parents

### electron-mssql-app (Node/Electron — the OLD connector)

Written by a previous developer. Small tray app (`main.js`, `server.js`,
`configStore.js`, `renderer/`) exposing an Express API on port 3001 over a
`mssql` connection pool.

**Its one big idea, which this repo adopted:** the backend never sends SQL.
SQL statements are **saved on the connector by name** in
`custom_sql_data.json` (in the Electron userData dir), managed via CRUD
endpoints, and executed via `GET /api/sqlqueries/:name?param=value` with
parameterized binding. Notable details we preserved:

- Params on each saved query are **defaults**; URL query-string overrides them.
- Forced-string params: `skuArray` (CSV for `STRING_SPLIT`, NVARCHAR(MAX)),
  `warehouse`, `sku`, `syncKey` (NVARCHAR(50) — SAP WhsCodes look numeric).
- String type inference: `^\d+$` → int, `^\d+\.\d+$` → float,
  `YYYY-MM-DD...` → date, else string.
- Saved queries are trusted (any SQL allowed); only the ad-hoc endpoint was
  restricted to SELECT.
- Response shape: `{ value: rows, rowsAffected }`.

Things we deliberately did NOT carry over: its JWT auth
(hardcoded creds `digitrade`/`123456` and a hardcoded secret — replaced by the
bearer-token model), the Electron shell, the ad-hoc `POST /api/query` endpoint.

### erp-connector (Go — the NEW-generation connector)

Written by the current developer (Go). Two binaries: `erp-connectord`
(Windows service / daemon, REST API) and `erp-connector` (lxn/walk GUI).
Full feature set: Hasavshevet order pipeline (IMOVEIN fixed-width
Windows-1255 files, single-worker queue, sequential order numbers,
GPRICE_Bulk/GetOnHandStockForSkus procedures, has.exe/digi.bat execution),
SAP B1 price/stock CTE query, remote-template PDF via headless Chrome,
printing (PDFtoPrinter → Acrobat → Sumatra), SMTP email, DPAPI secrets,
image-folder file serving, Inno Setup installer, auto-tag + release CI.

**Its one flaw, which this repo removed:** `POST /api/sql` accepted raw SQL
text from the backend (validated read-only, but still SQL-over-the-wire).

## The merge rule

> Take ALL logic from erp-connector; where the two repos overlap on data
> access, electron-mssql-app's saved-query model OVERRIDES and the raw-SQL
> endpoint is DELETED (user decision, 2026-07-20 — no transition flag).

See `../../PLAN.md` for the full research summary and migration plan.
