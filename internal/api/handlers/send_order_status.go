package handlers

import (
	"net/http"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// NewSendOrderStatusHandler serves GET /api/sendOrder/{jobId}, exposing the
// OrderQueue job map so callers can poll the jobId returned by POST /api/sendOrder.
func NewSendOrderStatusHandler(queue *hasavshevet.OrderQueue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if queue == nil {
			respond.Error(w, http.StatusServiceUnavailable, "Order queue unavailable", "QUEUE_UNAVAILABLE", nil)
			return
		}

		jobID := r.PathValue("jobId")
		result, ok := queue.Status(jobID)
		if !ok {
			respond.Error(w, http.StatusNotFound, "Job not found", "NOT_FOUND", nil)
			return
		}

		resp := dto.SendOrderStatusResponse{
			JobID:        result.ID,
			Status:       string(result.Status),
			OrderNumber:  result.OrderNumber,
			WrittenFiles: result.WrittenFiles,
		}
		if result.Err != nil {
			// Generic message only — raw processing errors may embed SQL or paths.
			resp.Error = "order processing failed"
		}
		if c := result.Import; c != nil {
			resp.Import = &dto.OrderImportCheck{
				Result: c.Result, Detail: c.Detail, Profile: c.Profile, LogStatus: c.LogStatus,
				StockIDs: c.StockIDs, DocumentID: c.DocumentID, StockStatus: c.StockStatus,
				MatchedBy: c.MatchedBy, TimedOut: c.TimedOut, CheckedAt: c.CheckedAt,
			}
		}
		respond.JSON(w, http.StatusOK, resp)
	}
}
