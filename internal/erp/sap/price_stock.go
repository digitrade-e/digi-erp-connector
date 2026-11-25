// Package sap answers price and stock for SAP Business One, read from its company database.
//
// v2 (engine "sap-pricing-v2") replaces v1's single CTE: the loaders read the ingredients and
// priceItem / kitStock decide, by SAP's documented rules (pricing.go). Every response key v1 sent is
// still sent with the same meaning; new information is added in new keys only.
package sap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp"
)

// Engine names the pricing logic in every item's details, so a caller can tell which rules priced it.
const Engine = "sap-pricing-v2"

// Request limits: one bound parameter per SKU and per warehouse, under SQL Server's 2100.
const (
	MaxSKUs       = 2000
	MaxWarehouses = 50
)

// ErrTooManySKUs and ErrTooManyWarehouses are request faults (the handler answers 400).
var (
	ErrTooManySKUs       = fmt.Errorf("at most %d SKUs per request", MaxSKUs)
	ErrTooManyWarehouses = fmt.Errorf("at most %d warehouses per request", MaxWarehouses)
)

type request struct {
	skus       []string
	cardCode   string
	warehouses []string
	day        time.Time
}

// FetchPriceAndStock prices req.SKUList for the business partner req.UserExtID on req.Date (today when
// empty) and reads its stock in req.Warehouses (at least one).
func FetchPriceAndStock(ctx context.Context, dbConn *sql.DB, cfg config.Config, req erp.PriceStockRequest) (erp.PriceStockResult, error) {
	_ = cfg
	if ctx == nil {
		ctx = context.Background()
	}
	if dbConn == nil {
		return erp.PriceStockResult{}, errors.New("db connection is required")
	}
	in, err := normalize(req, time.Now())
	if err != nil {
		return erp.PriceStockResult{}, err
	}
	if len(in.skus) == 0 {
		return erp.PriceStockResult{Items: []erp.PriceStockItem{}}, nil
	}
	snap, err := load(ctx, dbConn, in)
	if err != nil {
		return erp.PriceStockResult{}, err
	}
	return erp.PriceStockResult{Items: assemble(snap, in)}, nil
}

// normalize checks and cleans a request in v1's order: no SKU is an empty answer before anything is
// required; then the customer, the warehouse and the date.
func normalize(req erp.PriceStockRequest, now time.Time) (request, error) {
	in := request{skus: uniqueStrings(req.SKUList)}
	if len(in.skus) == 0 {
		return in, nil
	}
	if len(in.skus) > MaxSKUs {
		return in, ErrTooManySKUs
	}
	in.cardCode = strings.TrimSpace(req.UserExtID)
	if in.cardCode == "" {
		return in, errors.New("userExtId is required")
	}
	in.warehouses = uniqueInOrder(req.Warehouses)
	if len(in.warehouses) == 0 {
		return in, errors.New("warehouse is required")
	}
	if len(in.warehouses) > MaxWarehouses {
		return in, ErrTooManyWarehouses
	}
	in.day = dateOnly(now)
	if d := strings.TrimSpace(req.Date); d != "" {
		if len(d) > 10 {
			d = d[:10]
		}
		t, err := time.Parse("2006-01-02", d)
		if err != nil {
			return in, errors.New("date must be YYYY-MM-DD")
		}
		in.day = t
	}
	return in, nil
}

// assemble prices and stocks every SKU that exists with a price row on the customer's list, in SKU
// order. An unknown customer answers no items, as v1 did.
func assemble(s *snapshot, in request) []erp.PriceStockItem {
	items := make([]erp.PriceStockItem, 0, len(in.skus))
	if s == nil || s.customer == nil {
		return items
	}
	c := *s.customer
	for _, sku := range in.skus {
		it := s.items[sku]
		if it == nil {
			continue
		}
		p := priceItem(c, *it, s.bpSpecial[sku], s.listSpecial[sku], s.rules, s.blanket[sku], in.day, 1)

		prices := map[string]float64{}
		addPtr(prices, "priceListPrice", it.ListPrice)
		if p.Special != nil {
			prices["osppPrice"] = p.Special.Price
			prices["osppDiscount"] = p.Special.Discount
		}
		if p.PeriodVolume != nil {
			prices["periodVolumePrice"] = p.PeriodVolume.Price
		}
		if p.Blanket != nil {
			prices["blanketAgreementPrice"] = p.Blanket.UnitPrice
		}
		addPtr(prices, "bpGroupDiscount", p.GroupDiscount)
		addPtr(prices, "promoDiscount", p.PromoDiscount)
		addPtr(prices, "manufacturerDiscount", p.ManufacturerDiscount)
		addPtr(prices, "finalPrice", p.Final)

		stockBy, freeBy, first := stockFor(s, sku, in.warehouses)

		details := map[string]any{
			"cardCode":             c.CardCode,
			"priceList":            float64(c.ListNum),
			"currency":             p.Currency,
			"priceSource":          p.Source,
			"warehouseCode":        in.warehouses[0],
			"osppDiscount":         nil,
			"bpGroupDiscountType":  "",
			"oedgType":             p.GroupLevel,
			"manufacturerName":     "",
			"onOrder":              nil,
			"commited":             nil,
			"engine":               Engine,
			"discountRules":        p.RulesMatched,
			"itemActive":           it.Status.activeOn(in.day),
			"freeStockByWarehouse": freeBy,
		}
		if p.Special != nil {
			details["osppDiscount"] = p.Special.Discount
			details["specialPriceRule"] = p.SpecialRule
		}
		if p.GroupDiscount != nil {
			details["bpGroupDiscountType"] = p.GroupMode
		}
		if p.Blanket != nil {
			details["blanketAgreement"] = p.Blanket.AgreementID
		}
		if first != nil {
			details["onOrder"] = first.OnOrder
			details["commited"] = first.Committed
			details["freeStock"] = freeBy[in.warehouses[0]]
		}
		items = append(items, erp.PriceStockItem{SKU: sku, Prices: prices, StockByWarehouse: stockBy, Details: details})
	}
	return items
}

// stockFor is sku's on-hand and free stock per requested warehouse, and the first warehouse's row (for
// v1's onOrder / commited). Free stock is on hand minus committed; SAP's own "Available" also adds what
// is on order from suppliers (onOrder), so it is not called available here. A kit's stock is computed
// from its components.
func stockFor(s *snapshot, sku string, warehouses []string) (map[string]float64, map[string]float64, *stockRow) {
	onHand, available := map[string]float64{}, map[string]float64{}
	var first *stockRow
	components, isKit := s.kits[sku]
	for i, wh := range warehouses {
		var (
			row   stockRow
			avail float64
			ok    bool
		)
		if isKit && s.treeType[sku] == "S" {
			rows := map[string]stockRow{}
			for _, comp := range components {
				if r, found := s.stock[comp.Code][wh]; found {
					rows[comp.Code] = r
				}
			}
			row, avail, ok = kitStock(components, rows)
		} else {
			row, ok = s.stock[sku][wh]
			avail = row.OnHand - row.Committed
		}
		if !ok {
			continue
		}
		onHand[wh], available[wh] = row.OnHand, avail
		if i == 0 {
			r := row
			first = &r
		}
	}
	return onHand, available, first
}

func addPtr(out map[string]float64, key string, v *float64) {
	if v != nil {
		out[key] = *v
	}
}

// uniqueStrings trims, drops blanks and duplicates, and sorts (v1's SKU order).
func uniqueStrings(values []string) []string {
	out := uniqueInOrder(values)
	sort.Strings(out)
	return out
}

// uniqueInOrder trims and drops blanks and duplicates, keeping the first occurrence's order.
func uniqueInOrder(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
