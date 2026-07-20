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
