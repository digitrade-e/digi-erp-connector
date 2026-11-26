package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/sap"
)

// SavedQueryEngine answers a saved query that names it (queries.EngineDirective) from the request's
// merged parameters, as rows.
type SavedQueryEngine func(ctx context.Context, params map[string]any) ([]map[string]any, error)

// EngineSAPPriceStock is the directive name of the SAP price-and-stock engine.
const EngineSAPPriceStock = "sap-price-stock"

// SavedQueryEngines is the set this connector offers: the SAP engine only on a SAP connector.
func SavedQueryEngines(cfg config.Config, dbConn *sql.DB) map[string]SavedQueryEngine {
	engines := map[string]SavedQueryEngine{}
	if cfg.ERP == config.ERPSAP && dbConn != nil {
		engines[EngineSAPPriceStock] = NewSAPPriceStockEngine(cfg, dbConn)
	}
	return engines
}

func runEngine(w http.ResponseWriter, r *http.Request, queryName string, engine SavedQueryEngine, engineName string, params map[string]any, maxRows int) {
	if engine == nil {
		respond.Error(w, http.StatusBadRequest, fmt.Sprintf("This connector has no engine %q", engineName), "ENGINE_NOT_SUPPORTED", nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), priceStockTimeout)
	defer cancel()
	rows, err := engine(ctx, params)
	switch {
	case err == nil:
	case errors.Is(err, sap.ErrTooManySKUs) || errors.Is(err, sap.ErrTooManyWarehouses):
		respond.Error(w, http.StatusBadRequest, err.Error(), "PRICE_STOCK_TOO_LARGE", nil)
		return
	case errors.Is(err, sap.ErrInvalidRequest):
		respond.Error(w, http.StatusBadRequest, err.Error(), "ENGINE_BAD_REQUEST", nil)
		return
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		respond.Error(w, http.StatusGatewayTimeout, "Query timeout", "SQL_TIMEOUT", nil)
		return
	default:
		respond.Error(w, http.StatusInternalServerError, "Query execution failed", "DB_ERROR", nil)
		return
	}
	if len(rows) > maxRows {
		respond.Error(w, http.StatusRequestEntityTooLarge, "Row limit exceeded", "SQL_ROW_LIMIT", nil)
		return
	}
	if rows == nil {
		rows = make([]map[string]any, 0)
	}
	respond.JSON(w, http.StatusOK, dto.RunSavedQueryResponse{
		API:          r.URL.Path,
		Status:       "success",
		Name:         queryName,
		RowCount:     len(rows),
		Rows:         rows,
		Recordsets:   [][]map[string]any{rows},
		Value:        rows,
		RowsAffected: []int64{int64(len(rows))},
	})
}

// NewSAPPriceStockEngine prices and stocks a SKU list with the SAP engine (sap.FetchPriceAndStock)
// and answers the rows the v1 saved query GetPricesAndStock answered, so a data mapping written for
// v1 keeps working: one row per SKU and requested warehouse, every v1 column with its meaning, and
// the engine's new keys beside them. Parameters (v1's names first):
//
//	skuArray            SKUs: "A,B" or a JSON list '["A","B"]'
//	warehouse           warehouse codes, the same two forms
//	syncKey / userExtId the business partner (CardCode)
//	dateString / date   the day, YYYY-MM-DD (today when absent)
//
// v1's priceList is not read: the engine prices on the business partner's own list, as SAP does, and
// reports that list in the priceList column.
func NewSAPPriceStockEngine(cfg config.Config, dbConn *sql.DB) SavedQueryEngine {
	return func(ctx context.Context, params map[string]any) ([]map[string]any, error) {
		req := sapPriceStockRequest(params)
		result, err := sap.FetchPriceAndStock(ctx, dbConn, cfg, req)
		if err != nil {
			return nil, err
		}
		return sapPriceStockRows(result, req.Warehouses), nil
	}
}

func sapPriceStockRequest(params map[string]any) erp.PriceStockRequest {
	return erp.PriceStockRequest{
		SKUList:    listParam(firstParam(params, "skuArray", "sku")),
		Warehouses: listParam(firstParam(params, "warehouse", "warehouses")),
		UserExtID:  scalarParam(firstParam(params, "syncKey", "userExtId", "cardCode")),
		Date:       scalarParam(firstParam(params, "dateString", "date")),
	}
}

// sapPriceStockRows flattens the engine's items into v1's rows, in SKU order, then in the order the
// warehouses were asked. onOrder and commited are known for the first warehouse only (as in v1, which
// read one warehouse); other warehouses' rows carry null there.
func sapPriceStockRows(result erp.PriceStockResult, warehouses []string) []map[string]any {
	whs := uniqueTrimmed(warehouses)
	rows := make([]map[string]any, 0, len(result.Items)*len(whs))
	for _, it := range result.Items {
		free, _ := it.Details["freeStockByWarehouse"].(map[string]float64)
		for i, wh := range whs {
			row := map[string]any{
				"sku":                   it.SKU,
				"cardCode":              it.Details["cardCode"],
				"priceList":             it.Details["priceList"],
				"currency":              it.Details["currency"],
				"priceListPrice":        price(it.Prices, "priceListPrice"),
				"osppPrice":             price(it.Prices, "osppPrice"),
				"osppDiscount":          price(it.Prices, "osppDiscount"),
				"promoDiscount":         price(it.Prices, "promoDiscount"),
				"warehouseCode":         wh,
				"stock":                 price(it.StockByWarehouse, wh),
				"onOrder":               nil,
				"commited":              nil,
				"priceSource":           it.Details["priceSource"],
				"finalPrice":            price(it.Prices, "finalPrice"),
				"bpGroupDiscount":       price(it.Prices, "bpGroupDiscount"),
				"bpGroupDiscountType":   it.Details["bpGroupDiscountType"],
				"oedgType":              it.Details["oedgType"],
				"manufacturerDiscount":  price(it.Prices, "manufacturerDiscount"),
				"periodVolumePrice":     price(it.Prices, "periodVolumePrice"),
				"blanketAgreementPrice": price(it.Prices, "blanketAgreementPrice"),
				"freeStock":             price(free, wh),
				"itemActive":            it.Details["itemActive"],
				"discountRules":         it.Details["discountRules"],
				"engine":                it.Details["engine"],
			}
			if i == 0 {
				row["onOrder"], row["commited"] = it.Details["onOrder"], it.Details["commited"]
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func price(m map[string]float64, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	return nil
}

func firstParam(params map[string]any, names ...string) any {
	for _, n := range names {
		if v, ok := params[n]; ok && v != nil {
			return v
		}
	}
	return nil
}

// listParam reads "A,B", a JSON list '["A","B"]' (the form the B2B's live readers send) or a list.
func listParam(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			out = append(out, scalarParam(e))
		}
		return out
	}
	s := strings.TrimSpace(scalarParam(v))
	if strings.HasPrefix(s, "[") {
		var list []any
		if err := json.Unmarshal([]byte(s), &list); err == nil {
			return listParam(list)
		}
	}
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func scalarParam(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case time.Time:
		return x.Format("2006-01-02")
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
	default:
		return fmt.Sprint(x)
	}
}

func uniqueTrimmed(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
