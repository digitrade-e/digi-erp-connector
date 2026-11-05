# Hasavshevet Send-Order Flow

## Overview

`POST /api/sendOrder` accepts an order JSON payload, generates Hasavshevet
`IMOVEIN.doc` + `IMOVEIN.prm` import files, and optionally executes `has.exe`.
Processing is **asynchronous**: the API returns `202 Accepted` with a `jobId`
immediately; file writing and has.exe invocation happen in a single background
worker.

`jobId` is the reserved `lastOrderNumber` value (returned as a string).

**`historyId` is the idempotency key** (DIG-1291): one order per `historyId`. A
retry, a redelivery or a resend with the same `historyId` answers the first
order — same `jobId`, nothing written again. See [Idempotency](#idempotency--historyid-dig-1291).

## Improvements over the legacy Node.js flow

| Legacy (Node.js)                         | New (Go)                                      |
|------------------------------------------|-----------------------------------------------|
| External Windows Task Scheduler (5 min delay) | Immediate processing via in-process queue |
| BAT-based rename + has.exe orchestration | Direct Go exec.Command (Windows), no BAT     |
| Fragile JSON file increment (no locking) | Mutex-protected `OrderNumberStore`           |
| File collision possible under concurrency | Single-worker queue — one import at a time   |
| No structured logs or exit-code capture  | Structured logs with order number, duration, exit code |

---

## Architecture

```
HTTP POST /api/sendOrder
        │
        ▼
  Handler (validate)
        │
        ▼
  OrderQueue.Accept()  ──► historyId in DataDir/orders/index?
                           ├─ yes ─► 202 { status:"<its state>", jobId:"<its number>", replay:true }  (nothing else)
                           └─ no  ─► Peek counter → save entry (reserved) → move counter past it → queue
                                     └─► 202 { status:"queued", jobId:"<lastOrderNumber>" }
        │
        ▼ (background, single worker)
  Sender.ProcessOrder()
    1. validateOrderRequest()
    2. queryAccount()               — DB: [dbName].[dbo].[Accounts]
    3. queryRate()                  — DB: [dbName].[dbo].[Rates]
    4. buildIMOVEIN()               — map request → stockHeader + []stockMove
    5. generateDOC()                — fixed-length Windows-1255 bytes
    6. generatePRM()                — position-map Windows-1255 bytes
    7. Write IMOVEIN.doc/.prm       — to SendOrderDir (active files)
    8. Write IMOVEIN_N.doc/.prm     — to SendOrderDir/history/<N>/ (audit)
    9. runImporter()                — exec has.exe (Windows); no-op elsewhere
   10. index entry → written (files), retryable (failed before any file) or
       failed (error), then the job status
```

---

## File format

### IMOVEIN.doc

- One row per order line item.
- Fixed-length fields in Windows-1255 encoding.
- Each row is exactly **2891 bytes** followed by `\n`.
- Field positions match `IMOVEIN.prm`.

### IMOVEIN.prm

- Line 1: total record length (`2891`).
- Lines 2–87: `start end ;title` (1-based byte positions).
- Zero-length fields (line63) use `0 0 ;title`.
- Encoded in Windows-1255.

### Field mapping (key columns)

| PRM line | Field              | Width | Notes                                 |
|----------|--------------------|-------|---------------------------------------|
| line2    | AccountKey         | 15    | Required for some doc types           |
| line4    | DocumentID         | 2     | **Required** — Hasavshevet doc type   |
| line8    | Asmahta2 / historyId | 9   | **Required** — external reference    |
| line22   | ItemKey / SKU      | 20    | **Required**                          |
| line23   | Quantity           | 10    | **Required, must not be zero**        |
| line24   | Price              | 10    | originalPrice from request            |
| line63   | (unused)           | 0     | Skipped per Hasavshevet docs          |
| line87   | Allocation number  | 250   | Written as empty; Hasavshevet fills   |

### Document type codes

| Request `documentType` | Header `line4` | Currency condition |
|------------------------|----------------|--------------------|
| `ORDER`                | `30`           | `ש"ח` (ILS)        |
| `ORDER`                | `32`           | Any other currency |
| `QUOATE`               | `40`           | —                  |
| `RETURN`               | `74`           | —                  |

---

## Configuration

Add these fields to `digi-erp-connector.yaml`:

```yaml
sendOrderDir: "C:\\digiorders"       # Working directory for IMOVEIN files
hasBatFile:   "C:\\Hash7\\digi.bat"  # Masofon-generated BAT launcher (preferred)
# hasExePath and hasParamFile are the legacy direct has.exe path.
# Use hasBatFile instead when Masofon generates the BAT launcher.
hasExePath:   ""
hasParamFile: ""
```

`sendOrderDir` is also where `lastOrderNumber.json` is written (compatible
with the legacy Node app's `config/lastOrderNumber.json` format).

**Execution priority**: `hasBatFile` is checked first. If set, `cmd.exe /C <hasBatFile>`
is run from the BAT file's own directory (so relative paths like `-p"digi.bat"` inside
the BAT resolve correctly). If `hasBatFile` is empty, `hasExePath` is used as fallback.
If both are empty, files are written but no importer is invoked (useful for testing
file output on non-Windows or before the importer is configured).

Because orders are processed by a **single worker**, the sequence for each order is:
1. Write `IMOVEIN.doc` + `IMOVEIN.prm` to `sendOrderDir`
2. Run `digi.bat` and **wait** for it to exit
3. Only then dequeue and start the next order

This guarantees that `IMOVEIN.doc/.prm` always contain the current order when the
importer runs, eliminating the race that caused only the last order to be imported.

---

## Order numbering

`lastOrderNumber.json` format (identical to legacy Node):

```json
{ "lastOrderNumber": 1000295 }
```

The file can be seeded by copying the legacy file to `SendOrderDir/lastOrderNumber.json`.
The Go store will continue from where the legacy app left off.

A **missing** file starts at 1 (a fresh install). A file that exists but holds
no usable number (not JSON, no `lastOrderNumber`, `null`, a string, a fraction,
a negative) is refused and left as found: no new order is accepted (`503
ORDER_COUNTER_UNREADABLE`, the reason in the log) until a person repairs it. Until
DIG-1291 such a file silently restarted at 1. Other fields in a legacy file are
ignored. The file is written by atomic rename.

---

## Idempotency — historyId (DIG-1291)

Before DIG-1291 every POST reserved a new number, so a retry, a redelivery or a
resend of the same order wrote a second Hashavshevet document. Now `historyId`
is the order's key; the B2B sends `<cartId>-<releaseToken>` per release (the
same for every retry of that release, new for a new release) or `Web-<cartId>`
for an order that never waited for approval. The connector treats it as an
opaque string: compared byte for byte, never trimmed or re-cased.

**The index** — `DataDir/orders/index/<sha256(historyId)>.json`, one file per
key. The file name is the key's SHA-256 (the key is free text, so it never
becomes a path); the raw key is stored inside and checked against the name on
every read. Fields: `key`, `number`, `state`, `requestSha256`, `files`,
`error`, `retries`, `reservedAt`, `finishedAt` — no order content. Directories
0700 and files 0600 on Unix; on Windows the data directory's protected DACL
applies. Every reservation is logged — `sendOrder historyId="…": order N
reserved and queued (index <file>)` — which is how a person maps a key to its
number and file.

| `state` | Meaning | A repeat of the key answers |
|---|---|---|
| `reserved` | number owned; queued or being written | `queued` / `running` (from memory) — or **`unknown`** after a restart |
| `written` | files written (importer run, if any) | `done` |
| `retryable` | failed **before any file was written**, or never started (the connector stopped first); the error text is kept | **queues it again under the same number** → `queued` |
| `failed` | failed once a file may exist (IMOVEIN.doc written, a later file not); the error text is kept | `failed` — never written again |

**A repeat** (same `historyId`) answers `202` with the **first** `jobId`, the
order's current `status` and `"replay": true`; it reserves no number and writes
nothing — with one exception, below. It needs no queue slot, no counter and no
recovery (a known key is answered even while `lastOrderNumber.json` is
unreadable). A first answer has no `replay` key (its shape is unchanged). A
repeat whose body differs from the first is still answered with the first order
and logged as a warning. ⚠ So a `202` is not by itself "the order was written":
read `status` (`failed`, `unknown`), or poll `GET /api/sendOrder/{jobId}`.

**Retryable** — most failures happen before anything is written: validation,
the customer account (no details, database unreachable), building the file, the
history folder, or `IMOVEIN.doc` that cannot even be opened (a Windows sharing
violation while the importer holds it). The worker marks those
(`ErrOrderNotWritten`) and the entry `retryable`; so are orders still queued
when the worker is stopped (the drain timed out: `Stop` waits for that record).
A repeat of the key (the B2B's resend reuses it) queues the order again **under
its first number** — no new number, no gap — and answers `202 {"status":
"queued", "replay": true}`. The entry is saved `reserved` before the job is
sent, so racing repeats queue it once. A repeat uses its own body (the entry
holds none). A full queue answers `QUEUE_FULL` and leaves it retryable. A
retryable entry whose number is not safely its own — `history/<n>/IMOVEIN_<n>.doc`
already exists, or another entry holds `n` (a hand edit gone wrong, a number
reissued) — is never queued again: the repeat is answered `failed` and the log
says `ORDER NOT QUEUED AGAIN` with the reason. Once `IMOVEIN.doc` has been
opened a file may hold part of the order, so any later failure is `failed`,
final for the key: a re-release is a new key from the B2B.

⚠ **`failed` does not say whether a resend will write it.** The status
endpoint and a repeat's answer show a `retryable` order as `failed`, the same
word as a final failure, so a poller cannot tell the two apart. The caller's
rule must be **resend once on `failed`** (same `historyId`): a retryable order
is then queued again (the answer says `queued`), a final one answers `failed`
again, and that second `failed` is final. For the B2B this is plan 075 P1-4
(the outcome) and P1-5 (resend).

**`unknown`** — an entry still `reserved` when the connector starts: the order
was accepted, and whether its files were written (and imported) was never
recorded. It is never written again on its own; startup logs `ORDER OUTCOME
UNKNOWN` with the key, the number and where to look. The supported way back is
in [troubleshooting.md](troubleshooting.md): with no
`sendOrderDir/history/<n>/IMOVEIN_<n>.doc`, set `state` to `retryable` and the
next repeat writes it under order `n`.

**Order of writes** — under the queue's lock, so two POSTs of one key never
both miss the index: read the counter (`Peek`) → save the entry (`reserved`) →
move `lastOrderNumber.json` past it → queue. The entry that owns a number is on
disk before the counter moves on, so a number is never owned by nobody, and
never by two keys:

- crash after the entry, before the counter: at startup `Recover` moves the
  counter past the highest number issued (warning logged); the key answers
  `unknown`;
- the counter cannot be written: the order still goes ahead under its number
  (the index owns it), and no new order is accepted until the counter has been
  moved past it;
- the counter moved back (a restore, a hand edit): moved forward, with a
  warning, before any number is issued — at startup, and while running (any
  number at or below the highest issued is refused, and the next attempt
  recovers first).

"The highest number issued" is the highest readable index entry, or the
highest folder under `sendOrderDir/history/` if higher: orders written before
the index existed, or whose entry is damaged, still hold their numbers.

The worker saves `written` / `retryable` / `failed` **before** the job's
in-memory status changes. A crash after the files are written but before that
save leaves the entry `reserved` → `unknown` after the restart (never written
twice).

**An entry that cannot be read** (bad JSON, an unknown field, another key, a bad
number or state, data after it) refuses its own key: `500
ORDER_INDEX_UNREADABLE`, never `202`, never a second number. It costs only that
key: at startup recovery logs it (`ORDER INDEX ENTRY SKIPPED: <file>: <reason>`)
and skips it, and every other order goes on — the counter still moves past the
history folders, so the damaged entry's number is not reissued once its order
was written. Two keys holding one number are logged the same way. Never delete
an index file to "unblock" a 500 — see troubleshooting.md for what is safe.

**Status after a restart** — `GET /api/sendOrder/{jobId}` answers a jobId the
memory no longer has from the index (`done`, `failed` — also for `retryable`,
see the ⚠ above — and `unknown`).

**Windows** — a rename over an index entry (or any file written through
`atomicfile`) fails while another process holds the target open without
sharing delete access (Defender, a backup agent, a reader); it is retried for
up to about 0.8 s on `ERROR_ACCESS_DENIED` / `ERROR_SHARING_VIOLATION` before
the write fails.

**Back up both** with the install: `DataDir/orders/index/` and
`sendOrderDir/lastOrderNumber.json`. Never restore the counter backwards, and
never restore or copy the index while the service runs.

Not here (other tickets): a pre-send lookup of the number in Hashavshevet's
`Stock` (needs a database the write node does not have).

---

## Concurrency and safety

- **Single worker**: `OrderQueue` runs one goroutine. Only one order's
  IMOVEIN files exist in `SendOrderDir` at a time during active import.
- **Order number mutex**: `OrderNumberStore.Next()` holds a `sync.Mutex`
  for the read-increment-write cycle. Safe under concurrent HTTP requests.
- **Queue capacity**: defaults to 64. Returns `503 QUEUE_FULL` when exceeded.

---

## Observability

Every `ProcessOrder` call emits structured log lines:

```
[INFO]  processing order orderNumber=1000295 historyId=HID-001 userExtId=CUST001
[OK]    order complete orderNumber=1000295 files=[..., ...]
[INFO]  has.exe exit=0 durationMs=312 output="" orderNumber=1000295
```

Failures:

```
[ERROR] order job abc123def456 failed: query account "CUST001": account not found
[ERROR] has.exe failed orderNumber=1000295: exit status 1
```

---

## Audit trail

For each order `N`:

```
DataDir/orders/index/
└── <sha256(historyId)>.json ← the key's order number and outcome (DIG-1291)

SendOrderDir/
├── IMOVEIN.doc              ← active import file (overwritten each order)
├── IMOVEIN.prm              ← active param file  (overwritten each order)
├── lastOrderNumber.json
└── history/
    └── 1000295/
        ├── IMOVEIN_1000295.doc   ← permanent copy
        └── IMOVEIN_1000295.prm   ← permanent copy
```

---

## Runbook

### Start

The queue starts automatically when `digi-erp-connectord` starts. No separate
process or Task Scheduler job is needed.

### Monitor

Check `digi-erp-connectord` log file for `[OK] order complete` or `[ERROR]` lines.

### Troubleshoot

- **`sendOrderDir is not configured`**: set `sendOrderDir` in config and restart.
- **`account not found`**: verify `userExtId` exists in `[dbName].[dbo].[Accounts]`.
- **has.exe exit ≠ 0**: check `output` in the log; consult Hasavshevet error logs
  in `SendOrderDir` (Masofon generates diagnostic files on import failure).
- **`QUEUE_FULL`**: reduce request rate or increase `defaultQueueSize` in source.
- **`ORDER_INDEX_UNREADABLE`** (500): this key's index entry is damaged (the
  log names the file). See troubleshooting.md; never just delete it.
- **`ORDER_COUNTER_UNREADABLE`** (503): `lastOrderNumber.json` exists but holds
  no usable number. No new order until a person repairs it; known keys are
  still answered.
- **`ORDER_NOT_QUEUED`** (503): the index could not be written or recovered —
  the log says why. Nothing was reserved; retrying with the same `historyId` is
  safe.
- **A repeat answers `unknown`**: see [Idempotency](#idempotency--historyid-dig-1291).

### Recover from failed import

1. Find the history copy: `SendOrderDir/history/<N>/IMOVEIN_<N>.doc`.
2. Copy it to `SendOrderDir/IMOVEIN.doc` (and `.prm`).
3. Run `has.exe <paramFile>` manually from `SendOrderDir`.

---

## Phase 2 (not yet implemented)

- ODBC direct import path (Phase 2, behind adapter interface).
- Delimited-mode IMOVEIN (Masofon flexible import) as config option.
