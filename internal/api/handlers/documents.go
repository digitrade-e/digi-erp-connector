package handlers

import (
	"net/http"

	"github.com/digitrade-e/digi-erp-connector/internal/api/dto"
	"github.com/digitrade-e/digi-erp-connector/internal/api/respond"
	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// Plan 087 (DIG-1563): the read routes of named counters and typed folders.
// Both are read-only: a counter or a folder changes only through the signed
// provision commands counters and documents (digi-mcp), never on the data
// path, whose credential the B2B shop holds too.

// CountersReport is the counters as the box reads them now.
type CountersReport func() []hasavshevet.CounterStatus

// NewCountersHandler is GET /api/counters: [{key, kind, min, max, last,
// highestIssued, remaining, test}] (+ retired, error when they apply), one
// meaning of last (the last number issued) for both kinds.
func NewCountersHandler(report CountersReport) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		out := []dto.Counter{}
		for _, c := range report() {
			out = append(out, dto.Counter{Key: c.Key, Kind: c.Kind, Min: c.Min, Max: c.Max, Last: c.Last,
				HighestIssued: c.HighestIssued, Remaining: c.Remaining, Test: c.Test, Retired: c.Retired, Error: c.Error})
		}
		respond.JSON(w, http.StatusOK, out)
	}
}

// DocumentRuleContract is the documentRule version this connector reads.
const DocumentRuleContract = 1

// NewDocumentCapabilitiesHandler is GET /api/documents/capabilities: the
// contract version, the typed folders and the counters a documentRule may
// name (retired counters are not: they issue no number), and the name
// pattern's placeholders.
func NewDocumentCapabilitiesHandler(cfg config.Config, counters *hasavshevet.CounterSet) http.HandlerFunc {
	out := dto.DocumentCapabilities{Contract: DocumentRuleContract, Folders: []dto.DocumentKeyEntry{}, Counters: []dto.DocumentKeyEntry{},
		Placeholders: []string{"prefix", "n"}}
	for _, k := range cfg.FolderKeys() {
		f := cfg.Documents.Folders[k]
		out.Folders = append(out.Folders, dto.DocumentKeyEntry{Key: k, Kind: f.Kind, Test: f.Test})
	}
	for _, c := range counters.All() {
		if !c.Retired {
			out.Counters = append(out.Counters, dto.DocumentKeyEntry{Key: c.Key, Kind: c.Kind, Test: c.Test})
		}
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		respond.JSON(w, http.StatusOK, out)
	}
}
