package sap

import "testing"

func TestKitStock(t *testing.T) {
	rows := map[string]stockRow{
		"LAMP":   {OnHand: 9, Committed: 3, OnOrder: 4},
		"DRIVER": {OnHand: 10, Committed: 0, OnOrder: 0},
	}
	// v1 took the smallest component on hand (9) and ignored that a kit needs two lamps.
	kit, avail, ok := kitStock([]kitComponent{{Code: "LAMP", PerKit: 2}, {Code: "DRIVER", PerKit: 1}}, rows)
	if !ok || kit.OnHand != 4 || avail != 3 {
		t.Fatalf("got kit %+v free %v ok %v", kit, avail, ok)
	}
	if kit.Committed != 1.5 || kit.OnOrder != 2 { // the limiting component's, in kits
		t.Fatalf("committed / on order: %+v", kit)
	}
}

func TestKitStock_MissingRowIsNone(t *testing.T) {
	kit, avail, ok := kitStock([]kitComponent{{Code: "A", PerKit: 1}, {Code: "B", PerKit: 1}}, map[string]stockRow{"A": {OnHand: 5}})
	if !ok || kit.OnHand != 0 || avail != 0 {
		t.Fatalf("got %+v %v %v", kit, avail, ok)
	}
}

func TestKitStock_NoStockCarryingComponent(t *testing.T) {
	if _, _, ok := kitStock(nil, nil); ok {
		t.Fatal("a kit with no component has unknown stock")
	}
	if _, _, ok := kitStock([]kitComponent{{Code: "A", PerKit: 0}}, map[string]stockRow{"A": {OnHand: 5}}); ok {
		t.Fatal("a zero quantity line cannot limit a kit")
	}
}

func TestKitStock_FractionalQuantities(t *testing.T) {
	// 0.1 + 0.2 style floating error must not lose a whole kit.
	kit, _, ok := kitStock([]kitComponent{{Code: "CABLE", PerKit: 0.3}}, map[string]stockRow{"CABLE": {OnHand: 0.9}})
	if !ok || kit.OnHand != 3 {
		t.Fatalf("got %+v", kit)
	}
}
