//go:build windows

package print

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/logger"
)

// printPDFViaPDFtoPrinter invokes the PDFtoPrinter.exe utility (by Eric Mayer,
// Columbia University — freeware, redistribution allowed). Unlike Adobe Reader
// or SumatraPDF -silent, PDFtoPrinter is purpose-built for unattended /
// service-mode printing: it does not initialize a UI subsystem, does not
// require a window station or an interactive desktop, does not depend on
// per-user profile state, and surfaces real errors via stderr + exit code.
//
// Invocation: PDFtoPrinter.exe <file.pdf> "<Printer Name>"
// Exit 0 → submitted to spooler successfully.
func printPDFViaPDFtoPrinter(ctx context.Context, toolPath, pdfPath, printerName string, log logger.LoggerService) error {
	args := []string{pdfPath}
	if printerName != "" {
		args = append(args, printerName)
	}

	if log != nil {
		log.Info(fmt.Sprintf("print.pdftoprinter exec: %q %s", toolPath, strings.Join(args, " ")))
	}

	printCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(printCtx, toolPath, args...)
	output, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))

	if err != nil {
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return fmt.Errorf("PDFtoPrinter failed (exit=%d, output=%q): %w", exitCode, trimmed, err)
	}

	if log != nil {
		log.Info(fmt.Sprintf("print.pdftoprinter exec ok (exit=%d, output=%q)", cmd.ProcessState.ExitCode(), trimmed))
	}
	return nil
}

// detectPDFtoPrinter searches for PDFtoPrinter.exe alongside the daemon, on
// PATH, and in common install locations. Empty string if not found.
func detectPDFtoPrinter() string {
	if exePath, err := os.Executable(); err == nil {
		dir := filepath.Dir(exePath)
		candidate := filepath.Join(dir, "PDFtoPrinter.exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "PDFtoPrinter.exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	if p, err := exec.LookPath("PDFtoPrinter.exe"); err == nil {
		return p
	}

	for _, root := range []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("LOCALAPPDATA"),
	} {
		if root == "" {
			continue
		}
		for _, sub := range []string{
			filepath.Join(root, "digi-erp-connector", "PDFtoPrinter.exe"),
			filepath.Join(root, "PDFtoPrinter", "PDFtoPrinter.exe"),
		} {
			if info, err := os.Stat(sub); err == nil && !info.IsDir() {
				return sub
			}
		}
	}
	return ""
}
