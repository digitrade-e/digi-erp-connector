# Price & stock

`POST /api/priceAndStockHandler` answers "what does this customer pay for these
SKUs, and how many are in stock" for whichever ERP the machine is configured for.
It is the one endpoint where the connector contains real ERP business logic rather
than running operator-supplied SQL.

Dispatch is on `erp` in the config:

| `erp` | Implementation |
|---|---|
| `hasavshevet` | Two stored procedures the connector installs itself |
| `sap` | One large read-only CTE query against SAP B1 tables |
| `priority` | Not implemented → `501 NOT_IMPLEMENTED` |

Timeout: 12 seconds. Body limit: 1 MiB. Errors: `400 ERP_NOT_SUPPORTED` for an
unknown ERP, `500 PRICE_STOCK_FAILED` if the ERP call fails,
`503 DB_UNAVAILABLE` with no database.

## Request and response

Request carries the customer, the SKUs, and optionally which warehouses to count:

```json
{
  "accountKey": "1234",
  "skuList": ["1001", "1002"],
  "warehouses": ["10", "20"],
  "dateString": "2026-08-05"
}
```

The response pairs each SKU with its computed price and stock. Fields not
applicable to an ERP are omitted rather than faked — the shape is defined by
`internal/erp/types.go` (`PriceStockRequest`, `PriceStockItem`, `PriceStockResult`)
and the DTOs in `internal/api/dto/price_stock.go`, which are the authority if this
document and the code disagree.

Note `sku`, `warehouse` and `syncKey` are always treated as **strings** throughout
the connector, including here: SAP warehouse codes and many SKUs look numeric but
are not.

## Hasavshevet

Pricing in Hasavshevet lives in a native routine (`GPRICE`) that applies the
customer's price list, agreements and quantity breaks. The connector does not
reimplement it — it wraps it:

| Procedure | Purpose |
|---|---|
| `dbo.GPRICE_Bulk` / `dbo.GPRICE_BulkJson` | Wraps the native `GPRICE` so prices for many SKUs can be fetched in one round trip instead of one call per SKU |
| `dbo.GetOnHandStockForSkus` | On-hand quantities for a set of SKUs, optionally restricted to a warehouse |

**The connector installs both** with `CREATE OR ALTER` when you save configuration
in the GUI with `erp: hasavshevet` (`EnsureGPriceBulkProcedure`,
`EnsureOnHandStockForSkusProcedure`). That is why the configured DB user needs
`CREATE`/`ALTER` rights at least once; you can also pre-create them and then revoke
the right, but they will not be updated on upgrade if you do.

Both are read-only. They are also callable as saved queries — the production box
does exactly that with `getPrice` and `getStockBalance`, which `EXEC` them by name
instead of going through this endpoint.

## SAP Business One

One query, no stored procedures, nothing installed. It is a single large CTE that
resolves, in order:

- the customer's assigned price list, walking price-list inheritance
- special prices per business partner (`OSPP`) and per discount group
- quantity/volume discounts
- BOM component pricing where an item is assembled
- on-hand stock from `OITW` per warehouse

It is read-only and lives in `internal/erp/sap/price_stock.go`. It is the largest
single piece of SQL in the repo and, honestly, the least tested code here — see
[development.md](development.md#coverage-honestly). Treat changes to it with
care and verify against a real SAP database.

## Priority

`internal/erp/sap` declares `ErrNotImplemented` for a future Priority
implementation, and `priority` is selectable in the GUI. Selecting it gives a
`501` from this endpoint. Nothing else about Priority exists.

## Choosing this endpoint or a saved query

Both are legitimate:

- **This endpoint** when you want the connector to own the pricing logic — the
  backend sends SKUs and gets prices, and the ERP-specific detail stays here.
- **A saved query** when the customer needs something bespoke, or when the backend
  wants to control the SQL. The production box uses saved queries (`getPrice`,
  `getStockBalance`) that `EXEC` the same procedures.

The saved-query route means new customer-specific requirements do not need a
connector release, which is usually the deciding factor.
