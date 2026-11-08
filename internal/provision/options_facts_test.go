package provision

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// Contract 0 (plan 068 §13.5.1): discover reports connector.config.options,
// defaults written out, so digi-mcp's plan shows "was -> becomes", not "? -> new".
func TestConfigFacts_OptionsDefaultsWrittenOut(t *testing.T) {
	cfg := config.Default()
	cfg.ImageFolders = nil
	cfg.Queries = config.QueriesConfig{}

	o := configFacts(cfg, nil).Options
	if o == nil {
		t.Fatal("options missing from the config facts")
	}
	if o.ImageFolders == nil || len(o.ImageFolders) != 0 {
		t.Errorf("imageFolders = %#v, want an empty list, never null", o.ImageFolders)
	}
	if o.QueryTimeoutSeconds != 30 || o.QueryMaxRows != 10000 {
		t.Errorf("query limits = %d s / %d rows, want the daemon's defaults 30 / 10000", o.QueryTimeoutSeconds, o.QueryMaxRows)
	}
	if o.TokenTTL != "" {
		t.Errorf("tokenTTL = %q, want \"\" when unset", o.TokenTTL)
	}
}

func TestConfigFacts_OptionsAsSet(t *testing.T) {
	cfg := config.Default()
	cfg.ImageFolders = []string{`D:\תמונות`, `D:\img`}
	cfg.ERPUser = "39"
	cfg.Debug = true
	cfg.Queries = config.QueriesConfig{TimeoutSeconds: 120, MaxRows: 50000}
	cfg.Auth.TokenTTL = " 1h "

	o := configFacts(cfg, nil).Options
	want := OptionsFacts{
		ImageFolders: []string{`D:\תמונות`, `D:\img`}, ERPUser: "39", Debug: true,
		QueryTimeoutSeconds: 120, QueryMaxRows: 50000, TokenTTL: "1h",
	}
	if !slices.Equal(o.ImageFolders, want.ImageFolders) || o.ERPUser != want.ERPUser || o.Debug != want.Debug ||
		o.QueryTimeoutSeconds != want.QueryTimeoutSeconds || o.QueryMaxRows != want.QueryMaxRows || o.TokenTTL != want.TokenTTL {
		t.Errorf("options = %+v, want %+v", *o, want)
	}

	// The facts must not alias the config's slice.
	cfg.ImageFolders[0] = "changed"
	if o.ImageFolders[0] == "changed" {
		t.Error("options.imageFolders aliases the config's slice")
	}
}

// The wire shape digi-mcp decodes (onprem.ConfigFacts.Options), through the
// ASCII-only envelope: a Hebrew folder name must come back intact.
func TestConfigFacts_OptionsWireShapeASCII(t *testing.T) {
	cfg := config.Default()
	cfg.ImageFolders = []string{`D:\תמונות`}

	b, err := json.Marshal(configFacts(cfg, nil))
	if err != nil {
		t.Fatal(err)
	}
	out := asciiOnly(b)
	for _, c := range out {
		if c > 0x7f {
			t.Fatalf("non-ASCII byte in the envelope: %q", out)
		}
	}
	var wire struct {
		Options *struct {
			ImageFolders        []string `json:"imageFolders"`
			ERPUser             *string  `json:"erpUser"`
			Debug               *bool    `json:"debug"`
			QueryTimeoutSeconds *int     `json:"queryTimeoutSeconds"`
			QueryMaxRows        *int     `json:"queryMaxRows"`
			TokenTTL            *string  `json:"tokenTTL"`
		} `json:"options"`
	}
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatal(err)
	}
	o := wire.Options
	if o == nil || o.ERPUser == nil || o.Debug == nil || o.QueryTimeoutSeconds == nil || o.QueryMaxRows == nil || o.TokenTTL == nil {
		t.Fatalf("a contract key is missing: %s", out)
	}
	if len(o.ImageFolders) != 1 || o.ImageFolders[0] != `D:\תמונות` {
		t.Errorf("imageFolders = %q after the ASCII envelope, want the Hebrew name intact", o.ImageFolders)
	}
}
