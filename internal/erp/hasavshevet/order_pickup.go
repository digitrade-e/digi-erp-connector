package hasavshevet

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/platform/atomicfile"
)

// Model (a) for orders (DIG-1297, plan 075 P1-3; the receipts' A-2', DIG-929):
// with sendOrderPickupDir set, the connector writes each order as ONE file in
// that folder and the ERP's own scheduler imports it. On BFL that is WizSched
// reading C:\xampp\htdocs\herp with its static imovein.prm, the same pipeline
// the legacy app feeds, so there is one importer for the company and no race
// with it. The connector writes no .prm there and runs no importer; it never
// replaces a file there; its counter and history copies stay in sendOrderDir.

// PickupFileName is the name an order is published under: the legacy app's,
// IMOVEIN_<n>.doc, and TMP_IMOVEIN_<n>.doc for a return.
//
// ASSUMPTION(DIG-1297): a return keeps the legacy's TMP_ prefix, so BFL's
// WizSched takes it through its digitrade_tmp profile like the legacy's
// returns. Whether a return should arrive as a draft is BFL §8 Q6 (DIG-1288);
// B2B sends no returns today.
func PickupFileName(documentType string, orderNum int64) string {
	name := "IMOVEIN_" + strconv.FormatInt(orderNum, 10) + ".doc"
	if documentType == "RETURN" {
		return "TMP_" + name
	}
	return name
}

// writePickup writes the history copies, then publishes doc into the pickup
// folder. It returns the files written, the published one first.
//
// Errors before the first history copy is opened are ErrOrderNotWritten (the
// key may be retried under the same number) — among them a pickup folder that
// cannot be read or takes no file. A number already taken in the
// pickup folder is final: nothing is written, and writing under it would put
// a second document under the same reference into the books.
func (s *Sender) writePickup(req OrderRequest, orderNum int64, doc, prm []byte) ([]string, error) {
	return s.publishOrder(strings.TrimSpace(s.cfg.SendOrderPickupDir), "the pickup folder", PickupFileName(req.DocumentType, orderNum),
		[]string{"imovein_", "tmp_imovein_"}, orderNum, doc, prm)
}

// writeToFolder publishes an order with a documentRule into its typed folder
// (plan 087) as model (a) does into the pickup folder: the number must be
// free there under the folder's pattern — with and without the temporary
// prefix, any case — the folder must take a file, the history copies keep
// their fixed names, and the file appears whole and never over another.
func (s *Sender) writeToFolder(f *config.FolderConfig, fileName string, orderNum int64, doc, prm []byte) ([]string, error) {
	hasPrefix, stem, err := config.ParseNamePattern(f.NamePattern, f.TempPrefix)
	if err != nil {
		return nil, notWritten(err)
	}
	stems := []string{strings.ToLower(stem)}
	if hasPrefix {
		stems = append(stems, strings.ToLower(f.TempPrefix+stem))
	}
	return s.publishOrder(f.Path, "the folder", fileName, stems, orderNum, doc, prm)
}

// publishOrder writes the history copies, then publishes doc into dir as
// name. stems are the lower-case name stems whose "<stem><n>." marks number n
// as taken in dir.
func (s *Sender) publishOrder(dir, what, name string, stems []string, orderNum int64, doc, prm []byte) ([]string, error) {
	pickupDir := dir
	n := strconv.FormatInt(orderNum, 10)

	taken, err := numberTaken(pickupDir, stems, orderNum)
	if err != nil {
		return nil, notWritten(fmt.Errorf("read %s %s: %w", what, pickupDir, err))
	}
	if taken != "" {
		return nil, fmt.Errorf("order number %d is already used in %s %s (%s): the order counter overlaps numbers written there by someone else — seed it past them; nothing was written", orderNum, what, pickupDir, taken)
	}

	// The folder must take a file before anything is written: a read-only
	// folder or a write the ACL denies, found after the history copies, would
	// make the number final for an order that never reached the scheduler.
	if err := probeWritable(pickupDir, name); err != nil {
		return nil, notWritten(fmt.Errorf("%s %s does not take a file: %w", what, pickupDir, err))
	}

	// The history copies live in the connector's own folder, never in the
	// pickup folder (on a real install it sits under a web root). The queue's
	// recovery and re-queue checks read history/<n>/IMOVEIN_<n>.doc, so the
	// name stays that for a return too.
	historyDir := filepath.Join(s.cfg.SendOrderDir, "history", n)
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		return nil, notWritten(fmt.Errorf("create history dir: %w", err))
	}
	var written []string
	for i, f := range []struct {
		path string
		data []byte
	}{
		{filepath.Join(historyDir, "IMOVEIN_"+n+".doc"), doc},
		{filepath.Join(historyDir, "IMOVEIN_"+n+".prm"), prm},
	} {
		opened, err := writeOrderFile(f.path, f.data)
		if err != nil {
			err = fmt.Errorf("write %s: %w", f.path, err)
			if !opened && i == 0 {
				return nil, notWritten(err)
			}
			return nil, err
		}
		written = append(written, f.path)
	}

	// Last, the file the scheduler reads: whole or not at all, and never over
	// another file. Its temp name (.<name>.tmp-*) does not match the
	// scheduler's wildcard (imovein*.doc).
	dst := filepath.Join(pickupDir, name)
	if err := atomicfile.WriteNew(dst, doc, 0o644); err != nil {
		return nil, fmt.Errorf("publish %s: %w", dst, err)
	}
	return append([]string{dst}, written...), nil
}

// probeWritable creates and removes an empty file in dir under a name the
// scheduler's wildcard does not match (the same form as WriteNew's temp).
func probeWritable(dir, name string) error {
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Remove(tmp)
}

// pickupNumberTaken returns the name of a file in dir that already carries
// order number n — IMOVEIN_<n>.* or TMP_IMOVEIN_<n>.*, any case: an order of
// ours or of the legacy app waiting to be imported, its .prm, or one already
// imported (Masofon renames it to <name>.….MASOFON2.BAK) — or "" when none.
// A file the scheduler has moved away is not seen: seeding the counter past
// every other writer's numbers is what keeps them apart (plan 075 cutover);
// this check only stops the mistake it can see.
func pickupNumberTaken(dir string, orderNum int64) (string, error) {
	return numberTaken(dir, []string{"imovein_", "tmp_imovein_"}, orderNum)
}

// numberTaken is pickupNumberTaken for any name stems (lower case): the name
// of a file in dir starting "<stem><n>." for one of stems, any case, or "".
func numberTaken(dir string, stems []string, orderNum int64) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	n := strconv.FormatInt(orderNum, 10)
	for _, e := range entries {
		lower := strings.ToLower(e.Name())
		for _, stem := range stems {
			if strings.HasPrefix(lower, stem+n+".") {
				return e.Name(), nil
			}
		}
	}
	return "", nil
}
