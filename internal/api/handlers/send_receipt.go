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

	maxProviderTransactionIDBytes = 64
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

// receiptRequestFrom checks what the strict decode cannot — required fields
// present, currency, providerTransactionId — and maps the wire body to the
// writer's request. Everything written into the record (widths, encoding,
// dates, amounts, last4, the key's form) is the builder's to refuse.
func receiptRequestFrom(b dto.SendReceiptRequest) (hasavshevet.ReceiptRequest, *hasavshevet.FieldError) {
	missing := func(field string) *hasavshevet.FieldError {
		return &hasavshevet.FieldError{Field: field, Reason: "is required"}
	}
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"idempotencyKey", b.IdempotencyKey != nil},
		{"account", b.Account != nil},
	} {
		if !f.present {
			return hasavshevet.ReceiptRequest{}, missing(f.name)
		}
	}
	for _, f := range []struct {
		name    string
		present bool
	}{
		{"account/syncKey", b.Account.SyncKey != nil},
		{"account/name", b.Account.Name != nil},
		{"settledAt", b.SettledAt != nil},
		{"amountMinor", b.AmountMinor != nil},
		{"currency", b.Currency != nil},
		{"installments", b.Installments != nil},
		{"last4", b.Last4 != nil},
		{"confirmationCode", b.ConfirmationCode != nil},
		{"providerTransactionId", b.ProviderTransactionID != nil},
	} {
		if !f.present {
			return hasavshevet.ReceiptRequest{}, missing(f.name)
		}
	}

	if *b.Currency != "ILS" {
		return hasavshevet.ReceiptRequest{}, &hasavshevet.FieldError{Field: "currency", Reason: fmt.Sprintf("must be ILS, got %q", *b.Currency)}
	}
	ptx := strings.TrimSpace(*b.ProviderTransactionID)
	if ptx == "" {
		return hasavshevet.ReceiptRequest{}, missing("providerTransactionId")
	}
	if len(ptx) > maxProviderTransactionIDBytes {
		return hasavshevet.ReceiptRequest{}, &hasavshevet.FieldError{Field: "providerTransactionId",
			Reason: fmt.Sprintf("is %d bytes, at most %d", len(ptx), maxProviderTransactionIDBytes)}
	}

	if len(b.Details) == 0 {
		return hasavshevet.ReceiptRequest{}, &hasavshevet.FieldError{Field: "details", Reason: "at least one invoice reference is required"}
	}
	details := make([]hasavshevet.ReceiptDetail, 0, len(b.Details))
	for i, d := range b.Details {
		switch {
		case d == nil:
			return hasavshevet.ReceiptRequest{}, &hasavshevet.FieldError{Field: "details", Reason: fmt.Sprintf("details[%d] is null", i)}
		case d.Reference == nil:
			return hasavshevet.ReceiptRequest{}, &hasavshevet.FieldError{Field: "details/reference", Reason: fmt.Sprintf("is required (details[%d])", i)}
		case d.AmountMinor == nil:
			return hasavshevet.ReceiptRequest{}, &hasavshevet.FieldError{Field: "details/amountMinor", Reason: fmt.Sprintf("is required (details[%d])", i)}
		}
		details = append(details, hasavshevet.ReceiptDetail{Reference: *d.Reference, AmountMinor: *d.AmountMinor})
	}

	return hasavshevet.ReceiptRequest{
		IdempotencyKey: *b.IdempotencyKey,
		Account: hasavshevet.ReceiptAccount{
			SyncKey:      *b.Account.SyncKey,
			Name:         *b.Account.Name,
			Address:      orEmpty(b.Account.Address),
			City:         orEmpty(b.Account.City),
			Phone:        orEmpty(b.Account.Phone),
			AgentSyncKey: orEmpty(b.Account.AgentSyncKey),
		},
		Email:                 orEmpty(b.Email),
		SettledAt:             *b.SettledAt,
		AmountMinor:           *b.AmountMinor,
		Currency:              *b.Currency,
		Installments:          *b.Installments,
		FirstPaymentMinor:     b.FirstPaymentMinor,
		CardBrand:             orEmpty(b.CardBrand),
		CardIssuer:            orEmpty(b.CardIssuer),
		Last4:                 *b.Last4,
		ConfirmationCode:      *b.ConfirmationCode,
		AuthorizationRef:      orEmpty(b.AuthorizationRef),
		ProviderTransactionID: ptx,
		Details:               details,
		RemarkSuffix:          orEmpty(b.RemarkSuffix),
	}, nil
}

// orEmpty is a nullable wire string: null and absent are both "".
func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
