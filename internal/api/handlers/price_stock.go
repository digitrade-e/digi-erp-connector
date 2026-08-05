package handlers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/sap"
)

const (
	priceStockTimeout = 12 * time.Second
)

func NewPriceAndStockHandler(cfg config.Config, dbConn *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dbConn == nil {
			respond.Error(w, http.StatusServiceUnavailable, "Database connection unavailable", "DB_UNAVAILABLE", nil)
			return
		}

		var req dto.PriceStockRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			badJSONRequest(w)
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
			respond.Error(w, http.StatusBadRequest, "Unsupported ERP type", "ERP_NOT_SUPPORTED", nil)
			return
		}

		if err != nil {
			if errors.Is(err, sap.ErrNotImplemented) {
				respond.Error(w, http.StatusNotImplemented, "Price/stock not implemented", "NOT_IMPLEMENTED", nil)
				return
			}
			respond.Error(w, http.StatusInternalServerError, "Failed to load price and stock", "PRICE_STOCK_FAILED", nil)
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

		respond.JSON(w, http.StatusOK, dto.PriceStockResponse{
			Items: items,
			Meta: dto.PriceStockMeta{
				DurationMs: time.Since(start).Milliseconds(),
			},
		})
	}
}
