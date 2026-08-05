# API Reference

Base URL: `http://<apiListen>` (default `127.0.0.1:8080`).
All routes require `Authorization: Bearer <token>` and are rate-limited
(25 req/s, burst 50, per client IP → `429 RATE_LIMITED`).

Errors are always `{ "error": "<message>", "code": "<CODE>", "details": {} }`.

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
shortest form (`13085`, not `"13085.00"`).
