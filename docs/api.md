# API Reference

Base URL: `http://<apiListen>` (default `127.0.0.1:8080`). Plaintext HTTP — there
is no TLS; see [security.md](security.md).

Every `/api/*` route requires `Authorization: Bearer <token>` and is rate-limited
per client IP (25 req/s, burst 50 → `429 RATE_LIMITED`). Rate limiting is applied
**before** authentication. The single exception is `POST /auth/token`, which exists
only in legacy compatibility mode and is the credential exchange itself.

Errors are always `{ "error": "<message>", "code": "<CODE>", "details": {} }`.
Branch on `code`, not on the message — messages may be reworded, codes are stable.
Legacy compatibility routes answer in the old app's shape instead
(`{"error":"snake_case"}`), noted where they appear.

Request bodies are capped at 1 MiB, must be a single JSON document (trailing data
is rejected), and driver errors are never returned to callers.

## Contents

- [Health](#health) · [Saved queries](#saved-queries) · [Files](#files) ·
  [Orders](#orders-hasavshevet) · [Price & stock](#price--stock) ·
  [Legacy routes](#legacy-compatibility-routes-only-when-legacycompatenabled) ·
  [Error codes](#error-code-reference)

## Health

`GET /api/health` → `{"status":"ok"}` | `503 DB_UNAVAILABLE`

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

## Legacy compatibility routes (only when `legacyCompat.enabled`)

Absent by default. These reproduce electron-mssql-app so an unmigrated backend
keeps working; they answer errors in the **old shape** — `{"error":"not_found"}`,
not the `{error, code, details}` envelope above. See `legacy-compat.md`.

| Route | Method | Response |
|---|---|---|
| `/auth/token` | POST | `{access_token, token_type:"Bearer", expires_in}`; `401 {"error":"invalid_credentials"}`. **Unauthenticated** (rate-limited). The returned JWT is accepted on every route, as is the static bearer token. |
| `/api/ping` | GET | `{"ok":true,"ts":<epoch ms>}` — no DB access |
| `/api/test-connection` | POST | `{mssql:{server,database,user,password,port,encrypt,trustServerCertificate}}` → `{"ok":true}` \| `400 {"ok":false,"error":"connection_failed"}` |
| `/api/customers` | GET | `?limit=N` (default 50, max 200) → bare array of `dbo.Items` rows |
| `/api/orders/{id}` | GET | one `dbo.Items` row; `400 invalid_id`, `404 not_found` |
| `/api/query` | POST | **`allowRawSQL` only.** `{sql, params}` → `{value, rowsAffected}`; `400 sql_required` \| `only_select_allowed`, `500 query_failed` |

### Value formatting (all query routes)

Matches the old Node driver, deliberately: datetimes are
`2026-03-08T00:00:00.000Z`; `DECIMAL`/`NUMERIC`/`MONEY` are JSON numbers in
shortest form (`13085`, not `"13085.00"`). This is a wire contract with a live
backend — see [development.md](development.md#changing-response-value-formatting).

## Error code reference

Every code the API can return, with its status. Codes are stable; branch on these.

| Code | Status | Meaning |
|---|---|---|
| `UNAUTHORIZED` | 401 | Missing, malformed or wrong bearer token. No distinction between the cases, by design. |
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
| `NOT_IMPLEMENTED` | 501 | Reached a declared-but-unimplemented ERP path (Priority). |

Legacy compatibility routes do **not** use these codes; they return the old app's
snake_case strings (`not_found`, `invalid_id`, `sql_required`,
`only_select_allowed`, `query_failed`, `invalid_credentials`, `connection_failed`).
