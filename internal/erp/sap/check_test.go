package sap

import (
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/erp"
)

func TestSAPSourceCodes(t *testing.T) {
	// REFDB RDR1.SpecPrice lists a letter and a digit for each meaning.
	cases := map[string]string{
		"Y": catSpecial, "9": catSpecial, "W": catListGroups, "7": catListGroups, "R": catList, "2": catList,
		"U": catInactive, "5": catInactive, "V": catInactiveGrp, "6": catInactiveGrp, "A": catBlanket, "!": catBlanket,
		"P": catPeriod, "0": catPeriod, "Q": catPeriodGroups, "1": catPeriodGroups, "N": catManual, "": catManual,
	}
	for code, want := range cases {
		if got := sapSource(code); got != want {
			t.Errorf("SpecPrice %q: %q, want %q", code, got, want)
		}
	}
}

func TestEngineSourceAndParts(t *testing.T) {
	item := func(src string, prices map[string]float64) erp.PriceStockItem {
		return erp.PriceStockItem{Prices: prices, Details: map[string]any{"priceSource": src}}
	}
	cases := []struct {
		it                erp.PriceStockItem
		cat               string
		base, disc, final float64
	}{
		{item(sourceList, map[string]float64{"priceListPrice": 100, "finalPrice": 100}), catList, 100, 0, 100},
		{item("Discount groups (lowest)", map[string]float64{"priceListPrice": 100, "bpGroupDiscount": 10, "finalPrice": 90}), catListGroups, 100, 10, 90},
		{item(sourcePromo, map[string]float64{"priceListPrice": 100, "bpGroupDiscount": 5, "finalPrice": 95}), catListGroups, 100, 5, 95},
		{item(sourcePeriodVolume, map[string]float64{"priceListPrice": 100, "periodVolumePrice": 80, "finalPrice": 80}), catPeriod, 80, 0, 80},
		{item("Discount groups (lowest)", map[string]float64{"priceListPrice": 100, "periodVolumePrice": 80, "bpGroupDiscount": 10, "finalPrice": 72}), catPeriodGroups, 80, 10, 72},
		{item(sourceSpecial, map[string]float64{"priceListPrice": 100, "osppPrice": 70, "osppDiscount": 30, "finalPrice": 70}), catSpecial, 100, 30, 70},
		{item(sourceBlanket, map[string]float64{"blanketAgreementPrice": 60, "finalPrice": 54}), catBlanket, 60, 10, 54},
	}
	for _, c := range cases {
		if got := engineSource(c.it); got != c.cat {
			t.Errorf("%v: category %q, want %q", c.it.Details, got, c.cat)
		}
		base, disc, final := engineParts(c.it)
		if !near(base, c.base) || !near(disc, c.disc) || !near(final, c.final) {
			t.Errorf("%v: parts %v %v %v, want %v %v %v", c.it.Details, base, disc, final, c.base, c.disc, c.final)
		}
	}
}

func TestSamePriceAllowsSAPRounding(t *testing.T) {
	if !samePrice(12.344, 12.35) || samePrice(12.30, 12.35) || !samePrice(100000.4, 100005) {
		t.Fatal("tolerance is max(0.01, 1e-4 x price)")
	}
}
