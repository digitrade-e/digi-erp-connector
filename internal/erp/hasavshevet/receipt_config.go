package hasavshevet

import (
	"strings"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

// ValidateReceiptConfig is every startup precondition of the receipt keys, in
// one function that both the daemon (api.NewServer) and the GUI save path
// call, so the GUI can never save a config the daemon refuses: sendReceiptDir
// is not sendOrderDir, and every receiptCardNames override can be written in
// the receipt's card-name field.
//
// It reads no disk beyond resolving the two folders' symlinks.
func ValidateReceiptConfig(cfg config.Config) error {
	if err := cfg.ValidateReceipts(); err != nil {
		return err
	}
	return ValidateReceiptCardNames(cardNamesFrom(cfg.ReceiptCardNames))
}

// ReceiptWriterConfigFrom is the writer's view of the connector config;
// dataDir is paths.DataDir(). Paths are trimmed: the validator compared the
// trimmed folder, so that is the one the writer must use.
func ReceiptWriterConfigFrom(cfg config.Config, dataDir string) ReceiptWriterConfig {
	return ReceiptWriterConfig{
		SendReceiptDir:      strings.TrimSpace(cfg.SendReceiptDir),
		DataDir:             dataDir,
		HasReceiptBatFile:   strings.TrimSpace(cfg.HasReceiptBatFile),
		HasExePath:          strings.TrimSpace(cfg.HasExePath),
		HasReceiptParamFile: strings.TrimSpace(cfg.HasReceiptParamFile),
		CardNames:           cardNamesFrom(cfg.ReceiptCardNames),
	}
}

func cardNamesFrom(c config.ReceiptCardNamesConfig) ReceiptCardNames {
	return ReceiptCardNames{Issuer: c.Issuer, Brand: c.Brand}
}
