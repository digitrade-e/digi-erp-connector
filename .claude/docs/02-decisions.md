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

## 12. Split read/write deployment supported (2026-08-06)

The customer needs orders written into `C:\xampp\htdocs\herp` on a *second*
server (192.168.0.7) while the database stays on 192.168.0.5. Installing a
connector on .7 "for orders only" was impossible: order building needs the
database (`queryAccount` is mandatory), the GUI refused to save when the stored
procedures could not be installed, and the daemon exited when the DB was
unreachable.

Chosen topology: a connector **on** the write node with its database over the
network, rather than one connector writing files across an SMB share. The share
alternative needs the other team to grant the *computer account* rights on both
share and NTFS, leaves the question of what imports the files unanswered, and
puts `lastOrderNumber.json` on the far side of the network. See
`docs/deployment-topologies.md`.

Three code changes made it possible, each defensible on its own:

- **`db.OpenLazy`** — the daemon builds the pool without contacting the server,
  warns, and serves; `database/sql` reconnects on demand. Only a bad config is
  fatal. Startup no longer depends on the network, which matters far beyond this
  deployment.
- **Best-effort procedure install** — a failure to create `GPRICE_Bulk` /
  `GetOnHandStockForSkus` now warns instead of aborting the save. They serve
  price/stock only, so a write node with a least-privilege login does not need
  them, and refusing the save made such a node unconfigurable.
- **`ORDERS_NOT_CONFIGURED` (501)** — a node with no `sendOrderDir` refuses
  orders up front instead of returning 202 and failing inside the worker.
  Capability is derived from config rather than a new switch, so the two cannot
  disagree.

Constraint that must hold: **exactly one node owns `sendOrderDir`**.
`lastOrderNumber.json` is guarded by a process-local mutex, so two connectors
sharing that folder would issue duplicate order numbers and overwrite each
other's `IMOVEIN.doc`.

## 13. The legacy credentials are visible and editable in the GUI (2026-08-09)

The configuration window showed `bearerToken` and nothing else about
authentication, while `legacyCompat.jwtUser` / `jwtPassword` / `jwtSecret` existed
only in `config.yaml`. On a migrated box that inverts reality: the backend
(erp-manager) trades a login and password for a JWT at `POST /auth/token` and never
sends the static token, so the GUI advertised the unused credential and hid the
used one. Comparing the connector UI against the `ClientConnection` row in
erp-manager therefore led to the conclusion that the backend used Basic auth — it
does not; both sides are Bearer, and the login/password are a JSON body, not a
header.

The block is now a GUI section, editable rather than read-only, because the point
is to keep the two sides *in sync* — a read-only display would show the mismatch
without letting anyone fix it.

Making it editable removed the protection that decision 10b relied on (the
"settings with no widget survive a save" rule), so two guards replace it:

- `validateLegacyCompat` mirrors the `api.NewServer` precondition at save time and
  names every missing field at once. Without it, one save could leave a production
  box holding a config its own service refuses to start on.
- `confirmLegacyDisable` prompts before *Enabled* is switched off. It is the only
  confirmation dialog in the GUI, because it is the only click that silently cuts
  the backend off at the next restart.

Both are widget-free functions, so `cmd/digi-erp-connector/form_config_test.go`
covers them; CI runs on `windows-latest`, so a `//go:build windows` test executes
there.

Also written: `docs/erp-manager-integration.md`, the backend contract from this
side — the six routes erp-manager calls, the credentials it sends, its
401-triggered re-login, and the two response shapes whose loss fails silently
(`value` on saved queries, a bare array on `custom_sql`). It exists so Windows-side
development does not need the erp-manager repo open to know what the caller
expects. Retiring legacy compat is still blocked on erp-manager: its
`process()` is `getToken() ?? login()`, with no static-token branch.

## 13. TLS, least-privilege SQL, and dead-code removal (2026-08-09)

Asked for after establishing that the static bearer token is decisively more
secure than the legacy JWT (256-bit random vs the password `123456`, no forgery
path vs a signing secret that shipped in the old app's source) — but that the
bigger hole was the transport, not the credential.

**TLS.** `tls.certFile`/`tls.keyFile`; TLS 1.2 minimum; the pair is loaded in
NewServer so a bad path or mismatched key stops the service at startup. Setting
only one of the two is an error rather than a silent fallback to plaintext: that
fallback is precisely how a token leaks. With no TLS block and a non-loopback
apiListen the daemon logs a warning. Tested with a generated self-signed pair,
including a real handshake and the rejection of a plaintext request.

**Least-privilege SQL.** `scripts/sql/least-privilege-{read,write}-node.sql`.
The connector logs in as `sa` in production, so a stolen bearer token is a
database administrator. The write-node script grants SELECT on exactly two
tables — Accounts and Rates — which is all the order pipeline reads.

**Dead code removed**, each verified unreferenced first:

- `internal/erp/{hasavshevet,sap}/repo.go` — files containing only a TODO comment
- `respond.NotImplemented` — no callers
- `sap.ErrNotImplemented` and its handler branch — never returned, so the 501 was
  unreachable. This retires the "left as-is deliberately" note in decision 9.
- `PostOrderHook` — interface, field, variadic parameter and invocation loop with
  no implementations since the PDF hook was deleted on 2026-07-20. Decision 10
  kept it as "part of the queue design"; with the feature gone it was plumbing to
  nowhere, and git history has it if printing ever returns.
- `ERPPriority` — offered in the GUI dropdown but unimplemented, so choosing it
  produced a runtime failure. Decision 9 deferred this; it is now removed from the
  selectable list.

`NOT_IMPLEMENTED` is therefore no longer a code the API can return.

## 14. Legacy compatibility layer deleted (2026-08-09)

The user authorised a two-day maintenance window and asked for the old auth path
gone. Removed in full: `internal/legacyauth`, the five compat handlers,
`dto/legacy.go`, `queries.ValidateReadOnly` (only `/api/query` used it),
`middleware.AuthWithLegacy`, `config.LegacyCompatConfig`, the GUI compatibility
section, and every test for them — about 1,350 lines. `middleware.Auth` now
accepts the static bearer token and nothing else.

Sequencing matters more than the deletion: **removing the code breaks nothing
until the binary is deployed.** So the code went first, the release exists, and
the production box keeps running the older build until erp-manager is migrated.
That inverts the usual risk — no downtime is spent waiting on a backend change.

What deliberately stayed, because it is a *data* contract rather than an auth one:
the `value`/`rowsAffected` response keys, `.000Z` datetimes, shortest-form decimal
numbers, the `POST /api/create_custom_sql` alias and the `queries.json` format.
erp-manager reads `value` and nothing else; dropping it blanks every screen with a
200 and no error.

Evidence used before deleting the routes: the source review in
`docs/erp-manager-integration.md` establishes that erp-manager calls `/auth/token`
plus five saved-query routes and nothing else, and `/api/query` — the only compat
route that logged its caller — recorded 20 hits, all from 127.0.0.1 (local
probes). Note the request logger is gated on `debug`, which is false in
production, so "no log lines" was never evidence of "no callers"; the source
review was.

Handover for the backend developer: `docs/erp-manager-migration-plan.md`.

## 15. The credential exchange came back as a first-class feature (2026-08-09)

erp-manager declined the static-token migration proposed in decision 14, and the
reason was good: the `authLogin`/`authPassword`/`authEndpoint` fields on
`ClientConnection` are shared with four other ERP integrations, so deleting them
is not a change to one code path. Their reply is `docs/connector-adaptation-plan.md`;
the connector adapts instead.

`POST /auth/token` and `GET /api/ping` are therefore back — but not as the
compatibility layer that was deleted. What was actually wrong with the old
implementation was never the protocol:

| Old (electron / legacyCompat) | Now |
|---|---|
| `digitrade` / `123456` shipped in source | operator-set, per install; the old pair is rejected and a test says so |
| signing secret constant in source | 32 random bytes generated on first start, saved to config.yaml |
| a "legacy compatibility" section in the GUI | a **Credential exchange** section, documented as supported |
| enabled by default for compat | off by default; when off the route does not exist |

So the deletion in decision 14 was still right — it removed the credentials.
Restoring the protocol without them costs about 200 lines and closes the actual
hole: one leaked copy of the old source minted tokens for every customer.

Three choices worth recording:

**Dual acceptance stays.** The static bearer token and an issued token both
authenticate every route. That is what makes the eventual static-token migration
a configuration change on erp-manager's side rather than a synchronised deploy.

**Exactly 401, exactly 200.** erp-manager re-authenticates on 401 and on nothing
else, and treats any non-200 from the exchange as a login failure. Both are
pinned by tests in `internal/api/auth_exchange_test.go`, with the failure mode
written next to each assertion, because they read like style preferences and are
not.

**One validation function for two callers.** `config.AuthConfig.Validate` is
called by `api.NewServer` and by the GUI's `readFormConfig`. The house rule says
the GUI must not be able to save a config the daemon refuses to start on; a check
duplicated in both places is a check that will drift.

`POST /api/query` stayed deleted — the backend team asked for that explicitly.

Rollout has an order that matters: the operator sets the credentials, sends them
out of band, erp-manager updates ClientConnection rows 67 (`:8082`) and 76
(`:8083`, BFL-SendOrders), **and only then** is the connector deployed. Reversed,
BFL's screens go blank until the rows catch up. Until it ships, b4l stays on
1.0.4 — an installer without `/auth/token` breaks that box.
