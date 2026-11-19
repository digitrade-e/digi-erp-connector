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
				Outcome: c.Outcome, Result: c.Result, Detail: c.Detail, Profile: c.Profile, LogStatus: c.LogStatus,
				StockIDs: c.StockIDs, DocumentID: c.DocumentID, StockStatus: c.StockStatus,
				MatchedBy: c.MatchedBy, TimedOut: c.TimedOut, CheckedAt: c.CheckedAt, MovedTo: c.MovedTo,
			}
			if rl := c.RunLog; rl != nil {
				out := &dto.OrderImportRunLog{
					File: rl.File, OK: rl.OK, Live: rl.Live, Section: rl.Section, NoInput: rl.NoInput,
					Summary: rl.Summary, Errors: rl.Errors, Transient: rl.Transient,
					SystemError: rl.SystemError, SystemErrors: rl.SystemErrors, OtherFiles: rl.OtherFiles,
				}
				for _, r := range rl.Refusals {
					out.Refusals = append(out.Refusals, dto.OrderImportRunRefusal{Value: r.Value, Code: r.Code, Message: r.Message, Row: r.Row})
				}
				resp.Import.RunLog = out
			}
		}
		respond.JSON(w, http.StatusOK, resp)
	}
}
