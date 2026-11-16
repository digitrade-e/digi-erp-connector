package hasavshevet

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/dataacl"
)

// Plan 087 review M1 and R3: who can write to the folders the connector
// writes documents into, as the daemon sees it while it runs.
//
// Behind the link a typed folder (the documents command's) that accounts
// besides the administrators — and documents.trustedWriters — can write to
// gets nothing written: an order whose rule names it, and a receipt when it
// is the bound receipt folder, answer 503 FOLDER_WRITABLE with nothing
// reserved. The folders that predate plan 087 (sendOrderDir, the pickup
// folder, sendReceiptDir) are only warned about, as off the link every folder
// is: refusing them would stop the orders of every box upgraded from v1.0.29.
//
// R3: the ACLs are read off the start's critical path — a slow domain
// controller at boot (resolving whether an owner is an administrator) can
// neither hang the service's 30 s start nor leave a 503 until a restart:
//   - until its first check a guarded folder counts as refused (fail closed);
//     the first check runs right after the start, its progress logged;
//   - a refused folder is read again lazily — when an order or a receipt
//     needs it — at most once per FolderRecheckRefused;
//   - an allowed folder is read again every FolderRecheckAllowed (an
//     administrator may open one up while the service runs);
//   - a provision command that changes the folders or their ACL (documents,
//     folder-acl) restarts the service, which starts a fresh guard.
// Reads run in the background: Allowed never waits on one.

// The guard's re-check intervals.
const (
	FolderRecheckRefused = time.Minute
	FolderRecheckAllowed = 10 * time.Minute
)

// FolderAllower is what the order queue and the receipt writer ask before
// writing into a typed folder: nil, or ErrFolderWritable with why.
type FolderAllower interface {
	Allowed(key string) error
}

type guardState int

const (
	guardUnknown guardState = iota
	guardAllowed
	guardRefused
)

type guardedFolder struct {
	f        config.WriteFolder
	enforced bool // behind the link and typed: refused while writable or unknown
	state    guardState
	why      string
	checked  time.Time
	checking bool
}

// FolderGuard is the daemon's view of the document folders' writers.
type FolderGuard struct {
	writers func(dir string) ([]dataacl.Writer, error)
	trusted map[string]bool
	log     logger.LoggerService
	now     func() time.Time

	mu      sync.Mutex
	folders map[string]*guardedFolder // by typed key, or by name for the pre-087 folders
	order   []string
	wg      sync.WaitGroup // lazy re-checks in flight (tests wait on them)
}

// NewFolderGuard guards cfg's document folders (config.WriteFolders); linked
// says the box is behind the link. writers is dataacl.FolderWriters (tests
// pass a fake). Nothing is read until Run (or a test's checkAll).
func NewFolderGuard(cfg config.Config, linked bool, writers func(string) ([]dataacl.Writer, error), log logger.LoggerService) *FolderGuard {
	g := &FolderGuard{writers: writers, trusted: cfg.TrustedSIDs(), log: log, now: time.Now, folders: map[string]*guardedFolder{}}
	for _, f := range cfg.WriteFolders() {
		key := f.Key
		if !f.Typed {
			key = f.Name
		}
		g.folders[key] = &guardedFolder{f: f, enforced: linked && f.Typed}
		g.order = append(g.order, key)
	}
	return g
}

// Allowed is nil when the connector may write into the typed folder key: not
// behind the link, not a guarded folder, or read as the administrators' only.
// Otherwise ErrFolderWritable with why — "not read yet" before the first
// check — and, when the last read is FolderRecheckRefused old, a new read
// starts in the background. It never waits on a read.
func (g *FolderGuard) Allowed(key string) error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.folders[key]
	if !ok || !f.enforced || f.state == guardAllowed {
		return nil
	}
	why := f.why
	if f.state == guardUnknown {
		why = "who can write to it has not been read yet since the start"
	}
	if f.state == guardRefused && !f.checking && g.now().Sub(f.checked) >= FolderRecheckRefused {
		g.startCheckLocked(key)
	}
	return fmt.Errorf("%w: folder %s (%s) — %s; nothing reserved", ErrFolderWritable, key, f.f.Path, why)
}

// Run reads every folder once, now, then re-reads allowed ones every
// FolderRecheckAllowed until ctx ends.
func (g *FolderGuard) Run(ctx context.Context) {
	g.checkAll()
	t := time.NewTicker(FolderRecheckRefused)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.tick()
		}
	}
}

// checkAll is the first read of every folder, with its progress logged.
func (g *FolderGuard) checkAll() {
	g.mu.Lock()
	keys := append([]string(nil), g.order...)
	for _, k := range keys {
		g.folders[k].checking = true
	}
	g.mu.Unlock()
	if len(keys) == 0 {
		return
	}
	start := g.now()
	g.log.Info(fmt.Sprintf("FOLDER CHECK: reading who can write to %d document folder(s); behind the link a typed folder is refused until it is read", len(keys)))
	for _, k := range keys {
		g.check(k)
	}
	g.mu.Lock()
	allowed, refused := 0, 0
	for _, k := range keys {
		if f := g.folders[k]; f.enforced && f.state == guardRefused {
			refused++
		} else if f.enforced {
			allowed++
		}
	}
	g.mu.Unlock()
	g.log.Info(fmt.Sprintf("FOLDER CHECK: done in %s — typed folders behind the link: %d allowed, %d refused", g.now().Sub(start).Round(time.Millisecond), allowed, refused))
}

// tick re-reads the allowed folders whose last read is FolderRecheckAllowed
// old (refused ones are re-read lazily, by Allowed).
func (g *FolderGuard) tick() {
	g.mu.Lock()
	var due []string
	for _, k := range g.order {
		f := g.folders[k]
		if f.state == guardAllowed && !f.checking && g.now().Sub(f.checked) >= FolderRecheckAllowed {
			f.checking = true
			due = append(due, k)
		}
	}
	g.mu.Unlock()
	for _, k := range due {
		g.check(k)
	}
}

// startCheckLocked reads folder key again in the background. Caller holds mu.
func (g *FolderGuard) startCheckLocked(key string) {
	g.folders[key].checking = true
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		g.check(key)
	}()
}

// check reads folder key's writers (outside the lock: it may wait on the
// domain controller) and records the result, logging what changed.
func (g *FolderGuard) check(key string) {
	g.mu.Lock()
	f := g.folders[key]
	path, name, enforced, before := f.f.Path, f.f.Name, f.enforced, f.state
	g.mu.Unlock()

	start := g.now()
	all, err := g.writers(path)
	took := g.now().Sub(start)
	var who []string
	for _, w := range all {
		if !g.trusted[w.SID] {
			who = append(who, w.Name)
		}
	}
	state, why := guardAllowed, ""
	switch {
	case err != nil:
		state, why = guardRefused, fmt.Sprintf("who can write to it could not be read (%v)", err)
	case len(who) > 0:
		state, why = guardRefused, "it can be written by "+strings.Join(who, ", ")
	}

	g.mu.Lock()
	f.state, f.why, f.checked, f.checking = state, why, g.now(), false
	g.mu.Unlock()

	if took >= 2*time.Second {
		g.log.Warn(fmt.Sprintf("FOLDER CHECK: reading who can write to %s (%s) took %s", name, path, took.Round(time.Millisecond)))
	}
	if state == before && before != guardUnknown {
		return // only what changes is logged
	}
	switch {
	case state == guardAllowed && before == guardRefused:
		g.log.Info(fmt.Sprintf("FOLDER ALLOWED: %s (%s) is the administrators' only again: documents are written there", name, path))
	case state == guardAllowed:
	case enforced:
		g.log.Error(fmt.Sprintf("FOLDER REFUSED: %s (%s): %s, and this box is behind the link: nothing is written there (503 FOLDER_WRITABLE) "+
			"until only the administrators can write to it — lock it down (the folder-acl command) or trust the ERP's account (documents' trustedWriters); "+
			"it is read again at most once a minute when an order or a receipt needs it (docs/troubleshooting.md § A folder others can write to).",
			name, path, why), nil)
	default:
		g.log.Warn(fmt.Sprintf("FOLDER WRITABLE: %s (%s): %s. Whoever can add a file there can pre-create the next document's name, "+
			"drop a document the importer takes into the books, or plant what an importer running there loads; let only the "+
			"administrators write to it (docs/troubleshooting.md § A folder others can write to).", name, path, why))
	}
}

// ReceiptCheck is the receipt writer's FolderCheck for cfg: the bound typed
// receipt folder's Allowed; nil when receipts go elsewhere (sendReceiptDir
// is only warned about).
func (g *FolderGuard) ReceiptCheck(cfg config.Config) func() error {
	k := strings.TrimSpace(cfg.Documents.Receipts.Folder)
	if g == nil || k == "" {
		return nil
	}
	return func() error { return g.Allowed(k) }
}

// staticRefusals is a FolderAllower that refuses fixed folders (tests).
type staticRefusals map[string]string

func (s staticRefusals) Allowed(key string) error {
	if why, ok := s[key]; ok {
		return fmt.Errorf("%w: folder %s — %s; nothing reserved", ErrFolderWritable, key, why)
	}
	return nil
}
