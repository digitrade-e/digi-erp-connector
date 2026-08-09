# erp-manager integration — what the backend sends and expects

> **Superseded in part (2026-08-09).** The connector no longer has a
> `/auth/token` route: the legacy compatibility layer was deleted. Sections 3 and
> 4 below describe how the integration works *today, before the migration*, and
> are kept because they explain what has to change. The plan for changing it is
> [erp-manager-migration-plan.md](erp-manager-migration-plan.md). Sections 5 and 6
> — the routes and the response shapes — are unchanged and still authoritative.

This connector has one production client: **erp-manager**, the Symfony/API-Platform
service that sits between the customer-facing apps and every ERP. When you develop
here on Windows, erp-manager is the contract you are actually coding against — not
`docs/api.md` in the abstract.

Everything below was read out of erp-manager itself; each claim carries the
`file:line` it came from so you can re-check it when that repo changes. Paths are
relative to `erp-manager-backend/api/` in the `erp-manager` repo.

The connector-side counterpart of every statement is in
[api.md](api.md) (routes), [saved-queries.md](saved-queries.md) (the data model)
and [erp-manager-migration-plan.md](erp-manager-migration-plan.md) (the auth exchange).

---

## 1. Where the connector sits

```
customer app ──► erp-manager (Symfony, :8004 locally, GKE in prod)
                    │
                    │  plain HTTP, one ClientConnection row per connector
                    ▼
                 digi-erp-connectord on the customer's Windows box  ([::]:8082)
                    │
                    ▼
                 MSSQL (localhost:1433) + Hasavshevet import folder
```

erp-manager is multi-tenant: one deployment serves every customer, and *which*
connector a request reaches is decided entirely by a database row. There is no
service discovery and no registry — the row **is** the configuration.

## 2. The ClientConnection row is your config file, seen from the other side

Table `client_connection`, entity `src/Entity/ClientConnection.php`, exposed as
`/api/client_connections` and edited in the erp-manager UI under
*Clients → Connection*. The fields that matter to you:

| ClientConnection field | Meaning for this connector | Must match |
|---|---|---|
| `baseUrl` | Prefix for every data call, e.g. `http://<host>:8082/api` | your `apiListen` host/port + the literal `/api` |
| `authEndpoint` (`auth_endpoint`) | Where the credential exchange is POSTed, e.g. `http://<host>:8082/auth/token` | the `/auth/token` route, which exists **only** when `legacyCompat.enabled` |
| `authLogin` | Sent as JSON `username` | `legacyCompat.jwtUser` |
| `authPassword` | Sent as JSON `password` | `legacyCompat.jwtPassword` |
| `token` | Cache of the last issued JWT (written by erp-manager, plaintext) | signed by `legacyCompat.jwtSecret` |
| `connectionType` | `2` = `DIGITRADE_MSSQL` → this connector | — |
| `erp` | FK to an `Erp` row; **ignored** when `connectionType = 2` | — |
| `company` | Label only — how the connection appears in the UI | — |
| `userQueries` | The saved queries erp-manager believes exist here | names in `queries.json` |

`connectionType` is the whole routing decision: `src/Service/ErpProvider.php:26`
short-circuits to `DigitradeMssqlService` **before** it looks at the ERP name, so
an `erp` value of SAP or Hashavshevet on a `connectionType = 2` row changes
nothing. Enum: `src/Config/ConnectionType.php` — `STANDARD = 1`,
`DIGITRADE_MSSQL = 2`.

A customer can have several rows pointing at several connectors. On BFL there are
two: one for reads on `:8082` and a second, `BFL-SendOrders`, on `:8083` — the
split read/write topology described in
[deployment-topologies.md](deployment-topologies.md). Each node has its own
config.yaml and therefore its own credentials.

## 3. Authentication — the backend does not send your bearer token

This is the single most confusing thing about the integration, so it is worth
being blunt: **erp-manager never sends the `bearerToken` from the GUI.** It
authenticates with a login and a password, and the connector hands back a JWT.

`src/Service/DigitradeMssqlService.php:21-52` (`login()`):

```
POST {authEndpoint}                    e.g. http://host:8082/auth/token
Content-Type: application/json

{"username": "<authLogin>", "password": "<authPassword>"}
```

It requires HTTP **200** — anything else throws `Failed to login to Digitrade
MSSQL. Status code: N` — reads `access_token` out of the JSON body, writes it to
`ClientConnection.token`, and flushes. Then every data call goes out as:

```
Authorization: Bearer <that JWT>
Content-Type: application/json
```

On this side, `POST /auth/token` is a legacy-compat route
(`internal/api/handlers/legacy_compat.go:42-77`): it compares the credentials
against `legacyCompat.jwtUser` / `jwtPassword`, signs an HS256 token with
`jwtSecret`, and answers `{access_token, token_type:"Bearer", expires_in}`
(`internal/api/dto/legacy.go:16-21`). `middleware.AuthWithLegacy`
(`internal/api/middleware/auth.go:26-47`) then accepts **either** the static
bearer token **or** that JWT on every route — the constant-time static comparison
runs first and fails, the signature check succeeds.

Two consequences for development:

- **`legacyCompat.enabled: false` cuts erp-manager off completely.** `/auth/token`
  stops existing, so `login()` gets a 404 and every call fails. There is no
  fallback: erp-manager has no static-token mode (see §8).
- **Changing `jwtSecret` invalidates the cached token** in the database. The next
  call gets a 401, which erp-manager handles (§4) — so this self-heals, but the
  first request after the change fails if anything upstream does not retry.

The static bearer token is still worth keeping correct: it is what *you* use for
curl probes (§7), and it is the credential the integration should eventually move
to.

## 4. Token lifecycle — the 401 contract

`process()` at `src/Service/DigitradeMssqlService.php:173-207`:

1. `$token = $clientConnection->getToken() ?? $this->login($clientConnection);`
   — the cached token is used **as-is**, with no expiry check. erp-manager does
   not know or care that `expires_in` was 1800 seconds.
2. The call is made.
3. If it throws with code **401**, `login()` runs again and the call is retried
   **once**.
4. Any other exception becomes a `BadRequestHttpException` — no retry.

So the contract you must not break: **an expired or invalid token has to produce
exactly HTTP 401.** A 403, a 500, or an HTML error page all skip the re-login
branch and turn a self-healing hiccup into a permanently broken connection until
somebody clears `token` in the database by hand. The current middleware is
correct here — it answers `401` with code `UNAUTHORIZED` for a missing header, a
malformed header, and a bad credential alike.

`jwtExpiryMinutes` is therefore invisible to erp-manager except as *how often a
request pays for one extra round-trip*. 30 minutes (the electron default) means
roughly one re-login every half hour of traffic.

## 5. The six calls erp-manager makes

Every URL is `baseUrl + '/' + query` (`DigitradeMssqlService.php:176`), so with
`baseUrl = http://host:8082/api` the query `custom_sql` becomes
`http://host:8082/api/custom_sql`.

| # | Connector route | erp-manager call site | Expected body |
|---|---|---|---|
| 1 | `GET /api/custom_sql` | `getServiceLayerQueries()` :209-212 · `getCustomQueries()` :234-242 | **bare JSON array** of objects with at least `name` |
| 2 | `GET /api/custom_sql/{name}` | `getServiceLayerQuery()` :214-217 | one object: `{name, description, sql, params}` |
| 3 | `POST /api/create_custom_sql` | `createServiceLayerQuery()` :219-222 | `{ok, name}`; any 400/404 becomes `[]` → *Error creating ServiceLayerQuery* |
| 4 | `PATCH /api/custom_sql/{name}` | `editServiceLayerQuery()` :224-227 | `{ok, updated}` |
| 5 | `DELETE /api/custom_sql/{name}` | `deleteServiceLayerQuery()` :229-232 | `{ok}` |
| 6 | `GET /api/sqlqueries/{name}?<params>` | `UserQueryController.php:34` · `DataMappingController.php:47` and `:136` | object containing **`value`** (see §6) |

Call 6 is the data path — everything a customer actually sees comes through it.
Calls 1–5 are the query-management UI in erp-manager.

**Request bodies erp-manager sends.** Create
(`src/State/ServiceLayerQueryResourceProcessor.php:122-131`):

```json
{"name": "IndividualProductList", "description": "...", "sql": "SELECT ...",
 "params": {"top": "100", "sku": "ABC"}}
```

Note `params` arrives as a flat **object**, flattened from erp-manager's
`[{name, value}, …]` UI shape. Patch (`:47-64`) sends only the keys that changed —
`description`, `sql`, `params` — which is why `UpdateSavedQueryRequest` uses
pointer fields.

**What erp-manager does not call.** Nothing in its `DIGITRADE_MSSQL` code path
touches `/api/health`, `/api/folders/list`, `/api/file`,
`/api/priceAndStockHandler`, `/api/sendOrder`, `/api/ping`,
`/api/test-connection` or `/api/query`.

That is not the same as "unused" — **there is a second caller.** The
client-instance B2B backend (the shop on the customer's VM) has its own
`api/src/Service/ERP/DigitradeMssqlService.php`, and it is the one that:

- executes saved queries during ERP sync, same `sqlqueries/{name}` route (`:332`);
- **POSTs `/api/sendOrder`** for customers with an outbound profile — bflstore has
  one. Its write path expects **202 Accepted**, which is what the order queue
  returns (`:189`).

So the division of labour is: erp-manager *manages* saved queries, the B2B backend
*executes* them and sends orders. Both authenticate the same way, but the B2B
service is the more evolved of the two — it handles a pre-provisioned static token
and fails with an explicit message naming both options when neither a token nor an
`authEndpoint` is configured (`:31-36`). erp-manager has no such branch.

`allowRawSQL` is on in production, so assume `POST /api/query` has a caller until
`server.log` says otherwise for a full business cycle.

## 6. Response shapes — the trap that fails silently

`GET /api/sqlqueries/{name}` must keep the legacy `value` key. erp-manager reads
**only** that:

`src/Utils/ErpResponseNormalizer.php:27` — `$data = $response['value'] ?? [];`

The native envelope (`api`, `status`, `rowCount`, `rows`, `recordsets`) is
invisible to it. `RunSavedQueryResponse`
(`internal/api/dto/queries.go:43-52`) carries both on purpose. Drop `value` and
every screen goes blank with **no error anywhere** — 200 OK, empty list, nothing
in the logs. This is why the CLAUDE.md wire-compatibility list is not stylistic.

After `value`, erp-manager optionally walks `UserQuery.nestedJsonPath` and
JSON-decodes the result when `decodeJson` is set — that is the
`FOR JSON`/`JSON_F52E…` case. A path that no longer matches also returns `[]`
silently (`ErpResponseNormalizer.php:29-36`).

`GET /api/custom_sql` is the opposite shape: a **bare array**, because
`getCustomQueries()` does `array_column($response, 'name')`
(`DigitradeMssqlService.php:238`). Wrap that response in an envelope and every
saved query in erp-manager's UI flips to *not alive*
(`src/State/UserQueryStateProvider.php:51-65`) while the data path keeps working —
a confusing half-broken state.

**Statuses that become empty results rather than errors** (`fetch()` :72-74,
`create()` :102-104): a **404** returns `[]`, and on writes **400** returns `[]`
too. So a typo in a query name does not surface as "not found" — it surfaces as
"no data". When something is mysteriously empty on the erp-manager side, check the
connector log for the real status before assuming the SQL is wrong.

Non-2xx statuses other than those get the full diagnostic: `buildFailureMessage()`
(:254-293) puts method, URL, status and up to 8 KB of your response body into the
exception message, which erp-manager surfaces as a 400. Your `respond.Error`
envelope is readable there — so error bodies are worth keeping precise.

## 7. Live parameters — how the query string reaches the binder

`UserQueryController::createQueryStringParams()` (`:55-76`) merges the parameters
stored on the `UserQuery` row with any URL overrides (**URL wins**), drops nulls,
and runs `http_build_query`. The result is appended to
`sqlqueries/{name}?…`.

Which means every parameter arrives as a **query-string string**, and the
connector's type inference in `internal/queries/binder.go` decides what it binds
as: all-digits → `int64`, decimal → `float64`, `YYYY-MM-DD…` → datetime, anything
else → string. The forced-string list (`skuArray`, `warehouse`, `sku`, `syncKey`)
exists precisely because SAP warehouse codes and SKUs look numeric.

Stored defaults merged with URL overrides is the same rule the runner applies to
`params` from `queries.json`, so a parameter can be defaulted in either place.
Full rules: [saved-queries.md](saved-queries.md).

## 8. Rough edges on the erp-manager side

Things that will cost you an hour if you meet them cold:

- **No static-token mode.** `process()` is `getToken() ?? login()`. Pasting the
  connector's `bearerToken` straight into `ClientConnection.token` does work — it
  never expires, so the 401 branch never fires — but any transient 401 sends
  erp-manager to `/auth/token` and it stays broken. Retiring `legacyCompat`
  properly needs a real branch in `DigitradeMssqlService`, which does not exist
  yet.
- **`getCustomQuery()` returns the wrong thing.** At
  `DigitradeMssqlService.php:244-252` the guard is inverted: it returns `[]` when
  the response is **non-empty** and returns the empty response otherwise. Called
  from `UserQueryStateProvider.php:89`. Not a connector bug — do not "fix" it here.
- **TLS is not verified.** Every request sets `verify_peer: false` and
  `verify_host: false`. HTTPS in front of a connector buys transport encryption,
  not authentication.
- **The token is stored in plaintext** in `client_connection.token`, and is
  readable through the `/api/client_connections` API alongside `authPassword`.
  Treat any dump of that endpoint as a credential leak.
- **`erp` is not ignored everywhere.** `ErpProvider` bypasses it for
  `connectionType = 2`, but schema/metadata paths in `DataMappingController`
  branch on `ErpApi::REST` (`:150-158`) before the connection type is consulted.
  If you add routes for the schema flows, check that file first.

## 9. Exercising the contract from the Windows box

You do not need erp-manager running to reproduce what it does. These six curls
are the whole integration; run them from the connector machine with the **static**
token, which is accepted on every route:

```powershell
$t = "<bearerToken from the GUI>"
$b = "http://127.0.0.1:8082/api"

# 1  the list erp-manager uses for the "alive" flag  → must be a BARE ARRAY
curl.exe -s $b/custom_sql -H "Authorization: Bearer $t"

# 2  one saved query
curl.exe -s $b/custom_sql/IndividualProductList -H "Authorization: Bearer $t"

# 6  the data path  → must contain "value"
curl.exe -s "$b/sqlqueries/IndividualProductList?top=5" -H "Authorization: Bearer $t"
```

To reproduce the auth exchange exactly as erp-manager performs it — this is the
check that tells you whether `legacyCompat` is configured correctly:

```powershell
'{"username":"digitrade","password":"123456"}' | Set-Content -Encoding ascii $env:TEMP\b.json
curl.exe -s -X POST http://127.0.0.1:8082/auth/token `
  -H "Content-Type: application/json" --data-binary "@$env:TEMP\b.json"
# → {"access_token":"eyJ...","token_type":"Bearer","expires_in":1800}

# then prove the issued JWT is accepted everywhere the static token is
curl.exe -s $b/custom_sql -H "Authorization: Bearer eyJ..."
```

A 404 on `/auth/token` means `legacyCompat.enabled` is off — check the
**Legacy compatibility** section of the GUI, which is where those credentials now
live. `jwtUser`/`jwtPassword` shown there must equal `authLogin`/`authPassword` on
the ClientConnection row, or the exchange returns the old app's error shape and
erp-manager reports a login failure.

For the full end-to-end loop, erp-manager runs locally on **:8004**
(`docker compose up -d` in its repo) and you can point a ClientConnection row's
`baseUrl`/`authEndpoint` at your development box.

## 10. Source map

Connector side:

| Concern | File |
|---|---|
| Bearer / legacy JWT acceptance | `internal/api/middleware/auth.go:26-47` |
| `/auth/token` | `internal/api/handlers/legacy_compat.go:42-77` |
| compat config + validation | `internal/config/model.go:42-56`, `internal/api/server.go:53-67` |
| saved-query CRUD handlers | `internal/api/handlers/custom_sql.go` |
| saved-query runner + envelopes | `internal/queries/runner.go`, `internal/api/dto/queries.go:43-52` |
| parameter binding | `internal/queries/binder.go` |

erp-manager side (`erp-manager-backend/api/`):

| Concern | File |
|---|---|
| the whole HTTP client | `src/Service/DigitradeMssqlService.php` |
| connection-type routing | `src/Service/ErpProvider.php:26`, `src/Config/ConnectionType.php` |
| connection entity | `src/Entity/ClientConnection.php` |
| the data path + live params | `src/Controller/UserQueryController.php` |
| response unwrapping | `src/Utils/ErpResponseNormalizer.php` |
| query-management CRUD | `src/State/ServiceLayerQueryResourceProcessor.php`, `src/State/ServiceLayerQueryResourceStateProvider.php` |
| "alive" flag for saved queries | `src/State/UserQueryStateProvider.php:51-65` |
| mapping/preview flows | `src/Controller/DataMappingController.php` |
