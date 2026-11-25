package sap

import "math"

// stockRow is one OITW row: an item in a warehouse.
type stockRow struct {
	OnHand    float64
	Committed float64 // OITW.IsCommited: reserved by open sales orders
	OnOrder   float64 // ordered from suppliers, not received
}

// kitComponent is one stock-carrying line of a sales BOM (OITT.TreeType = 'S'): an inventory item
// (ITT1.Type 4, OITM.InvntItem = 'Y') and how many of it one kit takes (ITT1.Quantity / OITT.Qauntity).
// Text and resource lines and non-inventory items hold no stock and never limit a kit.
type kitComponent struct {
	Code   string
	PerKit float64
}

// kitStock is a sales BOM's stock in one warehouse, in kits: how many whole kits the components on
// hand make (and, for free stock, the components on hand minus committed). A component with no OITW
// row there counts as none in stock. Committed and on-order are the limiting component's, in kits.
// ok is false when the kit has no stock-carrying component: its stock is unknown, not zero.
func kitStock(components []kitComponent, rows map[string]stockRow) (kit stockRow, free float64, ok bool) {
	for _, c := range components {
		if c.PerKit <= 0 {
			continue
		}
		r := rows[c.Code]
		kits := math.Floor(r.OnHand/c.PerKit + 1e-9)
		avail := math.Floor((r.OnHand-r.Committed)/c.PerKit + 1e-9)
		if !ok || kits < kit.OnHand {
			kit = stockRow{OnHand: kits, Committed: r.Committed / c.PerKit, OnOrder: r.OnOrder / c.PerKit}
		}
		if !ok || avail < free {
			free = avail
		}
		ok = true
	}
	return kit, free, ok
}
