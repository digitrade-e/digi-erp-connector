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

// PrintPDF sends a PDF file to the printer.
//
// Engine selection: Adobe Reader / Acrobat is preferred when available
// (more reliable silent print across V4 / Class drivers and TCP/IP queues),
// falling back to SumatraPDF otherwise. SumatraPDF -silent has been observed
// to exit 0 without producing paper on certain driver+printer combinations,
// so Acrobat-first is the safer default.
//
// If printerName is empty, the system default printer is used.
// log may be nil; when non-nil, diagnostic info is recorded around the call.
func PrintPDF(ctx context.Context, pdfPath, printerName, sumatraPDFPath string, log logger.LoggerService) error {
	// Pre-flight: check the configured printer is actually visible to this
	// process. Both engines silently fail when the printer is invisible to
	// the calling account or uses a service-unsafe port, so logging this
	// up-front makes silent failures diagnosable from logs alone.
	if printerName != "" {
		if printers, perr := EnumeratePrinters(); perr == nil {
			match := FindPrinter(printers, printerName)
			switch {
			case match == nil:
				if log != nil {
					names := make([]string, 0, len(printers))
					for _, p := range printers {
						names = append(names, p.Name)
					}
					log.Warn(fmt.Sprintf(
						"print.PrintPDF: configured printer %q is not visible to this process; visible: [%s]. "+
							"Print will likely fail silently. If running as a service, the printer may be installed only for the interactive user.",
						printerName, strings.Join(names, ", "),
					))
				}
			case IsServiceUnsafePort(match.PortName):
				if log != nil {
					log.Warn(fmt.Sprintf(
						"print.PrintPDF: printer %q uses port %q (WSD). WSD ports do not work from a service running as LocalSystem. Switch to a Standard TCP/IP Port equivalent.",
						printerName, match.PortName,
					))
				}
			default:
				if log != nil {
					log.Info(fmt.Sprintf("print.PrintPDF: printer %q resolved (port=%q driver=%q)", match.Name, match.PortName, match.DriverName))
				}
			}
		} else if log != nil {
			log.Warn(fmt.Sprintf("print.PrintPDF: EnumeratePrinters pre-flight failed: %v", perr))
		}
	}

	// Engine preference (best → worst for service-mode printing):
	//   1. PDFtoPrinter.exe — purpose-built for unattended/service printing,
	//      works in Windows session 0, no user-session dependencies.
	//   2. Adobe Reader / Acrobat — reliable in interactive sessions only;
	//      hangs in session 0 (no desktop / window station) so unsuitable
	//      when the daemon runs as a Windows service.
	//   3. SumatraPDF -silent — exits 0 without producing paper on certain
	//      driver/printer combinations; last-resort fallback.
	if toolPath := detectPDFtoPrinter(); toolPath != "" {
		if log != nil {
			log.Info(fmt.Sprintf("print.PrintPDF: using PDFtoPrinter (%s) — service-safe unattended printer", toolPath))
		}
		return printPDFViaPDFtoPrinter(ctx, toolPath, pdfPath, printerName, log)
	}

	if acrobatPath := detectAcrobat(); acrobatPath != "" {
		if log != nil {
			log.Warn(fmt.Sprintf(
				"print.PrintPDF: PDFtoPrinter not found; falling back to Adobe (%s). Adobe reliably prints in interactive sessions but typically hangs when invoked from a Windows service (session 0).",
				acrobatPath,
			))
		}
		return printPDFViaAcrobat(ctx, acrobatPath, pdfPath, printerName, log)
	}

	if log != nil {
		log.Warn("print.PrintPDF: neither PDFtoPrinter nor Adobe Reader/Acrobat detected; falling back to SumatraPDF (less reliable in -silent mode on some drivers)")
	}
	return printPDFViaSumatra(ctx, pdfPath, printerName, sumatraPDFPath, log)
}

// printPDFViaSumatra is the legacy SumatraPDF path. Kept as a fallback for
// machines that don't have Adobe Reader installed.
func printPDFViaSumatra(ctx context.Context, pdfPath, printerName, sumatraPDFPath string, log logger.LoggerService) error {
	sumatraPath := resolveSumatraPDF(sumatraPDFPath)
	if log != nil {
		printerDisplay := printerName
		if printerDisplay == "" {
			printerDisplay = "<system default>"
		}
		resolvedDisplay := sumatraPath
		if resolvedDisplay == "" {
			resolvedDisplay = "<not found>"
		}
		log.Info(fmt.Sprintf(
			"print.sumatra: pdfPath=%q printer=%s configuredSumatra=%q resolvedSumatra=%s",
			pdfPath, printerDisplay, sumatraPDFPath, resolvedDisplay,
		))
	}
	if sumatraPath == "" {
		return fmt.Errorf("no PDF print engine available: Adobe Reader/Acrobat not installed and SumatraPDF not found")
	}

	args := []string{"-silent"}
	if printerName != "" {
		args = append(args, "-print-to", printerName)
	} else {
		args = append(args, "-print-to-default")
	}
	args = append(args, pdfPath)

	printCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if log != nil {
		log.Info(fmt.Sprintf("print.sumatra exec: %s %s", sumatraPath, strings.Join(args, " ")))
	}

	cmd := exec.CommandContext(printCtx, sumatraPath, args...)
	output, err := cmd.CombinedOutput()
	trimmedOutput := strings.TrimSpace(string(output))
	if err != nil {
		return fmt.Errorf("SumatraPDF print failed (exit: %v, output: %q): %w", cmd.ProcessState.ExitCode(), trimmedOutput, err)
	}
	if log != nil {
		log.Info(fmt.Sprintf("print.sumatra exec ok (exit=%d, output=%q) — note: SumatraPDF returns 0 even on silent failure; verify physical print", cmd.ProcessState.ExitCode(), trimmedOutput))
	}
	return nil
}

// DetectSumatraPDF searches for SumatraPDF.exe in common locations.
func DetectSumatraPDF() string {
	return resolveSumatraPDF("")
}

func resolveSumatraPDF(configPath string) string {
	if configPath != "" {
		if info, err := os.Stat(configPath); err == nil && !info.IsDir() {
			return configPath
		}
	}

	// Search next to our own executable
	if exePath, err := os.Executable(); err == nil {
		dir := filepath.Dir(exePath)
		candidate := filepath.Join(dir, "SumatraPDF.exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}

	// Search working directory
	if wd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(wd, "SumatraPDF.exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}

	// Search PATH
	if p, err := exec.LookPath("SumatraPDF.exe"); err == nil {
		return p
	}
	if p, err := exec.LookPath("SumatraPDF"); err == nil {
		return p
	}

	// Common install locations
	programFiles := os.Getenv("ProgramFiles")
	localAppData := os.Getenv("LOCALAPPDATA")
	candidates := []string{
		filepath.Join(programFiles, "SumatraPDF", "SumatraPDF.exe"),
		filepath.Join(localAppData, "SumatraPDF", "SumatraPDF.exe"),
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}

	return ""
}
