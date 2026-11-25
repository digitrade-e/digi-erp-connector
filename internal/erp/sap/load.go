package sap

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The loaders read what priceItem and kitStock decide on. Every query is read-only, binds every
// value (sql.Named; codes stay strings: SAP item and warehouse codes can look numeric) and reads
// WITH (NOLOCK) so a price read never waits on, or blocks, SAP's own writers.

// snapshot is everything one request needs, read once.
type snapshot struct {
	customer    *customer
	items       map[string]*item
	treeType    map[string]string
	bpSpecial   map[string]*specialPrice // OSPP CardCode = the customer
	listSpecial map[string]*specialPrice // OSPP CardCode = "*<list>"
	rules       []discountRule
	blanket     map[string]*blanketLine
	kits        map[string][]kitComponent
	stock       map[string]map[string]stockRow // item -> warehouse -> row
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func load(ctx context.Context, q querier, in request) (*snapshot, error) {
	s := &snapshot{}
	var err error
	if s.customer, err = loadCustomer(ctx, q, in.cardCode); err != nil || s.customer == nil {
		return s, err
	}
	skuList, skuArgs := skuListSQL(in.skus)
	if s.items, s.treeType, err = loadItems(ctx, q, skuList, skuArgs, s.customer.ListNum); err != nil {
		return nil, err
	}
	listCard := "*" + strconv.Itoa(s.customer.ListNum)
	if s.bpSpecial, s.listSpecial, err = loadSpecialPrices(ctx, q, skuList, skuArgs, in.cardCode, listCard); err != nil {
		return nil, err
	}
	if s.rules, err = loadDiscountRules(ctx, q, s.customer); err != nil {
		return nil, err
	}
	if needsProperties(s.rules) {
		if err = loadItemProperties(ctx, q, skuList, skuArgs, s.items); err != nil {
			return nil, err
		}
	}
	if s.blanket, err = loadBlanketAgreements(ctx, q, skuList, skuArgs, in.cardCode, in.day); err != nil {
		return nil, err
	}
	if s.kits, err = loadKits(ctx, q, skuList, skuArgs); err != nil {
		return nil, err
	}
	if s.stock, err = loadStock(ctx, q, skuList, skuArgs, in.warehouses); err != nil {
		return nil, err
	}
	return s, nil
}

// skuListSQL is a derived table of the SKUs, one bound parameter each.
func skuListSQL(skus []string) (string, []any) {
	parts := make([]string, 0, len(skus))
	args := make([]any, 0, len(skus))
	for i, sku := range skus {
		name := fmt.Sprintf("sku%d", i)
		if i == 0 {
			parts = append(parts, "SELECT @"+name+" AS sku")
		} else {
			parts = append(parts, "UNION ALL SELECT @"+name)
		}
		args = append(args, sql.Named(name, sku))
	}
	return "(" + strings.Join(parts, " ") + ")", args
}

func loadCustomer(ctx context.Context, q querier, cardCode string) (*customer, error) {
	rows, err := q.QueryContext(ctx, `
SELECT TOP 1 C.CardCode, C.ListNum, C.GroupCode, C.DiscRel, C.NoDiscount
FROM OCRD AS C WITH (NOLOCK)
WHERE C.CardCode = @cardCode`, sql.Named("cardCode", cardCode))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var (
		code              string
		list, group       sql.NullInt64
		discRel, noDiscnt sql.NullString
	)
	if err := rows.Scan(&code, &list, &group, &discRel, &noDiscnt); err != nil {
		return nil, err
	}
	return &customer{CardCode: code, ListNum: int(list.Int64), GroupCode: int(group.Int64),
		DiscRel: discRel.String, NoDiscount: noDiscnt.String == "Y"}, rows.Err()
}

// loadItems reads the SKUs that exist and have a price row on the customer's list (v1 answered only
// those), with what the discount rules match on.
func loadItems(ctx context.Context, q querier, skuList string, skuArgs []any, listNum int) (map[string]*item, map[string]string, error) {
	args := append([]any{sql.Named("listNum", listNum)}, skuArgs...)
	rows, err := q.QueryContext(ctx, `
SELECT I.ItemCode, I.FirmCode, I.ItmsGrpCod, I.TreeType, I.validFor, I.validFrom, I.validTo,
       I.frozenFor, I.frozenFrom, I.frozenTo, P.Price, P.Currency
FROM OITM AS I WITH (NOLOCK)
INNER JOIN ITM1 AS P WITH (NOLOCK) ON P.ItemCode = I.ItemCode AND P.PriceList = @listNum
WHERE I.ItemCode IN (SELECT sku FROM `+skuList+` AS S)`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := map[string]*item{}
	trees := map[string]string{}
	for rows.Next() {
		var (
			code                                 string
			firm, group                          sql.NullInt64
			tree, validFor, frozenFor, currency  sql.NullString
			validFrom, validTo, frozFrom, frozTo sql.NullTime
			price                                sql.NullFloat64
		)
		if err := rows.Scan(&code, &firm, &group, &tree, &validFor, &validFrom, &validTo,
			&frozenFor, &frozFrom, &frozTo, &price, &currency); err != nil {
			return nil, nil, err
		}
		it := &item{Code: code, FirmCode: int(firm.Int64), GroupCode: int(group.Int64), Currency: currency.String,
			Status: itemStatus{ValidFor: validFor.String, FrozenFor: frozenFor.String,
				ValidFrom: timePtr(validFrom), ValidTo: timePtr(validTo), FrozenFrom: timePtr(frozFrom), FrozenTo: timePtr(frozTo)}}
		if price.Valid {
			p := price.Float64
			it.ListPrice = &p
		}
		items[code] = it
		trees[code] = tree.String
	}
	return items, trees, rows.Err()
}

// loadSpecialPrices reads the BP's special prices and the list's period-and-volume prices for the
// SKUs, with their periods (SPP1) and volume breaks (SPP2).
func loadSpecialPrices(ctx context.Context, q querier, skuList string, skuArgs []any, cardCode, listCard string) (bp, list map[string]*specialPrice, err error) {
	type spKey struct{ card, code string }
	type lineKey struct {
		spKey
		line int
	}
	args := append([]any{sql.Named("cardCode", cardCode), sql.Named("listCard", listCard)}, skuArgs...)
	all := map[spKey]*specialPrice{}

	headers, err := q.QueryContext(ctx, `
SELECT P.ItemCode, P.CardCode, P.Price, P.Discount, P.Currency, P.ListNum, P.Valid, P.ValidFrom, P.ValidTo
FROM OSPP AS P WITH (NOLOCK)
WHERE P.CardCode IN (@cardCode, @listCard)
  AND P.ItemCode IN (SELECT sku FROM `+skuList+` AS S)`, args...)
	if err != nil {
		return nil, nil, err
	}
	for headers.Next() {
		var (
			code, card         string
			price, discount    sql.NullFloat64
			currency, valid    sql.NullString
			listNum            sql.NullInt64
			validFrom, validTo sql.NullTime
		)
		if err := headers.Scan(&code, &card, &price, &discount, &currency, &listNum, &valid, &validFrom, &validTo); err != nil {
			headers.Close()
			return nil, nil, err
		}
		all[spKey{card, code}] = &specialPrice{priceLine: priceLine{Price: price.Float64, Discount: discount.Float64, Currency: currency.String},
			ListNum: int(listNum.Int64), Valid: valid.String == "Y", ValidFrom: timePtr(validFrom), ValidTo: timePtr(validTo)}
	}
	if err := closeRows(headers); err != nil {
		return nil, nil, err
	}

	if len(all) > 0 {
		periods, err := q.QueryContext(ctx, `
SELECT P.ItemCode, P.CardCode, P.LINENUM, P.Price, P.Discount, P.Currency, P.FromDate, P.ToDate
FROM SPP1 AS P WITH (NOLOCK)
WHERE P.CardCode IN (@cardCode, @listCard)
  AND P.ItemCode IN (SELECT sku FROM `+skuList+` AS S)`, args...)
		if err != nil {
			return nil, nil, err
		}
		for periods.Next() {
			var (
				code, card       string
				line             int64
				price, discount  sql.NullFloat64
				currency         sql.NullString
				fromDate, toDate sql.NullTime
			)
			if err := periods.Scan(&code, &card, &line, &price, &discount, &currency, &fromDate, &toDate); err != nil {
				periods.Close()
				return nil, nil, err
			}
			if sp := all[spKey{card, code}]; sp != nil {
				sp.Periods = append(sp.Periods, period{priceLine: priceLine{Price: price.Float64, Discount: discount.Float64, Currency: currency.String},
					Line: int(line), From: timePtr(fromDate), To: timePtr(toDate)})
			}
		}
		if err := closeRows(periods); err != nil {
			return nil, nil, err
		}

		byLine := map[lineKey]*period{}
		for k, sp := range all {
			sortPeriods(sp.Periods)
			for i := range sp.Periods {
				byLine[lineKey{k, sp.Periods[i].Line}] = &sp.Periods[i]
			}
		}
		if len(byLine) > 0 {
			breaks, err := q.QueryContext(ctx, `
SELECT V.ItemCode, V.CardCode, V.SPP1LNum, V.Amount, V.Price, V.Discount, V.Currency
FROM SPP2 AS V WITH (NOLOCK)
WHERE V.CardCode IN (@cardCode, @listCard)
  AND V.ItemCode IN (SELECT sku FROM `+skuList+` AS S)`, args...)
			if err != nil {
				return nil, nil, err
			}
			for breaks.Next() {
				var (
					code, card              string
					line                    int64
					amount, price, discount sql.NullFloat64
					currency                sql.NullString
				)
				if err := breaks.Scan(&code, &card, &line, &amount, &price, &discount, &currency); err != nil {
					breaks.Close()
					return nil, nil, err
				}
				if p := byLine[lineKey{spKey{card, code}, int(line)}]; p != nil {
					p.Breaks = append(p.Breaks, volumeBreak{Amount: amount.Float64,
						priceLine: priceLine{Price: price.Float64, Discount: discount.Float64, Currency: currency.String}})
				}
			}
			if err := closeRows(breaks); err != nil {
				return nil, nil, err
			}
		}
	}

	bp, list = map[string]*specialPrice{}, map[string]*specialPrice{}
	for k, sp := range all {
		if k.card == cardCode {
			bp[k.code] = sp
		} else {
			list[k.code] = sp
		}
	}
	return bp, list, nil
}

// loadDiscountRules reads every discount-group line of the levels this customer can match: groups
// for the BP itself, for its BP group, and for all BPs. Which lines apply to which item is decided
// in matchRules.
func loadDiscountRules(ctx context.Context, q querier, c *customer) ([]discountRule, error) {
	rows, err := q.QueryContext(ctx, `
SELECT E.Type, E.ValidFor, E.ValidForm, E.ValidTo,
       CAST(X.ObjType AS nvarchar(20)), CAST(X.ObjKey AS nvarchar(50)), X.DiscType, X.Discount
FROM OEDG AS E WITH (NOLOCK)
INNER JOIN EDG1 AS X WITH (NOLOCK) ON X.AbsEntry = E.AbsEntry
WHERE (E.Type = 'S' AND E.ObjCode = @cardCode)
   OR (E.Type = 'C' AND E.ObjCode = @groupCode)
   OR (E.Type = 'A')`,
		sql.Named("cardCode", c.CardCode), sql.Named("groupCode", strconv.Itoa(c.GroupCode)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []discountRule
	for rows.Next() {
		var (
			level, validFor, objType, objKey, discType sql.NullString
			from, to                                   sql.NullTime
			discount                                   sql.NullFloat64
		)
		if err := rows.Scan(&level, &validFor, &from, &to, &objType, &objKey, &discType, &discount); err != nil {
			return nil, err
		}
		out = append(out, discountRule{Level: level.String, Active: validFor.String == "Y", From: timePtr(from), To: timePtr(to),
			ObjType: strings.TrimSpace(objType.String), ObjKey: objKey.String, DiscType: discType.String, Discount: discount.Float64})
	}
	return out, rows.Err()
}

func needsProperties(rules []discountRule) bool {
	for _, r := range rules {
		if r.ObjType == ruleItemProperty {
			return true
		}
	}
	return false
}

// loadItemProperties fills item.Props from OITM.QryGroup1..64, unpivoted in SQL.
func loadItemProperties(ctx context.Context, q querier, skuList string, skuArgs []any, items map[string]*item) error {
	values := make([]string, 64)
	for n := 1; n <= 64; n++ {
		values[n-1] = fmt.Sprintf("(%d, I.QryGroup%d)", n, n)
	}
	rows, err := q.QueryContext(ctx, `
SELECT I.ItemCode, V.Prop
FROM OITM AS I WITH (NOLOCK)
CROSS APPLY (VALUES `+strings.Join(values, ", ")+`) AS V(Prop, Flag)
WHERE V.Flag = 'Y'
  AND I.ItemCode IN (SELECT sku FROM `+skuList+` AS S)`, skuArgs...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			code string
			prop int64
		)
		if err := rows.Scan(&code, &prop); err != nil {
			return err
		}
		if it := items[code]; it != nil {
			if it.Props == nil {
				it.Props = map[int]bool{}
			}
			it.Props[int(prop)] = true
		}
	}
	return rows.Err()
}

// Blanket agreements (OOAT/OAT1) whose agreed unit price SAP puts on a document: a specific
// agreement, approved, not cancelled, by items, in force on the day, with an open line for the item.
const (
	agreementSpecific = "S"
	agreementApproved = "A"
	agreementByItems  = "I"
)

func loadBlanketAgreements(ctx context.Context, q querier, skuList string, skuArgs []any, cardCode string, day time.Time) (map[string]*blanketLine, error) {
	args := append([]any{sql.Named("cardCode", cardCode)}, skuArgs...)
	rows, err := q.QueryContext(ctx, `
SELECT O.AbsID, O.Type, O.Status, O.Method, O.Cancelled, O.StartDate, O.EndDate, O.TermDate,
       L.ItemCode, L.UnitPrice, L.Discount, L.Currency, L.LineStatus
FROM OOAT AS O WITH (NOLOCK)
INNER JOIN OAT1 AS L WITH (NOLOCK) ON L.AgrNo = O.AbsID
WHERE O.BpCode = @cardCode
  AND L.ItemCode IN (SELECT sku FROM `+skuList+` AS S)
ORDER BY O.AbsID DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*blanketLine{}
	for rows.Next() {
		var (
			id                                       int64
			typ, status, method, cancelled, lineStat sql.NullString
			start, end, term                         sql.NullTime
			code                                     string
			price, discount                          sql.NullFloat64
			currency                                 sql.NullString
		)
		if err := rows.Scan(&id, &typ, &status, &method, &cancelled, &start, &end, &term,
			&code, &price, &discount, &currency, &lineStat); err != nil {
			return nil, err
		}
		inForce := within(day, timePtr(start), timePtr(end)) && (!term.Valid || dateOnly(day).Before(dateOnly(term.Time)))
		if typ.String != agreementSpecific || status.String != agreementApproved || method.String != agreementByItems ||
			cancelled.String == "Y" || lineStat.String == "C" || !inForce || !price.Valid {
			continue
		}
		if _, seen := out[code]; !seen {
			out[code] = &blanketLine{AgreementID: int(id), UnitPrice: price.Float64, Discount: discount.Float64, Currency: currency.String}
		}
	}
	return out, rows.Err()
}

// loadKits reads the stock-carrying components of the SKUs that are sales BOMs.
func loadKits(ctx context.Context, q querier, skuList string, skuArgs []any) (map[string][]kitComponent, error) {
	rows, err := q.QueryContext(ctx, `
SELECT H.Code, H.Qauntity, L.Code, L.Quantity
FROM OITT AS H WITH (NOLOCK)
INNER JOIN ITT1 AS L WITH (NOLOCK) ON L.Father = H.Code
INNER JOIN OITM AS C WITH (NOLOCK) ON C.ItemCode = L.Code
WHERE H.TreeType = 'S'
  AND L.Type = 4
  AND C.InvntItem = 'Y'
  AND H.Code IN (SELECT sku FROM `+skuList+` AS S)`, skuArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]kitComponent{}
	for rows.Next() {
		var (
			father, code     string
			fatherQty, perIt sql.NullFloat64
		)
		if err := rows.Scan(&father, &fatherQty, &code, &perIt); err != nil {
			return nil, err
		}
		per := perIt.Float64
		if fatherQty.Valid && fatherQty.Float64 > 0 {
			per /= fatherQty.Float64
		}
		out[father] = append(out[father], kitComponent{Code: code, PerKit: per})
	}
	return out, rows.Err()
}

// loadStock reads OITW for the SKUs and their kit components in the requested warehouses.
func loadStock(ctx context.Context, q querier, skuList string, skuArgs []any, warehouses []string) (map[string]map[string]stockRow, error) {
	whParts := make([]string, len(warehouses))
	args := append([]any{}, skuArgs...)
	for i, wh := range warehouses {
		name := fmt.Sprintf("wh%d", i)
		whParts[i] = "@" + name
		args = append(args, sql.Named(name, wh))
	}
	rows, err := q.QueryContext(ctx, `
SELECT W.ItemCode, W.WhsCode, W.OnHand, W.IsCommited, W.OnOrder
FROM OITW AS W WITH (NOLOCK)
WHERE W.WhsCode IN (`+strings.Join(whParts, ", ")+`)
  AND (W.ItemCode IN (SELECT sku FROM `+skuList+` AS S)
       OR W.ItemCode IN (SELECT L.Code
                         FROM ITT1 AS L WITH (NOLOCK)
                         INNER JOIN OITT AS H WITH (NOLOCK) ON H.Code = L.Father AND H.TreeType = 'S'
                         WHERE H.Code IN (SELECT sku FROM `+skuList+` AS S2)))`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]stockRow{}
	for rows.Next() {
		var (
			code, wh                   string
			onHand, committed, onOrder sql.NullFloat64
		)
		if err := rows.Scan(&code, &wh, &onHand, &committed, &onOrder); err != nil {
			return nil, err
		}
		if out[code] == nil {
			out[code] = map[string]stockRow{}
		}
		out[code][strings.TrimSpace(wh)] = stockRow{OnHand: onHand.Float64, Committed: committed.Float64, OnOrder: onOrder.Float64}
	}
	return out, rows.Err()
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}
