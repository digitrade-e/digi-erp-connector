# Architecture

## Components

### 1) GUI: `cmd/digi-erp-connector` (walk — native Win32)
Responsibilities:
- Let the user select ERP type (SAP or Hasavshevet).
- Configure DB connection parameters + “Test connection”.
- Configure REST API port and bearer token.
- Configure N image folders (dynamic list with folder browser).
- Hasavshevet only: configure sendOrder output folder and digi.bat path.
- Hasavshevet only: initialize required DB procedures (`GPRICE_Bulk`, `GetOnHandStockForSkus`) on save if missing.
- Write config to disk.
- Start / stop the `digi-erp-connectord` Windows Service.

The GUI is built with **walk** (`github.com/lxn/walk`) which uses Win32 GDI native controls.
No OpenGL or GPU is required; works on any Windows display including Hyper-V and RDP sessions.

Also supports a full **headless / CLI mode** (`--headless`) for environments without any display.

### 2) Daemon: `cmd/digi-erp-connectord`
Responsibilities:
- Load config from disk.
- Start HTTP server on configured port.
- Enforce auth (Bearer token).
- Route requests to handlers:
  - SQL read-only executor
  - folders/files
  - ERP handlers
  - sendOrder (Hasavshevet only)

### 3) Internal packages
- `internal/config`: config model, validation, persistence
- `internal/api`: HTTP server + routing + middleware + handlers + DTO
- `internal/auth`: token validation
- `internal/db`: DB connections, query execution, SQL validation
- `internal/files`: folder registry, file listing, secure file open/stream
- `internal/erp`: ERP-specific logic (SAP/Hasavshevet)
- `internal/platform`: autostart helpers and OS paths
- `internal/pdf`: HTML invoice template + headless-Chrome PDF generator
- `internal/print`: SumatraPDF silent-print wrapper (Windows / stub)
- `internal/email`: SMTP sender for invoice PDF attachments
- `internal/secrets`: OS-level encrypted storage (Windows DPAPI / Unix keyring)

## Data flow

1. User configures settings in UI → config persisted.
2. Daemon starts (boot/service) → loads config → starts REST server.
3. Main app calls REST endpoints with Bearer token.
4. Daemon validates token:
   - Reject unauthorized requests.
5. Daemon executes logic:
   - SQL: validate SELECT-only → bind params → query → return rows
   - Files: validate folder allow-list → list/stream
   - ERP handlers: run ERP-specific DB queries and mapping
   - sendOrder: validate → enqueue → return 202; worker writes IMOVEIN files + executes has.exe

## sendOrder queue model

`POST /api/sendOrder` enqueues the job and returns immediately.
A single background goroutine (`OrderQueue`) processes jobs serially:

```
HTTP handler → OrderQueue (chan, capacity 64) → single worker goroutine
                                                      │
                                               Sender.ProcessOrder
                                                 ├─ OrderNumberStore (mutex + JSON)
                                                 ├─ DB: Accounts query
                                                 ├─ DB: Rates query
                                                 ├─ generateDOC / generatePRM (Windows-1255)
                                                 ├─ Write IMOVEIN.doc/.prm  (SendOrderDir)
                                                 ├─ Write history copy       (SendOrderDir/history/<N>/)
                                                 ├─ exec has.exe             (Windows only)
                                                 └─ PDFPostOrderHook.AfterOrder()
                                                      ├─ Generate PDF (headless Chrome, file:// temp HTML)
                                                      ├─ Save PDF → history/<N>/invoice_<N>.pdf
                                                      ├─ [printAfterOrder] SumatraPDF silent print
                                                      └─ [emailAfterOrder] SMTP email with attachment
```

The single-worker model guarantees that `IMOVEIN.doc/.prm` are never written
or imported concurrently, preventing file collisions without explicit locking
on the file path.

## Key constraints

- Daemon must run without UI.
- SQL endpoint must be SELECT-only.
- File access must be restricted to configured folders only.
- Prefer localhost binding unless explicitly configured otherwise.
