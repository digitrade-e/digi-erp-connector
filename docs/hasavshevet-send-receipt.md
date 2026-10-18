# Hasavshevet Send-Receipt Flow

`POST /api/sendReceipt` writes one Hashavshevet **KUPAIN** receipt ("קבלה") per
card payment and answers with its number. It is the connector half of plan 056
(DIG-922): the backend captures a card payment for open invoices, builds the
receipt from its frozen snapshot (DIG-928, `Model/Outbound/Receipt`) and sends
it here through its outbound pipeline — the body is exactly
`Receipt::jsonSerialize()`, no erp-manager mapping line touches it; this
connector writes `KUPAIN_{n}.doc` + `KUPAIN.prm` into the folder Hashavshevet's
receipt importer reads (DIG-929).

**Dormant by default.** Without `sendReceiptDir` the route answers
`503 RECEIPTS_NOT_CONFIGURED` and touches no disk, and the binary behaves like
v1.0.17 apart from that route and the `version` field in `/api/ping`. With it but
no seed, the route answers `503 RECEIPT_NUMBER_STORE_UNAVAILABLE`.

## Contents

- [Contract](#contract) · [Request fields](#request-fields) ·
  [Receipt states](#receipt-states) · [Import models](#import-models-a-and-b) ·
  [Numbering and the seed](#numbering-and-the-seed) ·
  [Idempotency and recovery](#idempotency-and-recovery) ·
  [Concurrency](#concurrency) · [Shutdown and upgrades](#shutdown-and-upgrades) ·
  [Configuration](#configuration) · [Open questions](#open-questions)

## Contract

Synchronous, unlike `sendOrder`: the caller needs the receipt number back. The
route extends its own write deadline to **120 s** (the server's default is 60 s).

```
POST /api/sendReceipt
Authorization: Bearer <token from /auth/token>
Content-Type: application/json
```

| Status | Body | Meaning |
|---|---|---|
| `200` | `{"receiptNumber":"1000","written":true,"idempotencyKey":"<uuid>","import":"not_run","warnings":[]}` | The receipt file is in place. A repeat of the key answers the same thing. |
| `400` | `VALIDATION_ERROR`, `details.field` names the field | The request is at fault. Nothing reserved, no number used. |
| `503` | `RECEIPTS_NOT_CONFIGURED` | No `sendReceiptDir`. Nothing read or written. |
| `503` | `RECEIPT_NUMBER_STORE_UNAVAILABLE` | The number store is missing (not seeded), unreadable, exhausted, or was moved back (a number's history already exists). |
| `503` | `RECEIPT_REQUEST_READ_FAILED` | The body could not be read to its end (the server's read timeout, a client that stalled or hung up). Nothing was read into a receipt — retry with the same key. Only the 1 MiB cap is a 400. |
| `503` | `RECEIPT_DATA_IN_USE` | Another connector process holds this data directory's receipt data (see [Concurrency](#concurrency)). Nothing reserved — retry with the same key. |
| `503` | `RECEIPT_WRITER_BUSY` | Another import held the lock for 20 s. Nothing reserved — retry with the same key. |
| `500` | `RECEIPT_CONFIG_INVALID` | A `receiptCardNames` override cannot be written. The daemon refuses such a config at startup, so this should never be seen. |
| `500` | `RECEIPT_INDEX_UNREADABLE` | An index entry is corrupt, or two keys hold one number. A person must look. |
| `500` | `RECEIPT_STORED_REQUEST_INVALID` | A receipt reserved earlier (being finished by recovery or a repeat) no longer builds. The connector's index is at fault, not this request — never a 400. |
| `500` | `RECEIPT_WRITE_FAILED` | The disk. The number stays with the key; a repeat (or the next receipt) finishes it. |

Rules the backend relies on:

- **Only a 400 is final.** The backend stops a receipt on a 400 (its content
  can never be written) and retries everything else — 503, 500, a timeout, a
  dropped answer — with the same `idempotencyKey`. So a 400 is answered only
  for the request's own content, and a retry of a key always gets the first
  number and never writes a second file.
- `written` is `true` on every 200. Treat only `written === true` with a
  `/^[1-9]\d{0,8}$/` `receiptNumber` as written (the backend accepts it as a
  JSON string, which is what it gets, or an integer).
- `import` is one of `not_run | started | ok | failed` — see
  [Receipt states](#receipt-states). It never changes whether the receipt is
  written.
- `warnings` is always an array. Today the only warning is the card name (F17)
  left blank because no issuer/brand code has a name.
- `idempotencyKey` is echoed in its canonical lower-case form.
- A 4xx is never the connector's fault and a 5xx never the request's. A
  `CardNamesConfigError` in particular is a 5xx, never a 400.
- The response never carries a file path.

## Request fields

Plan 056 §4.6a, flat: **exactly** the sixteen keys the backend's
`Receipt::jsonSerialize()` emits, as the types it emits them as. The backend
always sends all sixteen and never `null`; an empty value is `""`.

The body is decoded **strictly**. Each of these is a `400 VALIDATION_ERROR`
whose `details.field` is the key (or `body` for the document as a whole):

- a key absent, or `null`;
- a wrong JSON type — `amountMinor` / `installments` as a string or with a
  fraction (`693200.0` too), any other key as a number;
- an unknown key (the 10-11 nested wire's `account`, `details`, `currency`,
  `firstPaymentMinor`, `providerTransactionId`, `remarkSuffix` … included);
- a key given twice, or one that matches only in another case (`AmountMinor`)
  — `encoding/json` alone would accept those two silently;
- trailing data, more than 1 MiB, or not a single JSON object;
- nesting deeper than encoding/json's own limit, or a value nested where the
  receipt has a scalar (a flat body never nests);
- a value that breaks the rule below.

Every string value is **edge-trimmed** before any rule below is checked: the
whitespace around it (Unicode's, line breaks included) and the invisible marks
`normalize1255` removes are dropped, so "required" means something is left and
a width counts what is written.

| Key | Type | Rule (else 400) | KUPAIN |
|---|---|---|---|
| `idempotencyKey` | string | `8-4-4-4-12` hex UUID (any case; echoed lower-case) | index key |
| `accountKey` | string | required, ≤ 15 B | F2 |
| `settledDate` | string | `YYYY-MM-DD`, a real day | F5 + F6 as `dd/mm/YYYY` |
| `amountMinor` | int | 1 … 99 999 999 agorot | F12 `%d.%02d` |
| `installments` | int | exactly 1 | F16 |
| `cardIssuer` | string | `^\d{0,2}$` | F17, looked up first |
| `cardBrand` | string | `^\d{0,2}$` | F17, when the issuer has no name |
| `last4` | string | `^\d{4}$` (ASCII digits) | F28 |
| `confirmationCode` | string | required, ≤ 9 B | F22 and F43 |
| `providerUid` | string | ≤ 250 B | F23 |
| `remarks` | string | required, ≤ 250 B | F7, as sent |
| `agentKey` | string | ≤ 9 B | F20 |
| `name` | string | required, ≤ 50 B | F24 |
| `address` | string | ≤ 50 B | F25 |
| `phone` | string | ≤ 50 B | F27 |
| `email` | string | ≤ 50 B | F42 |

The connector's own fields: F3 the receipt number, F4 `33`, and F26 (city) and
F29 (first payment) **blank**, as the legacy wrote them. F7 is whatever the
backend built — the invoice references joined `", "`, cut by the backend at the
last whole reference with ` +N` — the connector neither builds nor cuts it.

"B" is bytes after Windows-1255 and `normalize1255` — one per Hebrew letter, so
250 Hebrew letters (500 bytes of UTF-8) fit F7, but a presentation form such as
U+FB2A (שׁ) is two (ש + shin dot) and an inner line break is two (`. `).

In order, per value:

1. **Edge trim** (on the raw value): Unicode whitespace at either end — space,
   tab, `\r`, `\n`, VT, NEL (U+0085), no-break space, U+2028/U+2029,
   ideographic space (U+3000) … — and the marks step 2 removes, at either end.
   A trailing break is therefore never written as `.`.
2. **`normalize1255`**: NFC; removes bidi controls (LRM/RLM, the embeddings,
   overrides and isolates, ALM), the zero-width non-joiner and joiner (U+200C,
   U+200D), the BOM, the soft hyphen and the apostrophe; writes an inner line
   break (`\r\n`, `\r`, `\n`) as `. ` and a tab or no-break space as a space.
3. **Strict Windows-1255**: anything left that the code page lacks, or a control
   character, is a 400. That includes, **inside** a value, VT, NEL, U+2028,
   U+2029 and U+3000 (trimmed at the edges by step 1), and the zero-width space
   U+200B **anywhere** — it is not whitespace, so step 1 keeps it even at an
   edge. Nothing is ever
truncated or transliterated: a value that does not fit or cannot be encoded
(Arabic, Cyrillic, an emoji) is a 400 naming the key. "Required" means
something is left after that normalisation and trimming.

Card codes compare as numbers, as the legacy PHP `==` did: `"02"` is issuer 2,
and a `receiptCardNames` key `"01"` matches code `"1"`.

The contract is pinned by `internal/api/handlers/testdata/receipt/wire.json`
and `wire-blanks.json`: the backend's real `Receipt` class serialized by its
real HTTP client (Symfony HttpClient's `json` option — Hebrew and quotes arrive
as `\uXXXX`), produced by `generate.php` beside them in a throwaway
`backend-php` container and pinned by sha256. If the backend's `Receipt`
changes, regenerate them; never hand-edit them.

## Receipt states

Per idempotency key, in `DataDir/receipts/index/<key>.json`:

| `state` | Meaning |
|---|---|
| `reserved` | The key owns number n; the file may not be in place yet (a crash, a disk error). Recovery or a repeat finishes it. |
| `written` | `KUPAIN_{n}.doc` is in `sendReceiptDir` and the history copy in `DataDir/history/receipts/{n}/`. |

| `import` | Meaning |
|---|---|
| `not_run` | No import run: model (a), or a receipt finished by recovery (in model (b) its `warnings` then say so on every replay). Imported by hand. |
| `started` | The import (BAT or `has.exe`) was started and the process died before its outcome was recorded. It may or may not have imported. **Never re-run** for this key — a person checks Hashavshevet. |
| `ok` | The import exited 0. |
| `failed` | The import exited non-zero, failed to start, or ran past 90 s. The receipt is still written; import it by hand. Never re-run. |

The backend records `written` regardless of `import`, and logs at error for
`failed` and `started`.

## Import models (a) and (b)

- **Model (a)** — `hasReceiptBatFile` and `hasReceiptParamFile` empty (the
  default). Files only; someone imports them in Hashavshevet. `import` is always
  `not_run`.
- **Model (b)** — the connector imports each receipt itself, right after writing
  it, under the import lock — as `sendOrder` does (plan 056 §8 A-2). Two
  runners, the mirror of the order importer's, **exactly one of which may be
  set** (both = the daemon refuses to start, the GUI to save):
  - `hasReceiptBatFile` — a Masofon KUPAIN BAT, run via `cmd.exe /C` from its own
    folder;
  - `hasReceiptParamFile` — the orders' `has.exe` (`hasExePath`, which must then
    be absolute) run with this parameter file, from `sendReceiptDir`.

  Either way it runs once, with a **90 s** deadline that survives the caller
  hanging up, plus a 5 s `WaitDelay` so a killed process whose child still holds
  the output pipe cannot stretch the deadline. `import` becomes `started` before
  it runs, then `ok` or `failed`; a key whose import started never runs it again.
  A `has.exe` that runs past 90 s is killed (with the BAT, `cmd.exe` is killed,
  and the importer it started may keep running) and the receipt is recorded
  `failed`, never re-run — but the import may already have happened: **check
  Hashavshevet for the receipt before importing it by hand**, or it is imported
  twice. Off Windows nothing can run either importer: the receipt is written and
  recorded `failed` ("the receipt import runs only on Windows; not run").
  Which runner a write node uses follows how its `sendOrder` runs (read its
  `config.yaml` at install: `hasBatFile` → a receipt BAT, `hasExePath` → a
  receipt parameter file).

## Numbering and the seed

The receipt number (F3, nine digits) comes from
`DataDir/receipts/receiptNumber.json`, `{"nextReceiptNumber": N}` — the number the
**next** receipt gets. Unlike the order counter it **fails closed**: missing,
corrupt, zero, negative, fractional, quoted, an unknown field, or past
999 999 999 is `ErrStoreUnavailable` (503), and the file is left as found. It never
restarts at 1.

Create it once, before the first receipt, with the service stopped or running:

```powershell
cutover-seed.exe -receipt-seed 5000000
```

`-receipt-seed` is a mode of its own, chosen by the flag being given — so
`-receipt-seed 0` (an empty script variable) is refused, not taken for
provisioning. It refuses any other flag (they would re-save `config.yaml` with
their defaults), requires N > 0 and exits non-zero otherwise, and writes **only when
no store exists** — a second seed, higher or lower, is an error and changes
nothing, so the numbering can never be lowered. Choose N in a range the legacy
writer cannot reach (see [Open questions](#open-questions)).

Numbers are gapless: the index entry that owns n is written **before** the store
moves to n+1, and recovery moves the store past every number the index holds.

A fresh number whose `DataDir/history/receipts/{n}/` already exists means the
store was moved back (a partial restore, a hand edit): the route answers 503 and
writes nothing. Move the store past the highest number in `history/receipts/`
by hand (`{"nextReceiptNumber": <highest+1>}`), then retry.

## Idempotency and recovery

One Write, under the import lock:

1. validate the whole request — before any number is touched;
2. a key already `written` answers its stored number and `import`, touching
   nothing (a different body is a warning; the stored request is what the
   receipt holds); a key `reserved` is finished from its stored request;
3. otherwise persist the index entry owning the next number (`reserved`), then
   move the store on;
4. write the history copies, `KUPAIN.prm` if missing or different, and **last**
   `KUPAIN_{n}.doc` — each by atomic rename (temp name `.KUPAIN_{n}.doc.tmp-*`),
   so a poller sees a whole file or none;
5. persist `written`;
6. model (b) only: the import (BAT or `has.exe`), as above.

If step 4 fails for a fresh number, n stays reserved and the **next** Write —
whatever its key — finishes n before issuing n+1, so the importer never sees n+1
while n is missing.

The `.doc` rename (step 4) and persisting `written` (step 5) are two steps in
**both** models: a crash between them leaves the key `reserved` with
`KUPAIN_{n}.doc` already visible, and the next repeat or `Recover` writes it
again (same number, same bytes) — so anything outside the connector that
consumed and **deleted** `KUPAIN_{n}.doc` in that window (an importer, a
scheduled task) sees it reappear and can import it twice. Check for such a
consumer when moving off the quarantine folder (see [Open questions](#open-questions)).

`Recover()` runs at daemon startup: it finishes every `reserved` receipt (files
only — never the import) and moves the store past every indexed number. In model
(b) such a receipt keeps `import: not_run`, which on its own reads like a
model-(a) answer, so recovery also stores a warning on its index entry ("…the
receipt BAT was not run for it — import it by hand", or "…the receipt has.exe
was not run…") that **every replay of the
key returns in `warnings`**, besides the line in `server.log`. If it fails, the
daemon still starts (orders and queries are unaffected) and each receipt request
retries it.

Recovery refuses — and every receipt answers 500 — if an index entry is
unreadable or two entries hold one number. Never delete index files to "fix"
this: **a lost index turns a retry into a second receipt**. Back up
`DataDir/receipts/` with the data directory, and restore it together with the
store.

**Never restore or copy `DataDir/receipts` while the service runs — stop it
first.** A running writer trusts its in-memory recovery state: it recovers once
at startup (or after a failed write), not before every new number, so index
files or a store swapped in underneath it are not re-checked and a number can
be issued twice.

## Concurrency

Receipts and orders share one process-wide import lock: an order job (IMOVEIN +
`has.exe`/`digi.bat`) and a receipt (KUPAIN + its BAT or `has.exe`) never run at once. A
receipt waits at most 20 s for it (`503 RECEIPT_WRITER_BUSY`, nothing reserved).

**One process per data directory.** The import lock is in-process only, so
two connector processes on one `DataDir` (a second service install, a console
run beside the service) would each read the number store and give one number
to two keys. The writer therefore holds an OS lock on
`DataDir/receipts/writer.lock` — `LockFileEx` on Windows, `flock` on Linux —
from its first `Recover` or receipt until it is closed or the process exits.
The lock is the operating system's, not the file's: a file left behind by a
crash locks nobody out. A second process **keeps serving orders and queries**
but answers every receipt `503 RECEIPT_DATA_IN_USE` (nothing reserved); when the
first process exits, the second takes over on its next receipt request without
a restart. Its startup recovery fails with the same reason and is logged — the
daemon still starts, as it does for any failed recovery. On Windows a held
`writer.lock` also cannot be deleted (the handle does not share delete).

The receipt data is personal data. On Windows the whole data directory gets a
**protected DACL** at every daemon start (SYSTEM, Administrators and the service
account only when it is not an admin; OWNER RIGHTS read-only, so the owner
cannot rewrite it; set by handle before server.log is opened and before the
config loads, with every entry that carries its own ACL rewritten and any link,
hard link or untrusted owner refusing startup — then with no file opened in it
and the reason in the Event Log; the elevated GUI walks it at launch and before
every save, refusing a save it cannot protect) — see
[configuration.md](configuration.md#data-directory-permissions-windows). On Linux
the receipt directories are 0700 and their files 0600.

⚠ The order importer has **no timeout** of its own (pre-existing). A hung
`has.exe`/`digi.bat` now also blocks receipts — each answers 503 after 20 s until
the order import ends.

## Shutdown and upgrades

`Stop()` (service stop, Ctrl-C) shuts down in the order that loses nothing,
all within one **150 s** stop budget (`autostart.StopTimeout`):

1. HTTP shutdown — a receipt being written finishes and gets its answer;
2. the order queue drains — every order already answered 202 is written. The
   drain gets **at most 120 s, and never more than what is left of the 150 s**
   after step 1;
3. the worker is cancelled.

The Windows service reports `StopPending` with a 160 s wait hint. The installer
runs `sc stop` and waits up to 180 s for `STOPPED` before replacing any binary,
and aborts with a message if the service has not stopped. The uninstaller does
the same stop-and-wait before `sc delete`; it cannot abort, so a service that
has not stopped by then is reported and deleted anyway.

⚠ **Reboots do not get the 150 s.** On an OS shutdown the service is sent
`SERVICE_CONTROL_SHUTDOWN`, and Windows caps that stop at
`WaitToKillServiceTimeout` (`HKLM\SYSTEM\CurrentControlSet\Control`, a few
seconds by default) whatever the wait hint says. A receipt cut off there is
safe — its key stays `reserved` (or `import: started`) and recovery finishes it
at the next start — but **orders still queued are lost**: they were answered
202 and hold their order numbers, yet no IMOVEIN is written. Before a planned
reboot (Windows Update included), stop the service first and let the stop
finish. The connector does not register for `SERVICE_ACCEPT_PRESHUTDOWN`: that
needs a preshutdown timeout set through `ChangeServiceConfig2` by both the GUI
and the installer, and would still be a cap. See `docs/operations.md`.

The GUI's **Stop server**
and **Restart server** wait up to 165 s (`autostart.ServiceStopWait`) and say the
stop may take up to ~2.5 min; a stop still pending after that is reported as
"still stopping", and Restart then does **not** start the service (starting a
`STOP_PENDING` service would leave it down once the stop completes).

## Configuration

```yaml
erp: hasavshevet
sendOrderDir: C:\Hash7\orders
sendReceiptDir: C:\Apache24\htdocs\herp   # the receipt importer's folder
hasReceiptBatFile: ""                     # model (b), BAT way — or:
hasReceiptParamFile: ""                   # model (b), has.exe way (uses hasExePath); never both
receiptCardNames:                          # optional overrides of F17
  issuer:
    "6": מקס
```

`sendReceiptDir` must be an absolute path (a relative one would resolve against
the service's working directory) and must not be `sendOrderDir` — compared after
resolving `..`,
trailing separators and symlinks, case-insensitively on Windows. The daemon
refuses to start on it and the GUI refuses to save it, as it does a
`receiptCardNames` name that cannot be written in F17 (not Windows-1255, or wider
than 25 bytes), or two keys of one table naming the same code (`"6"` and
`"06"` — codes compare as numbers, so F17 would depend on which came first).
Only `KUPAIN_{n}.doc` and `KUPAIN.prm` are ever written to
`sendReceiptDir`; the store, index and history live in the data directory,
because on a real install the herp folder sits under a web root. See
[configuration.md](configuration.md#hasavshevet-receipts).

Built-in card names (the legacy table): issuer `1` ישראכרט, `2` ויזה,
`5` אלפא קארד/לאומי קארד; brand `3` דיינרס, `4` אמריקן אקספרס. Anything else
leaves F17 blank with a warning — the legacy's "JCB" fallback is deliberately not
kept. Codes, the request's and the override keys alike, compare as numbers
(`"02"` = `2`).

## Open questions

Blocked on the customer side (plan 056 G1) and made configurable rather than
guessed:

- **BAT file pickup.** Does the receipt BAT (or a poller) pick up every file in
  the folder, including atomicfile's in-flight `.KUPAIN_{n}.doc.tmp-*`? The temp
  name starts with a dot and ends in `.tmp-<random>`, and exists only for the
  write; if the importer globs `*.doc` it is never matched, but this is not yet
  verified against BFL's BAT (Q-J2).
- **Seed range.** The legacy writer's F3 was its own credit-card row id. The seed
  must sit in a range the legacy can never reach, and the right N is not decided
  (Q-J3 / Q-J6).
- **F7's legacy suffix.** The legacy F7 was `"{OrdId} | 2824019"`; what
  `2824019` is has not been answered (July, G1). Until it is, F7 is the
  backend's invoice references alone. The connector writes `remarks` as sent,
  so adding it later is a backend change only.
- **Card codes.** HYP issuer codes beyond 1/2/5 (6 looks like Max/Leumi Card) are
  not in the legacy table; until named via `receiptCardNames`, F17 is blank with
  a warning (Q-B1).
- **Folder.** `sendReceiptDir` should first be a quarantine folder nobody imports
  from, until a real legacy KUPAIN pair has been compared byte for byte with the
  goldens (Q-J7). Before pointing it at the live folder, check whether anything
  consumes and deletes `KUPAIN_{n}.doc` there: a crash between the `.doc`
  rename and `written` rewrites the file (see
  [Idempotency and recovery](#idempotency-and-recovery)).
