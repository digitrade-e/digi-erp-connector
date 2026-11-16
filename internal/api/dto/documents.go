package dto

// Plan 087 (DIG-1563): the read routes of named counters and typed folders.

// Counter is one entry of GET /api/counters.
type Counter struct {
	Key  string `json:"key"`
	Kind string `json:"kind"` // orders | receipts
	Min  int64  `json:"min"`
	Max  int64  `json:"max"`
	// Last is the last number issued, one meaning for both kinds (the receipt
	// store's "next" minus one).
	Last int64 `json:"last"`
	// HighestIssued is the highest number in the range an index entry or a
	// history folder holds: the index can run ahead of the counter file (a
	// crash between the two; recovery closes the gap).
	HighestIssued int64 `json:"highestIssued"`
	// Remaining is how many numbers the range still has: max - max(last,
	// highestIssued), at least 0.
	Remaining int64 `json:"remaining"`
	Test      bool  `json:"test"`
	// Retired: it issues no new number (its range stays reserved).
	Retired bool `json:"retired,omitempty"`
	// Error says why last or highestIssued could not be read (a missing or
	// damaged store); they are 0 then.
	Error string `json:"error,omitempty"`
}

// DocumentCapabilities is GET /api/documents/capabilities: what a
// documentRule may name on this box (the admin's dropdowns, digi-mcp's lint).
type DocumentCapabilities struct {
	// Contract is the documentRule version this connector reads (its "v").
	Contract     int                `json:"contract"`
	Folders      []DocumentKeyEntry `json:"folders"`
	Counters     []DocumentKeyEntry `json:"counters"`
	Placeholders []string           `json:"placeholders"`
}

// DocumentKeyEntry is a folder or a counter a rule may name.
type DocumentKeyEntry struct {
	Key  string `json:"key"`
	Kind string `json:"kind"`
	Test bool   `json:"test"`
}
