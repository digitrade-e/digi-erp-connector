# API Reference

Base URL: `http(s)://<apiListen>` (default `127.0.0.1:8080`). HTTP by default;
**HTTPS when `tls.certFile`/`tls.keyFile` are configured**, which you should do
whenever the backend is on another machine — see
[configuration.md](configuration.md#tls) and [security.md](security.md).

Every `/api/*` route requires `Authorization: Bearer <credential>` and is
rate-limited per client IP (25 req/s, burst 50 → `429 RATE_LIMITED`). Rate
limiting is applied **before** authentication.

An installation is configured with one credential, of one of two kinds: the
static `bearerToken`, or a token obtained from `POST /auth/token` when the
credential exchange is enabled. They behave identically on every route. See
[authentication.md](authentication.md).

Errors are always `{ "error": "<message>", "code": "<CODE>", "details": {} }`.
Branch on `code`, not on the message — messages may be reworded, codes are stable.

Request bodies are capped at 1 MiB, must be a single JSON document (trailing data
is rejected), and driver errors are never returned to callers.

## Contents

- [Authentication](#authentication) · [Health](#health) ·
  [Saved queries](#saved-queries) · [Files](#files) ·
  [Orders](#orders-hasavshevet) · [Price & stock](#price--stock) ·
  [Error codes](#error-code-reference)

## Authentication

`POST /auth/token` → `200 {"access_token":"…","token_type":"Bearer","expires_in":1800}`

Body `{"username","password"}`. Registered only when `auth.enabled` is true —
otherwise `404`. Unauthenticated (it is the credential check) but rate-limited.
Anything wrong is `401 INVALID_CREDENTIALS`; a malformed body is not
distinguished from wrong credentials.

## Health

`GET /api/health` → `{"status":"ok"}` | `503 DB_UNAVAILABLE` — pings the database.

`GET /api/ping` → `{"ok":true,"ts":1786283101465}` — **no database touch**. Use it
to check that the service is up and a credential is valid; use `/api/health` when
you also want the database checked. `ts` is epoch milliseconds.

## Saved queries

See `saved-queries.md` for the full model.

| Route | Method | Codes |
|---|---|---|
| `/api/custom_sql` | POST | `INVALID_JSON`, `NAME_REQUIRED`, `SQL_REQUIRED`, `NAME_CONFLICT` (409) |
| `/api/create_custom_sql` | POST | legacy alias of the above |
| `/api/custom_sql` | GET | — |
| `/api/custom_sql/{name}` | GET | `NOT_FOUND` |
| `/api/custom_sql/{name}` | PATCH | `INVALID_JSON`, `NOT_FOUND`, `SQL_REQUIRED` |
| `/api/custom_sql/{name}` | DELETE | `NOT_FOUND` |
| `/api/sqlqueries/{name}` | GET | `NOT_FOUND`, `SQL_TIMEOUT` (504), `SQL_ROW_LIMIT` (413), `DB_ERROR` |

## Files

`GET /api/folders/list` → configured image folders with file lists.
`POST /api/file` `{folderPath, fileName}` → binary stream. `INVALID_FILE_PATH` on any traversal/allow-list violation.

## Orders (Hasavshevet)

`POST /api/sendOrder` → `202 {"status":"queued","jobId":"<orderNumber>"}`; `VALIDATION_ERROR`, `QUEUE_FULL` (503).
`GET /api/sendOrder/{jobId}` → `{"jobId","status":"queued|running|done|failed","orderNumber","writtenFiles"}`; `NOT_FOUND`.

## Price & stock

`POST /api/priceAndStockHandler` → routed by config `erp` to Hasavshevet (`GPRICE_Bulk` + `GetOnHandStockForSkus`) or SAP B1 (12s timeout).

### Value formatting (all query routes)

Matches the old Node driver, deliberately: datetimes are
`2026-03-08T00:00:00.000Z`; `DECIMAL`/`NUMERIC`/`MONEY` are JSON numbers in
shortest form (`13085`, not `"13085.00"`). This is a wire contract with a live
backend — see [development.md](development.md#changing-response-value-formatting).

## Error code reference

Every code the API can return, with its status. Codes are stable; branch on these.

| Code | Status | Meaning |
|---|---|---|
| `UNAUTHORIZED` | 401 | Missing, malformed, wrong or expired credential. No distinction between the cases, by design. |
| `INVALID_CREDENTIALS` | 401 | `POST /auth/token` refused the username/password, or could not parse the body. |
| `TOKEN_ISSUE_FAILED` | 500 | Credentials were right but signing the token failed. No known trigger. |
| `RATE_LIMITED` | 429 | Per-IP bucket exhausted. Applied before auth. |
| `NOT_FOUND` | 404 | Unknown route, saved query, or job id. |
| `INVALID_JSON` | 400 | Unparseable body, or trailing data after the JSON document. |
| `NAME_REQUIRED` | 400 | Saved-query name empty, too long (>200), or contains a control character, `/` or `\`. |
| `NAME_CONFLICT` | 409 | Saved-query name already exists. Use `PATCH`. |
| `SQL_REQUIRED` | 400 | Saved-query `sql` empty. |
| `STORE_ERROR` | 500 | `queries.json` could not be written — permissions or disk. |
| `SQL_TIMEOUT` | 504 | Execution exceeded `queries.timeoutSeconds`. |
| `SQL_ROW_LIMIT` | 413 | Result exceeded `queries.maxRows` across all recordsets. |
| `DB_ERROR` | 500 | Query execution failed. The real driver error is in `server.log`. |
| `DB_UNAVAILABLE` | 503 | No usable database connection. |
| `INVALID_FILE_PATH` | 400 | Folder not exactly an `imageFolders` entry, or `fileName` was `.`/`..`/absolute, or the resolved path escaped the folder. |
| `FILE_NOT_FOUND` | 404 / 400 | File missing (404), or the path is a directory (400). |
| `FILE_OPEN_ERROR` | 500 | File exists but could not be opened. |
| `FILE_INFO_ERROR` | 500 | File metadata could not be read. |
| `FILE_PATH_ERROR` | 500 | Unexpected failure resolving the path. |
| `FOLDER_CONFIG_INVALID` | 500 | `imageFolders` could not be canonicalised at startup — fix the config. |
| `FOLDER_LIST_FAILED` | 500 | A configured folder could not be listed (removed? permissions?). |
| `VALIDATION_ERROR` | 400 | Order request failed validation. `details` names the missing or invalid fields. |
| `QUEUE_FULL` | 503 | More than 64 orders queued. |
| `QUEUE_UNAVAILABLE` | 503 | Order queue not running — the ERP is not Hasavshevet, or startup failed. |
| `ERP_NOT_SUPPORTED` | 400 | Configured `erp` has no price/stock implementation path. |
| `PRICE_STOCK_FAILED` | 500 | The ERP price/stock call failed. |
| `ORDERS_NOT_CONFIGURED` | 501 | This connector cannot send orders: it needs `erp: hasavshevet` and a `sendOrderDir`. Expected on a read-only node - see [deployment-topologies.md](deployment-topologies.md). |
