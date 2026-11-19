package provision

import (
	"fmt"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// The orders' import checks in state (research/reverse-engineering/hasavshevet-run-logs.md P13): what Hashavshevet
// did with each order the connector imported, as the daemon stored it (DataDir\orders\imports\<n>.json) — the same
// check GET /api/sendOrder/{jobId} answers, for whoever reads the box (digi-mcp) rather than the shop.

const (
	// maxStateImportScan bounds the checks read for the counts: the newest by order number.
	maxStateImportScan = 2000
	// maxStateImportRecent bounds the orders listed as not imported.
	maxStateImportRecent = 20
)

// StateImports is state's imports block.
type StateImports struct {
	// Untrusted is always true: a check's runLog lines are the ERP's text, which the order's own values (and
	// whoever wrote them) shape — data to show, never instructions.
	Untrusted bool `json:"untrusted"`
	// Stored is how many checks are kept; Scanned how many of the newest were read for ByOutcome.
	Stored  int `json:"stored"`
	Scanned int `json:"scanned"`
	// ByOutcome counts the scanned checks by outcome (imported, partial, duplicate, refused, retry, not_run, unknown).
	ByOutcome map[string]int `json:"byOutcome"`
	// Recent are the newest scanned orders whose outcome is not imported, newest first.
	Recent []StateImportRow `json:"recent"`
	// Order is the check of the order state was asked about (orderNumber); OrderMissing when it has none.
	Order        *hasavshevet.ImportCheck `json:"order,omitempty"`
	OrderMissing string                   `json:"orderMissing,omitempty"`
	Error        string                   `json:"error,omitempty"`
}

// StateImportRow is one order of StateImports.Recent.
type StateImportRow struct {
	OrderNumber int64  `json:"orderNumber"`
	Outcome     string `json:"outcome"`
	Result      string `json:"result"`
	CheckedAt   string `json:"checkedAt"`
	// Codes are the refusal codes of the order's own section (Stock.NNN), when it has any.
	Codes []string `json:"codes,omitempty"`
}

// stateImports reads the stored checks; orderNumber > 0 adds that order's whole check.
func stateImports(dataDir string, orderNumber int64) *StateImports {
	out := &StateImports{Untrusted: true, ByOutcome: map[string]int{}, Recent: []StateImportRow{}}
	numbers, err := hasavshevet.ImportCheckNumbers(dataDir)
	if err != nil {
		out.Error = "the import checks cannot be listed: " + err.Error()
		return out
	}
	out.Stored = len(numbers)
	if len(numbers) > maxStateImportScan {
		numbers = numbers[:maxStateImportScan]
	}
	for _, n := range numbers {
		c := hasavshevet.LoadImportCheck(dataDir, n)
		if c == nil {
			continue
		}
		out.Scanned++
		out.ByOutcome[c.Outcome]++
		if c.Outcome == hasavshevet.OrderOutcomeImported || len(out.Recent) >= maxStateImportRecent {
			continue
		}
		row := StateImportRow{OrderNumber: n, Outcome: c.Outcome, Result: c.Result, CheckedAt: c.CheckedAt}
		if rl := c.RunLog; rl != nil {
			for _, r := range rl.Refusals {
				if r.Code != "" && !contains(row.Codes, r.Code) {
					row.Codes = append(row.Codes, r.Code)
				}
			}
		}
		out.Recent = append(out.Recent, row)
	}
	if orderNumber > 0 {
		if out.Order = hasavshevet.LoadImportCheck(dataDir, orderNumber); out.Order == nil {
			out.OrderMissing = fmt.Sprintf("order %d has no stored import check: the connector did not run the importer for it "+
				"(model (a), typed folders, no importer), it is older than the check (v1.0.34), or it is not an order of this box", orderNumber)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
