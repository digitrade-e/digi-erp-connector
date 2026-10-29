package provision

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

func TestSpecValidate_OptionsRefusals(t *testing.T) {
	orders := filepath.Join(os.TempDir(), "orders")
	abs := filepath.Join(os.TempDir(), "images")
	tests := []struct {
		name string
		opts OptionsSpec
	}{
		{"a relative image folder", OptionsSpec{ImageFolders: &[]string{`images\products`}}},
		{"an empty image folder", OptionsSpec{ImageFolders: &[]string{" "}}},
		{"an image folder twice", OptionsSpec{ImageFolders: &[]string{abs, abs + string(filepath.Separator)}}},
		{"too many image folders", OptionsSpec{ImageFolders: ptr(slices.Repeat([]string{abs}, MaxImageFolders+1))}},
		{"an erpUser with a newline", OptionsSpec{ERPUser: ptr("39\n")}},
		{"an erpUser too long", OptionsSpec{ERPUser: ptr(strings.Repeat("u", MaxERPUser+1))}},
		{"a zero query timeout", OptionsSpec{QueryTimeoutSeconds: ptr(0)}},
		{"a query timeout too long", OptionsSpec{QueryTimeoutSeconds: ptr(MaxQueryTimeoutSecs + 1)}},
		{"zero rows", OptionsSpec{QueryMaxRows: ptr(0)}},
		{"too many rows", OptionsSpec{QueryMaxRows: ptr(MaxQueryRows + 1)}},
		{"a token lifetime that is not a duration", OptionsSpec{TokenTTL: ptr("30 minutes")}},
		{"a token lifetime too short", OptionsSpec{TokenTTL: ptr("1m")}},
		{"a token lifetime too long", OptionsSpec{TokenTTL: ptr("48h")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := handInstallSpec(orders)
			o := tc.opts
			s.Options = &o
			r := s.Validate()
			if r == nil || r.Code != CodeSpecInvalid {
				t.Fatalf("got %v, want %s", r, CodeSpecInvalid)
			}
			if r.Fix == "" {
				t.Error("a refusal without a fix line")
			}
		})
	}
	s := handInstallSpec(orders)
	s.Options = &OptionsSpec{ImageFolders: &[]string{}, ERPUser: ptr(""), Debug: ptr(true),
		QueryTimeoutSeconds: ptr(60), QueryMaxRows: ptr(500), TokenTTL: ptr("")}
	if r := s.Validate(); r != nil {
		t.Fatalf("valid options refused: %v", r)
	}
}

// Every option is written when given and kept when left out.
func TestBuildTarget_Options(t *testing.T) {
	orders := filepath.Join(os.TempDir(), "orders")
	img := filepath.Join(os.TempDir(), "img")
	cur := handInstallConfig(orders)
	cur.ImageFolders = []string{img}
	cur.ERPUser = "39"
	cur.Queries.TimeoutSeconds, cur.Queries.MaxRows = 30, 10000
	cur.Auth.TokenTTL = "1h"

	kept := buildTarget(cur, handInstallSpec(orders), t.TempDir(), DefaultLogin)
	if !slices.Equal(kept.ImageFolders, cur.ImageFolders) || kept.ERPUser != "39" || kept.Debug ||
		kept.Queries != cur.Queries || kept.Auth.TokenTTL != "1h" {
		t.Errorf("options left out were not kept: %+v", kept)
	}

	s := handInstallSpec(orders)
	s.Options = &OptionsSpec{ImageFolders: &[]string{}, ERPUser: ptr(" 41 "), Debug: ptr(true),
		QueryTimeoutSeconds: ptr(90), QueryMaxRows: ptr(2000), TokenTTL: ptr("")}
	set := buildTarget(cur, s, t.TempDir(), DefaultLogin)
	if len(set.ImageFolders) != 0 || set.ImageFolders == nil || set.ERPUser != "41" || !set.Debug ||
		set.Queries.TimeoutSeconds != 90 || set.Queries.MaxRows != 2000 || set.Auth.TokenTTL != "" {
		t.Errorf("options given were not written: %+v", set)
	}
}

func TestApply_WritesOptions(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	img := t.TempDir()
	spec := handInstallSpec(orders)
	spec.Options = &OptionsSpec{ImageFolders: &[]string{img}, Debug: ptr(true), QueryMaxRows: ptr(5000), TokenTTL: ptr("2h")}
	if _, err := Apply(context.Background(), testEnv(sys, fdb), spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cfg, _ := readConfig(t, sys.dir)
	if !slices.Equal(cfg.ImageFolders, []string{filepath.Clean(img)}) || !cfg.Debug || cfg.Queries.MaxRows != 5000 || cfg.Auth.TokenTTL != "2h" {
		t.Errorf("config = %+v", cfg)
	}
}

func TestApply_RefusesAMissingImageFolder(t *testing.T) {
	sys, fdb, orders := handInstallBox(t)
	spec := handInstallSpec(orders)
	spec.Options = &OptionsSpec{ImageFolders: &[]string{filepath.Join(orders, "no-such-folder")}}
	_, err := Apply(context.Background(), testEnv(sys, fdb), spec)
	wantRefusal(t, err, CodeImageFolderMissing)
	if _, raw := readConfig(t, sys.dir); strings.Contains(string(raw), "no-such-folder") {
		t.Error("config.yaml was written on a refusal")
	}
}

func TestConfigDiff_NamesOptions(t *testing.T) {
	orders := filepath.Join(os.TempDir(), "orders")
	cur := handInstallConfig(orders)
	tgt := cur
	tgt.Debug = !cur.Debug
	if got := configDiff(cur, tgt); got != "changed: options" {
		t.Errorf("configDiff = %q", got)
	}
}
