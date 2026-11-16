# Agent Guidelines — digi-erp-connector

> Project memory lives in `.claude/docs/` — origin story, decision log,
> implementation history, operations runbook, and migration records. Read
> `.claude/docs/README.md` first when picking up this project.

## What this is

A Go application providing a local HTTP REST API gateway between the main app and ERP systems. It is the merge of two predecessor repos:

- **erp-connector** (Go) — supplied the architecture and all ERP features.
- **electron-mssql-app** (Node/Electron, legacy) — supplied the SQL access model: **named saved queries**. The backend never sends raw SQL; it registers queries by name via CRUD and executes them with parameters.

Three binaries:
- **`digi-erp-connectord`** — background daemon / Windows service; runs the REST API server
- **`digi-erp-connector`** — Windows-only native UI (`walk` library) for config management and service control
- **`digi-erp-connector-provision`** — headless discover / apply / options / verify / restore, and (plan 086) auth / db / service / firewall-remove / daemon / receipts / orders / receipt-seed, and (plan 087) counters / documents / folder-acl, JSON in and out, secrets on stdin only; what digi-mcp runs over WinRM (Digitrade plan 068) or through the link's fixed command set (`internal/link/commands.go`; `docs/provisioning.md`, `docs/link.md`)

**Supported ERPs:** Hasavshevet (complete), SAP (price/stock query implemented), Priority (not implemented)
**Primary target OS:** Windows. Linux supported for daemon only.

## Project structure

```
cmd/
  digi-erp-connector/   ← Windows GUI (walk library)
    main.go             ← entry point only
    form.go             ← widget declaration + per-widget helpers
    form_config.go      ← form ↔ config.Config mapping, persistence
    actions.go          ← configuration button handlers
    service_control.go  ← service start/stop/restart, daemon discovery
  digi-erp-connectord/  ← Background daemon/service
  digi-erp-connector-provision/ ← entry point of internal/provision: headless
                          discover / apply / verify / restore / auth / db /
                          service / firewall-remove / daemon / receipts / orders /
                          receipt-seed (docs/provisioning.md)
  cutover-seed/         ← operator tool: seed config/secret/queries for a
                          replacement install (not part of the service)

internal/
  api/
    server.go           ← HTTP mux, route registration, rate limiter, timeouts
    handlers/           ← One handler per endpoint
      health.go         ← GET /api/health
      auth.go           ← POST /auth/token (the credential check), GET /api/ping
      custom_sql.go     ← Saved-query CRUD (/api/custom_sql...)
      sql_queries.go    ← GET /api/sqlqueries/{name} (saved-query runner)
      folders.go        ← GET /api/folders/list
      file.go           ← POST /api/file (path-safe file streaming)
      price_stock.go    ← POST /api/priceAndStockHandler
      send_order.go     ← POST /api/sendOrder (async queue); POST /api/v2/sendOrder (documentRule, plan 087)
      documents.go      ← GET /api/counters, GET /api/documents/capabilities (read-only, plan 087)
      send_order_status.go ← GET /api/sendOrder/{jobId}
      send_order_queue.go  ← GET /api/sendOrder (queued/running counts, asked before a restart)
      send_receipt.go   ← POST /api/sendReceipt (synchronous, strict decode)
    middleware/
      auth.go           ← verifies the issued token on every route
      ratelimit.go      ← Per-IP token bucket (429 RATE_LIMITED)
      logging.go        ← Request/response logging (no secrets)
    respond/            ← JSON / Error — the one error envelope
    dto/                ← Request/response structs per endpoint
  queries/              ← Saved-query subsystem (THE data-access model)
    store.go            ← JSON registry (queries.json, atomic writes, mutex)
    binder.go           ← Param binding: forced strings, type inference, int hints
    runner.go           ← Execution: timeout, row cap, multi-recordset, DML exec
  auth/                 ← HS256 sign/verify + secret/password generation (no deps)
  config/               ← YAML config (atomic write, 0o600); documents.go = the documents block
                          (named counters, typed folders, closed name grammar; plan 087)
  db/                   ← MSSQL pool
  erp/hasavshevet/      ← Order pipeline (IMOVEIN, queue, GPRICE) + receipts
                          (KUPAIN, number store, idempotency index, writer);
                          counters.go + document_rule.go = named counters and
                          the documentRule (plan 087)
  buildinfo/            ← Version, stamped by the release build (-X)
  erp/sap/              ← SAP B1 price/stock query
  files/                ← Path traversal prevention
  logger/               ← LoggerService interface
  platform/
    autostart/          ← Windows service registration/control
    paths/              ← data dir + config path (PROGRAMDATA-based)
    atomicfile/         ← THE way to write a file: temp + sync + rename
    filelock/           ← OS lock on a file (flock / LockFileEx): one receipt writer per DataDir
    dataacl/            ← protected DACL on DataDir + config.yaml (Windows; no-op elsewhere)
  secrets/              ← Windows DPAPI (machine scope)
  formatfloor/          ← DataDir\format-floor (plan 088 D11): RollbackFloor, raised at
                          every start, never lowered; no automatic rollback below it
  provision/            ← discover / apply / verify / restore (+ the narrow
                          commands auth, db, service, firewall-remove, daemon,
                          receipts, orders, receipt-seed:
                          plan 086, each one part of apply) behind a System
                          interface (Windows: service manager, registry, TCP
                          table, firewall via fixed PowerShell); pre-images in
                          DataDir/provision/<changeSet>/
```

Data dir: `%PROGRAMDATA%\digi-erp-connector\` (config.yaml, queries.json, server.log, ui.log, secrets/, receipts/, history/receipts/, orders/index/, format-floor, counters/ — plan 087's own counter stores). Linux: `/etc/digi-erp-connector/`. On Windows the daemon gives it a **protected DACL** at every start (SYSTEM + Administrators Full Control, the service account only if it is not an admin, OWNER RIGHTS READ_CONTROL only; children carry the same as inherited ACEs) via `dataacl.ProtectTree`, run BEFORE server.log is opened and BEFORE config.Load (`cmd/digi-erp-connectord/dataacl.go`). **It works by handle only** (FILE_FLAG_OPEN_REPARSE_POINT, ancestors held without FILE_SHARE_DELETE, GetSecurityInfo / SetKernelObjectSecurity, no MARTA propagation — never reintroduce SetNamedSecurityInfo here: as SYSTEM a path can be redirected), refuses reparse points, multiply-linked files, entries resolving elsewhere and untrusted owners (fail closed), writes only what differs (a steady-state start only reads), sets SYSTEM as owner when running as SYSTEM, trusts owners that are Administrators members directly or via nested groups (NetUserGetLocalGroups, fail closed when unresolvable or orphaned) and then hands those entries to BUILTIN\Administrators (needsClaim) so the domain is needed at most once; the SCM restarts a refused service after 1 / 5 / every 30 min. **A refused start opens no file in DataDir**: the reason goes to the Windows Event Log and the service exits with service-specific code 2 (`autostart.ExitDataDirRefused`). server.log is kept only when it is provably the daemon's own object (`.server-log-id` file-ID marker); otherwise it is moved aside, or the daemon logs to `server-<UTC>.log` if it cannot be. The elevated GUI walks at launch before opening its logs and before **every** save — a failed walk refuses the save with nothing written — and hands what it created to Administrators (`ClaimForAdministrators`; `cmd/digi-erp-connector/config_acl.go`, `gui_logs.go`). Off Windows the receipt import stubs return an error, so model (b) records `failed`, never `ok`.

## API endpoints (all `/api/*` require `Authorization: Bearer <token from /auth/token>`; only `/auth/token` is rate-limited)

| Route | Method | What it does |
|-------|--------|--------------|
| `/auth/token` | POST | **Unauthenticated** (it is the credential check). Username+password → short-lived token. The only way in. |
| `/api/health` | GET | Pings DB; `{"status":"ok"}` or 503 |
| `/api/ping` | GET | Liveness + credential check; **no DB touch**. `{"ok":true,"ts":<epoch ms>,"version":"<tag or dev>"}` |
| `/api/custom_sql` | POST | Create saved query `{name, description, sql, params}` (409 on duplicate) |
| `/api/create_custom_sql` | POST | Legacy alias of the above (electron-mssql-app compat) |
| `/api/custom_sql` | GET | List saved queries |
| `/api/custom_sql/{name}` | GET / PATCH / DELETE | Fetch / partial-update / delete one |
| `/api/sqlqueries/{name}` | GET | **Execute saved query.** Stored default params merged with query-string (URL wins), typed binding, returns rows + recordsets + legacy `value`/`rowsAffected` |
| `/api/folders/list` | GET | Configured image folders with file lists |
| `/api/file` | POST | Path-validated binary file streaming |
| `/api/priceAndStockHandler` | POST | Hasavshevet or SAP price/stock fetch |
| `/api/sendOrder` | POST | Validates order (and, with `sendOrderFormat: legacy`, that its numbers fit the legacy widths: else 400), enqueues, `202 + jobId`. Optional `documentId` / `warehouse` / `vatPercent` / `asmahta2` (DIG-1295, per-tenant erp-manager config) set IMOVEIN line4 / 12 / 18 / 8; absent = today's bytes (goldens `testdata/imovein`), a value that does not fit = 400. Optional `header` object (DIG-1315: `accountNameDetail`, `address`, `city`, `phone`, `extraText1-5`, `extraSum1-3`, `extraNum1-2` → IMOVEIN line5/6/7/35/45-52/81-82, `hasavshevet/header_fields.go`): absent/null/blank = today's bytes; unknown or wrong-case keys and over-long values = 400 `header/<key>`, never cut; present → the 202 echoes `headerApplied` (an older connector drops `header` silently). `historyId` is the idempotency key (DIG-1291): a repeat answers the first `jobId`, its current `status` and `replay: true`, writes nothing — except an order that failed before any file was written, which it re-queues under the same number; this key's damaged index entry → 500 `ORDER_INDEX_UNREADABLE`, never 202 |
| `/api/v2/sendOrder` | POST | Plan 087 (DIG-1563): `/api/sendOrder` + optional `documentRule {v:1, temporary, folder, counter}` — keys only, resolved against config.yaml's `documents`. Strict (unknown/duplicate/wrong-case key, wrong type, v≠1, unknown folder/counter, wrong kind, test counter → non-test folder, temporary without a prefix, retired counter = 400 `documentRule/<key>`, nothing written); no rule = exactly v1's output; the rule is frozen in the index entry at reservation, a repeat with another rule = 409 `DOCUMENT_RULE_CONFLICT`; outside the range = 503 `COUNTER_EXHAUSTED`, missing store = 503 `COUNTER_MISSING`; 202 echoes `{counter, number, fileName, folder}`. An older connector answers 404. `/api/sendOrder` ignores `documentRule` |
| `/api/counters` | GET | Plan 087, read-only: `[{key, kind, min, max, last, highestIssued, remaining, test}]` (+ `retired`, `error`); `last` = last issued for both kinds |
| `/api/documents/capabilities` | GET | Plan 087, read-only: `{contract: 1, folders: [{key, kind, test}], counters: [{key, kind, test}] (not retired), placeholders: ["prefix","n"]}` |
| `/api/sendOrder` | GET | `{queued, running}`: accepted orders not written yet — a restart drops them (in-memory queue) |
| `/api/sendOrder/{jobId}` | GET | Poll job status (queued/running/done/failed/unknown); after a restart answered from the order index |
| `/api/sendReceipt` | POST | **Synchronous** (≤120 s): one KUPAIN receipt per `idempotencyKey` → `200 {receiptNumber, written, idempotencyKey, import, warnings}`; flat §4.6a body (16 keys, all present, none null), strict decode (also refuses duplicate and wrong-case keys); 400 only for the request's content (the backend stops on it); 503 when `sendReceiptDir` unset (`RECEIPTS_NOT_CONFIGURED`), the number store unusable (`RECEIPT_NUMBER_STORE_UNAVAILABLE`), the import lock busy (`RECEIPT_WRITER_BUSY`), the body not readable to its end (`RECEIPT_REQUEST_READ_FAILED`) or another process holding the data dir (`RECEIPT_DATA_IN_USE`). `docs/hasavshevet-send-receipt.md` |

## Saved-query model — hard constraints, NEVER bypass

- **There is NO raw-SQL endpoint.** The legacy `POST /api/sql` was deleted before the first release, and the electron-compatibility `POST /api/query` was deleted on 2026-08-09. Never add an endpoint that accepts SQL text for immediate execution. SQL enters the system only through the CRUD store.
- **Param binding only** — every request value binds via `sql.Named()` in `internal/queries/binder.go`; no string concatenation into SQL, ever.
- **Forced-string params:** `skuArray`, `warehouse`, `sku`, `syncKey` always bind as strings (never coerced to numbers) — SAP WhsCodes and SKUs can look numeric.
- **Type inference** for query-string values (electron compat): all-digits → int64, decimal → float64, `YYYY-MM-DD…` → datetime, else string. Integer hints from `TOP(@x)`/`OFFSET @x ROWS`/`FETCH NEXT @x ROWS`.
- Saved queries are trusted (operator/backend-managed) and MAY contain writes/EXEC — that is by design; the trust boundary is the credential + CRUD store, not a keyword filter.
- **Row limit** default 10,000; **execution timeout** default 30s (config `queries.maxRows` / `queries.timeoutSeconds`); CRUD body limit 1 MiB.
- `queries.json` format stays drop-in compatible with electron-mssql-app `custom_sql_data.json` (import = copy the file).

## File endpoint hard constraints — NEVER bypass

- `folderPath` must exactly match (after canonicalization) a configured `imageFolders` entry
- `fileName` must not contain `.`, `..`, or absolute paths
- Final path re-validated with `filepath.Rel` and symlink re-resolution — `ResolveFilePath()` in `internal/files/files.go`

## Authentication

**One credential, one scheme.** A caller posts the configured username and password to `POST /auth/token`, gets a short-lived HS256 token, and sends it as `Authorization: Bearer <token>` on every `/api/*` route. `middleware/auth.go` verifies the signature and nothing else; every failure is a flat 401. Nothing is ever logged. Rate limiting (`middleware/ratelimit.go`) guards `/auth/token` only.

**There is no static API token.** `bearerToken` was removed from the config and the code on 2026-08-09 at the operator's instruction: two credentials meant two things to rotate and two ways in, and erp-manager — the caller that matters — only ever used the exchange. **Do not reintroduce one.**

Hard rules:

- **No credential may ever have a default.** Username and password are operator-set; the signing secret is 32 random bytes generated on first run and saved to config.yaml. The Node app this replaced shipped `digitrade`/`123456` and a secret in its source — a test asserts those exact credentials are rejected.
- **Rejections are exactly 401**, never 403 and never 500. erp-manager re-authenticates on 401 and on nothing else; any other status turns a self-healing hiccup into a dead connection.
- **Success is exactly 200 with `access_token` present.** A caller treats anything else as failure, and stores a missing `access_token` as null.
- A blank username or password **stops the daemon and is refused by the GUI** (`config.AuthConfig.Validate`, called from both). There is no fallback credential, so an exchange accepting blanks would be an open door.

Full details: `docs/authentication.md`. The contract is pinned by `internal/api/auth_exchange_test.go` — those tests exist because a live backend breaks in production when any of it drifts.

## Hasavshevet send-order flow

1. `OrderQueue.Accept(req)` (under `closeMu`): a `historyId` already in `DataDir/orders/index/<sha256>.json` → 202 with its first jobId + `replay`, nothing else; otherwise Peek `lastOrderNumber.json` → save the entry `reserved` → move the counter past it → queue → 202 + jobId (= order number)
2. Single worker builds IMOVEIN.doc/.prm (fixed-width 2891-byte records, Windows-1255), writes history copies, runs `has.exe` or `digi.bat`
3. The queue supports optional PostOrderHook implementations (none registered — the PDF/print/email hook was removed 2026-07-20; hook errors must never fail the order)

**Model (a) and the legacy layout (DIG-1297):** `sendOrderPickupDir` set = each order is published as ONE `IMOVEIN_<n>.doc` (`TMP_` for a return) into that folder for the ERP's own scheduler (BFL: WizSched in `herp`): history first in `sendOrderDir`, then `atomicfile.WriteNew` (never replaces; temp `.IMOVEIN_n.doc.tmp-*`); no `.prm` there, no importer; needs an explicit `sendOrderFormat`; the folder is probed writable before history (retryable); a number already used there (`IMOVEIN_<n>.*`, `TMP_IMOVEIN_<n>.*`, `*.MASOFON2.BAK`) is final `failed`. `sendOrderFormat: legacy` writes the old app's layout (1296 bytes + LF, 37 fields + separator spaces; goldens in `testdata/imovein-legacy` made by RUNNING the legacy PHP — regenerate with `generate.php`, never hand-edit); numbers its narrower fields would change are a 400. `config.ValidateOrders` guards both (daemon, CheckConfig, GUI save). `docs/hasavshevet-send-order.md` § Model (a).

**Single-worker constraint:** never make the order queue concurrent — IMOVEIN files are shared.

**Named counters and typed folders (plan 087, DIG-1563; `docs/hasavshevet-send-order.md` § documentRule, `docs/provisioning.md` § counters / § documents):**
- A documentRule carries **keys only** (folder, counter) — never a path or a file name (LocalSystem writes; the importer decides the name). The box's `documents` block says what a key means; it is written **only** by the signed provision commands `counters` / `documents` (digi-mcp), never by an API route — the data-path credential is the B2B shop's too. Never add a write route for counters or folders.
- Today's `lastOrderNumber.json` and `receiptNumber.json` are adopted **in place** (`store:`), one kind at a time (a kind with no configured counter keeps today's store under its implicit key `orders`/`receipts`); others live in `DataDir\counters\<key>.json` `{"last":N}` and fail closed. With counters configured a missing `lastOrderNumber.json` is `ErrCounterMissing`, never 1. One meaning everywhere: **last issued**.
- **Ranges are disjoint per box across all counters** (test/prod, orders/receipts, retired): the jobId, status, history and re-queue check stay keyed by the number — never re-key them, never relax `validateCounters`' overlap check.
- Recovery and the "never ≤ highest" guard are **per counter over its range** (queue and receipt writer); never make them global again (the "second counter in another range" test).
- The order counter never issues past 999 999 999 (MyID's nine digits).
- The rule is frozen in the order index entry (`rule`) at reservation; re-queue/recovery use it, a repeat with another rule is 409. History copies keep `IMOVEIN_<n>.doc`; the "taken" scan follows the folder's pattern.
- A counter only moves forward: `start-from` (expectLast compare-and-set) stops the service, re-reads, sets (receipt counters under `writer.lock`), restarts; no store is ever in a pre-image, `restore` never lowers a counter.
- Never downgrade below v1.0.30 once `documents` is set or a ruled order was taken (lenient YAML ignores it; the strict index decode refuses `rule`; its global recovery pushes `lastOrderNumber.json` past the test ranges — back on v1.0.30 the production counter is above its max, `COUNTER_EXHAUSTED`, no command lowers it; its GUI save drops the block). Hand repair: `docs/troubleshooting.md` § A downgrade below v1.0.30.
- **Trusted writers never count as exempt where code runs** (review T1): `Config.ImporterFolders`/`ExecutesIn`/`TrustedSIDsFor` — `sendOrderDir` when an order importer runs (has.exe or BAT), the receipt folder when a receipt importer runs, and the folders of the importer files actually run (`ImporterFiles`: the BAT, or has.exe + its parameter file; never a configured-but-unused one, so a model (a) pickup folder stays the scheduler's). The guard has two file entries, `importer:orders` / `importer:receipts` (`dataacl.Writers`, nobody exempt; each reads its importer's files — `OrderImporterFiles` / `ReceiptImporterFiles`: the BAT, or has.exe and the parameter file it is handed, review R4-4): behind the link a writable importer is `ErrImporterWritable` → 503 `IMPORTER_WRITABLE` at Accept (rule-less model (b) orders), in the worker before a number or file (retryable), and for receipts before reserving; off the link a warning. `folder-acl` refuses a folder holding an importer file or an executable (`folder_has_executables`; PATHEXT ∪ defaults ∪ .dll .ps1 .scr .cpl .ocx .sys .drv, never .php).
- **Where a document folder may be** (review T3, `provision/folder_paths.go`): `documentFolderRefusal(p, FolderRules)` — drive root; in or holding `Sys.FolderRules().Dirs` (SystemRoot/windir, Program Files ×3, ProgramData/ALLUSERSPROFILE, SystemDrive\Users + Documents and Settings, profiles' parent, registry ProfilesDirectory, the install folder) or the data dir; a part `AppData`/`System Volume Information`/`$Recycle.Bin`. `Sys.ReparsePointOn` (Lstat every prefix, ModeSymlink|ModeIrregular) → `folder_reparse`. Scope: typed folders (reparse always, rules behind the link), every folder-acl lockdown target (always; restore exempt), and behind the link the pre-087 folders apply/orders/receipts set or move (`checkSetFolderPaths`). folder-acl also judges `plan.FinalPath` (must be the configured path, `finalPathRefusal`) and `ApplyLockdown` refuses a root whose final path differs from the plan's (`ErrOutsideTree`). The fake's rules are empty by default (the Windows CI temp dir is under `C:\Users\…\AppData`).
- **`acceptUnreadable` is one total per command** (review m3): guards call `a.noteUnreadable(index, n, why)` (orders/receipts, max per index — every scan reads the index whole) and never refuse on it themselves; `a.unreadableSettled()` checks `total == *a.acceptUnreadable` once — in `runConfigChange` after the checks, in `documentsIdle` unless `unreadableDeferred` (restore sets it, then settles after `countersRestorable`, implicit-store trap included), in `folderACLIdle`, and in `counterStartFrom` after its scan.
- **`inflight` (v1.0.32, plan 087 review R6)**: `provision/inflight_cmd.go` + `hasavshevet/inflight.go` resolve ONE unfinished entry (order by `historyId`, receipt by `number`) as `fail` (no candidate file may exist: frozen name in its folder, pickup `IMOVEIN_<n>.doc`/`TMP_…`, `history\<n>\IMOVEIN_<n>.doc/.prm`, the receipt's file and `DataDir\history\receipts\<n>\KUPAIN_<n>.doc`; an unlookable place counts as written → `may_be_written`) or `done` (one must exist → else `not_written`); CAS on `expect` (`entry_moved`), `entry_final`, `entry_busy` (`GET /api/sendOrder/<n>` queued/running; receipt `writer.lock` held). Run = idle → stop → re-read → resolve → start → prove. Orders keep v1.0.31-readable values (`failed`/`written`, the change set in `error`); a receipt resolved as failed gets the new state `failed` (`409 RECEIPT_FAILED` on a resend) and **raises `DataDir\format-floor` to 1.0.32 first** (`hasavshevet.ReceiptFailedFloor`). Link: `commands_v1032.go`.
- **Carry-over at lock-down (v1.0.32)**: `folder-acl` `carryOver: true` → `dataacl/carry.go` + `authz_windows.go`: per trusted writer per entry ONE explicit allow = AuthZ effective on the OLD descriptor (`AuthzInitializeContextFromSid` + `AuthzAccessCheck`, generic rights mapped first: `MapGenericACL`) ∩ Modify, only when the masking took some of it; inheritable OI/CI only if `inheritedCover` (lower bound: uncertain allows count 0, uncertain denies count, NP allows ignored) covers it; none on an entry with a conditional ACE for the writer (`skipped`); placed after leading explicit denies (`InsertCarried`). AuthZ failure → `CarryFailed` (warning, no carry); Apply re-resolves before writing (`ErrCarryOver` → `carry_over_failed`). Property test `TestCarry_NeverGrows`; Windows `TestLockdown_CarryOver` makes a local user with `net user`.
- **m6's rest (v1.0.32)**: `provision/linkpaths.go` `checkLinkedPaths` — behind the link, image folders (options/apply) and importer files (apply/orders/receipts: has.exe, BATs, param files) a command sets/changes: `Sys.ReparsePointOn` anywhere → refuse; existing → `Sys.FinalPath` / `Sys.FinalFilePath` (`dataacl.FinalFilePath`, by handle, no link/hard link) must equal the configured path (`image_folder_forbidden` / `importer_not_erp`).
- **Contracts with older builds (m8, v1.0.32)**: golden files `internal/provision/testdata/contract/state-runner-v1.0.31.json` (state's `ok`, `result.daemon.{asked,version,health,queue.{queued,running},error}`) and `internal/link/testdata/contract/session-runner-v1.0.31.json` (`session.json`'s `build, connected, epoch, relay, connectedAt, liveAt`) — the v1.0.31 upgrade runner's reads; `TestContract_*` fail on a drop/rename/type change. Never edit those files to make a test pass.
- **Handle writes and MARTA** (found on the Windows CI, round 4): `SetKernelObjectSecurity` **ignores** `PROTECTED_/UNPROTECTED_DACL_SECURITY_INFORMATION` (MARTA's flags) — protection is `SE_DACL_PROTECTED` in the descriptor's control — and drops `SE_DACL_AUTO_INHERITED` unless `SE_DACL_AUTO_INHERIT_REQ` is set too (with both the DACL still lands exactly as written): `dataacl.daclControl`. MARTA's readers (`GetSecurityInfo` even by handle, `GetNamedSecurityInfo`, icacls) show an entry **without** the AI mark converted against its parent (explicit entries matching the parent's inheritable ones shown `ID`, protection inferred): read the truth with `kernelSD` (`GetKernelObjectSecurity`); tests assert on `kernelOf`, not `daclOf`. `ProtectTree`'s older writes still drop the mark (follow-up).
- **Who can write to a document folder** (review M1): `dataacl.FolderWriters` (the folder: add/delete/rename/ACL/owner; each folder above: rename away — never `Writers`, whose file case flags creating files in the parent, i.e. every folder under `C:\`). The `documents`/`receipts`/`orders` commands refuse a writable folder only when they add, move, or newly make it typed/bound (review R1a: any other is a warning + result `writableFolders`; `allowWritable` over WinRM only; `apply` only warns); the daemon's `hasavshevet.FolderGuard` reads `cfg.WriteFolders()` in the background (review R3: never on the start's path; unknown = refused behind the link; refused re-read lazily ≤ once a minute, allowed every 10 min): behind the link a writable typed folder is refused (`503 FOLDER_WRITABLE`; queue `UseFolderGuard`, receipt writer `FolderCheck`), the pre-087 folders only warn (upgrade safety). `state.writableFolders` lists them. **Trusted writers** (review R1b): `documents.trustedWriters` `[{sid,name}]` (≤ 8, resolved on the box by `documents`' `trustedWriters`: `writer_unknown`, anything but a single user `writer_too_broad`) are never counted — matched by SID (`dataacl.FolderWriters` returns `[]Writer{SID,Name}`). **folder-acl** (review R1c; `provision/folder_acl_cmd.go`, `dataacl/lockdown_windows.go`): named folders only (typed key / `sendOrderDir` / `pickup` / `sendReceiptDir`), **never grants** (review T2): every entry's DACL (folder + ≤ 20 000 beneath, **by handle**) is masked in place by `dataacl.MaskDACL` — SY/BA/TI unchanged, trusted ∩ Modify, anyone else ∩ RX, generic mapped, emptied entries dropped, deny/conditional/object entries and flags kept, **no entry added**; the folder cut from its parent (inherited → explicit); untrusted owner → BA; reparse/hard link = `folder_reparse`, more = `folder_too_large`; never MARTA, never a parent; property test `TestMaskDACL_NeverGrants`; the pre-image (owner+DACL SDDL + file ID per entry) is `DataDir\folder-acl\<sha256>.json`, and `restore` takes only such a sha for that folder — never an SDDL from the input.

**Order idempotency (DIG-1291):** one order number per `historyId`, `docs/hasavshevet-send-order.md` § Idempotency.
- The index entry that owns a number is saved BEFORE the counter moves on; `OrderQueue.Recover` (daemon start, and Accept before a new number until it succeeds) moves the counter past max(highest readable entry, highest `sendOrderDir/history/<n>`), and Accept refuses any Peek ≤ that. An unreadable entry refuses only its own key (`ErrOrderIndexUnreadable`, 500); recovery logs and skips it. Repeats need no counter or recovery.
- The worker saves the outcome before the in-memory status. Failure before the first file is opened (`ErrOrderNotWritten`) or never started (worker stopped; `Stop` waits ≤5 s for that record) → `retryable`: a repeat re-queues it under the SAME number (entry saved `reserved` first). Failure once a file may exist → `failed`, final. A re-queue is refused (logged `ORDER NOT QUEUED AGAIN`, answered `failed`) when `history/<n>/IMOVEIN_<n>.doc` exists or another entry holds `n`. Status shows `retryable` as `failed`: callers resend once on `failed`. `reserved` after a restart → `unknown`, never written on its own; the way back is `docs/troubleshooting.md` § The order index. Never delete an index file to unblock a 500.
- `atomicfile` retries a Windows rename for ≤0.8 s on ERROR_ACCESS_DENIED / ERROR_SHARING_VIOLATION (a reader holding the target); nothing else changed for its other callers.
- `lastOrderNumber.json` stays in `sendOrderDir`; a present-but-unreadable counter now refuses instead of restarting at 1 (missing still = 1). Back up `orders/index/` AND the counter.

**Shutdown order (`serverApp.Stop`):** `srv.Shutdown` → `orderQueue.Drain` (≤120 s, within the one 150 s stop budget `autostart.StopTimeout`) → cancel the worker. Never cancel first — that dropped every queued order (pinned by `cmd/digi-erp-connectord/app_test.go`).

## Hasavshevet send-receipt flow

`POST /api/sendReceipt` → `ReceiptWriter.Write`, synchronously, under the **import lock shared with the order worker** (`importmu.go`). Full doc: `docs/hasavshevet-send-receipt.md`.

- The number store (`DataDir/receipts/receiptNumber.json`) **fails closed** — never restarts at 1; only `cutover-seed -receipt-seed N` or `provision receipt-seed N` (plan 086; also over the link) creates it (write-if-absent).
- One index file per key (`DataDir/receipts/index/<uuid>.json`) is what makes a retry the same receipt. Never delete one to "unblock" a 500 — that turns the retry into a second receipt. Never restore or copy `DataDir/receipts` while the service runs — stop it first (a running writer trusts its in-memory recovery state).
- **One process per data directory**: the writer holds an OS lock on `DataDir/receipts/writer.lock` (`internal/platform/filelock`: flock / LockFileEx) from its first Recover/Write to `Close` (called last in `Stop`) or exit; a second process answers receipts 503 `RECEIPT_DATA_IN_USE`. The importMu is in-process only.
- The store never persists a next number outside 1…999 999 999 + 1; history and index files are 0600 in 0700 dirs on Unix (no chmod on Windows — the DataDir's protected DACL, `internal/platform/dataacl`).
- Import model (b) has two runners, mirroring sendOrder: `hasReceiptBatFile` (cmd.exe /C) **or** `hasReceiptParamFile` (the orders' `hasExePath`, cwd sendReceiptDir) — never both (refused); same 90 s cap + 5 s WaitDelay, `started` recorded before it runs, never re-run for a key, never run by Recover.
- 4xx only for request faults — and never echo a value (a key path at most 64 bytes); a body read cut short is 503, not 400; `CardNamesConfigError`, a stored request that no longer builds (`ErrStoredReceiptUnbuildable`) and every other connector fault are 5xx. The response never carries `ReceiptResult.Files`.
- Config guard `hasavshevet.ValidateReceiptConfig` (sendReceiptDir ≠ sendOrderDir, one absolute receipt importer, card names writable) is called by both `api.NewServer` and the GUI save.
- Contract pinned by `handlers/send_receipt_test.go` against `testdata/receipt/wire.json` + `wire-blanks.json`: the backend's REAL `Receipt` serialized by its HTTP client (`generate.php` beside them, run in a throwaway `backend-php` container; sha256 checked). Regenerate, never hand-edit.
- The strict decoder's key walk descends only where the Go type nests (≤ 8 levels) and skips other values with `Decode` — walking them token by token cost O(depth²) memory.
- Every text value is edge-trimmed on the RAW value, then `normalize1255`, then strict Windows-1255; widths are encoded bytes (U+FB2A and an inner line break are two).

## Link diagnostics, upgrade and auto-update (plan 086 C4/C5/C8, plan 088)

`digi-erp-connector-link` (internal/link, docs/link.md) runs signed commands from a fixed set: provision commands
(commands.go, plus commands_086.go: `logs`, `state`) and ones it answers itself (`selfCommands`: `hashes`,
`link-status`, `upgrade`, `upgrade-result`). A test pins both sets — adding one is a reviewed change. Plan 088 adds
two stream types, not commands: `policy` ('P', relay → box, ≤ `MaxPolicyFrame`) and `report` ('R', box → relay),
`pkg/linkproto/policy.go` — after the security review a scope is `{"type":"all"}` or `{"type":"boxes","boxes":[…]}`,
a moving policy carries signed `rollbacks`, and `upgrade`'s spec (`linkproto.UpgradeSpec`) an optional `rollback`.

| command | where | hard rules |
|---|---|---|
| `logs` | provision/logs.go | read-only; files in DataDir only, by handle (`dataacl.OpenRead`: no reparse point, no hard link, final path under DataDir); every line redacted on the box before it is measured (D1: answers are signed, not sealed); ≤ 900 KiB |
| `state` | provision/state.go | writes nothing (no file, no DACL, no DB); the daemon only through its read-only routes; no secret, not even set/missing; a receipt's request never read out; since v1.0.30 also `counters` and `folders` (plan 087) |
| `counters`, `counters-check`, `documents`, `documents-check` | provision/counters_cmd.go, documents_cmd.go; link/commands_087.go | plan 087: the only writers of counters and typed folders; signed, not sealed (no secret); config.yaml pre-image, idle check, restart + proof; a counter store never in a pre-image |
| `link-status` | link/linkstatus.go | proxy by host only; URL credentials dropped from errors |
| `upgrade` / `upgrade-result` | link/upgrade.go, upgrade_runner.go, upgrade_windows.go | D2: official release only (compiled-in `releasePrefix`, tests alone change it), newer than the link's build, setup SHA-256 = envelope pin = release SHA256SUMS line; every check before `accepted`; the runner is a detached copy of the link in DataDir\upgrade\<ticket>\ (never in the install folder the setup replaces); a failed setup's stopped services are started again; results through plain folders only |
| runner health + rollback (plan 088 D5, §2b) | link/upgrade_health.go, upgrade_runner.go, upgrade_close.go, procs_windows.go, internal/formatfloor | right before the setup the order queue again (≤ 5 min, then `deferred`, unless `assumeIdle`); before each setup end the INSTALLED `digi-erp-connector.exe` in every session, by full path, through the handle its image was read on (G-143, m5), ≤ 15 s, else no setup; after it, health within `healthMinutes` (build, services as before, the daemon proved as `state` does, the new link in `link\session.json`); unhealthy → reinstall exactly the build replaced, once, ONLY to a SIGNED digest (D8: the policy's `rollbacks` / the command's `rollback`; = its SHA256SUMS line = the download; the recorded `fromSha256` is a cross-check, never an authority) — else `unhealthy_no_rollback`; never below `DataDir\format-floor` / the compiled `RollbackFloor` (D11, `rollback_below_floor`); a link-only failure rolls back only if the relay's `/readyz` answers 200 (D12, N5: its registry answering; else `unhealthy_link_inconclusive`); a deferred ticket's checked setup is taken again by the next attempt at that target (N10); progress in `upgrade\<ticket>\health.json`, resumed once by the link after a restart (m7); never after a setup still running; DIG-1297 guard kept. Later builds must keep `session.json`, `state`'s `daemon` fields and raise `format-floor` at start: the old build's runner judges by them; a build that changes a persisted format sets `formatfloor.RollbackFloor` to its own version |
| policies, reports, scheduler (plan 088, §2b) | link/policies.go, autoupgrade.go, session.go; pkg/linkproto/policy.go | a policy counts only signed by a compiled-in team key at the epoch floor, in its canonical bytes, active, naming every box or a signed list holding this box's id (D9: the relay's tenant/ring are information only, never weighed or stored), seq 1..2³¹−1 above the floor of its scope key (`all` / `boxes`; same bytes = re-delivery); floors `{seq, sha256, epoch}` and the signed policy that set each live in the secrets store (`link_policy_floors`), never pruned, the authority (`loadHeld`) with `policy.json` their mirror — a reinstall or a lost file loses no list or brake (N1; a kept list comes back only when it names the box's id), unreadable floors refuse (m3, m4); a floor of a retired key no longer binds (m1). `linkproto.Decide`: the most specific decides, an expired one holds for its scope, a hold issued at or after a moving policy wins at any scope (D10). Deliveries: ≤ 1 per connection per 10 s (the newest waits), `policy.json` written and the scheduler kicked only on a change, kicks ≤ 1 per 10 s (m2). Reports are signed by the box key, sent only on a connection whose relay opened a 'P' (an old relay drops the connection otherwise), ≤ 5 a minute. The scheduler decides only *when* and calls `startUpgrade`, the command's own path — never a second download path; stops and failures are tallied apart from the attempts (m10); `autoUpgrade: false`, `admin: false` or an unknown link.yaml key hold |

The data path reads the daemon's address/TLS per stream (`DaemonWatch`, link/daemon.go) — never cache it at service
start again: an apply that moved the port stranded the data path until someone restarted the link. The previous
address is a 3-minute bridge only (`prevTTL`, dropped once the new one answers), never plaintext behind TLS, and an
unusable certificate serves nothing (review B1). On Windows `DaemonOwnerCheck` pipes only to a socket the
`digi-erp-connectord` service's process owns (`internal/platform/tcptable`, shared with provision's discover) — never
drop it: a port squatter otherwise receives erp-manager's `/auth/token` login. The link starts
without config.yaml (a fresh install). release-windows.yml lists the setup in SHA256SUMS; the box refuses an upgrade
without that line.

## Tests

`go test ./...` — table-driven, stdlib only, `t.TempDir()` for FS, `httptest` for HTTP.
Key suites: `internal/queries` (binder inference, forced strings, store CRUD + electron format compat, DML detection), `handlers/send_order_test.go`, `files_test.go`, `imovein_test.go` (2891-byte layout), `order_number_test.go`, `internal/api/auth_exchange_test.go` (the credential contract: 200-with-access_token, only-an-issued-token-authenticates, exactly-401, shipped defaults rejected, ping, and that `/api/query` stays 404), `internal/config/auth_test.go` (the shared save-time/startup guard).

## Build

```bash
go build -o digi-erp-connectord ./cmd/digi-erp-connectord   # daemon (cross-platform)
go build -o digi-erp-connector.exe ./cmd/digi-erp-connector # GUI (Windows only)
```

## Wire-compatibility constraints (a live backend depends on these)

`internal/queries/runner.go` deliberately reshapes scanned values to match the old
Node driver's JSON. These are not stylistic choices — changing them breaks a
production backend. Covered by `queries/normalize_test.go`:

- datetimes render as `2026-03-08T00:00:00.000Z` (UTC, three fractional digits)
- `DECIMAL`/`NUMERIC`/`MONEY`/`SMALLMONEY` render as JSON **numbers** in shortest
  form (`13085`, never `"13085.00"`)
- the saved-query response carries both envelopes: native
  `api/status/rowCount/rows/recordsets` **and** legacy `value`/`rowsAffected`
- `queries.maxRows` is a real functional limit, not just a safety valve — the b4l
  box needs 100000 because a production query returns 16183 rows

## House rules (introduced by the 2026-08-05 refactor)

- **Never hand-roll a file write.** `internal/platform/atomicfile.Write` is the
  only way config.yaml, queries.json and secrets are written. Three separate
  temp+rename implementations had drifted apart, and one of them reported
  success when the rename failed.
- **Decode request bodies with `decodeJSONBody`** (handlers/json.go): it applies
  the shared 1 MiB limit, rejects trailing data and keeps numbers exact. Do not
  reintroduce per-handler decoders or per-handler size constants.
  `decodeJSONBodyStrict` is the same plus `DisallowUnknownFields` and a token
  pre-pass refusing duplicate keys and keys that match a field only
  case-insensitively, for contract bodies (sendReceipt).
- **`respond.JSON` / `respond.Error` only** — no direct `json.NewEncoder(w)` in
  handlers. There is exactly one error envelope; do not add a second shape.
- **`readFormConfig` must start from `f.cfg`**, never a zero `config.Config`.
  Settings with no widget (queries limits, db.encrypt, hasExePath, tls) would
  otherwise be wiped by a GUI save.
- **The GUI must not be able to save a config the daemon refuses to start on.**
  Any startup precondition in `api.NewServer` that a widget can violate needs a
  matching check at save time, so the GUI cannot leave a box holding a config its
  own service will not start on.
- **Keep the tree gofmt-clean.** `.gitattributes` pins `.go` to LF; if
  `gofmt -l ./cmd ./internal` prints anything, fix it rather than adding to it.

## Prohibited (zero exceptions)

- Adding any endpoint that executes SQL text straight from a request body. There
  is no longer an exception: `POST /api/query` was deleted on 2026-08-09 and the
  backend team explicitly asked that it not come back. A test asserts it 404s.
- Shipping a default for any credential — username, password, or signing secret.
  Generate them; never compile one in.
- Reintroducing a static API token as a second way to authenticate. One scheme.
- Storing secrets in logs (DB password, auth password, signing secret, token
  contents)
- Disabling auth on any route, or the `/auth/token` rate limit, "for testing" (the `/api/*` routes carry no rate limit by decision — Ilya, 2026-11-10; do not add one back without him)
- Returning raw DB driver errors to API clients
- Absolute-path `fileName` values in the file endpoint
- Making the OrderQueue worker concurrent
- Business logic in `cmd/` — belongs in `internal/`

## Removed features (do not resurrect casually)

The PDF/print/email subsystem (`internal/pdf`, `internal/print`,
`internal/email`, the post-order PDF hook, the GUI "PDF & Email Settings"
dialog, `PDFConfig`/`SMTPConfig`, PDFtoPrinter installer bundling) was
deleted on 2026-07-20 by user decision. If it ever comes back, recover it
from git history AND read erp-connector's `docs/printing.md` first — the
print path had hard-won session-0/WSD-port constraints.

## Known AI Failure Patterns (inherited from erp-connector — do not repeat)

### SQL safety
- ❌ Re-introducing a raw-SQL execution endpoint "for debugging"
- ❌ String-concatenating user input into SQL — always `sql.Named()` via the shared binder

### File path security
- ❌ `filepath.Join` on user paths without canonical checks — use `ResolveFilePath()`

## Migration notes

- Legacy electron-mssql-app installations: import their `custom_sql_data.json` by copying it to `%PROGRAMDATA%\digi-erp-connector\queries.json`.
- Legacy erp-connector installations can run side-by-side (different service name, data dir, installer AppId); seed `lastOrderNumber.json` from the old install to keep order-number continuity.
- The backend must call `GET /api/sqlqueries/{name}` — `POST /api/sql` does not exist here.
