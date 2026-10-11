# Hasavshevet Send-Receipt Flow

`POST /api/sendReceipt` writes one Hashavshevet **KUPAIN** receipt ("קבלה") per
card payment and answers with its number. It is the connector half of plan 005
(DIG-922): the backend captures a card payment for open invoices, builds the
receipt from its frozen snapshot (DIG-928) and sends it here through erp-manager;
this connector writes `KUPAIN_{n}.doc` + `KUPAIN.prm` into the folder
Hashavshevet's receipt importer reads (DIG-929).

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
| `503` | `RECEIPT_WRITER_BUSY` | Another import held the lock for 20 s. Nothing reserved — retry with the same key. |
| `500` | `RECEIPT_CONFIG_INVALID` | A `receiptCardNames` override cannot be written. The daemon refuses such a config at startup, so this should never be seen. |
| `500` | `RECEIPT_INDEX_UNREADABLE` | An index entry is corrupt, or two keys hold one number. A person must look. |
| `500` | `RECEIPT_STORED_REQUEST_INVALID` | A receipt reserved earlier (being finished by recovery or a repeat) no longer builds. The connector's index is at fault, not this request — never a 400. |
| `500` | `RECEIPT_WRITE_FAILED` | The disk. The number stays with the key; a repeat (or the next receipt) finishes it. |

Rules the backend relies on:

- `written` is `true` on every 200. Treat only `written === true` with a
  `/^\d{1,9}$/` `receiptNumber` as written.
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

The body is decoded **strictly**: an unknown field, a wrong type, trailing
data, a key given twice, or a key that matches a field only in another case
(`AmountMinor`) is `400` naming the field by its path (`account/city`).
`encoding/json` alone would accept the last two silently. Nullable fields accept `null` or absence. The
authoritative table is plan 005 §2; KUPAIN positions are in
`internal/erp/hasavshevet/kupain.go`.

| Field | Type | Rule (else 400) | KUPAIN |
|---|---|---|---|
| `idempotencyKey` | string | required, `8-4-4-4-12` hex UUID | index key |
| `account.syncKey` | string | required, ≤ 15 B, trimmed | F2 |
| `account.name` | string | required, ≤ 50 B | F24 |
| `account.address` / `city` / `phone` | string, null ok | ≤ 50 B | F25 / F26 / F27 |
| `account.agentSyncKey` | string, null ok | ≤ 9 B, trimmed | F20 |
| `email` | string, null ok | ≤ 50 B | F42 |
| `settledAt` | string | `YYYY-MM-DD` | F5 + F6 as `dd/mm/YYYY` |
| `amountMinor` | int | 1 … 99 999 999 agorot | F12 `%d.%02d` |
| `currency` | string | exactly `ILS` | — |
| `installments` | int | 1 … 99 | F16 |
| `firstPaymentMinor` | int, null ok | 1 … amount | F29 (blank for 1 installment, else the remainder rule) |
| `cardIssuer` / `cardBrand` | string, null ok | ≤ 32 B | F17 card name: issuer first, then brand |
| `last4` | string | `^\d{4}$` | F28 |
| `confirmationCode` | string | required, ≤ 9 B, trimmed | F22 and F43 |
| `authorizationRef` | string, null ok | ≤ 250 B | F23 |
| `providerTransactionId` | string | required, ≤ 64 B | index only |
| `details[]` | array | at least one | — |
| `details[].reference` | string | required, ≤ 32 B | F7, joined `, ` |
| `details[].amountMinor` | int | required, signed | audit only |
| `remarkSuffix` | string, null ok | ≤ 40 B | F7 ` \| {suffix}`, F7 ≤ 250 B |

"B" is bytes after Windows-1255. Nothing is ever truncated or transliterated: a
value that does not fit or cannot be encoded is a 400 naming the field.

## Receipt states

Per idempotency key, in `DataDir/receipts/index/<key>.json`:

| `state` | Meaning |
|---|---|
| `reserved` | The key owns number n; the file may not be in place yet (a crash, a disk error). Recovery or a repeat finishes it. |
| `written` | `KUPAIN_{n}.doc` is in `sendReceiptDir` and the history copy in `DataDir/history/receipts/{n}/`. |

| `import` | Meaning |
|---|---|
| `not_run` | No BAT run: model (a), or a receipt finished by recovery. Imported by hand. |
| `started` | The BAT was started and the process died before its outcome was recorded. It may or may not have imported. **Never re-run** for this key — a person checks Hashavshevet. |
| `ok` | The BAT exited 0. |
| `failed` | The BAT exited non-zero, failed to start, or ran past 90 s. The receipt is still written; import it by hand. Never re-run. |

The backend records `written` regardless of `import`, and logs at error for
`failed` and `started`.

## Import models (a) and (b)

- **Model (a)** — `hasReceiptBatFile` empty (the default). Files only; someone
  imports them in Hashavshevet. `import` is always `not_run`.
- **Model (b)** — `hasReceiptBatFile` set. After each receipt is written the BAT
  runs once via `cmd.exe /C`, from its own folder, with a **90 s** deadline that
  survives the caller hanging up, plus a 5 s `WaitDelay` so a killed `cmd.exe`
  whose importer still holds the output pipe cannot stretch the deadline.
  `import` becomes `started` before it runs, then `ok` or `failed`. Off until
  the BAT itself is confirmed (G-2).

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
6. model (b) only: the BAT, as above.

If step 4 fails for a fresh number, n stays reserved and the **next** Write —
whatever its key — finishes n before issuing n+1, so the importer never sees n+1
while n is missing.

`Recover()` runs at daemon startup: it finishes every `reserved` receipt (files
only — never the BAT; a warning in `server.log` says to import it by hand in
model (b)) and moves the store past every indexed number. If it fails, the
daemon still starts (orders and queries are unaffected) and each receipt request
retries it.

Recovery refuses — and every receipt answers 500 — if an index entry is
unreadable or two entries hold one number. Never delete index files to "fix"
this: **a lost index turns a retry into a second receipt**. Back up
`DataDir/receipts/` with the data directory, and restore it together with the
store.

## Concurrency

Receipts and orders share one process-wide import lock: an order job (IMOVEIN +
`has.exe`/`digi.bat`) and a receipt (KUPAIN + its BAT) never run at once. A
receipt waits at most 20 s for it (`503 RECEIPT_WRITER_BUSY`, nothing reserved).

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

## Configuration

```yaml
erp: hasavshevet
sendOrderDir: C:\Hash7\orders
sendReceiptDir: C:\Apache24\htdocs\herp   # the receipt importer's folder
hasReceiptBatFile: ""                     # model (b) only
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
than 25 bytes). Only `KUPAIN_{n}.doc` and `KUPAIN.prm` are ever written to
`sendReceiptDir`; the store, index and history live in the data directory,
because on a real install the herp folder sits under a web root. See
[configuration.md](configuration.md#hasavshevet-receipts).

Built-in card names (the legacy table): issuer `1` ישראכרט, `2` ויזה,
`5` אלפא קארד/לאומי קארד; brand `3` דיינרס, `4` אמריקן אקספרס. Anything else
leaves F17 blank with a warning — the legacy's "JCB" fallback is deliberately not
kept.

## Open questions

Blocked on the customer side (plan 005 G1) and made configurable rather than
guessed:

- **BAT file pickup.** Does the receipt BAT (or a poller) pick up every file in
  the folder, including atomicfile's in-flight `.KUPAIN_{n}.doc.tmp-*`? The temp
  name starts with a dot and ends in `.tmp-<random>`, and exists only for the
  write; if the importer globs `*.doc` it is never matched, but this is not yet
  verified against BFL's BAT (Q-J2).
- **Seed range.** The legacy writer's F3 was its own credit-card row id. The seed
  must sit in a range the legacy can never reach, and the right N is not decided
  (Q-J3 / Q-J6).
- **Card codes.** HYP issuer codes beyond 1/2/5 (6 looks like Max/Leumi Card) are
  not in the legacy table; until named via `receiptCardNames`, F17 is blank with
  a warning (Q-B1).
- **Folder.** `sendReceiptDir` should first be a quarantine folder nobody imports
  from, until a real legacy KUPAIN pair has been compared byte for byte with the
  goldens (Q-J7).
