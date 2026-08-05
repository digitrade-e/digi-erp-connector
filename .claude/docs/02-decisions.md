# Key Decisions (with rationale)

All decided 2026-07-20 during the initial build.

## 1. Raw SQL endpoint: DELETED, not flagged
The plan originally proposed keeping `POST /api/sql` behind an `allowRawSQL`
config flag for gradual backend migration. **The user chose full deletion.**
Consequence: the backend must be switched to `GET /api/sqlqueries/{name}` at
rollout time; there is no fallback. Never re-add a raw-SQL endpoint.

## 2. Auth: static bearer token, not JWT
electron-mssql-app used JWT with hardcoded credentials — a liability, and its
only benefit (expiry) is not needed for a localhost single-consumer API.
Kept erp-connector's per-installation bearer token, upgraded to
`subtle.ConstantTimeCompare`. If short-lived tokens are ever needed, add a
`/auth/token` exchange endpoint — do not resurrect the hardcoded-creds flow.

## 3. Saved queries are trusted (writes allowed)
Matches electron semantics ("keep your saved queries trusted"). The security
boundary is bearer token + CRUD store, NOT a keyword filter. A per-query
`readOnly` flag was considered and rejected for v1.

## 4. Runner response shape: merged envelope
`RunSavedQueryResponse` contains BOTH the erp-connector envelope
(`api/status/rowCount/rows/recordsets`) AND the electron fields
(`value/rowsAffected`) so either backend calling convention works unchanged.

## 5. rowsAffected semantics
database/sql cannot report affected rows from Query. Solution: plain
INSERT/UPDATE/DELETE/MERGE **without OUTPUT** run via ExecContext (real
affected count); everything else (SELECT/WITH/EXEC/OUTPUT) runs via
QueryContext and `rowsAffected[i]` = recordset lengths (matches what the
mssql Node driver reported for SELECTs).

## 6. queries.json format = custom_sql_data.json format
Drop-in compatible on purpose, including tolerating `"params": []` (electron
stored "no defaults" as an array because JS `typeof [] === 'object'`).
Migration = copy the file. Do not change this format incompatibly.

## 7. Names/paths renamed with side-by-side migration in mind
- module: `github.com/digitrade-e/digi-erp-connector`
- service: `digi-erp-connectord` (old: `erp-connectord`)
- data dir: `%PROGRAMDATA%\digi-erp-connector\` (old: `%PROGRAMDATA%\erp-connector\`)
- installer AppId: NEW GUID `{3A7E2F41-...}` (old erp-connector can stay installed during transition)

## 8. Branch is `main`, not `master`
CI workflows (auto-tag → release-windows) trigger on `main`. The first push
went to `master` by accident; it was renamed, default branch switched via
API, and remote `master` deleted.

## 9. Left as-is deliberately
- `sap.ErrNotImplemented` is declared but never returned (dead 501 branch in
  the price/stock handler) — harmless, may be used for Priority later.
- `priority` ERP is selectable in the GUI but unimplemented.
- Committed `rsrc.syso` / `app.manifest` in cmd/digi-erp-connector (inherited
  from erp-connector, needed for the walk GUI manifest).

## 10. PDF/print/email subsystem: fully removed (2026-07-20, user decision)
After the initial build, the user chose to delete everything PDF/email/print:
`internal/pdf`, `internal/print`, `internal/email`, the post-order PDF hook,
the GUI "PDF & Email Settings" dialog, `PDFConfig`/`SMTPConfig`, the
PDFtoPrinter/qpdf29.dll/resource.dat installer bundling, and the related
docs. Orders still process (IMOVEIN + has.exe) — there is simply no PDF
generated afterwards. The generic `PostOrderHook` interface in queue.go was
kept (part of the queue design). `CustomerEmail` stays in the sendOrder DTO
for wire compatibility (ignored). If the feature returns, recover from git
history (last full commit: 6f17999) and re-read erp-connector's
docs/printing.md for the session-0/WSD constraints.

## 10b. Legacy compat mode ADDED for the production cutover (2026-08-05)

Decisions 1 and 2 (delete raw SQL, drop the JWT flow) assumed the backend would
be migrated *before* a connector went live. On the b4l production box it had not
been: the live backend still did `POST /auth/token` → JWT and the connector had
to replace the electron app **without the backend changing**. The user chose to
add a compatibility layer rather than block on a backend deploy.

`legacyCompat` (default **off**) restores, config-gated: the `/auth/token` JWT
exchange, JWT-or-static-token acceptance in the auth middleware, `/api/ping`,
`/api/test-connection`, `/api/customers`, `/api/orders/{id}`, and — behind its own
`allowRawSQL` switch — `POST /api/query` with erp-connector's read-only validator
restored. Every legacy route logs when hit so its retirement can be evidence-based.

This does not reverse decisions 1 and 2: the default install still has no raw-SQL
route and no JWT. It is a migration bridge with an off switch and a documented
retirement procedure (`docs/legacy-compat.md`). Credentials live in config, never
in the binary, and `enabled: true` with blank credentials is a startup error.

## 10c. Wire-level output now matches the Node driver (2026-08-05)

Comparing all 25 production saved queries against the running old connector
surfaced three differences that would have broken the backend. Fixed in
`internal/queries/runner.go`:

- **Row cap**: the old app had none; `IndividualProductList` returns 16183 rows
  and hit the 10000 default as a 413. Raised to 100000 on that box (config, not
  code — the default stays 10000).
- **Datetimes**: now `…T00:00:00.000Z` (JS `Date.toJSON`), not Go's `…T00:00:00Z`.
- **DECIMAL/NUMERIC/MONEY**: now JSON numbers in shortest form (`13085`), not
  strings (`"13085.00"`). The driver returns these as raw bytes; leaving them as
  strings silently breaks backend arithmetic.

Accepted residual difference: `rowsAffected` is `[]` from the old driver for an
`EXEC` proc with `SET NOCOUNT ON` vs `[2]` here (rows identical). `database/sql`
does not expose NOCOUNT state. 24 of 25 queries are byte-identical.

## 10d. Runner fails closed without a database (2026-08-05)

`NewRunner` returns a non-nil `*Runner` wrapping a nil `*sql.DB`, so the handlers'
`runner == nil` guards never fired and executing any query with the DB down
panicked in the handler. `Run` now returns `ErrNoDatabase` and callers map it to
503. Latent since the initial build; only reachable because the daemon aborts on
a failed initial `db.Open`.

## 11. New additions beyond both parents
- Per-IP token-bucket rate limiting (25 rps, burst 50) before auth on all routes.
- `GET /api/sendOrder/{jobId}` — the OrderQueue job map existed in
  erp-connector but had no HTTP endpoint.
- Config section `queries: {timeoutSeconds: 30, maxRows: 10000}`.
- Server WriteTimeout raised 30s → 60s (saved queries may run up to 30s).
