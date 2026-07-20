# digi-erp-connector

Local HTTP REST API gateway between the Digitrade backend and customer ERP systems (Hasavshevet, SAP Business One) over MSSQL.

This repo is the successor to **erp-connector** (Go) merged with the data-access model of the legacy **electron-mssql-app**: the backend does not send SQL — it registers **named saved queries** on the connector and executes them by name with parameters.

## Binaries

- `digi-erp-connectord` — daemon / Windows service running the REST API (default `127.0.0.1:8080`)
- `digi-erp-connector` — Windows GUI (config, service control)

## Quick start (development)

```bash
go build -o digi-erp-connectord ./cmd/digi-erp-connectord
go build -o digi-erp-connector.exe ./cmd/digi-erp-connector   # Windows only
go test ./...
```

Config lives at `%PROGRAMDATA%\digi-erp-connector\config.yaml` (Linux: `/etc/digi-erp-connector/config.yaml`). Saved queries live next to it in `queries.json` (drop-in compatible with electron-mssql-app's `custom_sql_data.json`).

## API overview

All routes require `Authorization: Bearer <token>`.

- `GET /api/health` — DB connectivity
- `POST /api/custom_sql`, `GET /api/custom_sql`, `GET|PATCH|DELETE /api/custom_sql/{name}` — saved-query CRUD (`POST /api/create_custom_sql` kept as legacy alias)
- `GET /api/sqlqueries/{name}?param=value` — execute a saved query
- `GET /api/folders/list`, `POST /api/file` — allow-listed image folder access
- `POST /api/sendOrder`, `GET /api/sendOrder/{jobId}` — async Hasavshevet order intake + status
- `POST /api/priceAndStockHandler` — price & stock per ERP

See `PLAN.md` for the migration design and `CLAUDE.md` for hard constraints.

## Release

Push to `main` → `auto-tag` workflow creates the next patch tag → `release-windows` builds both EXEs, compiles the Inno Setup installer, and publishes a GitHub Release.
