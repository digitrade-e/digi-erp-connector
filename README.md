# digi-erp-connector

A local HTTP API gateway that lets the Digitrade backend read and write a
customer's ERP system — Hasavshevet or SAP Business One — over MSSQL, without the
backend ever holding SQL or ERP file-format knowledge.

It installs on the customer's machine as a Windows service plus a small
configuration GUI.

```
Digitrade backend ──HTTP + Bearer token──► digi-erp-connectord ──► MSSQL / IMOVEIN files
```

**The central idea:** the backend never sends SQL. SQL statements are stored on
the connector by name and executed by name with parameters
(`GET /api/sqlqueries/{name}?sku=1001`). That is inherited from the legacy
`electron-mssql-app` connector; the rest of the architecture comes from its
predecessor `erp-connector`. See [.claude/docs/01-project-origin.md](.claude/docs/01-project-origin.md).

## What it does

- **Saved queries** — CRUD-managed SQL, executed by name with bound parameters
- **Price & stock** — one endpoint, dispatched to Hasavshevet stored procedures or
  a SAP B1 query
- **Order intake** — asynchronous Hasavshevet orders: fixed-width Windows-1255
  IMOVEIN files plus the importer run, serialised through a single worker
- **Image folders** — files served from an operator-configured allow-list, hardened
  against traversal
- **Legacy compatibility** *(off by default)* — reproduces the old Node connector's
  JWT exchange and endpoints so a backend can be migrated without downtime

## Binaries

| Binary | What it is |
|---|---|
| `digi-erp-connectord` | The service. Runs the REST API. No UI. |
| `digi-erp-connector` | Windows GUI: configuration, DB test, service control. Serves no traffic. |
| `cutover-seed` | Operator tool: provision `config.yaml`, the DB secret and `queries.json` non-interactively. |

## Documentation

Start at **[docs/README.md](docs/README.md)** — it indexes everything by task
("I need to add an endpoint", "the daemon won't start", "I'm replacing an old
connector").

The essentials:

| Doc | For |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How it fits together and why |
| [docs/api.md](docs/api.md) | Every route, body and error code |
| [docs/configuration.md](docs/configuration.md) | Every `config.yaml` key |
| [docs/operations.md](docs/operations.md) | Install, upgrade, roll back, harden |
| [docs/troubleshooting.md](docs/troubleshooting.md) | Symptom → cause → fix |
| [docs/development.md](docs/development.md) | Build, test, CI, house rules |
| [CLAUDE.md](CLAUDE.md) | The hard constraints. Read before changing code. |

## Quick start (development)

Requires Go (version pinned in `go.mod`). The GUI is Windows-only — its files are
`//go:build windows`, so `go build ./...` only succeeds on Windows.

```bash
go build ./...
go vet ./...
go test ./...

go build -trimpath -ldflags "-s -w" -o digi-erp-connectord.exe ./cmd/digi-erp-connectord
go build -trimpath -ldflags "-s -w -H=windowsgui" -o digi-erp-connector.exe ./cmd/digi-erp-connector
```

Configuration lives at `%PROGRAMDATA%\digi-erp-connector\config.yaml`
(`/etc/digi-erp-connector/` on Linux) — created by the GUI on first run.

## Release

Push to `main` → `auto-tag` creates the next patch tag → `release-windows` vets,
tests, builds both binaries, compiles the Inno Setup installer and publishes a
GitHub Release. `ci` runs the same checks on every push and pull request. A test
failure blocks the release; see [docs/development.md](docs/development.md).

## Status

Hasavshevet is complete. SAP covers price/stock. Priority is selectable in the GUI
but unimplemented. In production since 2026-08-05.
