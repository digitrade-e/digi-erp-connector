package provision

import "github.com/digitrade-e/digi-erp-connector/internal/platform/tcptable"

// The TCP table's address families and row sizes (internal/platform/tcptable, shared with the link's data path).
const (
	afInet      = tcptable.AFInet
	afInet6     = tcptable.AFInet6
	tcpRowSize4 = tcptable.RowSize4
	tcpRowSize6 = tcptable.RowSize6
)

// parseTCPTable reads a MIB_TCPTABLE_OWNER_PID or MIB_TCP6TABLE_OWNER_PID of
// listening sockets: a row count, then the rows.
func parseTCPTable(buf []byte, family uint32) ([]Listener, error) {
	rows, err := tcptable.Parse(buf, family)
	if err != nil {
		return nil, err
	}
	return listenersOf(rows), nil
}

func listenersOf(rows []tcptable.Row) []Listener {
	out := make([]Listener, 0, len(rows))
	for _, r := range rows {
		out = append(out, Listener{Address: r.LocalIP.String(), Port: r.LocalPort, PID: r.PID})
	}
	return out
}
