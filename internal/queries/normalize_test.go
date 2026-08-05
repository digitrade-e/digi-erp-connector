package queries

import (
	"encoding/json"
	"testing"
	"time"
)

// The JSON shape of scanned values is a wire contract with the backend that the
// electron-mssql-app connector used to serve. These tests pin it.
func TestNormalizeScannedNumerics(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		typeName string
		wantJSON string
	}{
		// DECIMAL/MONEY arrive as bytes and must marshal as JSON numbers in the
		// shortest form, exactly as the Node driver's JS numbers stringified:
		// the server's "13085.00" was sent as 13085.
		{"decimal zero", []byte("0.00"), "DECIMAL", `0`},
		{"decimal amount", []byte("13085.00"), "DECIMAL", `13085`},
		{"negative decimal", []byte("-13085.00"), "DECIMAL", `-13085`},
		{"trailing zeros trimmed", []byte("4.000000"), "DECIMAL", `4`},
		{"fraction preserved", []byte("1.5"), "NUMERIC", `1.5`},
		{"money", []byte("42.99"), "MONEY", `42.99`},
		{"money trailing zero", []byte("42.90"), "MONEY", `42.9`},
		{"smallmoney lowercase type", []byte("7.25"), "smallmoney", `7.25`},

		// Non-numeric columns keep string semantics even when they look numeric.
		{"varchar digits stay a string", []byte("00123"), "VARCHAR", `"00123"`},
		{"nvarchar", []byte("hello"), "NVARCHAR", `"hello"`},
		{"unknown column type", []byte("5"), "", `"5"`},

		// A numeric column whose bytes are not a number must not produce
		// invalid JSON.
		{"unparseable numeric falls back to string", []byte("NaN-ish"), "DECIMAL", `"NaN-ish"`},

		// Other driver types pass through untouched.
		{"int64", int64(7), "INT", `7`},
		{"float64", 1.25, "FLOAT", `1.25`},
		{"bool", true, "BIT", `true`},
		{"nil", nil, "DECIMAL", `null`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeScanned(tc.value, tc.typeName)
			b, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tc.wantJSON {
				t.Errorf("normalizeScanned(%v, %q) marshalled to %s, want %s",
					tc.value, tc.typeName, b, tc.wantJSON)
			}
		})
	}
}

// Dates must carry three fractional digits and a Z, matching JS Date.toJSON.
func TestNormalizeScannedTime(t *testing.T) {
	tests := []struct {
		name  string
		value time.Time
		want  string
	}{
		{"midnight UTC", time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), `"2026-03-08T00:00:00.000Z"`},
		{"with millis", time.Date(2021, 12, 31, 23, 59, 58, 123000000, time.UTC), `"2021-12-31T23:59:58.123Z"`},
		{"non-UTC is converted", time.Date(2026, 3, 8, 2, 0, 0, 0, time.FixedZone("IST", 2*60*60)), `"2026-03-08T00:00:00.000Z"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(normalizeScanned(tc.value, "DATETIME"))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != tc.want {
				t.Errorf("got %s, want %s", b, tc.want)
			}
		})
	}
}

// Go's default marshalling is what we are deliberately overriding; if this ever
// starts matching, the override is redundant.
func TestDefaultTimeMarshallingDiffersFromLegacy(t *testing.T) {
	ts := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)
	def, _ := json.Marshal(ts)
	legacy, _ := json.Marshal(normalizeScanned(ts, "DATETIME"))
	if string(def) == string(legacy) {
		t.Errorf("expected the legacy layout to differ from Go's default, both were %s", def)
	}
}
