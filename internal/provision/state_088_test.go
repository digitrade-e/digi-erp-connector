package provision

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/link"
	"github.com/digitrade-e/digi-erp-connector/pkg/linkproto"
)

// Plan 088: state shows managed auto-update — link.yaml's switch as it reads now, the link's last view (the policies it
// holds, what decides, the next check) and the last automatic upgrade's outcome read afresh from its result.
func TestState_ShowsAutoUpgrade(t *testing.T) {
	sys, _, _ := handInstallBox(t)
	_ = os.WriteFile(filepath.Join(sys.dir, "link.yaml"), []byte("box: bfl-read\n"), 0o600)
	_, res, _ := runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), `{}`)
	if au := res.Link.AutoUpgrade; au == nil || !au.Enabled || !strings.Contains(au.Error, "not checked") || au.Policies == nil {
		t.Fatalf("before the link's first check: %+v", au)
	}

	ticket := strings.Repeat("ab", 16)
	scope := linkproto.PolicyScope{Type: linkproto.ScopeRing, Value: "canary"}
	next := time.Date(2026, 11, 17, 2, 10, 0, 0, time.UTC)
	view := link.AutoUpgradeStatus{Enabled: true, Ring: "canary",
		Policies: []link.PolicyStatus{{Scope: scope, Seq: 4, Target: &linkproto.PolicyTarget{Version: "1.0.31", SHA256: strings.Repeat("c", 64)},
			Window: linkproto.PolicyWindow{Days: []string{"mon"}, From: "02:00", To: "05:00"}, HealthMinutes: 15, By: "ilya", KeyID: "team-1", Applies: true}},
		Effective: linkproto.ReportEffective{Scope: &scope, Seq: 4, Version: "1.0.31", Why: linkproto.WhyUpgrading},
		NextCheck: next}
	af := map[string]any{"v": 1, "view": view, "attempts": []map[string]any{{"at": next.Add(-time.Hour), "scope": scope, "seq": 4,
		"from": "1.0.30", "to": "1.0.31", "windowStart": next.Add(-2 * time.Hour), "ticket": ticket, "outcome": "pending"}}}
	b, _ := json.Marshal(af)
	_ = os.MkdirAll(filepath.Join(sys.dir, "link"), 0o700)
	_ = os.WriteFile(filepath.Join(sys.dir, "link", "autoupgrade.json"), b, 0o600)
	result := link.UpgradeResult{Ticket: ticket, From: "1.0.30", To: "1.0.31", Error: "not healthy within 15 min after the setup",
		Health:   &linkproto.UpgradeHealth{Minutes: 15},
		Rollback: &linkproto.UpgradeRollback{To: "1.0.30", OK: true}}
	b, _ = json.Marshal(result)
	_ = os.MkdirAll(filepath.Join(sys.dir, "results"), 0o700)
	_ = os.WriteFile(filepath.Join(sys.dir, "results", "upgrade-"+ticket+".json"), b, 0o600)

	_, res, out := runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), `{}`)
	au := res.Link.AutoUpgrade
	if au == nil || au.Error != "" || len(au.Policies) != 1 || au.Effective.Version != "1.0.31" || !au.NextCheck.Equal(next) ||
		au.Last == nil || au.Last.Ticket != ticket || au.Last.Outcome != linkproto.OutcomeRolledBack || au.Last.Rollback == nil {
		t.Fatalf("%s", out)
	}
	_ = os.WriteFile(filepath.Join(sys.dir, "link.yaml"), []byte("box: bfl-read\nautoUpgrade: false\n"), 0o600)
	_, res, _ = runState(t, stateSys{fakeSys: sys}, testEnv(sys, &fakeDB{}), `{}`)
	if res.Link.AutoUpgrade.Enabled || res.Link.UnknownKeys {
		t.Fatalf("autoUpgrade: false: %+v", res.Link)
	}
}
