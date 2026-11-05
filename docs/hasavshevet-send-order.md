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
   10. index entry → written (files) or failed (error), then the job status
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
a negative) is refused and left as found: no order is accepted (`503
ORDER_NOT_QUEUED`, the reason in the log) until a person repairs it. Until
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
`error`, `reservedAt`, `finishedAt` — no order content. Directories 0700 and
files 0600 on Unix; on Windows the data directory's protected DACL applies.

| `state` | Meaning | A repeat of the key answers |
|---|---|---|
| `reserved` | number owned; queued or being written | `queued` / `running` (from memory) — or **`unknown`** after a restart |
| `written` | files written (importer run, if any) | `done` |
| `failed` | the worker gave up; the error text is kept | `failed` — never written again |

**A repeat** (same `historyId`) answers `202` with the **first** `jobId`, the
order's current `status` and `"replay": true`; it reserves no number, queues
nothing, writes no file, needs no queue slot. A first answer has no `replay` key
(its shape is unchanged). A repeat whose body differs from the first is still
answered with the first order and logged as a warning. A **failed** order is
never retried under its key: a re-release is a new key from the B2B. ⚠ So a
`202` is not by itself "the order was written": read `status` (`failed`,
`unknown`), or poll `GET /api/sendOrder/{jobId}`.

**`unknown`** — an entry still `reserved` when the connector starts: the order
was accepted, and whether its files were written (and imported) was never
recorded. It is never written again; startup logs `ORDER OUTCOME UNKNOWN` with
the key, the number and where to look (`sendOrderDir/history/<n>`, Hashavshevet
by Asmahta). A person decides; to record the outcome, stop the service and set
`state` to `written` or `failed` in the entry by hand.

**Order of writes** — under the queue's lock, so two POSTs of one key never
both miss the index: read the counter (`Peek`) → save the entry (`reserved`) →
move `lastOrderNumber.json` past it → queue. The entry that owns a number is on
disk before the counter moves on, so a number is never owned by nobody, and
never by two keys:

- crash after the entry, before the counter: at startup `Recover` moves the
  counter past every number the index holds (warning logged); the key answers
  `unknown`;
- the counter cannot be written: the order still goes ahead under its number
  (the index owns it), and no new order is accepted until the counter has been
  moved past it;
- the counter moved back (a restore, a hand edit): moved forward past the
  index, with a warning, before any number is issued.

The worker saves `written` / `failed` **before** the job's in-memory status
changes. A crash after the files are written but before that save leaves the
entry `reserved` → `unknown` after the restart (never written twice).

**An entry that cannot be read** (bad JSON, an unknown field, another key, a bad
number or state, data after it) refuses its key: `500 ORDER_INDEX_UNREADABLE`,
never `202`, never a second number. At startup any unreadable entry — or two
keys holding one number — fails recovery: every new order is refused until a
person repairs the file (it may own the highest number). Never delete an index
file to "unblock" a 500: that turns the next retry into a second document.

**Status after a restart** — `GET /api/sendOrder/{jobId}` answers a jobId the
memory no longer has from the index (`done`, `failed`, `unknown`).

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
- **`ORDER_INDEX_UNREADABLE`** (500): an index entry is damaged (the log names
  the file). Stop the service, repair it from a backup or by hand; never delete it.
- **`ORDER_NOT_QUEUED`** (503): the counter is unreadable, the index cannot be
  written, or recovery failed — the log says which. Nothing was reserved.
- **A repeat answers `unknown`**: see [Idempotency](#idempotency--historyid-dig-1291).

### Recover from failed import

1. Find the history copy: `SendOrderDir/history/<N>/IMOVEIN_<N>.doc`.
2. Copy it to `SendOrderDir/IMOVEIN.doc` (and `.prm`).
3. Run `has.exe <paramFile>` manually from `SendOrderDir`.

---

## Phase 2 (not yet implemented)

- ODBC direct import path (Phase 2, behind adapter interface).
- Delimited-mode IMOVEIN (Masofon flexible import) as config option.
