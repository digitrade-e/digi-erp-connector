package handlers

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/microsoft/go-mssqldb"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/sap"
)

// Past SAP's request limits the answer is a 400 naming the limit; before v2 the query reached SQL
// Server's 2100-parameter cap and the caller got a 500 that looked like an ERP failure.
func TestPriceStock_SAPRequestTooLargeIs400(t *testing.T) {
	db, err := sql.Open("sqlserver", "sqlserver://127.0.0.1:1?database=none") // never dialled: refused first
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	skus := make([]string, sap.MaxSKUs+1)
	for i := range skus {
		skus[i] = fmt.Sprintf("S%d", i)
	}
	body, _ := json.Marshal(map[string]any{"skuList": skus, "userExtId": "C1", "warehouses": []string{"10"}})
	rec := httptest.NewRecorder()
	NewPriceAndStockHandler(config.Config{ERP: config.ERPSAP}, db)(rec,
		httptest.NewRequest(http.MethodPost, "/api/priceAndStockHandler", bytes.NewReader(body)))
	var got struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusBadRequest || got.Code != "PRICE_STOCK_TOO_LARGE" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
