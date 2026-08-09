# Architecture

How the connector is put together and why. For the request/response contract see
[api.md](api.md); for the data-access model see [saved-queries.md](saved-queries.md).

## Where it sits

The connector runs **on the customer's machine**, next to their ERP database. The
Digitrade backend never talks to that database directly — it talks HTTP to the
connector, which owns the SQL and the ERP-specific file formats.

```
   Digitrade backend                    customer machine
   (remote)                             ┌─────────────────────────────────────────┐
        │                               │                                         │
        │  HTTPS/HTTP + Bearer token    │  digi-erp-connectord (Windows service)   │
        └──────────────────────────────►│    ├─ saved-query runner ───────────┐    │
                                        │    ├─ image folder access           │    │
                                        │    ├─ price & stock                 ▼    │
                                        │    └─ order intake ──────────┐   MSSQL   │
                                        │                              │  (ERP DB) │
                                        │  digi-erp-connector (GUI)    │           │
                                        │    edits config, controls    ▼           │
                                        │    the service          IMOVEIN files    │
                                        │                         + has.exe        │
                                        └─────────────────────────────────────────┘
```

The backend holds no SQL and no ERP file-format knowledge. That is the point: a
customer's schema quirks stay on the customer's machine.

## Two binaries, one config

| Binary | Runs as | Does |
|---|---|---|
| `digi-erp-connectord` | Windows service (LocalSystem), or a plain process on Linux | The REST API. No UI, no console. |
| `digi-erp-connector` | Interactive desktop app, elevated | Edits config, tests the DB, installs the Hasavshevet stored procedures, starts/stops the service. Serves no traffic. |

They share `config.yaml` and the data directory but never talk to each other; the
GUI's "Restart server" goes through the Windows service manager. The split exists
because the API must survive logout and reboot, while configuration needs a
desktop session.

## Request lifecycle

Every route is wrapped in the same chain, outermost first
(`internal/api/server.go`):

```
request
  └─ Logging      method, path, status, duration — never the token or DB password
      └─ RateLimit  per-IP token bucket, 25 rps / burst 50 → 429 RATE_LIMITED
          └─ Auth     the installation's credential → 401 UNAUTHORIZED
              └─ handler
```

`POST /auth/token` is the one route outside the Auth step — it *is* the
credential check — but it keeps Logging and RateLimit. The Auth step compares the
presented credential against `bearerToken` in constant time and, if the exchange
is enabled, verifies it as an HS256 token; either match authenticates, and every
failure is the same flat 401. An installation normally configures one of the two;
`NewServer` requires at least one and warns when both are live.

Rate limiting sits **before** auth deliberately: an unauthenticated flood is
exactly what you want to shed cheaply, and it means brute-forcing the token is
also rate-limited.

Server timeouts are set on `http.Server`: 5s read-header, 10s read, 60s write,
60s idle. The write timeout is 60s because a saved query may legitimately run for
`queries.timeoutSeconds` (default 30s).

## Packages

```
cmd/
  digi-erp-connectord/   daemon: lifecycle, service integration
  digi-erp-connector/    GUI: form.go, form_config.go, actions.go, service_control.go
  cutover-seed/          operator tool: seed config/secret/queries for a new install

internal/
  api/                   HTTP server, route table, middleware chain
    handlers/            one file per endpoint group; json.go holds the shared decoder
    middleware/          auth (static token or issued token), rate limit, logging
    respond/             the single JSON error envelope
    dto/                 request/response structs per endpoint
  queries/               THE data-access model: store, binder, runner
  erp/
    hasavshevet/         order pipeline (IMOVEIN, queue, order numbers), price/stock procs
    sap/                 SAP B1 price/stock (one large CTE)
    types.go             the ERP-neutral price/stock request and result
  auth/                  HS256 sign/verify for the credential exchange, no deps
  config/                config model + atomic YAML load/save
  db/                    MSSQL DSN construction, pool, ping
  files/                 allow-list + traversal defence for image folders
  secrets/               DB password at rest (Windows DPAPI machine scope)
  logger/                file-first logger used by the daemon and GUI
  platform/
    autostart/           Windows service registration and control
    paths/               data dir and config path resolution
    atomicfile/          the only way this app writes a file
```

Two rules keep this map honest: business logic never lives in `cmd/`, and every
file write goes through `platform/atomicfile`.

## The four subsystems

**1. Saved queries** (`internal/queries`) — the only SQL entry point. SQL text is
stored on the connector by name and executed with bound parameters. Full detail in
[saved-queries.md](saved-queries.md).

**2. Image folders** (`internal/files`) — serves files from an operator-configured
allow-list. Each request is canonicalised, checked against the list, re-checked
after symlink resolution, and rejected on any traversal. See
[security.md](security.md).

**3. Order intake** (`internal/erp/hasavshevet`) — asynchronous, single-worker.
Detail in [hasavshevet-send-order.md](hasavshevet-send-order.md):

```
POST /api/sendOrder
  └─ validate → OrderNumberStore.Next() reserves the number (mutex + JSON file)
      └─ enqueue (buffered channel, capacity 64) → 202 Accepted + jobId
          └─ single worker goroutine, strictly serial:
               ├─ DB: account details + currency rate
               ├─ build IMOVEIN.doc/.prm (fixed-width 2891-byte records, Windows-1255)
               ├─ write to sendOrderDir + a history copy under history/<orderNumber>/
               └─ run digi.bat (or has.exe) to import
GET /api/sendOrder/{jobId} polls the outcome.
```

The worker **must stay single-threaded**: `IMOVEIN.doc`/`.prm` are fixed filenames
in one directory, so two concurrent orders would corrupt each other's import. The
queue is the lock.

**4. Price & stock** (`internal/erp/*`) — one endpoint, dispatched on the
configured ERP. See [price-and-stock.md](price-and-stock.md).

## State on disk

Everything lives in one directory — `%PROGRAMDATA%\digi-erp-connector\` on
Windows, `/etc/digi-erp-connector/` on Linux:

| File | Written by | Notes |
|---|---|---|
| `config.yaml` | GUI, cutover-seed | 0600; holds the bearer token in plaintext |
| `queries.json` | the CRUD endpoints | drop-in compatible with electron-mssql-app |
| `secrets/db_password_<erp>.bin` | GUI, cutover-seed | DPAPI machine scope |
| `server.log`, `ui.log` | daemon, GUI | no secrets, ever |

`lastOrderNumber.json` is the exception: it lives in `sendOrderDir`, beside the
IMOVEIN files, so an order-number sequence travels with the files it numbers.

All four are rewritten while the service is live, so all four go through
`atomicfile.Write` (temp file → fsync → rename). A half-written `config.yaml`
would stop the daemon from starting at all.

## Concurrency

- **Order queue** — one worker, by design (above).
- **Query store** — `sync.RWMutex`; reads are concurrent, mutations serialise and
  persist while holding the lock.
- **Order numbers** — mutex plus a JSON file; reserved in the HTTP handler, not
  the worker, so a caller gets its number in the 202 response.
- **Rate limiter** — per-IP buckets, capped at 4096 entries with idle eviction
  after 10 minutes, so a scan of spoofed source IPs cannot grow it without bound.
- **DB pool** — 10 open / 10 idle, 30-minute connection lifetime.

## Failure behaviour

- **No database at startup → the daemon starts anyway.** It builds the pool
  without contacting the server (`db.OpenLazy`), logs a warning, and serves;
  `database/sql` connects on demand and reconnects after a failure. Only a bad
  configuration (missing host, invalid port, unknown driver) is fatal. This is
  what makes a database on another host workable — see
  [deployment-topologies.md](deployment-topologies.md) — and it removes a whole
  class of "the service won't stay started" incidents. The service dependency on
  SQL Server plus restart-on-failure (see [operations.md](operations.md)) is
  still worth configuring, but it is no longer load-bearing.
- **Database lost while running** → the affected request fails closed:
  `ErrNoDatabase` → `503 DB_UNAVAILABLE`. It never panics on a nil handle.
- **Driver errors are never returned to callers** — they are logged, and the
  caller gets a generic code. A DB error message can name schemas, users and
  hosts.
- **Order hook errors never fail an order.** `PostOrderHook` exists in the queue
  design with no implementations registered.

## Deliberately absent

| Not here | Why |
|---|---|
| A raw-SQL endpoint | Replaced by saved queries. The electron-compatibility `POST /api/query` was deleted on 2026-08-09 along with the rest of that layer. |
| PDF generation, printing, SMTP email | Removed 2026-07-20. Recoverable from git history; read the old repo's `docs/printing.md` first — the print path had session-0 and WSD-port constraints that are not obvious. |
| Priority ERP | Selectable in the GUI, not implemented. |
| A `--headless` GUI mode | Never existed, despite an earlier version of this document claiming otherwise. Configure headless installs with `cmd/cutover-seed` or by editing `config.yaml`. |
