package handlers

import (
	"net/http"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// NewSendOrderQueueHandler serves GET /api/sendOrder: how many accepted
// orders are still queued or being written. The queue lives in memory, so
// whoever restarts the service asks first (digi-erp-connector-provision
// refuses to restart unless both are 0). Counts only — never an order.
func NewSendOrderQueueHandler(queue *hasavshevet.OrderQueue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if queue == nil {
			respond.Error(w, http.StatusServiceUnavailable, "Order queue unavailable", "QUEUE_UNAVAILABLE", nil)
			return
		}
		queued, running := queue.Unfinished()
		respond.JSON(w, http.StatusOK, dto.SendOrderQueueResponse{Queued: queued, Running: running})
	}
}
