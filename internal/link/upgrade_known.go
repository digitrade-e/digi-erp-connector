package link

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// The setup digest of the build an upgrade replaces (plan 088 §3: the ticket records "from" with its setup's hash when
// known), which the runner's one rollback requires besides the release's SHA256SUMS.

// releaseOf is a build ("v1.0.31", "1.0.31") as a release number, "" when it is not one.
func releaseOf(build string) string {
	if v, ok := parseVersion(buildRe, build); ok {
		return v.String()
	}
	return ""
}

// knownSetupSHA is the SHA-256 of the setup that installed version on this box, when the box knows it: a ticket that
// installed it (its result; its job for a result older than plan 088), a rollback that installed it again, or a policy
// it holds that targets it. "" when none does: the rollback then rests on the release's SHA256SUMS alone.
func (l *Link) knownSetupSHA(version string) string {
	dir := l.upgradeDataDir()
	entries, _ := os.ReadDir(filepath.Join(dir, upgradeDirs.results))
	type found struct {
		ticket string
		at     time.Time
	}
	var results []found
	for _, e := range entries {
		name := e.Name()
		ticket, ok := strings.CutPrefix(strings.TrimSuffix(name, ".json"), "upgrade-")
		if e.IsDir() || !strings.HasSuffix(name, ".json") || !ok || !ticketRe.MatchString(ticket) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			results = append(results, found{ticket, fi.ModTime()})
		}
	}
	slices.SortFunc(results, func(a, b found) int { return b.at.Compare(a.at) })
	for _, f := range results {
		res, err := readResult(dir, f.ticket)
		if err != nil {
			continue
		}
		if rb := res.Rollback; rb != nil && rb.OK && releaseOf(rb.To) == version && sha256Re.MatchString(rb.SHA256) {
			return rb.SHA256
		}
		if res.Error != "" || releaseOf(res.To) != version {
			continue
		}
		if sha256Re.MatchString(res.SHA256) {
			return res.SHA256
		}
		if job, err := readJob(dir, f.ticket); err == nil && releaseOf(job.To) == version && sha256Re.MatchString(job.SHA256) {
			return job.SHA256
		}
	}
	l.cfg.Store.policyMu.Lock()
	pf, err := l.cfg.Store.loadPolicies()
	l.cfg.Store.policyMu.Unlock()
	if err == nil {
		for _, sp := range pf.Policies {
			if p, _, err := l.cfg.Team.VerifySignedPolicy(sp.Signed, 0); err == nil && p.Target != nil && p.Target.Version == version {
				return p.Target.SHA256
			}
		}
	}
	return ""
}

// readJob reads a ticket's job.json by handle.
func readJob(dataDir, ticket string) (UpgradeJob, error) {
	f, err := dataacl.OpenRead(dataDir, filepath.Join(dataDir, upgradeDirs.tickets, ticket, jobFile))
	if err != nil {
		return UpgradeJob{}, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return UpgradeJob{}, err
	}
	var job UpgradeJob
	err = json.NewDecoder(bytes.NewReader(b)).Decode(&job)
	return job, err
}
