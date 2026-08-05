# Legacy compatibility mode (electron-mssql-app)

`legacyCompat` makes digi-erp-connector a drop-in replacement for the old
Node/Electron connector (`electron-mssql-app`) so an existing backend can keep
calling it **unchanged** while it is migrated. It is **disabled by default** — a
fresh install exposes none of this.

It is a cutover aid, not a supported long-term mode. Every legacy route logs a
line when it is hit; when those lines stop appearing, the backend no longer needs
this and it should be switched off.

## Configuration

```yaml
legacyCompat:
  enabled: true
  jwtSecret: DIGITRADE_DEVEPOPMENT_MSSQL   # must match the old app's secret
  jwtUser: digitrade
  jwtPassword: "123456"
  jwtExpiryMinutes: 30
  allowRawSQL: true                        # exposes POST /api/query
```

`enabled: true` with any of `jwtSecret` / `jwtUser` / `jwtPassword` empty is a
**startup error** — the daemon refuses to run rather than expose `/auth/token`
with blank credentials. `allowRawSQL` is a separate switch so the raw-SQL route
can be retired before the rest.

Credentials live in config, never in the binary. Setting `enabled: false` (or
removing the block) instantly removes every route below and stops accepting JWTs.

## What it adds

| Route | Behaviour |
|-------|-----------|
| `POST /auth/token` | Credentials → HS256 JWT, `{access_token, token_type, expires_in}`. Unauthenticated (it *is* the credential exchange) but logged and rate-limited. |
| `GET /api/ping` | `{"ok":true,"ts":<epoch millis>}`. Does **not** touch the DB — use `/api/health` for that. |
| `POST /api/test-connection` | Tries the supplied `{mssql:{...}}` merged onto the running config. `{"ok":true}` / `{"ok":false,"error":"connection_failed"}`. |
| `GET /api/customers?limit=N` | Old sample route: `TOP (@limit) * FROM dbo.Items ORDER BY Id DESC`, default 50, capped at 200. |
| `GET /api/orders/{id}` | Old sample route: one `dbo.Items` row by Id. `invalid_id` / `not_found`. |
| `POST /api/query` | Ad-hoc SQL, **only when `allowRawSQL: true`**. See below. |

Additionally, `middleware.AuthWithLegacy` accepts **either** the static bearer
token **or** a valid legacy JWT on every route. The static comparison runs first
and stays constant-time, so the primary path is unchanged. Tokens the *old Node
app* issued also verify, provided the same secret is configured — the wire format
is identical, which is what makes the switchover seamless mid-session.

Legacy routes answer errors in the **old app's shape** — `{"error":"not_found"}`,
a single snake_case string — not the `{error, code, details}` envelope the native
routes use. That is deliberate: those bodies are a contract with software we do
not control.

## POST /api/query — the raw-SQL exception

`CLAUDE.md` forbids endpoints that execute SQL from a request body. This route is
the single, explicit, config-gated exception, added because the production
backend being migrated may still call it and the cutover had to not break it.
It is fenced in:

- **read-only validated** (`queries.ValidateReadOnly`): one statement only,
  `SELECT`/`WITH` prefix, no comments, keyword blocklist applied *after* string
  literals are stripped
- **every parameter bound** via `sql.Named` — no concatenation, ever
- **the full statement is logged on every call** — that log is the audit trail
  and the evidence for retiring the route
- absent entirely unless `allowRawSQL: true`

Two deliberate relaxations versus erp-connector's old validator, so queries the
Node endpoint accepted are not rejected: a single trailing semicolon is allowed,
and the multi-statement check runs on the literal-stripped text (so
`WHERE note = 'a;b'` passes).

Saved queries remain the supported model. Do not widen this route.

## Response-shape compatibility

The saved-query runner emits both envelopes at once — the native
`api/status/rowCount/rows/recordsets` fields **and** the legacy
`value`/`rowsAffected` — so either calling convention works.

Two value-level fixes in `internal/queries/runner.go` exist purely to match the
old driver's JSON, and are covered by `normalize_test.go`:

- **Datetimes** render as `2026-03-08T00:00:00.000Z` (UTC, always three
  fractional digits), like JS `Date.toJSON`. Go's default marshalling drops a
  zero fraction and would emit `...T00:00:00Z`.
- **DECIMAL/NUMERIC/MONEY/SMALLMONEY** render as JSON *numbers* in shortest form
  (`13085`, not `"13085.00"`). The MSSQL driver hands these back as raw bytes,
  which would otherwise marshal as strings and silently break arithmetic on the
  backend (in JS, `"0.00" + 1 === "0.001"`). Values pass through float64, which
  is exactly the precision the Node connector had.

## Known residual difference

`rowsAffected` for `EXEC`-style saved queries. The Node driver reported `[]` for
a stored procedure with `SET NOCOUNT ON` (observed on `getStockBalance`), whereas
this connector derives `rowsAffected` from recordset lengths and reports `[2]`.
The `value` rows are identical. `database/sql` does not expose the NOCOUNT state,
so this is not emulated. Verified against the live database: of 25 production
saved queries, 24 are byte-identical to the old connector's output and this is
the only difference.

## Retiring it

1. Migrate the backend to send the static `bearerToken` and to call
   `GET /api/sqlqueries/{name}` instead of `POST /api/query`.
2. Watch `server.log` for `legacy-compat route used:` lines. When none appear
   for a full business cycle, nothing depends on this any more.
3. Set `allowRawSQL: false`, restart, confirm still quiet.
4. Set `enabled: false`, restart. The routes and the JWT path are gone.
