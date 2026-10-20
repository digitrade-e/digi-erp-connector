package hasavshevet

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
)

// The GUI's save and digi-erp-connector-provision install the same two
// procedures, in this order.
func TestEnsureProcedures_TheSetAndItsOrder(t *testing.T) {
	var names []string
	for _, p := range procedureSet {
		names = append(names, p.name)
	}
	if want := []string{"GPRICE_Bulk", "GetOnHandStockForSkus"}; !slices.Equal(names, want) {
		t.Fatalf("procedures = %v, want %v", names, want)
	}
}

// A failure stops the run there, names the procedure, and keeps what was
// done before it — the caller reports both.
func TestEnsureProcedures_StopsAtTheFirstFailure(t *testing.T) {
	saved := procedureSet
	t.Cleanup(func() { procedureSet = saved })

	var called []string
	step := func(name string, created bool, err error) func(context.Context, *sql.DB) (bool, error) {
		return func(context.Context, *sql.DB) (bool, error) {
			called = append(called, name)
			return created, err
		}
	}
	procedureSet = []struct {
		name   string
		ensure func(context.Context, *sql.DB) (bool, error)
	}{
		{"A", step("A", true, nil)},
		{"B", step("B", false, errors.New("permission denied"))},
		{"C", step("C", true, nil)},
	}

	got, err := EnsureProcedures(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "install B") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want it to name B and its cause", err)
	}
	if want := []ProcedureResult{{Name: "A", Created: true}}; !slices.Equal(got, want) {
		t.Errorf("results = %v, want %v", got, want)
	}
	if !slices.Equal(called, []string{"A", "B"}) {
		t.Errorf("called = %v: nothing runs after a failure", called)
	}
}
