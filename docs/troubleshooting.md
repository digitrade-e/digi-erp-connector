# Troubleshooting

Symptom → cause → fix. Logs first: `%PROGRAMDATA%\digi-erp-connector\server.log`
(daemon) and `ui.log` (GUI). Neither contains secrets, so they are safe to share.

## The service will not start, or starts and stops

Check `server.log` — the daemon logs its whole startup sequence.

| Log line | Cause | Fix |
|---|---|---|
| `config not found; run digi-erp-connector UI to create it` | No `config.yaml`. Normal on a fresh install. | Configure via the GUI or `cutover-seed`. |
| `failed to load config` | Malformed YAML. | Fix the syntax; check indentation. |
| `config validation error` … `bearerToken is required` | No token. | Generate one in the GUI. |
| `apiListen must be in host:port format` / `port is invalid` | Bad bind address. | See [configuration.md](configuration.md#apilisten--bind-address). |
| `legacyCompat.enabled requires jwtSecret, jwtUser and jwtPassword` | Compat switched on with blanks. | Fill them in, or set `enabled: false`. |
| `failed to load db password` | No stored secret, or it was written under a different `erp` value. | Re-enter the password in the GUI and save. The secret key is per-ERP. |
| `db connection failed` | See the next section. | |

**The daemon exits when the database is unreachable at startup — by design.** So
"the service stops right after starting" is usually a database problem, not a
connector problem. This is exactly what the service dependency plus
restart-on-failure in [operations.md](operations.md) exists for.

Another cause of a service that dies on boot but works when started by hand: SQL
Server was not ready yet. Same fix.

## Database connection failures

`server.log` records the parameters used (never the password):
`calling db.Open: driver=mssql host=… port=… database=… user=…`.

| Symptom | Likely cause |
|---|---|
| `Login failed for user '…'` | Wrong password, or the stored secret is stale. Re-enter it in the GUI. Also check SQL Server allows SQL authentication. |
| certificate / TLS errors | `encrypt: true` without `trustServerCertificate: true` against a self-signed certificate. Set both, or neither. |
| timeouts, `no such host` | Wrong host or instance name. For a named instance use `host\instance` and make sure the SQL Browser service is running. |
| works in the GUI, fails in the service | The GUI runs as you; the service runs as LocalSystem. A DPAPI secret written with *user* scope would not be readable — this build uses machine scope, so suspect instead a firewall rule or a login restricted to specific users. |

Use the GUI's **Test connection** to iterate quickly: it tries the values in the
form without saving them.

## Every request returns 401

| Cause | Check |
|---|---|
| Token mismatch | `bearerToken` in `config.yaml` vs what the caller sends. The comparison is exact and constant-time. |
| Missing or malformed header | It must be `Authorization: Bearer <token>` — two whitespace-separated fields. |
| The backend is doing the old JWT exchange | It is calling `POST /auth/token` and getting 404, or sending a JWT that is not accepted. Either migrate the backend to the static token or enable `legacyCompat` — see [legacy-compat.md](legacy-compat.md). |
| Compat was switched off while tokens were live | Legacy JWTs are rejected the moment `legacyCompat.enabled` is false. |

A 401 with `{"error":"invalid_credentials"}` is different: that is `/auth/token`
rejecting the username/password, not a bearer-token problem.

## 429 RATE_LIMITED

25 requests/second with a burst of 50, per client IP, applied **before** auth. A
backend that fans out parallel requests can trip it. Batch the calls, or reconsider
the limits in `internal/api/server.go` — they are constants, not config.

## 413 SQL_ROW_LIMIT

The query returned more rows than `queries.maxRows` (default 10,000) across all
recordsets.

This bites specifically when replacing an `electron-mssql-app` connector, which had
**no** row cap: a query that always worked starts failing. Find the real row count
and raise `maxRows` accordingly, or add paging to the query
(`OFFSET`/`FETCH NEXT` — the binder coerces those parameters to integers
automatically).

## 504 SQL_TIMEOUT

Execution exceeded `queries.timeoutSeconds` (default 30). Either the query needs
work (check the plan and indexes on the customer's database) or the timeout needs
raising. Note the HTTP write timeout is 60s, so a query timeout above ~55s cannot
usefully be reported.

## 503 DB_UNAVAILABLE

The connection was lost after startup. The connector fails closed rather than
panicking. Check SQL Server, then let the service recovery actions restart the
connector, or restart it yourself.

## Saved query problems

| Symptom | Cause |
|---|---|
| `404 NOT_FOUND` on `/api/sqlqueries/{name}` | The name does not exist, or the case differs — names are case-sensitive. `GET /api/custom_sql` lists them. |
| `409 NAME_CONFLICT` on create | The name is taken. Use `PATCH` to change it. |
| A parameter binds as the wrong type | See the binding rules in [saved-queries.md](saved-queries.md). `skuArray`, `warehouse`, `sku` and `syncKey` are always bound as strings; other all-digit values become integers. |
| Numbers arrive as JSON strings | Should not happen: `DECIMAL`/`NUMERIC`/`MONEY` are emitted as JSON numbers deliberately. If you see quoted numbers, the column is a real string type. |
| `500 STORE_ERROR` on a write | `queries.json` could not be written — check permissions on the data directory and free disk space. |

## Orders

| Symptom | Cause |
|---|---|
| `202` then the job reports `failed` | `GET /api/sendOrder/{jobId}` gives the reason; `server.log` has the detail. Most often `sendOrderDir` is empty or not writable, or the importer returned non-zero. |
| Files appear but nothing is imported | No `hasBatFile`/`hasExePath` configured, so nothing runs after the files are written. |
| `503 QUEUE_FULL` | More than 64 orders queued. The worker is serial by design; investigate why the importer is slow. |
| Order numbers restarted from a low number | `lastOrderNumber.json` is missing from `sendOrderDir`. Restore it, or seed it from the previous install before going live. |
| Two orders interfered with each other | Should be impossible — the queue is single-worker. If the customer has *another* process importing IMOVEIN files from the same directory, that is the collision. Give the connector its own directory. |

## File endpoint returns INVALID_FILE_PATH

By design, the request must name a folder that is **exactly** one of the configured
`imageFolders` (after canonicalisation), and `fileName` must be a plain name — no
`.`, `..`, no absolute path. Subfolders are not reachable; add them to
`imageFolders` if they should be.

Note that paths are canonicalised on both sides, so a short 8.3 path or a junction
still matches the configured folder.

## GUI problems

| Symptom | Cause |
|---|---|
| "cannot run as a Windows Service or in a non-interactive session" | The GUI was launched by a service or in session 0. Launch it from the desktop shortcut. |
| Buttons do nothing / service control fails | It is not elevated. Use the shortcut, which goes through `launch-admin.vbs`. |
| "Error loading config: …" in the status bar | `config.yaml` is corrupt. The window still opens so you can fix the values. |
| Nothing in `ui.log` | The log file was created by an elevated process and the current one cannot append. Harmless. |
| Saved settings seem to have reverted | Check you clicked Save; the GUI preserves keys it does not display, so `legacyCompat` and `queries` are not the cause. |

## Nothing reaches the connector from another machine

1. Is `apiListen` a loopback address? Then only local callers can reach it.
2. Is there an inbound firewall rule for the port? A rule scoped to a *program*
   will not cover a replaced binary; prefer a port rule.
3. Does it listen where you think? `Get-NetTCPConnection -State Listen | Where-Object LocalPort -eq <port>`.
   `::` means dual-stack (both IPv4 and IPv6); `127.0.0.1` means local only.

## When you suspect a regression

Capture the responses of every saved query before and after the change and diff
them — see [operations.md](operations.md#verifying-a-change-did-not-alter-behaviour).
Beware live data: re-capture both sides close together, or a genuine data change
looks like a regression.
