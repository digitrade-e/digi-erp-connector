package hasavshevet

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
// key may be retried under the same number). A number already taken in the
// pickup folder is final: nothing is written, and writing under it would put
// a second document under the same reference into the books.
func (s *Sender) writePickup(req OrderRequest, orderNum int64, doc, prm []byte) ([]string, error) {
	pickupDir := strings.TrimSpace(s.cfg.SendOrderPickupDir)
	n := strconv.FormatInt(orderNum, 10)

	taken, err := pickupNumberTaken(pickupDir, orderNum)
	if err != nil {
		return nil, notWritten(fmt.Errorf("read the pickup folder %s: %w", pickupDir, err))
	}
	if taken != "" {
		return nil, fmt.Errorf("order number %d is already used in the pickup folder %s (%s): the order counter overlaps numbers written there by someone else — seed it past them; nothing was written", orderNum, pickupDir, taken)
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
	// another file. Its temp name (.IMOVEIN_<n>.doc.tmp-*) does not match the
	// scheduler's imovein*.doc.
	dst := filepath.Join(pickupDir, PickupFileName(req.DocumentType, orderNum))
	if err := atomicfile.WriteNew(dst, doc, 0o644); err != nil {
		return nil, fmt.Errorf("publish %s: %w", dst, err)
	}
	return append([]string{dst}, written...), nil
}

// pickupNumberTaken returns the name of a file in dir that already carries
// order number n — IMOVEIN_<n>.* or TMP_IMOVEIN_<n>.*, any case: an order of
// ours or of the legacy app waiting to be imported, its .prm, or one already
// imported (Masofon renames it to <name>.….MASOFON2.BAK) — or "" when none.
// A file the scheduler has moved away is not seen: seeding the counter past
// every other writer's numbers is what keeps them apart (plan 075 cutover);
// this check only stops the mistake it can see.
func pickupNumberTaken(dir string, orderNum int64) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	n := strconv.FormatInt(orderNum, 10)
	for _, e := range entries {
		lower := strings.ToLower(e.Name())
		if strings.HasPrefix(lower, "imovein_"+n+".") || strings.HasPrefix(lower, "tmp_imovein_"+n+".") {
			return e.Name(), nil
		}
	}
	return "", nil
}
