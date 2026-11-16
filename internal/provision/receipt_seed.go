package provision

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/digitrade-e/digi-erp-connector/internal/erp/hasavshevet"
)

// MaxReceiptSeed is the last nine-digit receipt number (KUPAIN F3).
const MaxReceiptSeed = 999999999

// SeedReceipts creates the receipt number store (DataDir\receipts\
// receiptNumber.json) with next as the number the NEXT receipt gets — what
// cutover-seed -receipt-seed N does, which the installer does not ship (plan
// 086 C9). Write-if-absent only: an existing store, valid or damaged, is
// never touched, so a seed can neither lower the numbering nor hide a damaged
// file; and a seed at or below a receipt already written
// (DataDir\history\receipts\<n>) is refused. No pre-image (there was no file)
// and no restart: the receipt writer reads the store on every request.
func SeedReceipts(_ context.Context, env Env, next int64) (*ApplyResult, error) {
	res := &ApplyResult{Steps: []Step{}}
	if next < 1 || next > MaxReceiptSeed {
		return res, refuse(CodeSpecInvalid, "use 1 to 999999999: the number the next receipt gets", "receipt seed %d is out of range", next)
	}
	dir := env.Sys.DataDir()
	unlock, err := lockRuns(dir)
	if err != nil {
		return res, err
	}
	defer unlock()
	store := hasavshevet.ReceiptNumberStorePath(dir)
	if _, err := os.Lstat(store); err == nil {
		return res, refuse(CodeReceiptStoreExists, "nothing to do: the numbering continues from the store (a seed is written once)",
			"the receipt number store exists already")
	} else if !errors.Is(err, os.ErrNotExist) {
		return res, refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the receipt number store: %v", err)
	}
	high, err := highestReceipt(filepath.Join(dir, "history", "receipts"))
	if err != nil {
		return res, refuse(CodeWriteFailed, "run it elevated (as an administrator)", "read the receipt history: %v", err)
	}
	if next <= high {
		return res, refuse(CodeReceiptSeedLow, "seed above the highest receipt already written",
			"receipt %d is already written: a seed of %d would issue its number again", high, next)
	}
	if err := env.Sys.ProtectDataDir(); err != nil {
		return res, refuse(CodeWriteFailed, `see "The service will not start" in docs/troubleshooting.md`,
			"the data directory could not be protected, so nothing was written: %v", err)
	}
	if err := hasavshevet.NewReceiptNumberStore(store).Seed(next); err != nil {
		return res, refuse(CodeWriteFailed, "run it elevated (as an administrator)", "%v", err)
	}
	res.Steps = append(res.Steps, Step{Name: "receipt_seed", Changed: true, Detail: "the next receipt is " + strconv.FormatInt(next, 10)})
	return res, nil
}

// highestReceipt is the highest receipt number with a history copy; 0 for none.
func highestReceipt(historyDir string) (int64, error) {
	entries, err := os.ReadDir(historyDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var high int64
	for _, e := range entries {
		if n, err := strconv.ParseInt(e.Name(), 10, 64); err == nil && n > high {
			high = n
		}
	}
	return high, nil
}
