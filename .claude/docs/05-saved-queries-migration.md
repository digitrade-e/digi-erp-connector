# Migrating saved queries from electron-mssql-app

The on-disk formats are drop-in compatible (including the legacy
`"params": []` quirk — see 02-decisions.md #6).

## Method 1 — file copy (preferred)

```powershell
Copy-Item "$env:APPDATA\electron-mssql-app\custom_sql_data.json" `
          "$env:PROGRAMDATA\digi-erp-connector\queries.json"
Restart-Service digi-erp-connectord
```

Notes:
- `%APPDATA%` must belong to the user who ran the electron app.
- The daemon reads `queries.json` only at startup → restart is mandatory.
- Requires connector version with the array-params fix (v1.0.1+, commit 646689e).

## Method 2 — via API

1. Old app (port 3001): `POST /auth/token` (`digitrade`/`123456`) → JWT,
   then `GET /api/custom_sql` → `[{name, description, sql, params}, ...]`.
2. New connector: `POST /api/custom_sql` per entry, same body, new bearer
   token. Duplicates → 409, so re-running is safe.

## Validation

```
GET /api/custom_sql            → all names present
GET /api/sqlqueries/<name>     → returns rows
```

Or offline, against the file itself:

```powershell
$env:MIGRATED_QUERIES_PATH = "C:\ProgramData\digi-erp-connector\queries.json"
go test -v -run TestMigratedFileParses ./internal/queries
```

## Status of performed migrations

- **2026-07-20, dev machine (C:\Users\digi):** 25 queries migrated and
  validated (items, Categories_lvl1-3, getCustomers, getItems,
  GetCategoriesFirst/Second/ThirdLevel, getBrands, getPrice, Customers,
  getStockBalance, "Lable Recommended", ProductAttributeBrand, getCustomer,
  Collection, IndividualProductList, ProductAttributeCollection, GetDocs,
  GetDocLines, getOpenDept, getKarteset, "Document Types", getDocument).
  Note: two names contain spaces — callers must URL-encode
  (`/api/sqlqueries/Lable%20Recommended`).

## Backend cutover reminder

digi-erp-connector has NO `POST /api/sql`. The backend must execute
everything through `GET /api/sqlqueries/{name}` before a customer is
switched from erp-connector to digi-erp-connector.
