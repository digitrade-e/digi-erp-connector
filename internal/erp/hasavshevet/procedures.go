package hasavshevet

import (
	"context"
	"database/sql"
	"fmt"
)

// ProcedureResult is one stored procedure EnsureProcedures looked at.
type ProcedureResult struct {
	Name string
	// Created is false when the procedure was already there; an existing one
	// is left exactly as it is.
	Created bool
}

// procedureSet is what price & stock depend on, in install order.
var procedureSet = []struct {
	name   string
	ensure func(context.Context, *sql.DB) (bool, error)
}{
	{"GPRICE_Bulk", EnsureGPriceBulkProcedure},
	{"GetOnHandStockForSkus", EnsureOnHandStockForSkusProcedure},
}

// EnsureProcedures creates the stored procedures price & stock depend on —
// GPRICE_Bulk and GetOnHandStockForSkus — where they are missing. It needs DDL
// rights, so it runs while the connection is still an administrator's,
// before a node moves to a least-privilege login. It stops at the first
// failure and returns what it did up to there.
//
// The GUI's save and digi-erp-connector-provision both call it: one list, one
// order.
func EnsureProcedures(ctx context.Context, dbConn *sql.DB) ([]ProcedureResult, error) {
	out := make([]ProcedureResult, 0, len(procedureSet))
	for _, p := range procedureSet {
		created, err := p.ensure(ctx, dbConn)
		if err != nil {
			return out, fmt.Errorf("install %s: %w", p.name, err)
		}
		out = append(out, ProcedureResult{Name: p.name, Created: created})
	}
	return out, nil
}
