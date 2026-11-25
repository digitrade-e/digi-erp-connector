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

No stored procedures, nothing installed, read-only (`WITH (NOLOCK)`, every value
bound). Engine `sap-pricing-v2` (`internal/erp/sap`): small queries read the
ingredients (`load.go`) and plain Go decides (`pricing.go`, `stock.go`), so every
rule is unit-tested. Each item's `details.engine` names it.

**How a price is decided** — SAP Business One's documented order, checked against
the prices SAP itself put on a live 10.0 company's order lines:

1. a **specific blanket agreement** (`OOAT` Type `S`, approved, not cancelled,
   items method, in force, open line): its unit price less its line discount;
2. the business partner's **special price** (`OSPP` CardCode = the BP, `Valid` =
   `Y`, inside its dates; the `SPP1` period covering the date and, in it, the
   `SPP2` volume break for the quantity). `OSPP.Price` is already the price after
   discount. Nothing else applies on top. It counts on whichever list it was built
   from (v1 dropped a special price based on another list);
3. otherwise the BP's **price list** price (`ITM1`; SAP keeps derived lists'
   prices there already factored and rounded), replaced by the list's **period and
   volume discount** when one is valid. Those rows are stored in `OSPP` with
   CardCode `*<list number>`: verified on data, not documented;
4. on top of that base, the **discount groups** (`OEDG`/`EDG1`) of every level
   that applies — the BP (`S`), its BP group (`C`), all BPs (`A`) — **pooled** and
   combined by the BP's effective-discount rule `OCRD.DiscRel`: `L` lowest (SAP's
   default when blank), `H` highest, `A` average, `S` total (≤ 100), `M`
   multiplied. A group counts only when `ValidFor` = `Y` (REFDB: "Active"; v1 also
   applied inactive groups) and the date is inside its optional dates. Lines match
   on item (`4`), item group (`52`), manufacturer (`43`) or item property (`8`).
   `DiscType` `P` (pay for / get free) is free goods, never a price. `OCRD.NoDiscount`
   = `Y` turns groups off.

Prices are for one unit (the catalogue price). Manual line or header discounts a
user types in SAP are not an engine output. `OCRD.EffecPrice` other than `D`
(lowest / highest of all sources) is not implemented: every customer of the first SAP client
is `D`.

**Stock** comes from `OITW` for every requested warehouse (v1 read the first only):
`stockByWarehouse` is on hand, `details.freeStockByWarehouse` is on hand minus
committed (SAP's own "Available" also adds what is on order: `onOrder`). A **sales BOM** (`OITT` TreeType `S`) counts whole kits: the fewest any
inventory component makes, ITT1 quantity per kit taken into account. v1 took the
smallest component on hand: on the first SAP client it overstated 5 kits needing 2+ units of
a part. Text, resource and non-inventory lines never limit a kit.

**Wire compatibility.** Every v1 key is still sent with the same meaning
(`priceListPrice`, `osppPrice`, `osppDiscount`, `bpGroupDiscount`, `promoDiscount`,
`manufacturerDiscount`, `finalPrice`; details `cardCode`, `priceList`, `currency`,
`priceSource`, `warehouseCode`, `osppDiscount`, `bpGroupDiscountType`, `oedgType`,
`manufacturerName`, `onOrder`, `commited`). `priceSource` keeps v1's labels and adds
`Period and volume discount` and `Blanket agreement`. New keys: prices
`periodVolumePrice`, `blanketAgreementPrice`; details `engine`, `discountRules`,
`specialPriceRule`, `itemActive` (SAP refuses an inactive item on a document),
`freeStock`, `freeStockByWarehouse`, `blanketAgreement`. More than 2000 SKUs or 50
warehouses is `400 PRICE_STOCK_TOO_LARGE` (v1 hit SQL Server's 2100-parameter cap
and answered 500).

**Prove it on a client before go-live:** `go run ./cmd/sap-price-check -database
<company DB> [-user <login>]` (password in `SAP_CHECK_PASSWORD`; no `-user` =
Windows authentication). It prices the order lines users entered in the SAP client
through this engine and compares with SAP's own result line by line. SAP records
where each line's price came from (`RDR1.SpecPrice`), so lines a user priced by
hand are left out. Read-only; it prints counts only. On the first SAP client (2026-11-25)
the last 30 days gave 402 of 403 lines equal (source, base, discount and final).
Over a year it gave 97–99%, the rest being rules and prices edited since. v1 scored
the same there: that client barely uses the cases v1 got wrong.

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
