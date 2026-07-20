package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/sap"
)

const (
	priceStockTimeout  = 12 * time.Second
	priceStockMaxBytes = 1 << 20
)

func NewPriceAndStockHandler(cfg config.Config, dbConn *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dbConn == nil {
			utils.WriteError(w, http.StatusServiceUnavailable, "Database connection unavailable", "DB_UNAVAILABLE", nil)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, priceStockMaxBytes)
		defer r.Body.Close()

		var req dto.PriceStockRequest
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}
		if err := ensureEOF(dec); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), priceStockTimeout)
		defer cancel()

		erpReq := erp.PriceStockRequest{
			SKUList:    req.SKUList,
			Warehouses: req.Warehouses,
			UserExtID:  req.UserExtID,
			Date:       req.Date,
		}
		if cfg.ERP != config.ERPHasavshevet {
			erpReq.PriceList = req.PriceList
		}

		start := time.Now()
		var result erp.PriceStockResult
		var err error

		switch cfg.ERP {
		case config.ERPHasavshevet:
			result, err = hasavshevet.FetchPriceAndStock(ctx, dbConn, cfg, erpReq)
		case config.ERPSAP:
			result, err = sap.FetchPriceAndStock(ctx, dbConn, cfg, erpReq)
		default:
			utils.WriteError(w, http.StatusBadRequest, "Unsupported ERP type", "ERP_NOT_SUPPORTED", nil)
			return
		}

		if err != nil {
			if errors.Is(err, sap.ErrNotImplemented) {
				utils.WriteError(w, http.StatusNotImplemented, "Price/stock not implemented", "NOT_IMPLEMENTED", nil)
				return
			}
			utils.WriteError(w, http.StatusInternalServerError, "Failed to load price and stock", "PRICE_STOCK_FAILED", nil)
			return
		}

		items := make([]dto.PriceStockItem, 0, len(result.Items))
		for _, item := range result.Items {
			items = append(items, dto.PriceStockItem{
				SKU:              item.SKU,
				Prices:           item.Prices,
				StockByWarehouse: item.StockByWarehouse,
				Details:          item.Details,
			})
		}

		utils.WriteJSON(w, http.StatusOK, dto.PriceStockResponse{
			Items: items,
			Meta: dto.PriceStockMeta{
				DurationMs: time.Since(start).Milliseconds(),
			},
		})
	}
}
