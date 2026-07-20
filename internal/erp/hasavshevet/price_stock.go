package hasavshevet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp"
)

type gpriceRecord struct {
	SKU          string
	Price        *float64
	Currency     *string
	DiscountPrc  *float64
	CommisionPrc *float64
	GPFlag       *int64
	Date         *string
	DocumentID   *int64
}

type gpriceRow struct {
	SKU          string
	Price        sql.NullFloat64
	Currency     sql.NullString
	DiscountPrc  sql.NullFloat64
	CommisionPrc sql.NullFloat64
	GPFlag       sql.NullInt64
	Date         sql.NullTime
	DocumentID   sql.NullInt64
}

type onHandStockRow struct {
	SKU             string
	WarehouseStock  sql.NullFloat64
	OpenOrdersStock sql.NullFloat64
	TotalStock      sql.NullFloat64
}

const hasavshevetDefaultDocumentID = 1

func FetchPriceAndStock(ctx context.Context, dbConn *sql.DB, cfg config.Config, req erp.PriceStockRequest) (erp.PriceStockResult, error) {
	if dbConn == nil {
		return erp.PriceStockResult{}, errors.New("db connection is required")
	}

	if strings.TrimSpace(cfg.DB.Database) == "" {
		return erp.PriceStockResult{}, errors.New("db.database is required")
	}
	skus := uniqueStrings(req.SKUList)
	if len(skus) == 0 {
		return erp.PriceStockResult{Items: []erp.PriceStockItem{}}, nil
	}

	documentID := hasavshevetDefaultDocumentID
	var datF any
	if strings.TrimSpace(req.Date) != "" {
		datF = strings.TrimSpace(req.Date)
	}

	customerID := strings.TrimSpace(req.UserExtID)
	priceBySKU, err := fetchGPriceBulk(ctx, dbConn, customerID, skus, documentID, datF)
	if err != nil {
		return erp.PriceStockResult{}, err
	}

	stockByItem, err := fetchStockData(ctx, dbConn, skus, req.Warehouses)
	if err != nil {
		return erp.PriceStockResult{}, err
	}

	items := make([]erp.PriceStockItem, 0, len(skus))
	for _, sku := range skus {
		rec, ok := priceBySKU[sku]
		prices := map[string]float64{}
		if ok && rec.Price != nil {
			prices["price"] = *rec.Price
			prices["priceAfterDiscount"] = *rec.Price
		}
		if len(prices) == 0 {
			prices = nil
		}

		details := map[string]any{}
		if ok {
			if rec.Currency != nil {
				details["currency"] = *rec.Currency
			}
			if rec.DiscountPrc != nil {
				details["discountPrc"] = *rec.DiscountPrc
			}
			if rec.CommisionPrc != nil {
				details["commisionPrc"] = *rec.CommisionPrc
			}
			if rec.GPFlag != nil {
				details["gpFlag"] = *rec.GPFlag
			}
			if rec.Date != nil {
				details["date"] = *rec.Date
			}
			if rec.DocumentID != nil {
				details["documentId"] = *rec.DocumentID
			}
		}
		if len(details) == 0 {
			details = nil
		}

		items = append(items, erp.PriceStockItem{
			SKU:              sku,
			Prices:           prices,
			StockByWarehouse: stockByItem[sku],
			Details:          details,
		})
	}

	return erp.PriceStockResult{Items: items}, nil
}

func fetchGPriceBulk(ctx context.Context, dbConn *sql.DB, customerID string, skus []string, documentID int, datF any) (map[string]gpriceRecord, error) {
	if len(skus) == 0 {
		return map[string]gpriceRecord{}, nil
	}

	skusJSON, err := json.Marshal(skus)
	if err != nil {
		return nil, err
	}

	query := `
		EXEC dbo.GPRICE_Bulk
			@CustomerId = @CustomerId,
			@SkusJson = @SkusJson,
			@DocumentID = @DocumentID,
			@DatF = @DatF;
	`

	rows, err := dbConn.QueryContext(ctx, query,
		sql.Named("CustomerId", customerID),
		sql.Named("SkusJson", string(skusJSON)),
		sql.Named("DocumentID", documentID),
		sql.Named("DatF", datF),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]gpriceRecord, len(skus))
	for rows.Next() {
		var row gpriceRow
		if err := rows.Scan(&row.SKU, &row.Price, &row.Currency, &row.DiscountPrc, &row.CommisionPrc, &row.GPFlag, &row.Date, &row.DocumentID); err != nil {
			return nil, err
		}
		rec := normalizeGPriceRow(row)
		if rec.SKU == "" {
			continue
		}
		out[rec.SKU] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

func normalizeGPriceRow(row gpriceRow) gpriceRecord {
	rec := gpriceRecord{SKU: strings.TrimSpace(row.SKU)}
	if row.Price.Valid {
		val := row.Price.Float64
		rec.Price = &val
	}
	if row.Currency.Valid {
		val := strings.TrimSpace(row.Currency.String)
		if val != "" {
			rec.Currency = &val
		}
	}
	if row.DiscountPrc.Valid {
		val := row.DiscountPrc.Float64
		rec.DiscountPrc = &val
	}
	if row.CommisionPrc.Valid {
		val := row.CommisionPrc.Float64
		rec.CommisionPrc = &val
	}
	if row.GPFlag.Valid {
		val := row.GPFlag.Int64
		rec.GPFlag = &val
	}
	if row.Date.Valid {
		val := row.Date.Time.Format(time.RFC3339)
		rec.Date = &val
	}
	if row.DocumentID.Valid {
		val := row.DocumentID.Int64
		rec.DocumentID = &val
	}
	return rec
}

func fetchStockData(ctx context.Context, dbConn *sql.DB, skus, warehouses []string) (map[string]map[string]float64, error) {
	if len(skus) == 0 {
		return map[string]map[string]float64{}, nil
	}
	warehouses = uniqueStrings(warehouses)
	if len(warehouses) == 0 {
		warehouses = []string{"10"}
	}

	skusJSONBytes, err := json.Marshal(skus)
	if err != nil {
		return nil, err
	}
	skusJSON := string(skusJSONBytes)

	out := make(map[string]map[string]float64)
	for _, warehouse := range warehouses {
		warehouse = strings.TrimSpace(warehouse)
		if warehouse == "" {
			continue
		}

		totalsBySKU, err := fetchOnHandStockForWarehouse(ctx, dbConn, warehouse, skusJSON)
		if err != nil {
			return nil, err
		}

		for sku, total := range totalsBySKU {
			if out[sku] == nil {
				out[sku] = make(map[string]float64)
			}
			out[sku][warehouse] = total
		}
	}

	return out, nil
}

func fetchOnHandStockForWarehouse(ctx context.Context, dbConn *sql.DB, warehouse, skusJSON string) (map[string]float64, error) {
	const query = `
		EXEC dbo.GetOnHandStockForSkus
			@Warehouse = @Warehouse,
			@SkusJson = @SkusJson;
	`

	rows, err := dbConn.QueryContext(ctx, query,
		sql.Named("Warehouse", warehouse),
		sql.Named("SkusJson", skusJSON),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]float64)
	for rows.Next() {
		var row onHandStockRow
		if err := rows.Scan(&row.SKU, &row.WarehouseStock, &row.OpenOrdersStock, &row.TotalStock); err != nil {
			return nil, err
		}

		sku := strings.TrimSpace(row.SKU)
		if sku == "" {
			continue
		}

		if row.TotalStock.Valid {
			out[sku] = row.TotalStock.Float64
			continue
		}
		out[sku] = 0
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func uniqueStrings(values []string) []string {
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
