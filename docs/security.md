# Security Model

## Threat model (local integration)
- The daemon is intended for local machine use (main app ↔ connector).
- Default bind is `127.0.0.1` to avoid LAN exposure.

## Authentication
- All `/api/*` endpoints require:
  - `Authorization: Bearer <token>`

Token storage:
- Stored in config file.
- Must never be printed to logs.

## Recommended hardening
- Bind to localhost by default.
- Add request logging without secrets.
- Add rate-limiting for expensive endpoints (SQL / file list).
- Add server-side timeouts:
  - SQL query timeout
  - Max response row limit
- Use least-privilege DB user:
  - Read-only permissions for SQL endpoint and handlers.
  - Hasavshevet setup requires CREATE/ALTER permission for `GPRICE_Bulk` and `GetOnHandStockForSkus` procedures (or pre-create them once, then revoke).

## File endpoint hardening
- Only serve files under configured folders.
- Canonicalize paths and block traversal.
- Enforce filename allow rules and return 404 for missing files.

## SQL endpoint hardening
- Enforce SELECT-only validation.
- Bind parameters; never interpolate strings.
- Consider an allow-list of schemas/tables if needed later.
