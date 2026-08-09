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

## 12. Production cutover on the b4l box (2026-08-05)

Replaced the still-running electron-mssql-app connector with digi-erp-connectord
on this machine. Full detail in `04-operations.md`; sequence and findings:

1. **Merge audit first.** Diffed every erp-connector `.go` file against this repo:
   31 identical bar import paths, 3 differing only by the documented PDF/print/email
   removal, missing files exactly the deliberate deletions. All electron logic
   present except `/api/ping`, `/api/test-connection`, the two sample routes and
   `POST /api/query`.
2. **Discovered the real deployment**, which contradicted assumptions: the live API
   was on **port 8082** (not 3001), bound to all interfaces with a firewall rule
   opening it to the LAN, authenticated by **JWT** — and digi had no `config.yaml`
   at all, so its service had never started.
3. **Built the compat layer** (see decision 10b) — `internal/legacyauth` (hand-rolled
   HS256, no new dependency), `AuthWithLegacy`, the five legacy handlers,
   `queries.ValidateReadOnly`, config `legacyCompat` + `db.encrypt`/
   `trustServerCertificate`. New tests: `legacyauth/jwt_test.go`,
   `queries/readonly_test.go`, `queries/normalize_test.go`,
   `api/legacy_compat_test.go`.
4. **Ran both connectors side by side** (old on 8082, new on 8092) and diffed the
   response of all 25 saved queries. That comparison is what found the row cap,
   datetime and decimal problems (decision 10c) — none were visible from reading
   code. `Lable Recommended` appeared to differ until an old-vs-old run at the same
   instant proved it was live data changing (a sale between calls).
5. **Cut over** with an auto-rollback script: deploy binaries, switch `apiListen`
   to `[::]:8082`, stop electron, remove its HKCU Run entry, start the service,
   probe `/auth/token` + `/api/health`, roll back to electron on any failure.
   Downtime 16:35:05 → 16:35:09, **~4 seconds**.
6. **Hardened the service**: `depend= MSSQL$WIZSOFT2017` and restart-on-failure,
   because the daemon aborts when the DB is unreachable at startup whereas the
   electron app did not (decision in 04-operations).

Toolchain note: `cmd/cutover-seed` was added to write config.yaml, store the DB
password through DPAPI and import queries using the app's own code paths, so the
on-disk formats could not drift from what the daemon reads.

Gotcha worth remembering: PowerShell 5.1 strips double quotes when passing a
string to a native exe, so `curl -d '{"a":1}'` silently sends invalid JSON. This
produced a completely bogus first test run (every endpoint 401). Pass bodies with
`--data-binary "@file"`.

## 11. Saved-query migration (commit 646689e)
- Real data found at `%APPDATA%\electron-mssql-app\custom_sql_data.json`
  (25 queries) and copied to `%PROGRAMDATA%\digi-erp-connector\queries.json`.
- Parser initially rejected it: electron stored `"params": []` for
  "no defaults". Fixed via tolerant UnmarshalJSON; validated all 25 queries
  parse; committed + pushed (auto-released v1.0.1).

## 13. DRY/KISS refactor (2026-08-05, branch refactor/dry-kiss)

Requested straight after the cutover. The directory layout was already close to
idiomatic Go, so nothing was reshuffled for its own sake; the work went into
duplication, naming, missing tests and the bugs those turned up.

1. **Baseline commit first** — the exact bits running in production, so the
   refactor had a rollback point (plus a filesystem snapshot in scratch).
2. **atomicfile** — `config.Save`, `secrets.Set` and `Store.persistLocked` each
   had their own temp+sync+rename. Consolidated with tests. Found: `secrets.Set`
   returned the nil encrypt error on a failed rename (silent failure), and all
   three removed the target before renaming (window with no file at all).
3. **Handlers** — seven copies of the same 12-line decode ritual and five
   identical 1 MiB constants became `decodeJSONBody` + `maxRequestBody`.
   `internal/api/utils` → `internal/api/respond` (`respond.JSON`/`respond.Error`,
   69 call sites). `/api/test-connection` stopped comparing `err.Error()` to the
   string `"EOF"`.
4. **Tests** — config, db and secrets had none. Data dir resolves through
   PROGRAMDATA, so `t.Setenv` redirects it and tests can never touch a live
   install. Notable: the DSN encrypt/trust options added at cutover were
   completely untested until now.
5. **Line endings** — git stored LF while `core.autocrlf` checked out CRLF, so
   `gofmt -l` flagged all 52 files and hid real problems. `.gitattributes` pins
   LF; with the noise gone exactly 4 files were genuinely misformatted.
6. **GUI split** — 683-line `main.go` → five files by responsibility. Found two
   dead-code bugs: `main()` re-checked an already-returned error instead of the
   config-load error (so a corrupt config.yaml was never shown in the window),
   and `onStartServer` branched on `runtime.GOOS` inside a `//go:build windows`
   file. Verified the window reaches its run loop with the data dir sandboxed.

**Deployed 2026-08-05 17:37** (commit 3567157), ~6s downtime, verified by
before/after response capture rather than by spot checks:

- captured all 25 saved-query responses plus 13 endpoint probes from the running
  production *before* the deploy, normalised (rows as sorted `key:type=value`)
- deployed with auto-rollback to the previous binaries on any failed probe
- captured again and diffed: **37 of 38 byte-identical**. The only difference was
  `/api/ping`'s `ts`, 85 seconds apart — the gap between the two captures.

That is the right test for a refactor: it proves nothing observable changed,
which spot-checking endpoints cannot. Scripts: `scratchpad/capture.ps1`,
`cutover-backup-2026-08-05/deploy-refactor.ps1`; the replaced binaries are kept
as `deploy-backup-2026-08-05-refactor/*.previous`.

## 2026-08-09 — credential exchange restored as a feature (`feature/auth-exchange`)

Same day as the legacy deletion, and a direct consequence of it: erp-manager
replied that it cannot drop the login/password fields
(`docs/connector-adaptation-plan.md`), so the connector implements §3 and §4 of
that plan. Decision and rationale: `.claude/docs/02-decisions.md` §15.

What was built:

- `internal/auth` — the HS256 signer recovered from `949f4fc^:internal/legacyauth/jwt.go`,
  renamed and re-documented as a supported package, plus `NewSecret()` (32 random
  bytes, hex) and `NewPassword()` (18 bytes, base64url).
- `config.AuthConfig` with `Validate()` and `TTL()`. `Validate` is called by both
  `api.NewServer` and the GUI's `readFormConfig`, so the two cannot drift.
- `handlers.NewTokenHandler` (`POST /auth/token`, registered only when enabled)
  and `handlers.NewPingHandler` (`GET /api/ping`, always registered, no DB touch).
- `middleware.AuthWithExchange` — static compare first, then signature. `Auth`
  remains as the single-credential wrapper.
- First-run secret generation in the daemon: if `auth.enabled` and the secret is
  blank, generate and `config.Save`. Logged as having happened, never with the
  value.
- GUI: a **Credential exchange** section with generate buttons for the password
  and the secret, and a show/hide toggle; `cutover-seed -auth-user/-auth-password/-auth-ttl`
  for headless provisioning, which prints the credentials to hand to the backend.

Verified on a sandboxed instance (`PROGRAMDATA` redirected to a scratch dir,
port 18099, production on 8082 untouched): exchange → 200 with `access_token`;
`digitrade`/`123456` → 401; issued token and static token both → 200 on
`/api/custom_sql`, which is still a bare array; wrong token → 401; ping → 200
with both credentials and 401 with none; `POST /api/query` → 404. Then the secret
was blanked and the daemon restarted: it generated a new one, persisted it with
the rest of the config intact, issued against it, and rejected the token from
before the regeneration.

Not deployed. b4l stays on 1.0.4 until erp-manager updates ClientConnection rows
67 and 76 — see `.claude/docs/04-operations.md`.
