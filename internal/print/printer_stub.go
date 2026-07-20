//go:build !windows

package print

import (
	"context"
	"errors"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

// PrintPDF is not supported on non-Windows platforms.
func PrintPDF(_ context.Context, _, _, _ string, _ logger.LoggerService) error {
	return errors.New("printing is only supported on Windows")
}

// DetectSumatraPDF always returns empty on non-Windows.
func DetectSumatraPDF() string {
	return ""
}
