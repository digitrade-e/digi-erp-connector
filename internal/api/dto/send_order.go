package dto

import "encoding/json"

// SendOrderRequest is the JSON body for POST /api/sendOrder.
// Numeric fields that are required-but-zero (discount, total, quantity, prices)
// are represented as pointers so null vs zero can be distinguished.
// dbName is not required — the connector uses the database from its config.
type SendOrderRequest struct {
	DocumentType  string              `json:"documentType"`
	UserExtID     string              `json:"userExtId"`
	DueDate       string              `json:"dueDate"`
	CreatedDate   string              `json:"createdDate"`
	Comment       string              `json:"comment"`
	Discount      *float64            `json:"discount"`
	HistoryID     string              `json:"historyId"`
	Total         *float64            `json:"total"`
	Currency      string              `json:"currency"`
	CustomerEmail string              `json:"customerEmail"` // optional; for PDF email delivery
	Details       []SendOrderLineItem `json:"details"`
	// Account optionally carries the customer details that would otherwise be
	// read from the ERP database. Supplying it lets a connector with no database
	// of its own build orders — see docs/deployment-topologies.md. When the
	// connector does have a database, anything set here overrides the lookup.
	Account *SendOrderAccount `json:"account,omitempty"`

	// Document fields (DIG-1295, optional): the IMOVEIN header's ERP details,
	// set per tenant in erp-manager (mapping constants per document mode).
	// Each absent or null keeps today's value. Raw, so the handler can take a
	// JSON number or a string of digits and answer anything else with a
	// VALIDATION_ERROR naming the field (a typed field would fail the whole
	// decode instead):
	//   documentId — Hashavshevet document type, 1-99 (line4)
	//   warehouse  — header warehouse, 1-999999999 (line12)
	//   vatPercent — VAT rate, 0-99.99, at most two decimals (line18, "17.00")
	//   asmahta2   — the external reference (the B2B cart id), 1-9 digits
	//                (line8); MyID (line3) stays the connector's order number
	DocumentID json.RawMessage `json:"documentId,omitempty"`
	Warehouse  json.RawMessage `json:"warehouse,omitempty"`
	VatPercent json.RawMessage `json:"vatPercent,omitempty"`
	Asmahta2   json.RawMessage `json:"asmahta2,omitempty"`

	// Header (DIG-1315, optional): the order's own Hashavshevet header
	// details, mapped per tenant in erp-manager. An object whose keys are
	// exactly SendOrderHeader's (case-sensitive; any other key is a 400);
	// absent or null = today's bytes. Raw, so the handler can refuse unknown,
	// duplicate and wrong-case keys by name (the body itself is decoded
	// leniently, as before).
	Header json.RawMessage `json:"header,omitempty"`
}

// SendOrderHeader is the "header" object of POST /api/sendOrder (DIG-1315).
// Each value is a JSON string or number; absent, null or "" = not applied.
// Texts are at most the IMOVEIN field's width in characters, numbers are
// decimals written as sent (docs/hasavshevet-send-order.md § Header fields).
type SendOrderHeader struct {
	AccountNameDetail json.RawMessage `json:"accountNameDetail"` // line5, "<account name>|<detail>"
	Address           json.RawMessage `json:"address"`           // line6
	City              json.RawMessage `json:"city"`              // line7
	Phone             json.RawMessage `json:"phone"`             // line35
	ExtraText1        json.RawMessage `json:"extraText1"`        // line45
	ExtraText2        json.RawMessage `json:"extraText2"`        // line46
	ExtraText3        json.RawMessage `json:"extraText3"`        // line47
	ExtraText4        json.RawMessage `json:"extraText4"`        // line48
	ExtraText5        json.RawMessage `json:"extraText5"`        // line49
	ExtraSum1         json.RawMessage `json:"extraSum1"`         // line50
	ExtraSum2         json.RawMessage `json:"extraSum2"`         // line51
	ExtraSum3         json.RawMessage `json:"extraSum3"`         // line52
	ExtraNum1         json.RawMessage `json:"extraNum1"`         // line81
	ExtraNum2         json.RawMessage `json:"extraNum2"`         // line82
}

// SendOrderAccount mirrors the columns the connector otherwise reads from
// [dbo].[Accounts]: they populate the IMOVEIN header.
type SendOrderAccount struct {
	AccountKey string `json:"accountKey"`
	FullName   string `json:"fullName"`
	Address    string `json:"address"`
	City       string `json:"city"`
	Phone      string `json:"phone"`
	Agent      string `json:"agent"`
	HProtect   string `json:"hProtect"`
}

type SendOrderLineItem struct {
	Title    string   `json:"title"`
	SKU      string   `json:"sku"`
	Quantity *float64 `json:"quantity"`
	// Packs is the number of packages (אריזות / IMOVEIN line33). Optional: older
	// callers omit it, in which case line33 stays blank (legacy behaviour).
	Packs         *float64 `json:"packs"`
	OriginalPrice *float64 `json:"originalPrice"`
	SinglePrice   *float64 `json:"singlePrice"`
	TotalPrice    *float64 `json:"totalPrice"`
	Discount      *float64 `json:"discount"`
}

type SendOrderMeta struct {
	DurationMs int64 `json:"durationMs"`
}

// SendOrderAccepted is returned immediately with 202 when the order is
// enqueued — and, for a repeat of a historyId already accepted, with the first
// order's jobId, its current status (queued, running, done, failed, unknown)
// and Replay true. The field is omitted on a first answer, which keeps its
// original shape.
type SendOrderAccepted struct {
	Status string `json:"status"`
	JobID  string `json:"jobId"`
	Replay bool   `json:"replay,omitempty"`
	// HeaderApplied (DIG-1315) is present exactly when the request carried a
	// "header" object: the keys this connector writes into the document
	// header, in a fixed order ([] when none was set). A connector older than
	// DIG-1315 never sends it — it silently drops "header" — so a caller that
	// sends a header and gets no headerApplied knows its fields were not
	// written. On a replay the document is the first request's, as for every
	// other field.
	HeaderApplied *[]string     `json:"headerApplied,omitempty"`
	Meta          SendOrderMeta `json:"meta"`
}
