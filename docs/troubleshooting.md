# Troubleshooting

Symptom → cause → fix. Logs first: `%PROGRAMDATA%\digi-erp-connector\server.log`
(daemon) and `ui.log` (GUI). Neither contains secrets, so they are safe to share.

## The service will not start, or starts and stops

Check `server.log` — the daemon logs its whole startup sequence.

| Log line | Cause | Fix |
|---|---|---|
| `config not found; run digi-erp-connector UI to create it` | No `config.yaml`. Normal on a fresh install. | Configure via the GUI or `cutover-seed`. |
| `failed to load config` | Malformed YAML. | Fix the syntax; check indentation. |
| `auth.username and auth.password are required` | No credential. The connector has only one and cannot serve without it. | Fill both in the GUI's **Authentication** section, or run `cutover-seed -auth-user …`. |
| `auth.secret is required` | The signing secret is blank and could not be generated — normally the daemon writes one on first start. | Check that `config.yaml` is writable, or press **Regenerate** in the GUI. |
| `apiListen must be in host:port format` / `port is invalid` | Bad bind address. | See [configuration.md](configuration.md#apilisten--bind-address). |
| `failed to load db password` | No stored secret, or it was written under a different `erp` value. | Re-enter the password in the GUI and save. The secret key is per-ERP. |
| `db connection failed` | See the next section. | |
| (Windows Event Log, Application, source `digi-erp-connectord`) `refused to start: …` — `sc query digi-erp-connectord` shows `SERVICE_EXIT_CODE : 2`; the GUI says "The service refused to start" | Windows: the data directory could not be created or protected, so the daemon opened no file there; the event names the path and one of the reasons below. `server.log` is not written on such a start. | Fix the reason below, then start the service (the service manager also retries after 1 and 5 min). |
| `could not set the protected DACL on the data directory …; not starting` | Windows: the walk could not open or rewrite an entry — the directory or something under it denies the service (e.g. a folder planted with a protected ACL that leaves SYSTEM out), or one of the refusals below. The path is in the line. | Someone may still hold the planted object open, and `takeown`/`icacls` keep the object itself. Prefer: copy the file (`copy`), delete the original, put the copy back — or, for a folder, move its contents out, delete it, recreate it. Reboot first if you suspect a held handle. Only for an access problem on a known-good tree: `takeown /A /F "<path>" /R /D Y` then `icacls "<path>" /reset /T`. |
| `… a symlink, junction or other reparse point is in the data directory: <path>` | Windows: that path is a link. Receipt data written through it would leave the protected tree, so the daemon changes nothing and stops. | Inspect it with `fsutil reparsepoint query "<path>"`. A junction or directory symlink: `rmdir "<path>"` removes the link, not its target; a file symlink: `del "<path>"`. Then start the service. |
| `… a file with more than one hard link is in the data directory: <path>` | Windows: that file is also reachable under another name; its permissions are shared with it. | `fsutil hardlink list "<path>"` shows the other names. Copy the file, delete the original, put the copy back (or delete it if it is not the connector's). |
| `… an entry in the data directory has an untrusted owner: <path> is owned by <SID>` (optionally `could not resolve whether … is an administrator`) | Windows: something under the data directory was created by an account that is not an administrator — a planted entry — or one whose administrator membership could not be looked up (domain controller unreachable). | Check it is the connector's ([probe](configuration.md#data-directory-permissions-windows)). A planted entry: remove links first (`dir /AL /S "%ProgramData%\digi-erp-connector"`), then copy → delete the original → put the copy back (a `takeown` keeps the planted object and any handle on it); reboot first if in doubt. A genuine admin file: save once from the elevated GUI (it hands its files to Administrators), or `takeown /A /F "<path>"`. |
| `could not create the data directory …; not starting` (Event Log) | Windows: `%ProgramData%\digi-erp-connector` does not exist and the service could not create it (a file of that name in the way, a disk or permissions problem on `%ProgramData%`). | Remove whatever is in the way, check `%ProgramData%` is writable by SYSTEM, start the service. |
| `… is owned by S-1-5-21-… (could not resolve whether … is an administrator: … resolves to no account …)` or `… list the local groups of … (domain controller unreachable)` | Windows: "resolves to no account" means a deleted account (an orphaned SID — refused until the file is replaced or re-owned) **or** a domain account while the domain controller cannot be reached (Windows answers the same for both); "list the local groups" means membership could not be checked. Either way the entry stays refused until resolved; the service is retried every 30 min, and the Event Log repeats the same reason at most once a day. | Orphaned: copy the file, delete the original, put the copy back (it is then owned by you), or `takeown /A /F "<path>"`. Unreachable DC: restore the connection, or take the same steps; once resolved the entry is handed to Administrators and the DC is not needed again. |
| `server.log is not provably this service's own file and could not be moved aside …; logging to …server-<time>.log this time` | Windows: a program holds `server.log` open (e.g. a `Get-Content -Wait`, or a 1.0.17 GUI) while it had to be replaced. The service runs and logs to the named file. | Close that program; the next start moves `server.log` aside. |
| `dataacl: … resolves outside the data directory` / `GetFinalPathNameByHandle` errors | Windows: an entry turned out to be elsewhere than its path says (a link swapped in during the walk), or its final path could not be read. | Restart the service; if it repeats, look for links (`dir /AL /S`) and remove them. |
| `… The process cannot access the file because it is being used by another process` | Windows: another program held a log or a folder in the data directory without sharing it, for longer than the walk's retries (3 × 250 ms). | Close it (backup or antivirus scanners included) and start again. |
| `receipt configuration: … ambiguous` / `hasReceiptParamFile …` | Both `hasReceiptBatFile` and `hasReceiptParamFile` set, a relative path, or `hasReceiptParamFile` without `hasExePath`. | Keep one receipt importer; absolute paths. See [configuration.md](configuration.md#hasavshevet-receipts). |

**An unreachable database no longer stops the daemon.** It starts, logs

```
database not reachable at startup (...) — the API will start anyway and reconnect
on demand; database-backed endpoints return 503 until it succeeds
```

and serves. So if the service itself is *stopping*, the cause is one of the
configuration errors above, not the database — check the last line before it
exits. If instead the service is running but every DB-backed call returns
`503 DB_UNAVAILABLE`, that is the database, and the connector will pick it up
again by itself once it is reachable.

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

There is one credential — a token from `POST /auth/token` — and every way of
getting it wrong looks identical from outside.
[authentication.md](authentication.md) has the full picture.

| Cause | Check |
|---|---|
| The caller sends a static API token | There is no such thing here any more. It must call `/auth/token` first and send the token that comes back. |
| Missing or malformed header | It must be `Authorization: Bearer <token>` — two whitespace-separated fields. |
| The token expired | Expected, and self-healing: the caller re-authenticates on the 401. Only a problem if it repeats. |
| The credentials do not match | `server.log` shows `rejected /auth/token: user="…"`. Compare `auth.username`/`auth.password` with what the caller is configured with. The old `digitrade`/`123456` is rejected by design. |
| `auth.secret` was regenerated | Every previously issued token stops verifying. Callers recover after one failed request; a caller that does not retry needs its cached token cleared. |
| Testing with curl on PowerShell 5.1 | An inline `-d '{"username":…}'` loses its double quotes before curl sees it, and everything returns 401. Write the body to a file and use `--data-binary "@body.json"`. |

`GET /api/ping` with a fresh token separates "service down" from "credential
wrong" — it returns 200 without touching the database.

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
| Order numbers restarted from a low number | `lastOrderNumber.json` is missing from `sendOrderDir`. Restore it, or seed it from the previous install before going live. (A counter behind the order index is moved past it at startup, with a warning.) |
| `503 ORDER_COUNTER_UNREADABLE` | `lastOrderNumber.json` exists but holds no usable number. It is never reset to 1: repair it (`{"lastOrderNumber": N}`, N at least the highest folder under `sendOrderDir/history/`). Known `historyId`s are still answered meanwhile. |
| `503 ORDER_NOT_QUEUED` | The order index could not be written (disk, permissions) or recovered (the log says why). Nothing was reserved; a retry with the same `historyId` is safe. |
| `500 ORDER_INDEX_UNREADABLE`, or log `ORDER INDEX ENTRY SKIPPED` | One key's entry in `DataDir/orders/index/` is damaged; only that key is refused. See [A damaged order index entry](#a-damaged-order-index-entry). |
| A repeat of an order answers `"status":"unknown"` / log `ORDER OUTCOME UNKNOWN` | The connector stopped between accepting the order and recording its outcome. See [An order whose outcome is unknown](#an-order-whose-outcome-is-unknown). |
| A repeat answers `"status":"queued","replay":true` for an order that had failed | Expected: it failed before any file was written (`retryable` — e.g. `IMOVEIN.doc` held by the importer, the account lookup failed), so the repeat queued it again under its first number. |
| `503 COUNTER_EXHAUSTED` (plan 087) | The counter's next number is outside its range — `GET /api/counters` shows `remaining: 0`. Nothing was reserved. Widen the range (`counters range`, digi-mcp) or move the rule to another counter; resend with the same `historyId`. Also the order counter past 999 999 999. |
| `503 COUNTER_MISSING` (plan 087) | A counter's store does not exist where it fails closed: `DataDir\counters\<key>.json`, or `lastOrderNumber.json` once counters are configured. It is never restarted at 1. Restore the file from a backup, or set it (`counters start-from`, `expectLast: -1`, `from` above every number issued — the command scans the index and history). |
| `409 DOCUMENT_RULE_CONFLICT` (plan 087) | The `historyId` was accepted with another `documentRule` (or none). The rule is fixed at reservation; a re-release under the new rule is a new key from the B2B. |
| `503 FOLDER_TEST_CHANGED` (plan 087 review R2) | A retryable order's folder changed its test flag since the order was reserved (only a hand edit gets past `documents` and `restore`): a test order would land in production, or the reverse. Nothing is written; `state`'s `orders.folderTestChanged` lists them. Put the flag back (or resolve the order by hand, § The order index). |
| `503 FOLDER_WRITABLE` (orders or receipts), provision `folder_writable`, log `FOLDER REFUSED` / `FOLDER WRITABLE` | Accounts besides the administrators can write to a folder the connector writes documents into. See [A folder others can write to](#a-folder-others-can-write-to). |
| `503 IMPORTER_WRITABLE` (orders or receipts), log `IMPORTER REFUSED` / `IMPORTER WRITABLE` | Review T1: accounts besides the administrators — `trustedWriters` too — can change the importer the connector runs as LocalSystem (has.exe or the BAT, its folder or one above), or, since v1.0.31, the parameter file has.exe is handed (`hasParamFile`, `hasReceiptParamFile`; review R4-4: the reason says `its parameter file …`); a parameter file that does not exist refuses too. Behind the link it is not run and nothing is reserved; a queued order stays retryable. Let only SYSTEM and Administrators write the importer and its folders (`icacls`, as an administrator); the daemon reads it again at most once a minute when an order or receipt needs it. Off the link it runs, with the warning. |
| provision `folder_has_executables` | Review T1: `folder-acl` never locks down a folder that holds an importer file or a file Windows runs (PATHEXT, `.dll`, `.ps1`, …; never `.php`). Move the programs out of the document folder, or leave that folder alone. |
| Two orders interfered with each other | Should be impossible — the queue is single-worker. If the customer has *another* process importing IMOVEIN files from the same directory, that is the collision. Give the connector its own directory. |

## A folder others can write to

The connector writes orders and receipts, as LocalSystem, into folders an ERP importer reads: `sendOrderDir` (the
order counter, the history copies, model (b)'s `IMOVEIN.doc` — and `has.exe` runs there), the pickup folder, the
typed folders of plan 087 and the receipt folder. Whoever else can add a file to one can pre-create the next
document's name (every order into it then fails for good, each burning a number), drop a document the importer takes
into the books, or plant a program or DLL that an importer running in that folder loads as LocalSystem. On a default
Windows Server every folder an administrator creates under `C:\` lets every local user (`BUILTIN\Users`) create files
in it, so a folder is writable until someone locks it down (plan 087 review M1).

Who can write is read on the folder itself (add a file or a folder, delete an entry, rename it, change its ACL or
owner, and its owner) and on every folder above it (rename one away: delete on it, or delete-child on its parent).
Creating files in a folder *above* is harmless and not counted. `state`'s `writableFolders` lists every such folder
with the accounts, and:

- **Behind the link** the daemon writes nothing into a typed folder others can write to (orders with a rule naming it,
  receipts when it is the bound receipt folder: `503 FOLDER_WRITABLE`, nothing reserved; server.log `FOLDER REFUSED`);
  the `documents`, `receipts` and `orders` commands refuse such a folder when they add or move it (`folder_writable`),
  `allowWritable` with it — one they leave as it is is a warning in their result's `writableFolders` (review R1a).
  `sendOrderDir`, the pickup folder and `sendReceiptDir` predate plan 087: the daemon only warns about them (server.log
  `FOLDER WRITABLE`), so a box upgraded from v1.0.29 keeps taking orders until they are locked down.
- **Over WinRM** the commands refuse it unless `allowWritable` (a human accepted the risk: a `folder_writable:` warning),
  and the daemon warns.

**Lock a folder down — the command** (plan 087 review R1c): digi-mcp sends `folder-acl` with `op: lockdown` and the
folder's name (a typed folder's key, or `sendOrderDir` / `pickup` / `sendReceiptDir`); `folder-acl-check` first shows
every principal's strongest rights anywhere in the folder before and after. It records the previous ACLs on the box
(`preimage`, for `op: restore`), masks the folder's and every entry's DACL by handle — it **never grants**: SYSTEM,
Administrators and TrustedInstaller keep theirs, a trusted writer keeps at most Modify of what it had, everyone else at
most read & execute (review T2) — and restarts the service. Name the ERP's scheduler account in `documents`'
`trustedWriters` **before** the lock-down, or it too drops to read & execute — and since nothing is ever granted, it
keeps only what **its own** entries give it: write it had only through a group (`Users`, `Authenticated Users`) is
capped with the group. `folder-acl-check` warns `trusted_writer_no_write:` for such an account; give it its own entry
first (as an administrator: `icacls "<folder>" /grant "<DOMAINccount>:(OI)(CI)M"`), then lock down. Folders above it
are never changed — `parentWritable` says who could still rename the folder away through one.

**Lock a folder down by hand** (background, for a box without the link: a stricter, simpler recipe than the command's
masking — it replaces the folder's DACL instead of lowering it; an elevated prompt; the path is an example):

```bat
icacls "C:\xampp\htdocs\herp" /setowner "*S-1-5-32-544"
icacls "C:\xampp\htdocs\herp" /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F" "*S-1-5-32-545:(OI)(CI)RX"
```

That makes Administrators the owner, drops the inherited entries (Users' create-files right comes from `C:\`), and
leaves SYSTEM and Administrators full control and Users read. Then:

1. **The importer's account.** The ERP's scheduler renames what it imported (`*.MASOFON2.BAK`), so the account it runs
   as needs Modify there. An administrator or SYSTEM is covered above. Any other account must be added
   (`icacls … /grant "DOMAIN\account:(OI)(CI)M"`) and named in `documents`' `trustedWriters` (review R1b): a trusted
   account is never counted as a writer, over WinRM and behind the link.
2. **The folders above.** Nobody but the administrators may delete or rename them: `icacls "C:\xampp\htdocs"` (and each
   folder up to the drive) must not give `Users`, `Authenticated Users`, `Everyone` or `Domain Users` `(D)`, `(DC)`,
   `(F)`, `(M)`, `(WDAC)` or `(WO)`.
3. **What is already there.** A file someone created before the lock-down is still theirs. `dir /q` shows each file's
   owner: move aside every `*.doc`, `*.prm`, `*.dll`, `*.exe`, `*.bat` or `*.cmd` neither the connector nor the ERP
   wrote — before the next order.
4. **Check.** `state`'s `writableFolders` no longer lists it (a trusted writer shows under `trusted`). The daemon reads the folders' ACLs in the background
   (review R3): right after its start (until then, behind the link, a typed folder counts as refused), a refused folder
   again at most once a minute when an order or a receipt needs it, an allowed one every ten minutes — no restart
   needed (`folder-acl` and `documents` restart it anyway). server.log says `FOLDER ALLOWED` when a refused folder is
   the administrators' only again. The folders are read in parallel, each waited on at most 10 s (review m1): a read
   hung on an unresponsive domain controller logs `FOLDER CHECK: … did not finish in 10s` and holds up no other folder.
   Until it ends, a folder read before keeps that read's answer (review R4-3, v1.0.31): an allowed folder stays allowed
   — a slow domain controller causes no `503` at its ten-minute re-check — and a refused one stays refused; only a
   folder never read is unknown (refused behind the link). A request for a folder not read yet starts its read.

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
| "Not saved: the data directory … could not be protected, so nothing was written" | The walk before the save refused or failed (a link, a hard link, an untrusted owner, access). Nothing was written. Fix the reason as for the service (see above) and save again. |
| "The data directory could not be protected, so saving is refused until it is fixed" (at launch) | The GUI's launch walk failed; it logs nowhere in the data directory. Same reasons and fixes; or the GUI is not elevated. |
| "Saved, but could not restrict access to what was saved" | The walk after the save failed; the save itself succeeded. Start the service — it re-checks at start and refuses if it cannot. |
| "The service refused to start: its data directory could not be protected" | See the Event Log row above. |
| "Error loading config: … Access is denied" | Not elevated: since the service locks the data directory down (SYSTEM + Administrators only), a non-elevated process cannot read `config.yaml`. Use the shortcut. |
| Saved settings seem to have reverted | Check you clicked Save; the GUI preserves keys it does not display, so `queries` and `tls` are not the cause. |

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

## The order index (DIG-1291)

Everything below is done **with the service stopped** (first check
`GET /api/sendOrder` answers `{"queued":0,"running":0}`). An entry is
`DataDir/orders/index/<sha256(historyId)>.json`. To find it:

- from the log: `server.log` names the file on every reservation (`sendOrder
  historyId="…": order N reserved and queued (index <file>)`), on a 500 and on
  `ORDER INDEX ENTRY SKIPPED`; `historyId=` and `orderNumber=` appear together
  on the worker's `processing order` line;
- from a `historyId`, in PowerShell:
  `-join ([Security.Cryptography.SHA256]::Create().ComputeHash([Text.Encoding]::UTF8.GetBytes('<historyId>')) | % { $_.ToString('x2') })`

"Order `n` was written" means `sendOrderDir\history\<n>\IMOVEIN_<n>.doc`
exists (the history copy is written after `IMOVEIN.doc` and `.prm`, and the
importer runs after all four). An empty `history\<n>` folder means nothing was
written. One caveat: if something other than the connector imports
`IMOVEIN.doc` from `sendOrderDir` on its own (no `hasBatFile` / `hasExePath`
configured), check Hashavshevet (Asmahta `n`) in every case below.

**Model (a)** (`sendOrderPickupDir`, DIG-1297): the history copy is written
*before* the order is published into the pickup folder, so it only proves the
order was built. What the scheduler can import is `IMOVEIN_<n>.doc` (or
`TMP_IMOVEIN_<n>.doc`) in the pickup folder, or, once imported, its
`*.MASOFON2.BAK`. Look there and in Hashavshevet (Asmahta `n`) before treating
an order with a history copy as written — and never set `retryable` while
either exists. Neither there nor in Hashavshevet, with `history\<n>` present:
the publish failed; `retryable` is safe only after confirming no other key holds
`n` (below).

### A damaged order index entry

1. Read the file and the log to learn its `historyId` and number `n`.
2. If you know `n` and `history\<n>\IMOVEIN_<n>.doc` exists: the order was
   written. Rewrite the entry by hand (`{"key": "<historyId>", "number": n,
   "state": "written", "reservedAt": "<any text>"}`) so its repeats answer it.
3. If you know `n` and there is no `IMOVEIN_<n>.doc`: nothing reached
   Hashavshevet. First search `server.log` for `both hold order <n>`: if another
   `historyId` holds `n` too, do not set `retryable` — sort out which key owns
   `n` first. Otherwise either rewrite it with `"state": "retryable"` (the next
   repeat writes it under `n` — preferred: the number and the key stay
   together), or move the file out of `orders\index\` (the next repeat becomes
   a new order under a new number; `n` stays unused).
4. If you cannot learn `n`: look the order up in Hashavshevet (customer, date,
   items). Found → step 2 with its Asmahta. Not found → move the file aside.

Never delete an entry without these checks: a repeat of a key with no entry is
a new order — a second Hashavshevet document if the first was written.

### An order whose outcome is unknown

The entry is still `reserved` after a restart. The connector never writes it
again on its own.

- No `history\<n>\IMOVEIN_<n>.doc` → the connector never ran the importer for
  it: check `server.log` for `both hold order <n>` (as in step 3 above), then
  set `"state": "retryable"` in the entry. The next repeat of the `historyId`
  writes it under order `n`.
- `IMOVEIN_<n>.doc` exists → the files were written and the importer may have
  run. Check Hashavshevet (Asmahta `n`): there → set `"state": "written"`; not
  there → import the history copy by hand ([Recover from failed
  import](hasavshevet-send-order.md#recover-from-failed-import)) and set
  `"state": "written"`.

The connector backs these edits up: it never queues a `retryable` entry again
when `history\<n>\IMOVEIN_<n>.doc` already exists or another entry holds `n`.
It logs `ORDER NOT QUEUED AGAIN: order <n> (historyId=…) is retryable, but …`
with the reason, answers the repeat as `failed` and leaves the entry as it is —
fix the entry by the steps above.

## A downgrade below v1.0.30 (plan 087)

**Never put a build older than v1.0.30 back on a box with `documents` (counters or typed folders) in `config.yaml`,
or that took an order with a documentRule.** It is worse than an ignored setting (review m12):

- **v1.0.29 ignores `documents`** (its YAML reading is lenient): it numbers every order from `lastOrderNumber.json`
  and every receipt from `receiptNumber.json`, writes them where `sendOrderPickupDir` / `sendReceiptDir` say (a bound
  receipt folder is forgotten), and answers `/api/v2/sendOrder` 404.
- **Its recovery is global**, not per range: it moves `lastOrderNumber.json` past the highest
  `sendOrderDir\history\<n>` of *every* range — test counters' and other counters' included — so its next production
  order takes a number in a test range (9 000 000+ at BFL), written to the production destination. The receipt store
  jumps the same way past the highest receipt in `history\receipts`. Index entries holding a `rule` are refused by its
  strict decoder (`ORDER INDEX ENTRY SKIPPED`; a repeat of such a key answers 500 `ORDER_INDEX_UNREADABLE` — nothing is
  written twice).
- **A save from v1.0.29's GUI drops the whole `documents` block** (its config has no such field).

Upgraded back, the adopted production counter (e.g. `b2b`, 1 000 000–1 999 999) holds a number above its `max`:
every order without a rule answers `503 COUNTER_EXHAUSTED`, and no command can lower it — `start-from` only moves
forward, and a `range` holding the number would overlap the test counter's. The per-range recovery of the test
counter moves it past the numbers v1.0.29 issued there, so none is issued twice; only the production counter needs a
hand.

**The hand repair** — with the service stopped (`GET /api/sendOrder` `{"queued":0,"running":0}` first), as an
administrator, on the box:

1. **Put `documents` back** if a v1.0.29 GUI save dropped it: copy `config.yaml` from a backup made before the
   downgrade, or from the pre-image of a change set made after the last `counters` / `documents` command
   (`DataDir\provision\<changeSet>\`); otherwise digi-mcp sends the `documents` and `counters` commands again once
   step 3 is done (a `create` of today's store refuses a store above its `max`).
2. **Note what v1.0.29 wrote outside its range.** Each `sendOrderDir\history\<n>` folder dated after the downgrade
   with `n` in a test (or another counter's) range is a real order written to production under that number: it is in
   Hashavshevet (Asmahta `n`) — nothing to undo, but the books should know. The same for `history\receipts\<n>`.
3. **Set each production store back to the highest number its own range issued.** `state`'s `counters` (it reads
   the files; the service may stay stopped) give `highestIssued` per range — the highest index entry or history folder
   *inside the range*. Write `{"lastOrderNumber": <b2b's highestIssued>}` into
   `sendOrderDir\lastOrderNumber.json`, and `{"nextReceiptNumber": <receipts' highestIssued + 1>}` (keep `seededAt`)
   into `DataDir\receipts\receiptNumber.json`. This is the one time a store goes back by hand, and it is safe only
   because every number above it that v1.0.29 issued lies in another counter's range: check that no index entry and
   no history folder inside the counter's range is above the value you write.
4. **Start the service** and read `GET /api/counters`: each counter's `remaining` above 0, no `error`, `last` = the
   value written; `state`'s `counters` agree.

## The box did not upgrade itself (auto-update, v1.0.31)

`link-status` (or provision's `state`, `link.autoUpgrade`) says why, in `effective.why`
([link.md § Managed auto-update](link.md#managed-auto-update-plan-088-v1031)):

| `why` | Meaning, and the way on |
|---|---|
| `no_policy` | No policy names this box: none was published for every box or for a list holding its id. A lost `link\policy.json` (a reinstall) is not a reason: the box holds what its seq floors keep. The tenant and ring the relay states play no part. |
| `policy_hold` | The deciding policy holds (`hold`, or no target) — a box list's hold outranks every box's target — or a hold issued at or after the deciding target brakes it, at any scope (`braked` names the target it overrides). A box list issued after that hold moves its boxes again. |
| `policy_expired` | The deciding policy expired (or the box's clock is behind its issue time): it holds for its scope and never falls back to a less specific policy. Publish a new one for that scope (a box list naming the box, to return it to the fleet's target). |
| `policy_revoked` | The deciding policy was signed by a team key now below the box's epoch floor: it holds for its scope until a policy signed by a current key replaces it. |
| `scheduled` | It will: `nextWindow` is when, in the box's local time. A `waiting` entry says why the last check in the window did not (`orders_pending`, `busy`); `last` with `outcome: refused` says why the last attempt failed (a download, a digest) — tried again in the next window; `outcome: deferred`: the runner found orders queued right before the setup and did not run it — tried again at the next check. |
| `up_to_date` | The box already runs the target or a newer build. |
| `upgrading` | A ticket is running: `last.ticket`, and `upgrade-result` with it. |
| `stopped` | This target ended unhealthy under this policy (`rolled_back`, `unhealthy`, `unhealthy_no_rollback`, `rollback_below_floor`, `unhealthy_link_inconclusive`), or failed three times; `last` has the health checks and the rollback. A newer policy (a higher seq, even for the same release) tries again. |
| `auto_upgrade_off`, `admin_off` | `link.yaml` turns it off (`autoUpgrade: false`, `admin: false`). |
| `state_unreadable` | `link.yaml` names a key the link does not know (a misspelt `autoUpgrade`?), `link\autoupgrade.json` does not read, or the seq floors in the secrets store do not read (or `link\policy.json` does not read and no floors were ever kept): the box refuses every delivery and holds — fix by hand, on Digitrade's request. `error` names the file. A `policy.json` that does not read, with the floors readable, is no reason: the floors and the policies kept with them stand in, and the next delivery writes the file back. |

A policy refused on delivery is in `refused` with its code: `seq_not_newer` (the box holds this scope key at that seq
or a higher one — publish a newer seq: box lists share one counter), `wrong_scope` (not every box, and no list holding
this box's id), `expired`, `malformed` (a seq above 2147483647, a lifetime over 90 days, a window shorter than
`healthMinutes` + 30 minutes…), `key_revoked` / `key_unknown` / `signature_refused` (not signed by a team key this build
trusts at its epoch floor).

## An automatic upgrade ended unhealthy and the new build stayed (v1.0.31)

The result (`upgrade-result`, or `last` in `link-status`) names the runner's `outcome`; the new build is installed and
a person decides — upgrade over the link to a fixed release, or downgrade by hand at the server (provisioning.md: never
below 1.0.25 on a box with a pickup folder, never below `format-floor`).

| `outcome` | Why there was no rollback |
|---|---|
| `unhealthy_no_rollback` | No signed digest names the setup of the build replaced (a policy's `rollbacks` lacked it, or the command had no `rollback`), or the signed one disagrees with the digest this box recorded when it installed that build. digi-mcp fills `rollbacks` from its pins: pin the build the box ran. |
| `rollback_below_floor` | The build replaced is below `DataDir\format-floor` (the new build changed a file format the old one cannot read), below the runner's compiled floor, or the floor cannot be read. A `format-floor` that does not hold a release number (`1.0.30`) is left as it is by every start and blocks every rollback: put back the highest release that has run on the box, by hand. |
| `unhealthy_link_inconclusive` | Only the new link failed, and the relay's `/readyz` did not answer 200 from the box: the relay, its registry, or the way to it (the proxy) was down, or the instance was draining — not proven the build (relay 0.3.0's `/readyz` answers 200 only while its registry answers within 2 s). Check `link-status` once the relay is back; the box is likely fine. |
| `unhealthy` | The rollback ran and did not end healthy, its checks refused its setup (a download, a digest), or the server restarted during the rollback (or twice in the health window): read `rollback`, `runner.log` and `setup.log` in `upgrade\<ticket>\`. |
