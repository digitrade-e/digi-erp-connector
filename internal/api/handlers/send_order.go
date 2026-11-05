package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// NewSendOrderHandler returns a handler that validates an order request,
// enqueues it on the Hasavshevet single-worker queue, and returns 202 Accepted
// with a job ID that can be used to track processing status.
//
// Using async processing means the HTTP response is returned immediately;
// the caller does not block while IMOVEIN files are written and has.exe runs.
//
// historyId is the order's idempotency key (DIG-1291) when the queue has the
// key index: a repeat answers 202 with the first jobId, the order's current
// status and "replay": true, and writes nothing — except an order that failed
// before any file was written, which it queues again under the same number
// (status queued). An index entry that cannot be read answers 500
// ORDER_INDEX_UNREADABLE — never 202, never a new number.
//
// ordersConfigured tells the handler whether this installation can process
// orders at all — it needs erp: hasavshevet and a sendOrderDir. A node deployed
// only to serve queries has neither, and must refuse orders up front: accepting
// them with 202 and failing inside the worker looks like success to the caller
// and only surfaces on a status poll nobody makes.
func NewSendOrderHandler(queue *hasavshevet.OrderQueue, ordersConfigured bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		if !ordersConfigured {
			respond.Error(w, http.StatusNotImplemented,
				"This connector is not configured to send orders (requires erp: hasavshevet and sendOrderDir)",
				"ORDERS_NOT_CONFIGURED", nil)
			return
		}

		var req dto.SendOrderRequest
		if err := decodeJSONBody(w, r, &req); err != nil {
			badJSONRequest(w)
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
			respond.Error(w, http.StatusBadRequest,
				"Missing required fields: "+joinStrings(missing), "VALIDATION_ERROR", nil)
			return
		}

		// Validate document type
		switch req.DocumentType {
		case "ORDER", "QUOATE", "RETURN":
		default:
			respond.Error(w, http.StatusBadRequest,
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
				respond.Error(w, http.StatusBadRequest,
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
				respond.Error(w, http.StatusBadRequest,
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

		// Document fields (DIG-1295): checked here, before anything is
		// reserved, so a bad one is a 400 and never a number given away.
		if fe := applyDocumentFields(&orderReq, req); fe != nil {
			respond.Error(w, http.StatusBadRequest, fe.Error(), "VALIDATION_ERROR", map[string]any{"field": fe.Field})
			return
		}

		// Customer details supplied with the order skip the [dbo].[Accounts]
		// lookup, which is what lets a connector with no database build orders.
		if a := req.Account; a != nil {
			orderReq.Account = &hasavshevet.AccountDetails{
				AccountKey: a.AccountKey,
				FullName:   a.FullName,
				Address:    a.Address,
				City:       a.City,
				Phone:      a.Phone,
				Agent:      a.Agent,
				HProtect:   a.HProtect,
			}
		}

		acc, err := queue.Accept(orderReq)
		if err != nil {
			writeSendOrderError(w, err)
			return
		}

		respond.JSON(w, http.StatusAccepted, dto.SendOrderAccepted{
			Status: string(acc.Status),
			JobID:  acc.JobID,
			Replay: acc.Replay,
			Meta:   dto.SendOrderMeta{DurationMs: time.Since(start).Milliseconds()},
		})
	}
}

// applyDocumentFields parses the optional document fields of the wire request
// into orderReq and validates them against the IMOVEIN widths. Each may be a
// JSON number or a string; absent and null keep today's value. Anything else
// — another kind of value, a sign, a fraction where a whole number belongs —
// fails the digit checks and is refused with the field's name.
func applyDocumentFields(orderReq *hasavshevet.OrderRequest, req dto.SendOrderRequest) *hasavshevet.FieldError {
	wholeNumber := func(field string, raw json.RawMessage, dst *int) *hasavshevet.FieldError {
		s, given := wireNumberText(raw)
		if !given {
			return nil
		}
		n, err := strconv.Atoi(s)
		if !asciiDigits(s) || err != nil || n == 0 { // 0 would read as "absent"
			return &hasavshevet.FieldError{Field: field, Reason: "must be a positive whole number"}
		}
		*dst = n
		return nil
	}
	if fe := wholeNumber("documentId", req.DocumentID, &orderReq.DocumentID); fe != nil {
		return fe
	}
	if fe := wholeNumber("warehouse", req.Warehouse, &orderReq.Warehouse); fe != nil {
		return fe
	}
	if s, given := wireNumberText(req.VatPercent); given {
		vat, err := hasavshevet.FormatVatPercent(s)
		if err != nil {
			var fe *hasavshevet.FieldError
			if errors.As(err, &fe) {
				return fe
			}
			return &hasavshevet.FieldError{Field: "vatPercent", Reason: "must be a VAT rate"}
		}
		orderReq.VatPercent = vat
	}
	if s, given := wireNumberText(req.Asmahta2); given {
		if !asciiDigits(s) {
			return &hasavshevet.FieldError{Field: "asmahta2", Reason: "must be digits only (the B2B cart id)"}
		}
		orderReq.Asmahta2 = s
	}
	if err := hasavshevet.ValidateDocumentFields(*orderReq); err != nil {
		var fe *hasavshevet.FieldError
		if errors.As(err, &fe) {
			return fe
		}
		return &hasavshevet.FieldError{Field: "body", Reason: err.Error()}
	}
	return nil
}

// wireNumberText is the text a field was sent as: a JSON string's value, or
// any other JSON value's literal ("31", "18.5", "true", "{}" — the callers'
// digit checks refuse what is not a number). given is false for an absent
// field or null.
func wireNumberText(raw json.RawMessage) (text string, given bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if raw[0] == '"' && json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	return string(raw), true
}

func asciiDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// writeSendOrderError maps an Accept error to a status. Messages are fixed
// text: the errors name files under the data directory (the queue logs them).
func writeSendOrderError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, hasavshevet.ErrOrderIndexUnreadable):
		// Only ever this key's own entry: recovery skips other damaged files.
		respond.Error(w, http.StatusInternalServerError,
			"The order index entry for this historyId cannot be read; nothing was queued and no number given until a person repairs it — see the connector log",
			"ORDER_INDEX_UNREADABLE", nil)
	case errors.Is(err, hasavshevet.ErrQueueFull), errors.Is(err, hasavshevet.ErrQueueClosed):
		respond.Error(w, http.StatusServiceUnavailable,
			"Order queue full; try again later", "QUEUE_FULL", nil)
	case errors.Is(err, hasavshevet.ErrOrderCounterUnreadable):
		respond.Error(w, http.StatusServiceUnavailable,
			"The order counter (lastOrderNumber.json) cannot be read; no new order is accepted until a person repairs it — see the connector log",
			"ORDER_COUNTER_UNREADABLE", nil)
	default:
		respond.Error(w, http.StatusServiceUnavailable,
			"The order was not queued: the order index could not be written or recovered; nothing was reserved — retry with the same historyId (see the connector log)",
			"ORDER_NOT_QUEUED", nil)
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
