package sap

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/erp"
)

func TestNormalize(t *testing.T) {
	now := time.Date(2026, 11, 25, 22, 30, 0, 0, time.Local)
	in, err := normalize(erp.PriceStockRequest{SKUList: []string{" B ", "A", "B", ""}, UserExtID: " C1 ", Warehouses: []string{"", "10", "02", "10"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(in.skus, ",") != "A,B" || in.cardCode != "C1" || strings.Join(in.warehouses, ",") != "10,02" {
		t.Fatalf("got %+v", in)
	}
	if !in.day.Equal(day("2026-11-25")) {
		t.Fatalf("default day %v", in.day)
	}
	in, err = normalize(erp.PriceStockRequest{SKUList: []string{"A"}, UserExtID: "C1", Warehouses: []string{"10"}, Date: "2026-03-08T00:00:00.000Z"}, now)
	if err != nil || !in.day.Equal(day("2026-03-08")) {
		t.Fatalf("date with a clock: %v %v", in.day, err)
	}
}

func TestNormalize_Refusals(t *testing.T) {
	many := make([]string, MaxSKUs+1)
	for i := range many {
		many[i] = fmt.Sprintf("S%d", i)
	}
	whs := make([]string, MaxWarehouses+1)
	for i := range whs {
		whs[i] = fmt.Sprintf("W%d", i)
	}
	cases := []struct {
		name string
		req  erp.PriceStockRequest
		want error
		text string
	}{
		{"too many skus", erp.PriceStockRequest{SKUList: many, UserExtID: "C", Warehouses: []string{"1"}}, ErrTooManySKUs, ""},
		{"too many warehouses", erp.PriceStockRequest{SKUList: []string{"A"}, UserExtID: "C", Warehouses: whs}, ErrTooManyWarehouses, ""},
		{"no customer", erp.PriceStockRequest{SKUList: []string{"A"}, Warehouses: []string{"1"}}, nil, "userExtId is required"},
		{"no warehouse", erp.PriceStockRequest{SKUList: []string{"A"}, UserExtID: "C"}, nil, "warehouse is required"},
		{"bad date", erp.PriceStockRequest{SKUList: []string{"A"}, UserExtID: "C", Warehouses: []string{"1"}, Date: "25/11/2026"}, nil, "YYYY-MM-DD"},
	}
	for _, c := range cases {
		_, err := normalize(c.req, time.Now())
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) || (c.text != "" && !strings.Contains(err.Error(), c.text)) {
			t.Errorf("%s: got %v", c.name, err)
		}
	}
	// No SKU is an empty answer before anything else is required (v1's order).
	if in, err := normalize(erp.PriceStockRequest{}, time.Now()); err != nil || len(in.skus) != 0 {
		t.Fatalf("empty request: %v", err)
	}
}

func TestSkuListSQLBindsEverySKU(t *testing.T) {
	text, args := skuListSQL([]string{"001", "A'B"})
	if text != "(SELECT @sku0 AS sku UNION ALL SELECT @sku1)" || len(args) != 2 {
		t.Fatalf("got %q %d", text, len(args))
	}
	if a, ok := args[1].(sql.NamedArg); !ok || a.Name != "sku1" || a.Value != "A'B" {
		t.Fatalf("SKUs must bind as strings: %#v", args[1])
	}
	if strings.Contains(text, "A'B") || strings.Contains(text, "001") {
		t.Fatal("a SKU value reached the SQL text")
	}
}

func testSnapshot() *snapshot {
	return &snapshot{
		customer: &customer{CardCode: "C1", ListNum: 5, DiscRel: "M"},
		items: map[string]*item{
			"A1":  {Code: "A1", FirmCode: 7, ListPrice: f(100), Currency: "ILS", Status: itemStatus{ValidFor: "Y", FrozenFor: "N"}},
			"KIT": {Code: "KIT", ListPrice: f(300), Currency: "ILS", Status: itemStatus{ValidFor: "N", FrozenFor: "Y"}},
		},
		treeType: map[string]string{"A1": "N", "KIT": "S"},
		rules:    []discountRule{rule(levelPartner, ruleItem, "A1", 10), rule(levelAll, ruleManufacturer, "7", 20)},
		kits:     map[string][]kitComponent{"KIT": {{Code: "A1", PerKit: 2}}},
		stock: map[string]map[string]stockRow{
			"A1": {"10": {OnHand: 9, Committed: 1, OnOrder: 5}, "02": {OnHand: 4}},
		},
	}
}

func TestAssemble(t *testing.T) {
	in := request{skus: []string{"A1", "KIT", "NOPE"}, cardCode: "C1", warehouses: []string{"10", "02", "99"}, day: day("2026-11-25")}
	items := assemble(testSnapshot(), in)
	if len(items) != 2 || items[0].SKU != "A1" || items[1].SKU != "KIT" {
		t.Fatalf("items %+v", items)
	}
	a := items[0]
	if !near(a.Prices["finalPrice"], 72) || a.Prices["priceListPrice"] != 100 || !near(a.Prices["bpGroupDiscount"], 28) {
		t.Fatalf("prices %+v", a.Prices)
	}
	// Every key v1 sent is still there.
	for _, k := range []string{"cardCode", "priceList", "currency", "priceSource", "warehouseCode", "osppDiscount",
		"bpGroupDiscountType", "oedgType", "manufacturerName", "onOrder", "commited"} {
		if _, ok := a.Details[k]; !ok {
			t.Errorf("details lost v1 key %q", k)
		}
	}
	if a.Details["priceList"] != float64(5) || a.Details["warehouseCode"] != "10" || a.Details["bpGroupDiscountType"] != "M" ||
		a.Details["oedgType"] != "S" || a.Details["engine"] != Engine || a.Details["itemActive"] != true {
		t.Fatalf("details %+v", a.Details)
	}
	if a.StockByWarehouse["10"] != 9 || a.StockByWarehouse["02"] != 4 || len(a.StockByWarehouse) != 2 {
		t.Fatalf("stock %+v", a.StockByWarehouse)
	}
	if a.Details["onOrder"] != float64(5) || a.Details["commited"] != float64(1) || a.Details["freeStock"] != float64(8) {
		t.Fatalf("first warehouse details %+v", a.Details)
	}
	k := items[1]
	if k.StockByWarehouse["10"] != 4 || k.StockByWarehouse["02"] != 2 || k.Details["itemActive"] != false {
		t.Fatalf("kit %+v %+v", k.StockByWarehouse, k.Details)
	}
	if avail := k.Details["freeStockByWarehouse"].(map[string]float64); avail["10"] != 4 {
		t.Fatalf("kit free stock %+v", avail)
	}
}

func TestAssemble_UnknownCustomerAnswersNoItems(t *testing.T) {
	s := testSnapshot()
	s.customer = nil
	if items := assemble(s, request{skus: []string{"A1"}, warehouses: []string{"10"}}); len(items) != 0 {
		t.Fatalf("got %+v", items)
	}
}
