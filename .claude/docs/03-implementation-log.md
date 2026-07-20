# Implementation Log — 2026-07-20 initial build

Chronological record of how this repo was built (single session).

## 1. Research
- Read electron-mssql-app completely (server.js, main.js, configStore.js, renderer).
- Deep-scanned erp-connector (all packages, routes, config, CI, installer).
- Findings and plan written to `PLAN.md` (root).

## 2. Port (commit 1608f24)
- Copied erp-connector tree: `internal/` verbatim, `cmd/erp-connector` →
  `cmd/digi-erp-connector`, `cmd/erp-connectord` → `cmd/digi-erp-connectord`,
  `assets/`, `.github/`, accurate docs only (README/config.md/gui-migration.md
  were stale in the old repo and were rewritten instead of copied).
- Bulk import rewrite: `"erp-connector/...` → `"github.com/digitrade-e/digi-erp-connector/...`.
- Regex rename with negative lookbehind `(?<!digi-)erp-connector` →
  `digi-erp-connector` for service name, AppName, binary-discovery globs,
  log messages. User-agent strings set to `digi-erp-connector/...`.

## 3. Deletions
- `internal/api/handlers/sql.go` + `sql_test.go` + `internal/api/dto/sql.go`
  (the raw-SQL endpoint). `ensureEOF` moved to `handlers/json.go`; the binder
  logic (normalizeParamValue, detectIntegerParams, stripStringLiterals,
  collectRecordsets) moved into `internal/queries`.

## 4. New package: internal/queries
- `store.go` — JSON registry, RWMutex, atomic writes (temp+rename, 0600),
  name validation (≤200 chars, no control/`/`/`\`), legacy-array-params
  tolerance in `Definition.UnmarshalJSON`.
- `binder.go` — `BuildNamedArgs` (sorted, `sql.Named`), forced-string params,
  `InferStringValue`, integer hints for TOP/OFFSET/FETCH.
- `runner.go` — timeout + row cap + multi-recordset; plain-DML-without-OUTPUT
  detection routes to ExecContext for real rowsAffected.

## 5. New/changed API
- Handlers: `custom_sql.go` (CRUD), `sql_queries.go` (runner),
  `send_order_status.go`; DTOs in `dto/queries.go`.
- `middleware/auth.go`: `subtle.ConstantTimeCompare`.
- `middleware/ratelimit.go`: per-IP token bucket, 4096-bucket cap with idle
  eviction.
- `server.go`: full route table (see docs/api.md), requires QueryStore,
  builds Runner from `cfg.Queries`.
- `cmd/digi-erp-connectord/app.go`: loads the store from
  `DataDir()/queries.json` at startup.

## 6. Rebranding of ship artifacts
- `assets/installer/digi-erp-connector.iss` — new AppId GUID, new names,
  service `digi-erp-connectord`; `launch-admin.vbs` updated.
- `release-windows.yml` — new binary/installer names; dropped the stale
  "MinGW for Fyne" step (walk needs no cgo).
- Fresh `CLAUDE.md`, `README.md`, `docs/saved-queries.md`, `docs/api.md`;
  ported accurate docs (architecture, security, printing, autostart,
  pdf-email, hasavshevet-send-order) with renames.

## 7. Tests added
- `internal/queries`: binder (hints, coercion, forced strings, inference,
  isPlainDML), store (CRUD, partial update, validation, electron-format
  round-trip), env-gated `TestMigratedFileParses` (real-file validation).
- `internal/api/handlers/custom_sql_test.go`: full CRUD flow over httptest,
  409/400/404 paths, runner fails closed without DB.
- `internal/api/middleware/middleware_test.go`: auth matrix, burst/refill,
  429 response.
- `internal/api/server_test.go`: route-table registration (mux panics on
  conflicts), QueryStore requirement.

## 8. Toolchain
No Go on this machine; installed Go 1.26.5 (zip) to `C:\Users\digi\tools\go`.
`go build ./...`, `go vet ./...`, `go test ./...` all green; both release-style
binaries build (GUI with `-H=windowsgui`).

## 9. Publish saga (worth remembering)
- First push rejected: stored OAuth token lacked `workflow` scope (repo
  contains `.github/workflows/`). GCM interactive re-auth is impossible in
  this environment (no TTY, `/dev/tty` unavailable even via `!`).
- Solved with a classic PAT (repo+workflow) pushed via token-in-URL; token
  then embedded in the local remote URL for future pushes (see 04-operations).
- Branch renamed master → main (CI triggers on main), default branch set via
  GitHub API, remote master deleted.
- CI ran end-to-end on first push: tag v1.0.0 + released installer.

## 10. PDF/print/email removal (2026-07-20, after v1.0.2)
User requested deletion of everything related to PDF & Email settings; scope
confirmed as the full subsystem. Deleted: internal/pdf, internal/print,
internal/email, hasavshevet/pdf_hook.go, cmd GUI pdf_settings.go + its
button/handler, PDFConfig/SMTPConfig + defaults, app.go hook wiring +
printer-validation helpers, installer print-binary entries, workflow
PDFtoPrinter bundling step, docs/printing.md + docs/pdf-email.md.
go mod tidy dropped chromedp + go-mail. Kept: PostOrderHook interface,
CustomerEmail DTO field (wire compat). All builds/tests green.

## 11. Saved-query migration (commit 646689e)
- Real data found at `%APPDATA%\electron-mssql-app\custom_sql_data.json`
  (25 queries) and copied to `%PROGRAMDATA%\digi-erp-connector\queries.json`.
- Parser initially rejected it: electron stored `"params": []` for
  "no defaults". Fixed via tolerant UnmarshalJSON; validated all 25 queries
  parse; committed + pushed (auto-released v1.0.1).
