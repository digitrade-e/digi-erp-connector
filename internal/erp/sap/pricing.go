package sap

// Price determination, decided in Go from the rows the loaders read (load.go), so every rule is unit
// tested. The order is SAP Business One's documented one (SAP learning, "Managing Discounts and
// Special Prices in SAP Business One"), checked on a live 10.0 company against the prices SAP itself
// put on UI-entered sales-order lines (docs/price-and-stock.md § SAP Business One):
//
//  1. a specific blanket agreement's agreed unit price;
//  2. the business partner's special price (OSPP CardCode = the BP), or the line of its period
//     (SPP1) / volume (SPP2) table that covers the date and quantity: it overrides everything else;
//  3. otherwise the BP's price list price (ITM1), replaced by that list's period-and-volume price
//     (OSPP CardCode = "*<list>") when one is valid, and on top of it
//  4. the discount groups (OEDG/EDG1) of every level that applies — the BP itself (S), its BP group
//     (C) and all BPs (A) — pooled and combined by the BP's effective-discount rule (OCRD.DiscRel).
//     Inside one group an item line overrides the group's other lines (measured on a company copy:
//     an item line of 10% beside an item-group line of 20% in one group gave 10% under every rule).
//
// Manual line and header discounts a user types are not an engine output and are never guessed.

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// priceSource labels. The first five are the values the v1 query returned (wire compatible); the
// others are new sources v1 did not handle.
const (
	sourceSpecial      = "OSPP explicit price"
	sourcePromo        = "Promo (EDG Type A)"
	sourceGroups       = "Discount groups"
	sourceList         = "Base price list"
	sourcePeriodVolume = "Period and volume discount"
	sourceBlanket      = "Blanket agreement"
)

// Effective-discount rules (OCRD.DiscRel). SAP's default is the lowest discount.
const (
	discLowest     = "L"
	discHighest    = "H"
	discAverage    = "A"
	discTotal      = "S"
	discMultiplied = "M"
)

// EDG1.ObjType: what a discount-group line points at.
const (
	ruleItem         = "4"
	ruleItemProperty = "8"
	ruleManufacturer = "43"
	ruleItemGroup    = "52"
)

// Discount-group levels (OEDG.Type) a customer can match.
const (
	levelPartner = "S"
	levelGroup   = "C"
	levelAll     = "A"
)

type customer struct {
	CardCode   string
	ListNum    int
	GroupCode  int
	DiscRel    string // OCRD.DiscRel
	NoDiscount bool   // OCRD.NoDiscount = 'Y': "do not apply discount groups"
}

type item struct {
	Code      string
	FirmCode  int          // OITM.FirmCode (manufacturer)
	GroupCode int          // OITM.ItmsGrpCod
	Props     map[int]bool // OITM.QryGroupN = 'Y'; read only when a property rule exists
	ListPrice *float64     // ITM1.Price for the customer's list
	Currency  string
	Status    itemStatus
}

// priceLine is one price row: an OSPP header, an SPP1 period or an SPP2 volume line. Price is the
// price after Discount (OSPP.Price = base list price x (1 - Discount/100)).
type priceLine struct {
	Price    float64
	Discount float64
	Currency string
}

type specialPrice struct {
	priceLine
	ListNum   int
	Valid     bool // OSPP.Valid = 'Y'
	ValidFrom *time.Time
	ValidTo   *time.Time
	Periods   []period // SPP1, ordered by line number
}

type period struct {
	priceLine
	Line   int
	From   *time.Time
	To     *time.Time
	Breaks []volumeBreak // SPP2
}

// volumeBreak applies from Amount units up.
type volumeBreak struct {
	priceLine
	Amount float64
}

type discountRule struct {
	Group    int    // OEDG.AbsEntry: the discount group the line belongs to
	Level    string // OEDG.Type
	Active   bool   // OEDG.ValidFor = 'Y': the group's Active box; the dates are optional limits
	From, To *time.Time
	ObjType  string
	ObjKey   string
	DiscType string // EDG1.DiscType: D is a percentage; any other is free goods, not a price
	Discount float64
}

type blanketLine struct {
	AgreementID int
	UnitPrice   float64
	Discount    float64
	Currency    string
}

// priced is one item's decision, with every ingredient the response reports.
type priced struct {
	Final                *float64
	Currency             string
	Source               string
	Special              *priceLine // the BP special-price line that decided
	SpecialRule          string     // base | period | volume
	PeriodVolume         *priceLine // the list's "*<list>" line used as the base
	GroupDiscount        *float64   // combined discount-group percentage
	GroupMode            string     // the effective-discount rule used
	GroupLevel           string     // the most specific level among the matched rules
	RulesMatched         int
	PromoDiscount        *float64 // the all-BP (A) rules alone: informational
	ManufacturerDiscount *float64 // the manufacturer rules alone: informational
	Blanket              *blanketLine
}

// priceItem decides one item's price for customer c on day for qty units.
func priceItem(c customer, it item, bp, list *specialPrice, rules []discountRule, ba *blanketLine, day time.Time, qty float64) priced {
	out := priced{Currency: it.Currency}
	if ba != nil {
		final := round4(ba.UnitPrice * (100 - ba.Discount) / 100)
		out.Final, out.Source, out.Blanket = &final, sourceBlanket, ba
		if ba.Currency != "" {
			out.Currency = ba.Currency
		}
		return out
	}
	if bp != nil && bp.validOn(day) {
		line, rule := bp.lineFor(day, qty)
		final := round4(line.Price)
		out.Final, out.Special, out.SpecialRule, out.Source = &final, &line, rule, sourceSpecial
		if line.Currency != "" {
			out.Currency = line.Currency
		}
		return out
	}
	if it.ListPrice == nil {
		return out
	}
	base := *it.ListPrice
	out.Source = sourceList
	if list != nil && list.validOn(day) {
		line, _ := list.lineFor(day, qty)
		base = line.Price
		out.PeriodVolume, out.Source = &line, sourcePeriodVolume
		if line.Currency != "" {
			out.Currency = line.Currency
		}
	}
	matched := matchRules(it, rules, day)
	out.PromoDiscount = maxDiscount(matched, func(r discountRule) bool { return r.Level == levelAll })
	out.ManufacturerDiscount = maxDiscount(matched, func(r discountRule) bool { return r.ObjType == ruleManufacturer })
	if c.NoDiscount || len(matched) == 0 {
		final := round4(base)
		out.Final = &final
		return out
	}
	mode := effectiveMode(c.DiscRel)
	pcts := make([]float64, len(matched))
	for i, r := range matched {
		pcts[i] = r.Discount
	}
	d := round4(combineDiscounts(mode, pcts))
	final := round4(base * (100 - d) / 100)
	out.Final, out.GroupDiscount, out.GroupMode = &final, &d, mode
	out.GroupLevel, out.RulesMatched = mostSpecificLevel(matched), len(matched)
	// The discount is the last step, so it names the source; a period-and-volume base is still
	// reported in its own key (periodVolumePrice).
	out.Source = groupSource(mode, matched)
	return out
}

func (s *specialPrice) validOn(day time.Time) bool {
	return s.Valid && within(day, s.ValidFrom, s.ValidTo)
}

// lineFor is the special price's line for day and qty: the first period covering day, and in it the
// volume break with the highest amount not above qty; else the header.
func (s *specialPrice) lineFor(day time.Time, qty float64) (priceLine, string) {
	for _, p := range s.Periods {
		if !within(day, p.From, p.To) {
			continue
		}
		best := -1
		for i, b := range p.Breaks {
			if b.Amount <= qty && (best < 0 || b.Amount > p.Breaks[best].Amount) {
				best = i
			}
		}
		if best >= 0 {
			return p.Breaks[best].priceLine, "volume"
		}
		return p.priceLine, "period"
	}
	return s.priceLine, "base"
}

// matchRules are the discount-group lines that apply to it on day. The loader already kept only the
// groups of this customer's levels.
func matchRules(it item, rules []discountRule, day time.Time) []discountRule {
	var out []discountRule
	for _, r := range rules {
		if !r.Active || r.DiscType != "D" || !within(day, r.From, r.To) {
			continue
		}
		key := strings.TrimSpace(r.ObjKey)
		switch r.ObjType {
		case ruleItem:
			if key != it.Code {
				continue
			}
		case ruleManufacturer:
			if key != strconv.Itoa(it.FirmCode) {
				continue
			}
		case ruleItemGroup:
			if key != strconv.Itoa(it.GroupCode) {
				continue
			}
		case ruleItemProperty:
			n, err := strconv.Atoi(key)
			if err != nil || !it.Props[n] {
				continue
			}
		default:
			continue
		}
		out = append(out, r)
	}
	return itemLinesOverrideTheirGroup(out)
}

// itemLinesOverrideTheirGroup keeps, in every discount group that has a matched item line, only its
// item lines: SAP does not combine an item line with the item-group, manufacturer or property lines
// of its own group. Lines of different groups, and so of different levels, still pool.
func itemLinesOverrideTheirGroup(matched []discountRule) []discountRule {
	type groupKey struct {
		level string
		group int
	}
	hasItem := map[groupKey]bool{}
	for _, r := range matched {
		if r.ObjType == ruleItem {
			hasItem[groupKey{r.Level, r.Group}] = true
		}
	}
	if len(hasItem) == 0 {
		return matched
	}
	out := matched[:0:0]
	for _, r := range matched {
		if r.ObjType == ruleItem || !hasItem[groupKey{r.Level, r.Group}] {
			out = append(out, r)
		}
	}
	return out
}

// combineDiscounts applies an effective-discount rule to the matched percentages.
func combineDiscounts(mode string, pcts []float64) float64 {
	if len(pcts) == 0 {
		return 0
	}
	switch mode {
	case discHighest:
		m := pcts[0]
		for _, p := range pcts[1:] {
			m = math.Max(m, p)
		}
		return m
	case discAverage:
		sum := 0.0
		for _, p := range pcts {
			sum += p
		}
		return sum / float64(len(pcts))
	case discTotal:
		sum := 0.0
		for _, p := range pcts {
			sum += p
		}
		return math.Min(sum, 100)
	case discMultiplied:
		keep := 1.0
		for _, p := range pcts {
			if p >= 100 {
				return 100
			}
			keep *= (100 - p) / 100
		}
		return 100 * (1 - keep)
	default:
		m := pcts[0]
		for _, p := range pcts[1:] {
			m = math.Min(m, p)
		}
		return m
	}
}

func effectiveMode(discRel string) string {
	switch m := strings.ToUpper(strings.TrimSpace(discRel)); m {
	case discHighest, discAverage, discTotal, discMultiplied, discLowest:
		return m
	}
	return discLowest
}

func groupSource(mode string, matched []discountRule) string {
	allA := true
	for _, r := range matched {
		if r.Level != levelAll {
			allA = false
			break
		}
	}
	if allA {
		return sourcePromo
	}
	name := map[string]string{discHighest: "highest", discLowest: "lowest", discAverage: "average", discTotal: "sum", discMultiplied: "mixed"}[mode]
	return sourceGroups + " (" + name + ")"
}

func mostSpecificLevel(matched []discountRule) string {
	rank := map[string]int{levelPartner: 3, levelGroup: 2, levelAll: 1}
	best := ""
	for _, r := range matched {
		if rank[r.Level] > rank[best] {
			best = r.Level
		}
	}
	return best
}

func maxDiscount(rules []discountRule, keep func(discountRule) bool) *float64 {
	var out *float64
	for _, r := range rules {
		if !keep(r) {
			continue
		}
		if out == nil || r.Discount > *out {
			d := r.Discount
			out = &d
		}
	}
	return out
}

// itemStatus is OITM's active/inactive state. SAP refuses an inactive item on a document, so the
// response says whether the item is active on the day; it does not change the price.
type itemStatus struct {
	ValidFor, FrozenFor  string
	ValidFrom, ValidTo   *time.Time
	FrozenFrom, FrozenTo *time.Time
}

func (s itemStatus) activeOn(day time.Time) bool {
	if s.FrozenFor == "Y" && within(day, s.FrozenFrom, s.FrozenTo) {
		return false
	}
	if s.ValidFor == "Y" && !within(day, s.ValidFrom, s.ValidTo) {
		return false
	}
	return true
}

// within: from <= day <= to by calendar date; a missing bound is open.
func within(day time.Time, from, to *time.Time) bool {
	d := dateOnly(day)
	if from != nil && d.Before(dateOnly(*from)) {
		return false
	}
	if to != nil && d.After(dateOnly(*to)) {
		return false
	}
	return true
}

// dateOnly keeps the calendar date and drops the clock and zone: SAP stores dates as midnight with no
// zone, and the driver hands them back as UTC.
func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

func sortPeriods(ps []period) {
	sort.Slice(ps, func(i, j int) bool { return ps[i].Line < ps[j].Line })
}
