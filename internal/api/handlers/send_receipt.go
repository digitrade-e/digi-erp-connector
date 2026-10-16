package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

const (
	// ReceiptWriteDeadline is how long POST /api/sendReceipt may take to
	// answer. The route is synchronous — the caller needs the receipt number
	// back — and in model (b) it runs the receipt BAT (90 s cap + 5 s wait
	// delay) after up to receiptLockWait for the import lock, so it extends
	// its own write deadline past the server's 60 s WriteTimeout.
	ReceiptWriteDeadline = 120 * time.Second

	// receiptLockWait bounds the wait for the import lock, which an order
	// import may be holding (the order importer has no timeout of its own).
	// A receipt that never got the lock reserved nothing; 503 tells the caller
	// to retry with the same key.
	receiptLockWait = 20 * time.Second
)

// ReceiptWriter is what the route needs from hasavshevet.ReceiptWriter. The ctx
// is used only to wait for the import lock; the write and the import, once
// started, are not cut short by it.
type ReceiptWriter interface {
	Write(ctx context.Context, req hasavshevet.ReceiptRequest) (*hasavshevet.ReceiptResult, error)
}

// NewSendReceiptHandler serves POST /api/sendReceipt: one KUPAIN receipt per
// idempotency key, written synchronously, answered with its number.
//
//   - 200 {receiptNumber, written:true, idempotencyKey, import, warnings} —
//     also for a repeat of a key, which is the same receipt and writes
//     nothing new.
//   - 400 VALIDATION_ERROR {details:{field}} — the request is at fault: an
//     unknown field, a missing required field, a wrong type, or a value that
//     cannot be written. No number is used.
//   - 503 RECEIPT_REQUEST_READ_FAILED — the body could not be read to its end
//     (a read timeout, a client that stalled or hung up); nothing was read
//     into a receipt, so it is not the request's fault and never a 400.
//   - 503 RECEIPTS_NOT_CONFIGURED — no sendReceiptDir; nothing is read or
//     written. 503 RECEIPT_NUMBER_STORE_UNAVAILABLE — the number store is not
//     seeded or not usable. 503 RECEIPT_WRITER_BUSY — the import lock was not
//     free in time.
//   - 500 — the connector is at fault (its card-name config, its index, the
//     disk). Never 400: the backend must be free to send it again.
//
// The response never carries a file path.
func NewSendReceiptHandler(writer ReceiptWriter, configured bool, log logger.LoggerService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !configured || writer == nil {
			respond.Error(w, http.StatusServiceUnavailable,
				"This connector is not configured to write receipts (requires erp: hasavshevet and sendReceiptDir)",
				"RECEIPTS_NOT_CONFIGURED", nil)
			return
		}

		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(ReceiptWriteDeadline)); err != nil && log != nil {
			log.Warn("sendReceipt: could not extend the write deadline (" + err.Error() + "); a slow import may lose its response")
		}

		var body dto.SendReceiptRequest
		if err := decodeJSONBodyStrict(w, r, &body); err != nil {
			var re *bodyReadError
			if errors.As(err, &re) {
				// Not the content's fault, so never a 400: the backend stops a
				// receipt on a 400 and retries anything else with the same key.
				if log != nil {
					log.Warn("sendReceipt: the request body could not be read (" + re.err.Error() + "); answered 503")
				}
				respond.Error(w, http.StatusServiceUnavailable,
					"The request body could not be read to its end; nothing was written — retry with the same idempotencyKey",
					"RECEIPT_REQUEST_READ_FAILED", nil)
				return
			}
			field, msg := describeReceiptDecodeError(err)
			receiptValidationError(w, &hasavshevet.FieldError{Field: field, Reason: msg})
			return
		}
		req, fe := receiptRequestFrom(body)
		if fe != nil {
			receiptValidationError(w, fe)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), receiptLockWait)
		defer cancel()
		res, err := writer.Write(ctx, req)
		if err != nil {
			writeReceiptError(w, log, req.IdempotencyKey, err)
			return
		}

		warnings := res.Warnings
		if warnings == nil {
			warnings = []string{}
		}
		respond.JSON(w, http.StatusOK, dto.SendReceiptResponse{
			ReceiptNumber:  strconv.FormatInt(res.ReceiptNumber, 10),
			Written:        res.Written,
			IdempotencyKey: res.IdempotencyKey,
			Import:         string(res.Import),
			Warnings:       warnings,
		})
	}
}

func receiptValidationError(w http.ResponseWriter, fe *hasavshevet.FieldError) {
	respond.Error(w, http.StatusBadRequest, fe.Error(), "VALIDATION_ERROR", map[string]any{"field": fe.Field})
}

// writeReceiptError maps a writer error to a status. Messages are fixed text:
// the errors themselves name files under the data directory.
func writeReceiptError(w http.ResponseWriter, log logger.LoggerService, key string, err error) {
	var fe *hasavshevet.FieldError
	var ce *hasavshevet.CardNamesConfigError
	status, code, msg := http.StatusInternalServerError, "RECEIPT_WRITE_FAILED",
		"The receipt could not be written; see the connector log"
	switch {
	case errors.As(err, &ce):
		// Checked first and never a 400: the operator's table is at fault.
		code, msg = "RECEIPT_CONFIG_INVALID", "The connector's receiptCardNames configuration cannot be written; see the connector log"
	case errors.As(err, &fe):
		receiptValidationError(w, fe)
		return
	case errors.Is(err, hasavshevet.ErrStoreUnavailable):
		status, code, msg = http.StatusServiceUnavailable, "RECEIPT_NUMBER_STORE_UNAVAILABLE",
			"The receipt number store is not available (not seeded, unreadable or exhausted); see the connector log"
	case errors.Is(err, hasavshevet.ErrStoredReceiptUnbuildable):
		// A receipt already reserved (by an earlier request) no longer builds:
		// the connector's index is at fault, not this request.
		code, msg = "RECEIPT_STORED_REQUEST_INVALID", "A receipt already reserved by this connector cannot be finished; see the connector log"
	case errors.Is(err, hasavshevet.ErrReceiptIndexUnreadable):
		code, msg = "RECEIPT_INDEX_UNREADABLE", "The receipt idempotency index cannot be trusted; see the connector log"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status, code, msg = http.StatusServiceUnavailable, "RECEIPT_WRITER_BUSY",
			"Another import is running; nothing was written — retry with the same idempotencyKey"
	}
	if log != nil {
		log.Error(fmt.Sprintf("sendReceipt key=%s: %s", key, code), err)
	}
	respond.Error(w, status, msg, code, nil)
}

// describeReceiptDecodeError names the field a decode error is about ("body"
// when it is about the document as a whole).
func describeReceiptDecodeError(err error) (field, msg string) {
	var ke *jsonKeyError
	if errors.As(err, &ke) {
		if ke.Duplicate {
			return ke.Path, "is given more than once"
		}
		if ke.TooDeep {
			return ke.Path, "is nested too deeply"
		}
		return ke.Path, "is not a sendReceipt field (keys are case-sensitive)"
	}
	if name := unknownJSONField(err); name != "" {
		return name, "is not a sendReceipt field"
	}
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		if te.Field == "" {
			return "body", "must be a JSON object"
		}
		return strings.ReplaceAll(te.Field, ".", "/"), fmt.Sprintf("must be a %s, got a JSON %s", jsonKind(te.Type.Kind().String()), te.Value)
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return "body", fmt.Sprintf("is larger than %d bytes", mbe.Limit)
	}
	return "body", "is not a single JSON object"
}

func jsonKind(goKind string) string {
	switch goKind {
	case "int", "int64":
		return "whole number"
	case "string":
		return "string"
	case "slice":
		return "array"
	case "struct", "ptr":
		return "object"
	}
	return goKind
}

// receiptRequestFrom checks what the strict decode cannot — every key present
// and not null — and maps the wire body to the writer's request. Everything
// written into the record (widths, encoding, dates, amounts, codes, the key's
// form) is the builder's to refuse.
func receiptRequestFrom(b dto.SendReceiptRequest) (hasavshevet.ReceiptRequest, *hasavshevet.FieldError) {
	var req hasavshevet.ReceiptRequest
	for _, f := range []struct {
		name string
		src  *string
		dst  *string
	}{
		{"idempotencyKey", b.IdempotencyKey, &req.IdempotencyKey},
		{"accountKey", b.AccountKey, &req.AccountKey},
		{"settledDate", b.SettledDate, &req.SettledDate},
		{"cardIssuer", b.CardIssuer, &req.CardIssuer},
		{"cardBrand", b.CardBrand, &req.CardBrand},
		{"last4", b.Last4, &req.Last4},
		{"confirmationCode", b.ConfirmationCode, &req.ConfirmationCode},
		{"providerUid", b.ProviderUID, &req.ProviderUID},
		{"remarks", b.Remarks, &req.Remarks},
		{"agentKey", b.AgentKey, &req.AgentKey},
		{"name", b.Name, &req.Name},
		{"address", b.Address, &req.Address},
		{"phone", b.Phone, &req.Phone},
		{"email", b.Email, &req.Email},
	} {
		if f.src == nil {
			return hasavshevet.ReceiptRequest{}, required(f.name)
		}
		*f.dst = *f.src
	}
	if b.AmountMinor == nil {
		return hasavshevet.ReceiptRequest{}, required("amountMinor")
	}
	if b.Installments == nil {
		return hasavshevet.ReceiptRequest{}, required("installments")
	}
	req.AmountMinor = *b.AmountMinor
	req.Installments = *b.Installments
	return req, nil
}

// required is the 400 for a key that is absent or null: the backend sends all
// of them, every time, with "" for an empty value.
func required(field string) *hasavshevet.FieldError {
	return &hasavshevet.FieldError{Field: field, Reason: "is required (absent or null)"}
}
