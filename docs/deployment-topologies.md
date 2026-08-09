# Deployment topologies

Most installations run one connector on one machine. This document covers the
case where they cannot: the ERP database and the Hasavshevet import folder live
on **different servers**.

## A. Single node (the normal case)

One connector, one machine, everything local.

```
backend ──HTTP──► connector (192.168.0.5)
                     ├─ MSSQL          localhost
                     └─ sendOrderDir   local folder + local has.exe/digi.bat
```

Nothing special to configure. Prefer this whenever the database and the ERP
import folder are on the same machine.

## B. Split read/write across two servers

The situation this exists for: the database is on one server, the Hasavshevet
import folder and importer are on another, and the connector cannot be installed
"for orders only" on the second one because **order building needs the
database** — `processOrderWithNumber` looks up the customer account
(`queryAccount`, mandatory) and the currency rate before it can build IMOVEIN.

```
                 ┌─ reads  ─────► READ NODE   (192.168.0.5)
backend ─────────┤                   ├─ saved queries, price & stock
                 │                   └─ MSSQL localhost, sendOrderDir empty
                 │
                 └─ orders ─────► WRITE NODE  (192.168.0.7)
                                     ├─ POST /api/sendOrder only
                                     ├─ MSSQL over the network -> 192.168.0.5:1433
                                     └─ sendOrderDir = C:\xampp\htdocs\herp  (LOCAL)
                                        + the importer that already runs there
```

The backend simply points at two base URLs.

### Why this beats a file share

The obvious alternative is to keep one connector on the database server and have
it write IMOVEIN files onto the other machine over SMB. Comparing honestly:

| | Connector on the write node | One connector writing over SMB |
|---|---|---|
| Needs from the other server's owners | install the connector | a share, plus NTFS **and** share ACLs for the *computer account* (`DOMAIN\HOST$`, because the service runs as LocalSystem), plus SMB 445 |
| Who runs the importer | the write node, locally, where the files land | unsolved — something must watch the folder |
| Network in the write path | SQL: authenticated, pooled, retried automatically | SMB file writes; a dropped share fails the order |
| `sendOrderDir` | a plain local path | a UNC path (a mapped drive will not work — services do not see drive letters) |

The share approach also puts `lastOrderNumber.json` on the far side of the
network, so the order sequence depends on the share being available.

Use the share approach only when installing a connector on the write node is
genuinely impossible.

### Configuring the write node

```yaml
erp: hasavshevet
apiListen: '[::]:8082'
bearerToken: <its own token>
db:
    driver: mssql
    host: 192.168.0.5          # the database server, not localhost
    port: 1433
    user: <login>
    database: BFL
    encrypt: true
    trustServerCertificate: true
sendOrderDir: C:\xampp\htdocs\herp     # LOCAL path on this machine
hasBatFile: C:\Hash7\digi.bat          # only if the importer runs here
```

Prerequisite on the **database** server: inbound `TCP 1433` from the write node.
SQL Server must also accept remote connections (it usually already listens on
all interfaces; a named instance additionally needs the SQL Browser service, or a
fixed port).

Give the write node its own SQL login with only what order building needs —
`SELECT` on the accounts and rates tables. It does not need the stored
procedures, DDL rights, or write access.

### Configuring the read node

Leave `sendOrderDir` empty. The connector then refuses orders explicitly:

```
POST /api/sendOrder -> 501 ORDERS_NOT_CONFIGURED
```

rather than accepting them with `202` and failing later inside the worker. Saved
queries and price/stock continue as before.

### Rules to respect

**Exactly one node may own the order sequence.** `lastOrderNumber.json` lives in
`sendOrderDir` and is guarded by a mutex *within one process*. Two connectors
writing to the same folder would hand out duplicate order numbers and overwrite
each other's `IMOVEIN.doc`. Only the write node gets a `sendOrderDir`.

**Send every order to the same node.** The single-worker queue serialises orders
within a process; it cannot serialise across machines.

**Each node gets its own bearer token.** They are separate installations.

**Only the write node needs the stored procedures**… actually it does not: the
procedures serve price/stock, which the read node handles. The GUI attempts to
install them on save and now only *warns* if it cannot — a write node with a
read-only SQL login saves fine.

## Behaviour that makes these topologies workable

Two properties matter when the database is across a network, and both are
deliberate:

**The daemon starts even if the database is unreachable.** It builds the pool
without contacting the server (`db.OpenLazy`), logs a warning, and serves.
`database/sql` connects on demand and reconnects after a failure, so a database
that is briefly unavailable — or a service that starts before the network is
ready — is a temporary condition. Database-backed endpoints answer
`503 DB_UNAVAILABLE` until it recovers.

Check `server.log` for:

```
database not reachable at startup (...) — the API will start anyway and reconnect
on demand; database-backed endpoints return 503 until it succeeds
```

**Saving configuration does not require DDL rights.** Installing the Hasavshevet
stored procedures is best-effort: if the database is unreachable or the login
lacks `CREATE`/`ALTER`, the GUI saves the configuration and shows a note instead
of refusing. Without this, a write node with a least-privilege login could not be
configured at all.

## Verifying a split deployment

From the write node:

```powershell
Test-NetConnection 192.168.0.5 -Port 1433      # database reachable?
curl.exe -s http://127.0.0.1:8082/api/health -H "Authorization: Bearer <token>"
```

`{"status":"ok"}` proves it reaches the database across the network. Then send a
real order and confirm the files appear in `sendOrderDir` and that
`GET /api/sendOrder/{jobId}` reports `done`.

From the read node, confirm orders are refused rather than silently accepted:

```powershell
curl.exe -s -X POST http://127.0.0.1:8082/api/sendOrder -H "Authorization: Bearer <token>" -d "{}"
# -> 501 ORDERS_NOT_CONFIGURED
```
