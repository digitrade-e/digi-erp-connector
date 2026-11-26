package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/sap"
	"github.com/digitrade-e/digi-erp-connector/internal/queries"
)

// engineMux serves the saved-query route with a runner whose database is never dialled: an engine
// query must not reach it.
func engineMux(t *testing.T, sqlText string, params map[string]any, maxRows int, engines map[string]SavedQueryEngine) *http.ServeMux {
	t.Helper()
	store, err := queries.NewStore(filepath.Join(t.TempDir(), "queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create("GetPricesAndStock", queries.Definition{SQL: sqlText, Params: params}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlserver", "sqlserver://127.0.0.1:1?database=none")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mux := http.NewServeMux()
	mux.Handle("GET /api/sqlqueries/{name}", NewRunSavedQueryHandler(store, queries.NewRunner(db, time.Second, maxRows), engines))
	return mux
}

type engineAnswer struct {
	Status     string             `json:"status"`
	Name       string             `json:"name"`
	RowCount   int                `json:"rowCount"`
	Rows       []map[string]any   `json:"rows"`
	Value      []map[string]any   `json:"value"`
	Recordsets [][]map[string]any `json:"recordsets"`
	Code       string             `json:"code"`
}

func get(t *testing.T, mux *http.ServeMux, path string) (int, engineAnswer) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var a engineAnswer
	_ = json.Unmarshal(rec.Body.Bytes(), &a)
	return rec.Code, a
}

func TestSavedQueryEngine_AnswersWithTheSavedQueryResponse(t *testing.T) {
	var got map[string]any
	engines := map[string]SavedQueryEngine{"sap-price-stock": func(_ context.Context, p map[string]any) ([]map[string]any, error) {
		got = p
		return []map[string]any{{"sku": "A"}, {"sku": "B"}}, nil
	}}
	mux := engineMux(t, "-- digi-engine: sap-price-stock", map[string]any{"warehouse": "01", "priceList": 4}, 100, engines)
	code, a := get(t, mux, "/api/sqlqueries/GetPricesAndStock?skuArray=A,B&warehouse=10&syncKey=21122")
	if code != http.StatusOK || a.Status != "success" || a.Name != "GetPricesAndStock" || a.RowCount != 2 ||
		len(a.Rows) != 2 || len(a.Value) != 2 || len(a.Recordsets) != 1 || len(a.Recordsets[0]) != 2 {
		t.Fatalf("got %d %+v", code, a)
	}
	// The stored defaults merge with the URL (URL wins), and forced-string names stay strings.
	want := map[string]any{"skuArray": "A,B", "warehouse": "10", "syncKey": "21122", "priceList": int64(4)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("params %v, want %v", got, want)
	}
}

func TestSavedQueryEngine_Refusals(t *testing.T) {
	failing := func(err error) map[string]SavedQueryEngine {
		return map[string]SavedQueryEngine{"sap-price-stock": func(context.Context, map[string]any) ([]map[string]any, error) { return nil, err }}
	}
	three := map[string]SavedQueryEngine{"sap-price-stock": func(context.Context, map[string]any) ([]map[string]any, error) {
		return []map[string]any{{}, {}, {}}, nil
	}}
	cases := []struct {
		name    string
		text    string
		engines map[string]SavedQueryEngine
		code    int
		errCode string
	}{
		{"engine missing on this connector", "-- digi-engine: sap-price-stock", nil, 400, "ENGINE_NOT_SUPPORTED"},
		{"unknown engine", "-- digi-engine: nope", three, 400, "ENGINE_NOT_SUPPORTED"},
		{"too many skus", "-- digi-engine: sap-price-stock", failing(sap.ErrTooManySKUs), 400, "PRICE_STOCK_TOO_LARGE"},
		{"bad request", "-- digi-engine: sap-price-stock", failing(fmt.Errorf("%w: warehouse is required", sap.ErrInvalidRequest)), 400, "ENGINE_BAD_REQUEST"},
		{"timeout", "-- digi-engine: sap-price-stock", failing(context.DeadlineExceeded), 504, "SQL_TIMEOUT"},
		{"database error", "-- digi-engine: sap-price-stock", failing(errors.New("login failed")), 500, "DB_ERROR"},
		{"over the row cap", "-- digi-engine: sap-price-stock", three, 413, "SQL_ROW_LIMIT"},
	}
	for _, c := range cases {
		code, a := get(t, engineMux(t, c.text, nil, 2, c.engines), "/api/sqlqueries/GetPricesAndStock")
		if code != c.code || a.Code != c.errCode {
			t.Errorf("%s: got %d %q, want %d %q", c.name, code, a.Code, c.code, c.errCode)
		}
	}
}

func TestSavedQueryEngine_SQLNamingTheDirectiveInsideStaysSQL(t *testing.T) {
	called := false
	engines := map[string]SavedQueryEngine{"sap-price-stock": func(context.Context, map[string]any) ([]map[string]any, error) {
		called = true
		return nil, nil
	}}
	code, _ := get(t, engineMux(t, "-- digi-engine: sap-price-stock\nSELECT 1", nil, 10, engines), "/api/sqlqueries/GetPricesAndStock")
	if called || code == http.StatusOK {
		t.Fatalf("engine called %v, status %d: SQL must go to the database (unreachable here)", called, code)
	}
}

func TestSavedQueryEngines_SAPOnly(t *testing.T) {
	db, _ := sql.Open("sqlserver", "sqlserver://127.0.0.1:1?database=none")
	defer db.Close()
	if _, ok := SavedQueryEngines(config.Config{ERP: config.ERPSAP}, db)[EngineSAPPriceStock]; !ok {
		t.Fatal("a SAP connector offers the SAP engine")
	}
	if len(SavedQueryEngines(config.Config{ERP: config.ERPHasavshevet}, db)) != 0 {
		t.Fatal("a Hashavshevet connector offers no engine")
	}
	if len(SavedQueryEngines(config.Config{ERP: config.ERPSAP}, nil)) != 0 {
		t.Fatal("no database, no engine")
	}
}

func TestSAPPriceStockRequest(t *testing.T) {
	day := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	got := sapPriceStockRequest(map[string]any{
		"skuArray": `["A","B"]`, "warehouse": "10, 02", "syncKey": "21122", "userExtId": int64(9), "dateString": day, "priceList": int64(4),
	})
	want := erp.PriceStockRequest{SKUList: []string{"A", "B"}, Warehouses: []string{"10", " 02"}, UserExtID: "21122", Date: "2026-11-26"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	got = sapPriceStockRequest(map[string]any{"sku": "A", "warehouses": []any{"10"}, "userExtId": int64(21122), "date": "2026-11-26"})
	want = erp.PriceStockRequest{SKUList: []string{"A"}, Warehouses: []string{"10"}, UserExtID: "21122", Date: "2026-11-26"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback names: got %+v, want %+v", got, want)
	}
	if r := sapPriceStockRequest(map[string]any{}); r.SKUList != nil || r.Warehouses != nil || r.UserExtID != "" {
		t.Fatalf("empty: %+v", r)
	}
}

func TestSAPPriceStockRows_V1Shape(t *testing.T) {
	result := erp.PriceStockResult{Items: []erp.PriceStockItem{{
		SKU:              "A",
		Prices:           map[string]float64{"priceListPrice": 100, "bpGroupDiscount": 10, "finalPrice": 90},
		StockByWarehouse: map[string]float64{"10": 5},
		Details: map[string]any{
			"cardCode": "C1", "priceList": float64(4), "currency": "ILS", "priceSource": "Discount groups (lowest)",
			"onOrder": float64(2), "commited": float64(1), "engine": sap.Engine, "discountRules": 1, "itemActive": true,
			"freeStockByWarehouse": map[string]float64{"10": 4},
		},
	}}}
	rows := sapPriceStockRows(result, []string{"10", "02", "10", " "})
	if len(rows) != 2 {
		t.Fatalf("rows %d: one per sku and distinct warehouse", len(rows))
	}
	first, second := rows[0], rows[1]
	for k, v := range map[string]any{"sku": "A", "cardCode": "C1", "priceList": float64(4), "currency": "ILS", "priceListPrice": float64(100),
		"finalPrice": float64(90), "warehouseCode": "10", "stock": float64(5), "freeStock": float64(4), "onOrder": float64(2),
		"commited": float64(1), "bpGroupDiscount": float64(10), "engine": sap.Engine} {
		if first[k] != v {
			t.Errorf("first row %s = %v, want %v", k, first[k], v)
		}
	}
	for _, k := range []string{"osppPrice", "osppDiscount", "promoDiscount"} {
		if v, present := first[k]; !present || v != nil {
			t.Errorf("v1 column %s must be present and null, got %v %v", k, v, present)
		}
	}
	if second["warehouseCode"] != "02" || second["stock"] != nil || second["onOrder"] != nil || second["commited"] != nil {
		t.Errorf("second warehouse: %v", second)
	}
}
