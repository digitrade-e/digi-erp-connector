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

// printPDFViaAcrobat invokes Adobe Reader / Acrobat in silent print mode
// (/n /s /h /t). Adobe's silent print is significantly more reliable than
// SumatraPDF -silent on many Windows configurations: SumatraPDF returns 0
// without producing paper on some V4 / Class drivers and TCP/IP queues,
// whereas Adobe submits the job to the spooler and exits cleanly.
//
// The process is given an aggressive timeout — Adobe Reader DC exits after
// printing in most cases, but if it lingers we force-kill it once the
// spooler has had time to accept the job.
func printPDFViaAcrobat(ctx context.Context, acrobatPath, pdfPath, printerName string, log logger.LoggerService) error {
	if printerName == "" {
		// Adobe's /t requires a printer name. Resolving the system default
		// here so the caller doesn't have to special-case empty.
		def, err := defaultPrinterName()
		if err != nil || def == "" {
			return fmt.Errorf("Adobe Reader requires an explicit printer name and the system default could not be resolved: %w", err)
		}
		printerName = def
	}

	args := []string{"/n", "/s", "/h", "/t", pdfPath, printerName}
	if log != nil {
		log.Info(fmt.Sprintf("print.acrobat exec: %q %s", acrobatPath, strings.Join(args, " ")))
	}

	printCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(printCtx, acrobatPath, args...)
	output, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(output))

	if err != nil {
		// Adobe Reader frequently survives /t and gets killed by the context
		// timeout — that is OK as long as it submitted the job. We treat any
		// non-zero exit as success-with-warning here; the operator can confirm
		// from the spooler log whether the job actually printed.
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		if log != nil {
			log.Warn(fmt.Sprintf(
				"print.acrobat returned non-zero (exit=%d, output=%q, err=%v) — Adobe Reader sometimes does not exit cleanly after /t; the job may still have been submitted. Check the spooler.",
				exitCode, trimmed, err,
			))
		}
		return nil
	}

	if log != nil {
		log.Info(fmt.Sprintf("print.acrobat exec ok (exit=%d, output=%q)", cmd.ProcessState.ExitCode(), trimmed))
	}
	return nil
}

// detectAcrobat returns a path to Adobe Reader / Acrobat if one is installed,
// or "" if none is found. Acrobat is preferred over Reader (Acrobat.exe over
// AcroRd32.exe) when both are present, but either is sufficient for /t.
func detectAcrobat() string {
	candidates := []string{}

	for _, root := range []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
	} {
		if root == "" {
			continue
		}
		candidates = append(candidates,
			filepath.Join(root, "Adobe", "Acrobat DC", "Acrobat", "Acrobat.exe"),
			filepath.Join(root, "Adobe", "Acrobat", "Acrobat.exe"),
			filepath.Join(root, "Adobe", "Acrobat Reader DC", "Reader", "AcroRd32.exe"),
			filepath.Join(root, "Adobe", "Reader 11.0", "Reader", "AcroRd32.exe"),
		)
	}

	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}

	if p, err := exec.LookPath("Acrobat.exe"); err == nil {
		return p
	}
	if p, err := exec.LookPath("AcroRd32.exe"); err == nil {
		return p
	}
	return ""
}
