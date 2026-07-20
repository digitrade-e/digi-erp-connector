package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/utils"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

const sendOrderMaxBytes = 1 << 20 // 1 MiB

// NewSendOrderHandler returns a handler that validates an order request,
// enqueues it on the Hasavshevet single-worker queue, and returns 202 Accepted
// with a job ID that can be used to track processing status.
//
// Using async processing means the HTTP response is returned immediately;
// the caller does not block while IMOVEIN files are written and has.exe runs.
func NewSendOrderHandler(queue *hasavshevet.OrderQueue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		r.Body = http.MaxBytesReader(w, r.Body, sendOrderMaxBytes)
		defer r.Body.Close()

		var req dto.SendOrderRequest
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&req); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}
		if err := ensureEOF(dec); err != nil {
			utils.WriteError(w, http.StatusBadRequest, "Invalid JSON body", "INVALID_JSON", nil)
			return
		}

		// Validate required top-level fields
		var missing []string
		if req.DocumentType == "" {
			missing = append(missing, "documentType")
		}
		if req.UserExtID == "" {
			missing = append(missing, "userExtId")
		}
		if req.DueDate == "" {
			missing = append(missing, "dueDate")
		}
		if req.CreatedDate == "" {
			missing = append(missing, "createdDate")
		}
		if req.Discount == nil {
			missing = append(missing, "discount")
		}
		if req.HistoryID == "" {
			missing = append(missing, "historyId")
		}
		if req.Total == nil {
			missing = append(missing, "total")
		}
		if len(req.Details) == 0 {
			missing = append(missing, "details (must be non-empty array)")
		}
		if len(missing) > 0 {
			utils.WriteError(w, http.StatusBadRequest,
				"Missing required fields: "+joinStrings(missing), "VALIDATION_ERROR", nil)
			return
		}

		// Validate document type
		switch req.DocumentType {
		case "ORDER", "QUOATE", "RETURN":
		default:
			utils.WriteError(w, http.StatusBadRequest,
				"Invalid documentType; allowed: ORDER, QUOATE, RETURN", "VALIDATION_ERROR", nil)
			return
		}

		// Validate each detail line
		for i, item := range req.Details {
			var itemMissing []string
			if item.Title == "" {
				itemMissing = append(itemMissing, "title")
			}
			if item.SKU == "" {
				itemMissing = append(itemMissing, "sku")
			}
			if item.Quantity == nil {
				itemMissing = append(itemMissing, "quantity")
			} else if *item.Quantity == 0 {
				utils.WriteError(w, http.StatusBadRequest,
					"details["+itoa(i)+"]: quantity cannot be zero (Hasavshevet spec line23)",
					"VALIDATION_ERROR", nil)
				return
			}
			if item.OriginalPrice == nil {
				itemMissing = append(itemMissing, "originalPrice")
			}
			if item.SinglePrice == nil {
				itemMissing = append(itemMissing, "singlePrice")
			}
			if item.TotalPrice == nil {
				itemMissing = append(itemMissing, "totalPrice")
			}
			if item.Discount == nil {
				itemMissing = append(itemMissing, "discount")
			}
			if len(itemMissing) > 0 {
				utils.WriteError(w, http.StatusBadRequest,
					"Missing fields in details["+itoa(i)+"]: "+joinStrings(itemMissing),
					"VALIDATION_ERROR", nil)
				return
			}
		}

		// Map DTO → internal request
		details := make([]hasavshevet.OrderLineItem, 0, len(req.Details))
		for _, d := range req.Details {
			var packs float64
			if d.Packs != nil {
				packs = *d.Packs
			}
			details = append(details, hasavshevet.OrderLineItem{
				Title:         d.Title,
				SKU:           d.SKU,
				Quantity:      *d.Quantity,
				Packs:         packs,
				OriginalPrice: *d.OriginalPrice,
				SinglePrice:   *d.SinglePrice,
				TotalPrice:    *d.TotalPrice,
				Discount:      *d.Discount,
			})
		}

		orderReq := hasavshevet.OrderRequest{
			DocumentType:  req.DocumentType,
			UserExtID:     req.UserExtID,
			DueDate:       req.DueDate,
			CreatedDate:   req.CreatedDate,
			Comment:       req.Comment,
			Discount:      *req.Discount,
			HistoryID:     req.HistoryID,
			Total:         *req.Total,
			Currency:      req.Currency,
			CustomerEmail: req.CustomerEmail,
			Details:       details,
		}

		lastOrderNumber, err := queue.Submit(orderReq)
		if err != nil {
			utils.WriteError(w, http.StatusServiceUnavailable,
				"Order queue full; try again later", "QUEUE_FULL", nil)
			return
		}

		utils.WriteJSON(w, http.StatusAccepted, dto.SendOrderAccepted{
			Status: "queued",
			JobID:  lastOrderNumber,
			Meta:   dto.SendOrderMeta{DurationMs: time.Since(start).Milliseconds()},
		})
	}
}

// joinStrings joins string slices with ", " separator.
func joinStrings(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// itoa converts an int to its decimal string representation.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
