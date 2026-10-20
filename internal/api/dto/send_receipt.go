package dto

// SendReceiptRequest is the JSON body of POST /api/sendReceipt — plan 056
// §4.6a, flat: exactly the keys the backend's Receipt::jsonSerialize() emits,
// as the types it emits them as. It is decoded strictly: an unknown key, a
// duplicate or wrong-case key, a wrong type or trailing data is a 400.
//
// Every field is a pointer so a missing key can be told apart from a zero or
// an empty string. The backend always sends all sixteen and never sends null,
// so a missing key or a null is a 400 too; an optional value comes as "".
type SendReceiptRequest struct {
	IdempotencyKey   *string `json:"idempotencyKey"` // the payment's uuid
	AccountKey       *string `json:"accountKey"`
	SettledDate      *string `json:"settledDate"` // YYYY-MM-DD, Asia/Jerusalem day
	AmountMinor      *int64  `json:"amountMinor"`
	Installments     *int    `json:"installments"`
	CardIssuer       *string `json:"cardIssuer"` // raw HYP Issuer, "" when none
	CardBrand        *string `json:"cardBrand"`  // raw HYP Brand, "" when none
	Last4            *string `json:"last4"`
	ConfirmationCode *string `json:"confirmationCode"` // HYP ACode
	ProviderUID      *string `json:"providerUid"`      // HYP UID
	Remarks          *string `json:"remarks"`          // the invoices paid
	AgentKey         *string `json:"agentKey"`
	Name             *string `json:"name"`
	Address          *string `json:"address"`
	Phone            *string `json:"phone"`
	Email            *string `json:"email"`
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
