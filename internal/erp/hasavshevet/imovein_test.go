package hasavshevet

import (
	"bytes"
	"fmt"
	"testing"
)

// TestPRMTotalRecordLength verifies the canonical constant matches the sum of all
// active field lengths (line2..line87, excluding line63=0).
func TestPRMTotalRecordLength(t *testing.T) {
	sum := 0
	for i := 2; i <= 87; i++ {
		key := fmt.Sprintf("line%d", i)
		sum += imoveInFieldLengths[key]
	}
	if sum != prmTotalRecordLength {
		t.Errorf("field length sum = %d, want prmTotalRecordLength = %d", sum, prmTotalRecordLength)
	}
}

// TestPadW1255_ASCII verifies padding and truncation for pure ASCII values.
func TestPadW1255_ASCII(t *testing.T) {
	cases := []struct {
		value string
		width int
		want  []byte
	}{
		{"AB", 5, []byte{'A', 'B', ' ', ' ', ' '}},
		{"ABCDE", 5, []byte{'A', 'B', 'C', 'D', 'E'}},
		{"ABCDEF", 5, []byte{'A', 'B', 'C', 'D', 'E'}}, // truncated
		{"", 3, []byte{' ', ' ', ' '}},
		{"X", 1, []byte{'X'}},
		{"XY", 0, nil}, // zero width → nil
	}
	for _, c := range cases {
		got := padW1255(c.value, c.width)
		if !bytes.Equal(got, c.want) {
			t.Errorf("padW1255(%q, %d) = %v, want %v", c.value, c.width, got, c.want)
		}
	}
}

// TestPadW1255_Length verifies the output is always exactly width bytes.
func TestPadW1255_Length(t *testing.T) {
	tests := []struct {
		value string
		width int
	}{
		{"hello", 10},
		{"שלום עולם", 20},  // Hebrew characters
		{"", 5},
		{"very long string that exceeds the width", 10},
	}
	for _, tt := range tests {
		got := padW1255(tt.value, tt.width)
		if len(got) != tt.width {
			t.Errorf("padW1255(%q, %d) returned %d bytes, want %d", tt.value, tt.width, len(got), tt.width)
		}
	}
}

// TestBuildDOCRecord_Length verifies a DOC record is exactly prmTotalRecordLength bytes.
func TestBuildDOCRecord_Length(t *testing.T) {
	hdr := stockHeader{
		AccountKey:   "ACCT001",
		MyID:         12345,
		DocumentID:   30,
		AccountName:  "Test Customer",
		WareHouse:    1,
		DiscountPrcR: "0.00",
		VatPrc:       "18.00",
		Copies:       "1",
		Currency:     "ILS",
		Rate:         "1.0000",
		ShortDate:    "01/01/2026",
	}
	move := stockMove{
		ItemKey:     "SKU-001",
		ItemName:    "Test Item",
		Quantity:    "2.00",
		Price:       "100.00",
		DiscountPrc: "0.00",
		Unit:        "יח'",
	}

	record := buildDOCRecord(hdr, move)
	if len(record) != prmTotalRecordLength {
		t.Errorf("buildDOCRecord length = %d, want %d", len(record), prmTotalRecordLength)
	}
}

// line33Offset returns the 0-based byte offset of line33 within a DOC record
// (sum of field widths line2..line32, which are written before it).
func line33Offset() int {
	offset := 0
	for i := 2; i <= 32; i++ {
		offset += imoveInFieldLengths[fmt.Sprintf("line%d", i)]
	}
	return offset
}

// TestBuildDOCRecord_Line33Packs verifies the packs count lands in line33 (אריזות)
// and that an unset Packs leaves the field blank (legacy behaviour, backward compat).
func TestBuildDOCRecord_Line33Packs(t *testing.T) {
	hdr := stockHeader{
		MyID: 1, DocumentID: 30, WareHouse: 1, ShortDate: "01/01/2026",
		VatPrc: "18.00", DiscountPrcR: "0.00", Copies: "1", Rate: "1.0000",
	}
	offset := line33Offset()
	width := imoveInFieldLengths["line33"]

	withPacks := buildDOCRecord(hdr, stockMove{
		ItemKey: "1298", ItemName: "X", Quantity: "11.00", Packs: "1.00",
		Price: "51.00", DiscountPrc: "0.00", Unit: "יח'",
	})
	got := string(withPacks[offset : offset+width])
	want := string(padW1255("1.00", width))
	if got != want {
		t.Errorf("line33 with packs = %q, want %q", got, want)
	}

	noPacks := buildDOCRecord(hdr, stockMove{
		ItemKey: "1298", ItemName: "X", Quantity: "11.00", Packs: "",
		Price: "51.00", DiscountPrc: "0.00", Unit: "יח'",
	})
	blank := string(noPacks[offset : offset+width])
	if blank != string(padW1255("", width)) {
		t.Errorf("line33 without packs = %q, want all spaces", blank)
	}
}

// TestGenerateDOC_RowCount verifies generateDOC produces one row per move.
func TestGenerateDOC_RowCount(t *testing.T) {
	hdr := stockHeader{
		MyID:        1,
		DocumentID:  30,
		WareHouse:   1,
		ShortDate:   "01/01/2026",
		VatPrc:      "18.00",
		DiscountPrcR: "0.00",
		Copies:      "1",
		Rate:        "1.0000",
	}
	moves := []stockMove{
		{ItemKey: "A", Quantity: "1.00", Price: "10.00", DiscountPrc: "0.00", Unit: "יח'"},
		{ItemKey: "B", Quantity: "2.00", Price: "20.00", DiscountPrc: "0.00", Unit: "יח'"},
		{ItemKey: "C", Quantity: "3.00", Price: "30.00", DiscountPrc: "0.00", Unit: "יח'"},
	}

	doc, err := generateDOC(hdr, moves)
	if err != nil {
		t.Fatalf("generateDOC error: %v", err)
	}

	// Each row = prmTotalRecordLength bytes + 1 newline
	expectedBytes := len(moves) * (prmTotalRecordLength + 1)
	if len(doc) != expectedBytes {
		t.Errorf("generateDOC byte count = %d, want %d", len(doc), expectedBytes)
	}

	// Every row must end with '\n'
	rowSize := prmTotalRecordLength + 1
	for i := 0; i < len(moves); i++ {
		nl := doc[i*rowSize+prmTotalRecordLength]
		if nl != '\n' {
			t.Errorf("row %d: expected newline at position %d, got 0x%02x", i, i*rowSize+prmTotalRecordLength, nl)
		}
	}
}

// TestGeneratePRM_LineCount verifies the PRM has the expected number of lines (1 + 86).
func TestGeneratePRM_LineCount(t *testing.T) {
	prmBytes := generatePRM()
	if len(prmBytes) == 0 {
		t.Fatal("generatePRM returned empty result")
	}
	// Count newline-delimited lines
	lines := bytes.Count(prmBytes, []byte{'\n'})
	// Line 1 (record length) + lines 2..87 = 87 total newlines
	const want = 87
	if lines != want {
		t.Errorf("generatePRM line count = %d, want %d", lines, want)
	}
}

// TestEncodeWindows1255_Hebrew verifies Hebrew characters are encoded to non-ASCII bytes.
func TestEncodeWindows1255_Hebrew(t *testing.T) {
	// 'א' (aleph) should encode to 0xE0 in Windows-1255
	encoded := encodeWindows1255("א")
	if len(encoded) != 1 {
		t.Fatalf("expected 1 byte for 'א', got %d", len(encoded))
	}
	if encoded[0] != 0xE0 {
		t.Errorf("'א' encoded to 0x%02x, want 0xE0", encoded[0])
	}
}
