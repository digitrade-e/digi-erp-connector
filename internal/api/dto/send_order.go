package dto

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

// SendOrderAccepted is returned immediately with 202 when the order is enqueued.
type SendOrderAccepted struct {
	Status string        `json:"status"`
	JobID  string        `json:"jobId"`
	Meta   SendOrderMeta `json:"meta"`
}
