package queries

import "testing"

func TestEngineName(t *testing.T) {
	cases := []struct {
		text string
		name string
		ok   bool
	}{
		{"-- digi-engine: sap-price-stock", "sap-price-stock", true},
		{"  -- digi-engine:sap-price-stock \n", "sap-price-stock", true},
		{"-- digi-engine: ", "", false},
		{"-- digi-engine: two words", "", false},
		{"SELECT 1 -- digi-engine: sap-price-stock", "", false},
		{"-- digi-engine: sap-price-stock\nSELECT 1", "", false}, // SQL after the directive stays SQL
		{"-- digi-engine: sap-price-stock\r\nSELECT 1", "", false},
		{"-- digi-engine: sap-price-stock\nGO", "", false},
		{"--digi-engine: sap-price-stock", "", false},
		{"SELECT 1", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		name, ok := EngineName(c.text)
		if name != c.name || ok != c.ok {
			t.Errorf("%q: got %q %v, want %q %v", c.text, name, ok, c.name, c.ok)
		}
	}
}
