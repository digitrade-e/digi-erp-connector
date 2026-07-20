# Hasavshevet Send-Order Flow

## Overview

`POST /api/sendOrder` accepts an order JSON payload, generates Hasavshevet
`IMOVEIN.doc` + `IMOVEIN.prm` import files, and optionally executes `has.exe`.
Processing is **asynchronous**: the API returns `202 Accepted` with a `jobId`
immediately; file writing and has.exe invocation happen in a single background
worker.

`jobId` is the reserved `lastOrderNumber` value (returned as a string).

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
  OrderQueue.Submit()  ──► reserve OrderNumberStore.Next()
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

### Recover from failed import

1. Find the history copy: `SendOrderDir/history/<N>/IMOVEIN_<N>.doc`.
2. Copy it to `SendOrderDir/IMOVEIN.doc` (and `.prm`).
3. Run `has.exe <paramFile>` manually from `SendOrderDir`.

---

## Phase 2 (not yet implemented)

- ODBC direct import path (Phase 2, behind adapter interface).
- Delimited-mode IMOVEIN (Masofon flexible import) as config option.
- `/api/sendOrder/status/:jobId` polling endpoint.
