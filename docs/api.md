# API Reference

Base URL: `http(s)://<apiListen>` (default `127.0.0.1:8080`). HTTP by default;
**HTTPS when `tls.certFile`/`tls.keyFile` are configured**, which you should do
whenever the backend is on another machine — see
[configuration.md](configuration.md#tls) and [security.md](security.md).

Every `/api/*` route requires `Authorization: Bearer <credential>`. Only
`POST /auth/token` is rate-limited per client IP (25 req/s, burst 50 →
`429 RATE_LIMITED`); the `/api/*` routes carry no limit since 2026-11-10.

The credential is a token obtained from `POST /auth/token` by posting the
configured username and password. There is no static API token. See
[authentication.md](authentication.md).

Errors are always `{ "error": "<message>", "code": "<CODE>", "details": {} }`.
Branch on `code`, not on the message — messages may be reworded, codes are stable.

Request bodies are capped at 1 MiB, must be a single JSON document (trailing data
is rejected), and driver errors are never returned to callers.

## Contents

- [Authentication](#authentication) · [Health](#health) ·
  [Saved queries](#saved-queries) · [Files](#files) ·
  [Orders](#orders-hasavshevet) · [Receipts](#receipts-hasavshevet) ·
  [Price & stock](#price--stock) ·
  [Error codes](#error-code-reference)

## Authentication

`POST /auth/token` → `200 {"access_token":"…","token_type":"Bearer","expires_in":1800}`

Body `{"username","password"}`. Unauthenticated (it is the credential check) but
logged and rate-limited. Anything wrong is `401 INVALID_CREDENTIALS`; a malformed
body is not distinguished from wrong credentials. **This is the only way to get a
credential** — every other route requires the token it returns.

## Health

`GET /api/health` → `{"status":"ok"}` | `503 DB_UNAVAILABLE` — pings the database.

`GET /api/ping` → `{"ok":true,"ts":1786283101465,"version":"v1.0.18"}` — **no
database touch**. Use it to check that the service is up and a credential is
valid; use `/api/health` when you also want the database checked. `ts` is epoch
milliseconds. `version` is the release tag the binary was built from (`"dev"`
for a local build); a connector without it predates `POST /api/sendReceipt`.

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

`POST /api/sendOrder` → `202 {"status":"queued","jobId":"<orderNumber>"}`; `VALIDATION_ERROR`, `QUEUE_FULL` (503), `ORDER_INDEX_UNREADABLE` (500), `ORDER_COUNTER_UNREADABLE` (503), `ORDER_NOT_QUEUED` (503).
Optional document fields (DIG-1295), per-tenant erp-manager config, each absent or `null` = today's value: `documentId` (1–99, IMOVEIN line4; else 30/32/40/74 by type), `warehouse` (1–999999999, line12; else 1), `vatPercent` (0–99.99, ≤ 2 decimals, written `17.00`, line18; else `18.00`), `asmahta2` (1–9 digits, the B2B cart id, line8; else the order number). A JSON number or a string; anything that does not fit is `400 VALIDATION_ERROR` with `details.field`, never cut. See [hasavshevet-send-order.md](hasavshevet-send-order.md#document-fields-dig-1295).
Optional `header` object (DIG-1315): `accountNameDetail` (≤ 49, line5 as `<account name>|<detail>`), `address` (≤ 50, line6), `city` (≤ 50, line7), `phone` (≤ 30, line35), `extraText1`–`extraText5` (≤ 50, lines 45–49), `extraSum1`–`extraSum3` and `extraNum1`–`extraNum2` (decimal numbers ≤ 13 characters, 12 with `sendOrderFormat: legacy`; lines 50–52, 81–82). Absent / `null` / blank = today's value. Unknown, duplicate or wrong-case keys, wrong kinds and over-long values are `400 VALIDATION_ERROR`, `details.field` `header/<key>`, never cut. With `header` present the 202 adds `"headerApplied":[<keys written>]`; an older connector drops `header` silently and never answers it. See [hasavshevet-send-order.md](hasavshevet-send-order.md#header-fields-dig-1315).
With `sendOrderFormat: legacy` (DIG-1297) a number the legacy layout's narrower fields would change is also `400 VALIDATION_ERROR`, `details.field` one of `vatPercent`, `discount`, `details[i].quantity`, `details[i].originalPrice`, `details[i].discount`, before a number is reserved; a cut of trailing zeros only (`18.00` → `18.0`) passes. With `sendOrderPickupDir` (model a) `writtenFiles` lists the published pickup file first, then the history copies; no fixed `IMOVEIN.doc/.prm`. See [§ Model (a)](hasavshevet-send-order.md#model-a--sendorderpickupdir-dig-1297).
`historyId` is the idempotency key (DIG-1291): a repeat answers `202 {"status":"queued|running|done|failed|unknown","jobId":"<first orderNumber>","replay":true,...}` and writes nothing — except an order that failed before any file was written, which it queues again under the same number (`"status":"queued"`). See [hasavshevet-send-order.md](hasavshevet-send-order.md#idempotency--historyid-dig-1291).
`GET /api/sendOrder/{jobId}` → `{"jobId","status":"queued|running|done|failed|unknown","orderNumber","writtenFiles"}`; `NOT_FOUND`. After a restart a jobId is answered from the order index. `failed` covers both a final failure and one a resend will write (nothing was written yet): resend once on `failed` with the same `historyId` — a second `failed` is final.
### `POST /api/v2/sendOrder` — documentRule (plan 087, v1.0.30)

Today's body plus an optional `documentRule` — which counter numbers the order and which typed folder it goes to,
**by key only** (the box says what each key means; never a path or a file name):

```json
"documentRule": {"v": 1, "temporary": false, "folder": "herp", "counter": "b2b"}
```

- No rule (absent or `null`) = exactly `/api/sendOrder`'s answer and output.
- Strict: an unknown, duplicate or wrong-case key, a value of the wrong type (`folder` / `counter` non-empty strings;
  `temporary` a JSON boolean, or — review R8, as the B2B sends ERP-manager's BOOL constants — exactly the string
  `"true"` or `"false"` in any case, `"1"` or `"0"`, normalised so a repeat in the other spelling is the same rule),
  `v` other than the number `1` → `400 VALIDATION_ERROR`, `details.field`
  `documentRule/<key>`. An unknown folder or counter, one of the wrong kind, a test counter into a folder that is not
  a test folder or a production counter into a test folder (`documentRule/counter`, review m10), `temporary: true` on
  a folder without a temporary prefix, a retired counter → 400 the same way (for a new order: a repeat is answered by
  its frozen rule).
  Nothing is reserved or written.
- `202` adds `{"counter","number","fileName","folder"}`: the counter that issued the number, the number (= `jobId`),
  the file name in the folder (the folder's pattern; `temporary` applies its prefix) and the folder's key. On a
  replay, the rule frozen at reservation.
- A repeat of a `historyId` with another rule (or none, through `/api/sendOrder`) → `409 DOCUMENT_RULE_CONFLICT`,
  nothing written: the rule is fixed when the number is reserved. A repeat is looked up **before** the rule's keys are
  checked: a known key replays (or re-queues, or answers 409) by its frozen rule even when its folder or counter key
  no longer exists on the box; the keys-and-layout 400s are for new orders only (both routes: a v1 repeat no longer
  meets today's layout check either).
- The counter's next number outside its range → `503 COUNTER_EXHAUSTED`; its store missing → `503 COUNTER_MISSING`.
- Behind the link, a folder accounts besides the administrators can write to → `503 FOLDER_WRITABLE`, nothing
  reserved (the daemon checks at its start; a re-queue into it waits the same way).
- Behind the link, an order without a rule whose importer (has.exe or the BAT — the file, its folder or one above) can
  be changed by accounts besides the administrators → `503 IMPORTER_WRITABLE`, nothing reserved; an order the worker
  meets it on stays retryable (review T1).
- A resend of a retryable order whose folder's test flag changed since its reservation → `503 FOLDER_TEST_CHANGED`,
  nothing written; it stays retryable until the flag is back (review R2).
- A connector older than v1.0.30 answers the route `404 NOT_FOUND` (nothing written). `/api/sendOrder` ignores
  `documentRule`, as every older connector did.

See [hasavshevet-send-order.md § documentRule](hasavshevet-send-order.md#documentrule--post-apiv2sendorder-plan-087).

### `GET /api/counters`, `GET /api/documents/capabilities` (plan 087)

Read-only. `GET /api/counters` → `[{"key","kind":"orders|receipts","min","max","last","highestIssued","remaining","test"}]`
(+ `"retired":true`, and `"error"` when something could not be read — a **code**, never text: `store_missing` (the
store file does not exist, or has no folder), `store_damaged` (it holds no usable number or cannot be read) — `last`
is then 0 — or `issued_unreadable` (the range's index or history could not be read: `highestIssued` is not known).
The text, with its paths, is in the connector's log (review m7)). The answer comes from a cache of at most 3 s, read
again at once after any number this connector reserves (review m13: each read lists the history and the indexes, and
`/api/*` has no rate limit); a store edited by hand shows within 3 s. `last` is the last number
issued, one meaning for both kinds; `highestIssued` the highest number an index entry or a history folder holds in the
range (the index can run ahead of the store); `remaining` = `max − max(last, highestIssued)`. With no counter configured
the box lists today's two under the keys `orders` and `receipts` (range 1–999 999 999).

`GET /api/documents/capabilities` → `{"contract":1,"folders":[{"key","kind","test"}],"counters":[{"key","kind","test"}],"placeholders":["prefix","n"]}`
— what a rule may name (retired counters are left out).

Counters and folders change only through the signed provision commands `counters` and `documents` (digi-mcp), never
on this API.

`GET /api/sendOrder` → `{"queued","running"}`: orders accepted (202) and not written yet. The queue is in memory, so a restart with either above 0 loses them; `digi-erp-connector-provision` asks before it restarts the service. `QUEUE_UNAVAILABLE` (503).

## Receipts (Hasavshevet)

`POST /api/sendReceipt` → `200 {"receiptNumber":"1000","written":true,"idempotencyKey":"<uuid>","import":"not_run|started|ok|failed","warnings":[]}`.
Synchronous (up to 120 s), one KUPAIN receipt per `idempotencyKey`; a repeat of
the key answers the same receipt. The body is flat — exactly the sixteen keys of
the backend's `Receipt` (plan 056 §4.6a), all present, none `null` — and decoded
strictly: a missing or `null` key, an unknown field, a wrong type, a duplicate key
or a key in the wrong case is a 400. Only a 400 is final for the backend; it
retries anything else with the same key. Errors: `VALIDATION_ERROR` (400, `details.field`),
`RECEIPTS_NOT_CONFIGURED` / `RECEIPT_NUMBER_STORE_UNAVAILABLE` /
`RECEIPT_WRITER_BUSY` / `RECEIPT_REQUEST_READ_FAILED` / `RECEIPT_DATA_IN_USE` / `COUNTER_EXHAUSTED` / `COUNTER_MISSING` / `FOLDER_WRITABLE` / `IMPORTER_WRITABLE` (503), `RECEIPT_CONFIG_INVALID` /
`RECEIPT_INDEX_UNREADABLE` / `RECEIPT_STORED_REQUEST_INVALID` /
`RECEIPT_WRITE_FAILED` (500). Full contract, field
table and recovery: [hasavshevet-send-receipt.md](hasavshevet-send-receipt.md).

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
| `UNAUTHORIZED` | 401 | Missing, malformed, wrong or expired token. No distinction between the cases, by design. |
| `INVALID_CREDENTIALS` | 401 | `POST /auth/token` refused the username/password, or could not parse the body. |
| `TOKEN_ISSUE_FAILED` | 500 | Credentials were right but signing the token failed. No known trigger. |
| `RATE_LIMITED` | 429 | `POST /auth/token` only: its per-IP bucket is exhausted. |
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
| `VALIDATION_ERROR` | 400 | Order or receipt request failed validation. For receipts `details.field` names the key (`name`, `remarks`, `body` for the document as a whole) — see [hasavshevet-send-receipt.md](hasavshevet-send-receipt.md#request-fields). |
| `QUEUE_FULL` | 503 | More than 64 orders queued. |
| `ORDER_INDEX_UNREADABLE` | 500 | The order index entry for this `historyId` is damaged. Nothing queued, no number given; other keys are unaffected. A person must look ([troubleshooting.md](troubleshooting.md#a-damaged-order-index-entry)); never just delete index files. |
| `ORDER_COUNTER_UNREADABLE` | 503 | `lastOrderNumber.json` exists but holds no usable number; no new order until a person repairs it (known keys are still answered). |
| `ORDER_NOT_QUEUED` | 503 | The order index could not be written or recovered. Nothing reserved; retry with the same `historyId`. |
| `DOCUMENT_RULE_CONFLICT` | 409 | Plan 087: this `historyId` was accepted with another `documentRule` (or none). Nothing written; the rule is fixed at reservation. |
| `COUNTER_EXHAUSTED` | 503 | Plan 087: the counter's next number is outside its range (orders and receipts; also the order counter past 999 999 999). Nothing reserved; resend with the same key once the range is widened (`counters range`). |
| `COUNTER_MISSING` | 503 | Plan 087: the counter's store does not exist where it fails closed (a counter of its own, or `lastOrderNumber.json` once counters are configured). Never 1 again; restore it or set it (`counters start-from` with `expectLast: -1`). |
| `FOLDER_TEST_CHANGED` | 503 | Plan 087 review R2: the resend of a retryable order whose folder's test flag is no longer the one frozen at its reservation. Nothing written; the order stays retryable; resend once the flag is back (`state`'s `orders.folderTestChanged`). |
| `FOLDER_WRITABLE` | 503 | Plan 087 review M1: behind the link, the order's typed folder (or the bound receipt folder) can be written by accounts besides the administrators, so the connector writes nothing there. Nothing reserved; resend with the same key once the folder is locked down and the service restarted ([troubleshooting.md](troubleshooting.md#a-folder-others-can-write-to)). |
| `IMPORTER_WRITABLE` | 503 | Plan 087 review T1: behind the link, the importer the order or receipt would run (has.exe or a BAT: the file, its folder or a folder above — and, since v1.0.31, the parameter file has.exe is handed: review R4-4) can be changed by accounts besides the administrators, or does not read — `trustedWriters` included — so the connector runs nothing and reserves nothing. Read before each run (the daemon's guard; a refused one again at most once a minute when needed). A queued order stays retryable. |
| `QUEUE_UNAVAILABLE` | 503 | Order queue not running — the ERP is not Hasavshevet, or startup failed. |
| `ERP_NOT_SUPPORTED` | 400 | Configured `erp` has no price/stock implementation path. |
| `PRICE_STOCK_FAILED` | 500 | The ERP price/stock call failed. |
| `ORDERS_NOT_CONFIGURED` | 501 | This connector cannot send orders: it needs `erp: hasavshevet` and a `sendOrderDir`. Expected on a read-only node - see [deployment-topologies.md](deployment-topologies.md). |
| `RECEIPTS_NOT_CONFIGURED` | 503 | This connector does not write receipts: it needs `erp: hasavshevet` and a `sendReceiptDir`. Nothing was read or written. |
| `RECEIPT_NUMBER_STORE_UNAVAILABLE` | 503 | The receipt number store is not seeded (`cutover-seed -receipt-seed N`), unreadable, exhausted, or was moved back. Never falls back to 1. |
| `RECEIPT_WRITER_BUSY` | 503 | Another import held the import lock for 20 s. Nothing reserved; retry with the same key. |
| `RECEIPT_CONFIG_INVALID` | 500 | A `receiptCardNames` override cannot be written. Refused at startup, so not expected. |
| `RECEIPT_INDEX_UNREADABLE` | 500 | A receipt index entry is corrupt, or two keys hold one number. A person must look; never delete index files. |
| `RECEIPT_STORED_REQUEST_INVALID` | 500 | A receipt reserved by an earlier request no longer builds while being finished. The connector's index is at fault, not this request. |
| `RECEIPT_WRITE_FAILED` | 500 | The receipt files could not be written (disk, permissions). The number stays with the key; a retry finishes it. |
