package sap

import (
	"math"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func dayPtr(s string) *time.Time { t := day(s); return &t }

func f(v float64) *float64 { return &v }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestCombineDiscounts(t *testing.T) {
	cases := []struct {
		mode string
		in   []float64
		want float64
	}{
		{"L", []float64{10, 20}, 10},
		{"H", []float64{10, 20}, 20},
		{"A", []float64{10, 20}, 15},
		{"S", []float64{10, 20}, 30},
		{"S", []float64{60, 70}, 100}, // a total never passes 100
		{"M", []float64{10, 20}, 28},  // 1 - 0.9*0.8
		{"M", []float64{10, 100}, 100},
		{"", []float64{10, 20}, 10}, // SAP's default is the lowest discount
		{"X", []float64{10, 20}, 10},
		{"L", nil, 0},
		{"L", []float64{-5, 10}, -5}, // a negative group line is a surcharge, kept as SAP keeps it
	}
	for _, c := range cases {
		if got := combineDiscounts(effectiveMode(c.mode), c.in); !near(got, c.want) {
			t.Errorf("combineDiscounts(%q, %v) = %v, want %v", c.mode, c.in, got, c.want)
		}
	}
}

func baseItem() item {
	return item{Code: "A1", FirmCode: 7, GroupCode: 105, ListPrice: f(100), Currency: "ILS"}
}

func rule(level, objType, key string, pct float64) discountRule {
	return discountRule{Level: level, Active: true, ObjType: objType, ObjKey: key, DiscType: "D", Discount: pct}
}

func TestPriceItem_ListPriceOnly(t *testing.T) {
	p := priceItem(customer{DiscRel: "L"}, baseItem(), nil, nil, nil, nil, day("2026-11-25"), 1)
	if p.Final == nil || *p.Final != 100 || p.Source != sourceList || p.GroupDiscount != nil {
		t.Fatalf("got %+v", p)
	}
}

func TestPriceItem_DiscountGroupsPoolAcrossLevels(t *testing.T) {
	// Verified on a live 10.0 company: a customer with effective discount "multiplied" and both a
	// BP-specific line (10%) and an all-BP line (20%) for the item got 28% from SAP, not 10 or 20.
	rules := []discountRule{rule(levelPartner, ruleItem, "A1", 10), rule(levelAll, ruleManufacturer, "7", 20)}
	cases := []struct {
		mode      string
		wantDisc  float64
		wantFinal float64
	}{
		{"M", 28, 72},
		{"L", 10, 90},
		{"H", 20, 80},
		{"S", 30, 70},
		{"A", 15, 85},
	}
	for _, c := range cases {
		p := priceItem(customer{DiscRel: c.mode}, baseItem(), nil, nil, rules, nil, day("2026-11-25"), 1)
		if p.GroupDiscount == nil || !near(*p.GroupDiscount, c.wantDisc) || !near(*p.Final, c.wantFinal) {
			t.Errorf("mode %s: got discount %v final %v, want %v / %v", c.mode, p.GroupDiscount, p.Final, c.wantDisc, c.wantFinal)
		}
		if p.GroupLevel != levelPartner || p.RulesMatched != 2 {
			t.Errorf("mode %s: level %q rules %d", c.mode, p.GroupLevel, p.RulesMatched)
		}
		if p.PromoDiscount == nil || *p.PromoDiscount != 20 || p.ManufacturerDiscount == nil || *p.ManufacturerDiscount != 20 {
			t.Errorf("mode %s: informational discounts %v %v", c.mode, p.PromoDiscount, p.ManufacturerDiscount)
		}
	}
}

func TestPriceItem_ItemLineOverridesItsGroupsOtherLines(t *testing.T) {
	// Measured on a company copy (digi-brain erp/sap-b1/pricing.md, open-questions #8): one discount
	// group with an item line of 10% and an item-group line of 20% gave 10% under all five
	// effective-discount rules. The rule does not combine an item line with its own group's lines.
	sameGroup := func(objType, key string, pct float64) discountRule {
		r := rule(levelPartner, objType, key, pct)
		r.Group = 41
		return r
	}
	rules := []discountRule{sameGroup(ruleItemGroup, "105", 20), sameGroup(ruleItem, "A1", 10)}
	for _, mode := range []string{"L", "H", "A", "S", "M"} {
		p := priceItem(customer{DiscRel: mode}, baseItem(), nil, nil, rules, nil, day("2026-11-25"), 1)
		if p.GroupDiscount == nil || !near(*p.GroupDiscount, 10) || !near(*p.Final, 90) || p.RulesMatched != 1 {
			t.Errorf("mode %s: got discount %v final %v rules %d, want 10 / 90 / 1", mode, p.GroupDiscount, p.Final, p.RulesMatched)
		}
	}
	// The item line of another group does not silence this group's item-group line: the levels still
	// pool (TestPriceItem_DiscountGroupsPoolAcrossLevels), and so do two groups of one level.
	other := rule(levelAll, ruleItem, "A1", 10)
	other.Group = 7
	group := rule(levelPartner, ruleItemGroup, "105", 20)
	group.Group = 41
	p := priceItem(customer{DiscRel: "M"}, baseItem(), nil, nil, []discountRule{group, other}, nil, day("2026-11-25"), 1)
	if p.GroupDiscount == nil || !near(*p.GroupDiscount, 28) || p.RulesMatched != 2 {
		t.Errorf("two groups: got discount %v rules %d, want 28 / 2", p.GroupDiscount, p.RulesMatched)
	}
	// An item line for another item does not silence the group.
	notOurs := sameGroup(ruleItem, "A2", 5)
	p = priceItem(customer{DiscRel: "L"}, baseItem(), nil, nil, []discountRule{sameGroup(ruleItemGroup, "105", 20), notOurs}, nil, day("2026-11-25"), 1)
	if p.GroupDiscount == nil || !near(*p.GroupDiscount, 20) {
		t.Errorf("other item's line: got discount %v, want 20", p.GroupDiscount)
	}
}

func TestPriceItem_AllBPRulesAloneKeepV1Label(t *testing.T) {
	p := priceItem(customer{DiscRel: "L"}, baseItem(), nil, nil, []discountRule{rule(levelAll, ruleItem, "A1", 15)}, nil, day("2026-11-25"), 1)
	if p.Source != sourcePromo || !near(*p.Final, 85) {
		t.Fatalf("got %+v", p)
	}
	p = priceItem(customer{DiscRel: "M"}, baseItem(), nil, nil, []discountRule{rule(levelPartner, ruleItem, "A1", 15)}, nil, day("2026-11-25"), 1)
	if p.Source != "Discount groups (mixed)" {
		t.Fatalf("source %q", p.Source)
	}
}

func TestPriceItem_InactiveOrExpiredGroupsDoNotApply(t *testing.T) {
	inactive := rule(levelPartner, ruleItem, "A1", 30)
	inactive.Active = false // OEDG.ValidFor = 'N': the group's Active box is off (v1 applied these)
	expired := rule(levelPartner, ruleItem, "A1", 40)
	expired.To = dayPtr("2026-01-31")
	future := rule(levelPartner, ruleItem, "A1", 50)
	future.From = dayPtr("2027-01-01")
	freeGoods := rule(levelPartner, ruleItem, "A1", 60)
	freeGoods.DiscType = "P"
	p := priceItem(customer{DiscRel: "H"}, baseItem(), nil, nil, []discountRule{inactive, expired, future, freeGoods}, nil, day("2026-11-25"), 1)
	if p.GroupDiscount != nil || *p.Final != 100 || p.Source != sourceList {
		t.Fatalf("got %+v", p)
	}
	inWindow := rule(levelPartner, ruleItem, "A1", 5)
	inWindow.From, inWindow.To = dayPtr("2026-11-25"), dayPtr("2026-11-25") // both bounds inclusive
	p = priceItem(customer{DiscRel: "H"}, baseItem(), nil, nil, []discountRule{inWindow}, nil, day("2026-11-25"), 1)
	if p.GroupDiscount == nil || *p.GroupDiscount != 5 {
		t.Fatalf("in-window rule: %+v", p)
	}
}

func TestPriceItem_RuleTargets(t *testing.T) {
	it := baseItem()
	it.Props = map[int]bool{12: true}
	cases := []struct {
		name  string
		r     discountRule
		match bool
	}{
		{"item", rule(levelPartner, ruleItem, "A1", 10), true},
		{"other item", rule(levelPartner, ruleItem, "A2", 10), false},
		{"manufacturer", rule(levelPartner, ruleManufacturer, "7", 10), true},
		{"other manufacturer", rule(levelPartner, ruleManufacturer, "8", 10), false},
		{"item group", rule(levelPartner, ruleItemGroup, "105", 10), true},
		{"item group padded key", rule(levelPartner, ruleItemGroup, " 105 ", 10), true},
		{"property set", rule(levelPartner, ruleItemProperty, "12", 10), true},
		{"property not set", rule(levelPartner, ruleItemProperty, "13", 10), false},
		{"unknown target", rule(levelPartner, "99", "A1", 10), false},
	}
	for _, c := range cases {
		got := len(matchRules(it, []discountRule{c.r}, day("2026-11-25"))) == 1
		if got != c.match {
			t.Errorf("%s: matched %v, want %v", c.name, got, c.match)
		}
	}
}

func TestPriceItem_NoDiscountBox(t *testing.T) {
	p := priceItem(customer{DiscRel: "H", NoDiscount: true}, baseItem(), nil, nil, []discountRule{rule(levelPartner, ruleItem, "A1", 10)}, nil, day("2026-11-25"), 1)
	if p.GroupDiscount != nil || *p.Final != 100 {
		t.Fatalf("got %+v", p)
	}
}

func special(price, disc float64) *specialPrice {
	return &specialPrice{priceLine: priceLine{Price: price, Discount: disc, Currency: "ILS"}, Valid: true}
}

func TestPriceItem_BPSpecialPriceOverridesGroups(t *testing.T) {
	sp := special(70, 30)
	p := priceItem(customer{DiscRel: "H"}, baseItem(), sp, nil, []discountRule{rule(levelPartner, ruleItem, "A1", 50)}, nil, day("2026-11-25"), 1)
	if p.Source != sourceSpecial || *p.Final != 70 || p.GroupDiscount != nil || p.SpecialRule != "base" {
		t.Fatalf("got %+v", p)
	}
}

func TestPriceItem_BPSpecialPriceValidity(t *testing.T) {
	off := special(70, 30)
	off.Valid = false
	ended := special(70, 30)
	ended.ValidTo = dayPtr("2026-11-24")
	for _, sp := range []*specialPrice{off, ended} {
		p := priceItem(customer{DiscRel: "L"}, baseItem(), sp, nil, nil, nil, day("2026-11-25"), 1)
		if p.Source != sourceList || *p.Final != 100 {
			t.Fatalf("got %+v", p)
		}
	}
}

func TestSpecialPrice_PeriodAndVolume(t *testing.T) {
	sp := special(90, 10)
	sp.Periods = []period{
		{Line: 0, priceLine: priceLine{Price: 85, Discount: 15}, From: dayPtr("2026-01-01"), To: dayPtr("2026-06-30")},
		{Line: 1, priceLine: priceLine{Price: 80, Discount: 20}, From: dayPtr("2026-07-01"), To: nil,
			Breaks: []volumeBreak{{Amount: 10, priceLine: priceLine{Price: 75}}, {Amount: 50, priceLine: priceLine{Price: 70}}}},
	}
	cases := []struct {
		on        string
		qty       float64
		wantPrice float64
		wantRule  string
	}{
		{"2025-12-31", 1, 90, "base"},
		{"2026-03-01", 100, 85, "period"},
		{"2026-11-25", 1, 80, "period"},
		{"2026-11-25", 10, 75, "volume"},
		{"2026-11-25", 49, 75, "volume"},
		{"2026-11-25", 50, 70, "volume"},
	}
	for _, c := range cases {
		line, rule := sp.lineFor(day(c.on), c.qty)
		if line.Price != c.wantPrice || rule != c.wantRule {
			t.Errorf("%s x%v: got %v %s, want %v %s", c.on, c.qty, line.Price, rule, c.wantPrice, c.wantRule)
		}
	}
}

func TestPriceItem_ListPeriodVolumeIsTheBaseForGroups(t *testing.T) {
	list := special(80, 20) // OSPP CardCode "*<list>": the list's period-and-volume price
	p := priceItem(customer{DiscRel: "L"}, baseItem(), nil, list, nil, nil, day("2026-11-25"), 1)
	if p.Source != sourcePeriodVolume || *p.Final != 80 || p.PeriodVolume == nil {
		t.Fatalf("without groups: %+v", p)
	}
	p = priceItem(customer{DiscRel: "L"}, baseItem(), nil, list, []discountRule{rule(levelPartner, ruleItem, "A1", 10)}, nil, day("2026-11-25"), 1)
	if !near(*p.Final, 72) || p.Source != "Discount groups (lowest)" {
		t.Fatalf("with groups: %+v", p)
	}
}

func TestPriceItem_BlanketAgreementFirst(t *testing.T) {
	ba := &blanketLine{AgreementID: 9, UnitPrice: 60, Discount: 10, Currency: "USD"}
	p := priceItem(customer{DiscRel: "H"}, baseItem(), special(70, 30), nil, []discountRule{rule(levelPartner, ruleItem, "A1", 50)}, ba, day("2026-11-25"), 1)
	if p.Source != sourceBlanket || *p.Final != 54 || p.Currency != "USD" {
		t.Fatalf("got %+v", p)
	}
}

func TestPriceItem_NoListPrice(t *testing.T) {
	it := baseItem()
	it.ListPrice = nil
	if p := priceItem(customer{}, it, nil, nil, nil, nil, day("2026-11-25"), 1); p.Final != nil {
		t.Fatalf("got %+v", p)
	}
}

func TestItemStatus(t *testing.T) {
	cases := []struct {
		name string
		s    itemStatus
		want bool
	}{
		{"active", itemStatus{ValidFor: "Y", FrozenFor: "N"}, true},
		{"inactive", itemStatus{ValidFor: "N", FrozenFor: "Y"}, false},
		{"inactive later", itemStatus{ValidFor: "N", FrozenFor: "Y", FrozenFrom: dayPtr("2026-12-01")}, true},
		{"active until", itemStatus{ValidFor: "Y", FrozenFor: "N", ValidTo: dayPtr("2026-11-24")}, false},
		{"neither box", itemStatus{ValidFor: "N", FrozenFor: "N"}, true},
	}
	for _, c := range cases {
		if got := c.s.activeOn(day("2026-11-25")); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWithinIgnoresClockAndZone(t *testing.T) {
	late := time.Date(2026, 11, 25, 23, 59, 0, 0, time.FixedZone("IST", 2*3600))
	if !within(late, dayPtr("2026-11-25"), dayPtr("2026-11-25")) {
		t.Fatal("a time on the last day must be inside")
	}
	if within(late, dayPtr("2026-11-26"), nil) {
		t.Fatal("the day before the first day must be outside")
	}
}
