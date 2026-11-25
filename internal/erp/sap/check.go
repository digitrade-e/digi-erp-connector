package sap

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp"
)

// Check compares the engine with SAP's own pricing on a client's data. It reads the sales-order lines
// users entered in the SAP client (ORDR.DataSource = 'I') over the last days, prices each (customer,
// document date) group's items through FetchPriceAndStock, and compares line by line with what SAP put
// there. SAP records where a line's price came from (RDR1.SpecPrice, "Price Source Type"); a line a user
// priced by hand (N = Manual) is counted and left out, so every compared line is SAP's engine against
// ours. Read-only. The report holds counts only — never a customer, an item or a price.
//
// A compared line can still differ when the list price changed after the document date: compare
// sources first, prices second.

// CheckOptions bounds a check.
type CheckOptions struct {
	Days      int // look-back in days (default 30)
	MaxGroups int // (customer, date) groups priced (default 300)
}

// CheckRow is one SAP price source's agreement with the engine.
type CheckRow struct {
	SAPSource     string `json:"sapSource"`
	Lines         int    `json:"lines"`
	SameSource    int    `json:"sameSource"`
	BaseEqual     int    `json:"baseEqual"`
	DiscountEqual int    `json:"discountEqual"`
	FinalEqual    int    `json:"finalEqual"`
}

// CheckReport is the whole check.
type CheckReport struct {
	Engine      string     `json:"engine"`
	From        string     `json:"from"`
	Groups      int        `json:"groups"`
	Lines       int        `json:"lines"`
	ManualLines int        `json:"manualLines"` // priced by hand in SAP (SpecPrice N): not compared
	NotPriced   int        `json:"notPriced"`   // the engine answered nothing for the item (no price row)
	BySource    []CheckRow `json:"bySource"`
}

// Price source categories, SAP's (RDR1.SpecPrice) and the engine's in the same words.
const (
	catSpecial      = "special price"
	catList         = "price list"
	catListGroups   = "price list + discount groups"
	catPeriod       = "period and volume"
	catPeriodGroups = "period and volume + discount groups"
	catBlanket      = "blanket agreement"
	catInactive     = "inactive price list"
	catInactiveGrp  = "inactive price list + discount groups"
	catManual       = "manual"
)

// sapSource maps RDR1.SpecPrice; REFDB lists a letter and a digit for each meaning.
func sapSource(code string) string {
	switch strings.TrimSpace(code) {
	case "Y", "9":
		return catSpecial
	case "W", "7":
		return catListGroups
	case "R", "2":
		return catList
	case "U", "5":
		return catInactive
	case "V", "6":
		return catInactiveGrp
	case "A", "!":
		return catBlanket
	case "P", "0":
		return catPeriod
	case "Q", "1":
		return catPeriodGroups
	}
	return catManual
}

func engineSource(it erp.PriceStockItem) string {
	src, _ := it.Details["priceSource"].(string)
	_, period := it.Prices["periodVolumePrice"]
	_, groups := it.Prices["bpGroupDiscount"]
	switch {
	case src == sourceSpecial:
		return catSpecial
	case src == sourceBlanket:
		return catBlanket
	case groups && period:
		return catPeriodGroups
	case groups:
		return catListGroups
	case period:
		return catPeriod
	}
	return catList
}

type checkLine struct {
	card, item, wh, specPrice     string
	date                          time.Time
	priceBefDi, discPrcnt, priced float64
}

// Check runs the comparison on db.
func Check(ctx context.Context, db *sql.DB, opt CheckOptions) (CheckReport, error) {
	if db == nil {
		return CheckReport{}, errors.New("db connection is required")
	}
	if opt.Days <= 0 {
		opt.Days = 30
	}
	if opt.MaxGroups <= 0 {
		opt.MaxGroups = 300
	}
	from := dateOnly(time.Now()).AddDate(0, 0, -opt.Days)
	lines, err := readCheckLines(ctx, db, from)
	if err != nil {
		return CheckReport{}, err
	}
	type groupKey struct {
		card string
		date time.Time
	}
	var order []groupKey
	groups := map[groupKey][]checkLine{}
	rep := CheckReport{Engine: Engine, From: from.Format("2006-01-02")}
	for _, l := range lines {
		if sapSource(l.specPrice) == catManual {
			rep.ManualLines++
			continue
		}
		k := groupKey{l.card, l.date}
		if _, seen := groups[k]; !seen {
			if len(order) == opt.MaxGroups {
				continue
			}
			order = append(order, k)
		}
		groups[k] = append(groups[k], l)
	}
	rep.Groups = len(order)
	rows := map[string]*CheckRow{}
	for _, k := range order {
		ls := groups[k]
		skus := make([]string, 0, len(ls))
		for _, l := range ls {
			skus = append(skus, l.item)
		}
		res, err := FetchPriceAndStock(ctx, db, config.Config{}, erp.PriceStockRequest{
			SKUList: skus, UserExtID: k.card, Warehouses: []string{ls[0].wh}, Date: k.date.Format("2006-01-02")})
		if err != nil {
			return CheckReport{}, err
		}
		byItem := map[string]erp.PriceStockItem{}
		for _, it := range res.Items {
			byItem[it.SKU] = it
		}
		for _, l := range ls {
			rep.Lines++
			it, ok := byItem[l.item]
			if !ok {
				rep.NotPriced++
				continue
			}
			sap := sapSource(l.specPrice)
			r := rows[sap]
			if r == nil {
				r = &CheckRow{SAPSource: sap}
				rows[sap] = r
			}
			r.Lines++
			if engineSource(it) == sap {
				r.SameSource++
			}
			base, disc, final := engineParts(it)
			if samePrice(base, l.priceBefDi) {
				r.BaseEqual++
			}
			if math.Abs(disc-l.discPrcnt) < 0.001 {
				r.DiscountEqual++
			}
			if samePrice(final, l.priced) {
				r.FinalEqual++
			}
		}
	}
	for _, r := range rows {
		rep.BySource = append(rep.BySource, *r)
	}
	sort.Slice(rep.BySource, func(i, j int) bool { return rep.BySource[i].Lines > rep.BySource[j].Lines })
	return rep, nil
}

// engineParts is the engine's answer as SAP shows a line: unit price before discount, discount %, and
// the price after it.
func engineParts(it erp.PriceStockItem) (base, disc, final float64) {
	final = it.Prices["finalPrice"]
	src, _ := it.Details["priceSource"].(string)
	switch src {
	case sourceSpecial:
		base, disc = it.Prices["priceListPrice"], it.Prices["osppDiscount"]
	case sourceBlanket:
		base = it.Prices["blanketAgreementPrice"]
		if base != 0 {
			disc = 100 * (1 - final/base)
		}
	default:
		base = it.Prices["priceListPrice"]
		if pv, ok := it.Prices["periodVolumePrice"]; ok {
			base = pv
		}
		disc = it.Prices["bpGroupDiscount"]
	}
	return base, disc, final
}

// samePrice allows SAP's own rounding of a stored price.
func samePrice(a, b float64) bool {
	return math.Abs(a-b) <= math.Max(0.01, 1e-4*math.Abs(b))
}

func readCheckLines(ctx context.Context, db *sql.DB, from time.Time) ([]checkLine, error) {
	rows, err := db.QueryContext(ctx, `
SELECT H.CardCode, H.DocDate, L.ItemCode, L.WhsCode, L.SpecPrice, L.PriceBefDi, L.DiscPrcnt, L.Price
FROM ORDR AS H WITH (NOLOCK)
INNER JOIN RDR1 AS L WITH (NOLOCK) ON L.DocEntry = H.DocEntry
WHERE H.DataSource = 'I'
  AND H.CANCELED = 'N'
  AND H.DocDate >= @from
  AND L.TreeType IN ('N', 'S')
  AND L.Quantity > 0
  AND ISNULL(L.WhsCode, '') <> ''
ORDER BY H.DocDate DESC, H.DocEntry, L.LineNum`, sql.Named("from", from))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []checkLine
	for rows.Next() {
		var (
			l                    checkLine
			spec                 sql.NullString
			before, disc, priced sql.NullFloat64
		)
		if err := rows.Scan(&l.card, &l.date, &l.item, &l.wh, &spec, &before, &disc, &priced); err != nil {
			return nil, err
		}
		l.date = dateOnly(l.date)
		l.specPrice = spec.String
		l.priceBefDi, l.discPrcnt, l.priced = before.Float64, disc.Float64, priced.Float64
		out = append(out, l)
	}
	return out, rows.Err()
}
