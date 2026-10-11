package dto

// SendReceiptRequest is the JSON body of POST /api/sendReceipt — plan 005 §2,
// the one field table the backend's projector, erp-manager's mapping and this
// decoder share. It is decoded strictly: an unknown field, a wrong type or
// trailing data is a 400.
//
// Every field is a pointer so a missing required field can be told apart from
// a zero or an empty string. For the nullable ones (marked "null ok") JSON
// null and absence both mean "none".
type SendReceiptRequest struct {
	IdempotencyKey        *string              `json:"idempotencyKey"` // canonical UUID
	Account               *SendReceiptAccount  `json:"account"`
	Email                 *string              `json:"email"`     // null ok
	SettledAt             *string              `json:"settledAt"` // YYYY-MM-DD, Asia/Jerusalem day
	AmountMinor           *int64               `json:"amountMinor"`
	Currency              *string              `json:"currency"` // must be ILS
	Installments          *int                 `json:"installments"`
	FirstPaymentMinor     *int64               `json:"firstPaymentMinor"` // null ok
	CardBrand             *string              `json:"cardBrand"`         // null ok; raw HYP Brand
	CardIssuer            *string              `json:"cardIssuer"`        // null ok; raw HYP Issuer
	Last4                 *string              `json:"last4"`
	ConfirmationCode      *string              `json:"confirmationCode"` // HYP ACode
	AuthorizationRef      *string              `json:"authorizationRef"` // null ok; HYP UID
	ProviderTransactionID *string              `json:"providerTransactionId"`
	Details               []*SendReceiptDetail `json:"details"`
	RemarkSuffix          *string              `json:"remarkSuffix"` // null ok
}

// SendReceiptAccount is the customer block, frozen in the order snapshot.
type SendReceiptAccount struct {
	SyncKey      *string `json:"syncKey"`
	Name         *string `json:"name"`
	Address      *string `json:"address"`      // null ok
	City         *string `json:"city"`         // null ok
	Phone        *string `json:"phone"`        // null ok
	AgentSyncKey *string `json:"agentSyncKey"` // null ok
}

// SendReceiptDetail is one invoice the receipt pays.
type SendReceiptDetail struct {
	Reference   *string `json:"reference"`
	AmountMinor *int64  `json:"amountMinor"` // signed: a credit is negative
}

// SendReceiptResponse is the 200 body. Exactly these five keys — the backend
// reads written and receiptNumber and nothing that is not here. warnings is
// always an array, [] when there are none.
//
// import is not_run (model (a), or a receipt finished by recovery), started
// (the process died during the BAT — outcome unknown, never re-run), ok or
// failed. A receipt answered here is written either way: the import is a
// separate step.
type SendReceiptResponse struct {
	ReceiptNumber  string   `json:"receiptNumber"`
	Written        bool     `json:"written"`
	IdempotencyKey string   `json:"idempotencyKey"`
	Import         string   `json:"import"`
	Warnings       []string `json:"warnings"`
}
