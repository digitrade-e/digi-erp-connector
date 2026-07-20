# Printing — Engines, Pitfalls, and Runbook

The post-order print step has been rewritten to survive the realities of
Windows print queues and Windows services. **Read this before debugging a
"order sent but not printed" report.** The wrong assumption costs hours.

## TL;DR

```
Engine preference: PDFtoPrinter.exe → Adobe Reader /t → SumatraPDF -silent
                   ✓ service-safe   ⚠ user session   ⚠ silent failures
```

- **PDFtoPrinter.exe** is the engine of record. It is bundled with the
  installer (alongside `qpdf29.dll` and `resource.dat`) and copied to
  `C:\Program Files\erp-connector\` at install time.
- The two fallbacks exist for machines that (for whatever reason) do not
  have PDFtoPrinter present. **The fallbacks are unreliable from a
  Windows service** — see *Known pitfalls* below.
- `printerName` in `config.yaml` **must not** point at a printer whose
  port starts with `WSD-` when the daemon runs as a Windows service.
  Use the Standard TCP/IP Port equivalent of the same physical printer.

If a print "succeeded" in the log but no paper came out, jump to the
[Decision tree](#decision-tree-print-not-coming-out).

---

## Why three engines

We learned the hard way that no single Windows PDF print tool is
universally reliable. The ranking reflects field experience, not theory:

### 1. PDFtoPrinter.exe (primary)

Freeware by Edward Mendelson (Columbia U.); maintained at
<https://github.com/emendelson/pdftoprinter>; license explicitly permits
redistribution. Designed for unattended/silent printing — does not
initialize a UI subsystem, does not require a window station or
interactive desktop, **works correctly from a Windows service in
session 0**, and surfaces real errors via stderr + exit code.

Invocation: `PDFtoPrinter.exe <pdf> "<Printer Name>"`.

### 2. Adobe Reader / Acrobat /t (fallback)

`/n /s /h /t <pdf> "<printer>"`. Reliable when invoked from an
**interactive user session**. Hangs in **service session 0** because
Adobe Reader DC depends on a real desktop / window station for
initialization, even with `/h /s /n`. The service-account does not
matter — switching from `LocalSystem` to a real user account does not
move the process out of session 0.

Symptom of the session-0 hang: the daemon log shows a 60-second gap
between `print.acrobat exec:` and `print.acrobat returned non-zero
(exit=1, output="")` — the daemon's context timeout killing it.

### 3. SumatraPDF -silent (last resort)

`-silent -print-to "<printer>" <pdf>`. Has a long-standing bug where
the process exits **0 with no error output even when no job is
submitted**, especially on Type 4 / V4 "Class Driver" printers and
Standard TCP/IP Port queues. We keep it only because it is small and
embedded in some legacy installations.

**Never trust SumatraPDF's exit code.** A "success" line of the form
`exec ok (exit=0, output="ParseFlags: ...")` may have produced no
paper at all.

---

## Engine selection at runtime

`internal/print/printer_windows.go → PrintPDF()` does, in order:

1. **Pre-flight printer enumeration** via `EnumPrintersW` — logs every
   printer the calling process can see, with port and driver. Warns
   if the configured `PrinterName` is missing OR uses a `WSD-*` port
   (incompatible with services).
2. **Pick engine**:
   - `detectPDFtoPrinter()` — looks next to the daemon EXE, in the
     working directory, on `PATH`, and in common install locations.
   - If not found: `detectAcrobat()` — `Acrobat.exe` or `AcroRd32.exe`
     in `Program Files` / `Program Files (x86)` / `PATH`. Logs a WARN
     about session-0 unreliability when this path is taken.
   - If neither: `resolveSumatraPDF()`. Logs a WARN.
3. **Run** with a 30 s (Sumatra) or 60 s (Acrobat / PDFtoPrinter)
   context timeout. PDFtoPrinter normally returns in < 5 s.

Each engine has its own log prefix: `print.pdftoprinter`, `print.acrobat`,
`print.sumatra`. Always check which one ran.

---

## Required configuration

### config.yaml

```yaml
pdf:
  printAfterOrder: true
  printerName: "BADIR-NET"     # NOT "BADIR" if BADIR uses a WSD-* port
  sumatraPdfPath: ""           # auto-detected; only fallback engine
  chromePath: ""               # for HTML→PDF rendering, separate concern
```

Stored at `%PROGRAMDATA%\erp-connector\config.yaml`. The GUI (PDF &
Email Settings dialog) writes this file; the daemon must be restarted
to pick up changes.

### Service account

The daemon runs as a Windows service named `erp-connectord`. It can
log on as either:

- `LocalSystem` (default; created by the installer) — sees only
  machine-wide printers; cannot see per-user printers; cannot use WSD
  printers; cannot use Adobe Reader.
- A real user account (e.g. `MBADIR-TS\<username>`) — sees that user's
  printers; **still runs in session 0**, so PDF readers (Adobe,
  Sumatra) still hang or fail; user-installed Standard TCP/IP printers
  work; per-user WSD printers still don't (session 0 has no Function
  Discovery / PNP-X plumbing).

**Recommendation:** keep `LocalSystem` and rely on PDFtoPrinter — it
works in either configuration, and machine-wide TCP/IP printers don't
need user account context.

If a deployment really needs a user account (per-user printer that
can't be reinstalled machine-wide), change it via `services.msc` →
`erp-connectord` → Properties → Log On → "This account". The user
must have "Log on as a service" right (Windows grants it
automatically when you save the dialog).

### Printer port — the WSD trap

WSD ("Web Services for Devices") printer ports look like
`WSD-1d5cf700-6ce6-4659-926f-7417469283ed`. They depend on
session-bound Function Discovery / PNP-X services and **do not work
from a service in session 0**, regardless of which user owns the
service. The print queue accepts the job, the queue drains, and
nothing happens at the device — silent failure.

Physical printers commonly appear in Windows under two names — one
WSD, one "Standard TCP/IP Port":

```
BADIR        port=WSD-1d5cf700-...    ❌ session-0 unsafe
BADIR-NET    port=BADIR-NET-TCP       ✓ same device, TCP/IP RAW 9100
```

Always pick the TCP/IP one for the daemon. The startup log
explicitly prints port type for every visible printer and a WARN if
the configured one is `WSD-*` — see *Operational checks*.

---

## Build & release

### Bundled binaries

The installer ships these files into `C:\Program Files\erp-connector\`:

| File | Purpose | Source |
|---|---|---|
| `erp-connectord.exe` | Daemon | `go build ./cmd/erp-connectord` |
| `erp-connector.exe` | GUI | `go build ./cmd/erp-connector` (with `-H=windowsgui`) |
| `PDFtoPrinter.exe` | Primary print engine | downloaded in CI |
| `qpdf29.dll` | PDFtoPrinter dependency | downloaded in CI |
| `resource.dat` | PDFtoPrinter dependency | downloaded in CI |
| `SumatraPDF.exe` | Fallback engine | already present in legacy installs; not freshly downloaded |
| `icon.ico`, `launch-admin.vbs` | Desktop shortcut helpers | `assets/installer/` |

### Release pipeline

`.github/workflows/release-windows.yml` (triggered by the `auto-tag`
workflow on push to `main`):

1. Checkout, set up Go from `go.mod`, install MinGW + Inno Setup.
2. `go build` both binaries into `dist/bin/`.
3. **Bundle PDFtoPrinter step** — download `PDFtoPrinter.exe`,
   `qpdf29.dll`, `resource.dat` from
   `raw.githubusercontent.com/emendelson/pdftoprinter/main` into
   `dist/bin/`. *Do not use the columbia.edu URL — Columbia returns
   HTTP 403 to scripted curl.*
4. `iscc` runs `assets/installer/erp-connector.iss` to package
   everything into `erp-connector-setup-vX.Y.Z.exe`.
5. `softprops/action-gh-release` publishes the EXE to GitHub Releases.

### Inno Setup script

`assets/installer/erp-connector.iss` — the `[Files]` section pulls
each binary from `{#BuildDir}` (= `dist/bin/`). When adding a new
file the daemon depends on at runtime, add it both to the workflow's
`dist/bin/` and to `[Files]`.

The `[Run]` section creates the Windows service via `sc.exe create`.
This is idempotent — `sc create` of an existing service fails
silently (the `& exit /b 0` swallows the error), so reinstalling
**preserves** the service's current Log On account.

### Local rebuild & deploy (without going through CI)

```powershell
# 1. Build daemon
cd C:\Users\<you>\Documents\erp-connector
go build -trimpath -ldflags "-s -w" -o dist\bin\erp-connectord.exe .\cmd\erp-connectord

# 2. Stop service, copy, start
sc.exe stop erp-connectord
Copy-Item dist\bin\erp-connectord.exe "C:\Program Files\erp-connector\erp-connectord.exe" -Force
sc.exe start erp-connectord

# 3. (One-off) drop PDFtoPrinter binaries next to the daemon
$base = "https://raw.githubusercontent.com/emendelson/pdftoprinter/main"
foreach ($f in "PDFtoPrinter.exe","qpdf29.dll","resource.dat") {
    Invoke-WebRequest "$base/$f" -OutFile "C:\Program Files\erp-connector\$f"
}
```

---

## Operational checks

### Startup log — the smoke test

Every daemon start prints a printer snapshot. Look for these three
lines in `%PROGRAMDATA%\erp-connector\server.log`:

```
[INFO] PDF config snapshot at startup: PrintAfterOrder=true ... PrinterName="BADIR-NET"
[INFO] printers visible to daemon (account=MBADIR-TS\<user>, count=13): ...
[INFO] configured PrinterName="BADIR-NET" resolved to port="BADIR-NET-TCP" driver="..." (service-safe)
```

What to verify:

- **`account=`** — `NT AUTHORITY\SYSTEM` if running as LocalSystem,
  `MBADIR-TS\<user>` if running as a user.
- **`count=`** — should match what the same account sees in
  `Get-Printer`. A LocalSystem service typically sees more printers
  than expected because of RDP-redirected entries; a user-account
  service sees the user's normal list.
- **`(service-safe)`** — present means the configured port is not
  `WSD-*`. If you see a WARN saying the port *is* WSD, the connector
  will not print anything regardless of engine.

### Per-order log — what success looks like

```
[INFO] AfterOrder invoked: order=1000023 ...
[OK]   remote template rendered for order 1000023 (123099 bytes, ...)
[INFO] dispatchPDF start: order=1000023 ... PrinterName="BADIR-NET" SumatraPDFPath=""
[INFO] PDF saved to C:\digiorders\history\1000023\invoice_1000023.pdf
[INFO] calling print.PrintPDF for order 1000023: ...
[INFO] print.PrintPDF: printer "BADIR-NET" resolved (port="BADIR-NET-TCP" driver="...")
[INFO] print.PrintPDF: using PDFtoPrinter (...) — service-safe unattended printer
[INFO] print.pdftoprinter exec: "...PDFtoPrinter.exe" <pdf> BADIR-NET
[INFO] print.pdftoprinter exec ok (exit=0, output="")
[OK]   PDF printed for order 1000023
```

Total time: typically 1–3 seconds end-to-end. Any of these
deviations is a red flag:

- `using Adobe (...)` instead of PDFtoPrinter → PDFtoPrinter binary is
  missing from the install dir, fix by re-deploying.
- `using Sumatra` → both PDFtoPrinter and Adobe missing.
- 60-second gap before exec result → engine hang (Adobe in session 0).
- `print.pdftoprinter exec ok (exit=0, output="")` followed by no
  paper → spooler/printer/network problem; see decision tree.

### Spool-side check (when log says success but no paper)

```powershell
# Are there stuck jobs on the printer?
Get-PrintJob -PrinterName 'BADIR-NET'

# Was the port even reachable?
Test-NetConnection -ComputerName <printer-ip> -Port 9100

# Did the spooler log a port-side failure?
Get-WinEvent -LogName 'Microsoft-Windows-PrintService/Admin' -MaxEvents 30 |
  Where-Object { $_.Message -match 'BADIR' }

# Native test page (bypasses our entire stack — proves queue health)
$p = Get-CimInstance -Class Win32_Printer -Filter "Name='BADIR-NET'"
Invoke-CimMethod -InputObject $p -MethodName PrintTestPage
```

If `PrintTestPage` produces paper but the connector doesn't, the
problem is upstream of the spooler (engine choice, EXE not present,
PDF rendering). If `PrintTestPage` does *not* produce paper, the
problem is the queue, the port, the device, or the network — none
of which the connector can fix.

---

## Decision tree: print not coming out

1. **Find the latest order's print lines in `server.log`.**
   - No `print.PrintPDF` at all? → `PrintAfterOrder` is false in
     config, or the post-order hook isn't registered (Chrome missing
     for PDF generation). Check the *PDF config snapshot at startup*
     line. Fix config, restart daemon.

2. **Which engine ran?** (the `using ...` line)
   - `using PDFtoPrinter` → continue to step 3.
   - `using Adobe` → re-deploy `PDFtoPrinter.exe` + `qpdf29.dll` +
     `resource.dat` next to the daemon EXE; restart service. Adobe
     does not work from session 0.
   - `using Sumatra` → same fix; Sumatra is unreliable.

3. **What was the exit?**
   - `print.pdftoprinter exec ok (exit=0, output="")` → engine
     submitted the job. Move to step 4 (queue diagnostics).
   - `print.pdftoprinter ... exit=N, output="..."` non-empty →
     engine refused. The output usually says why (printer not found,
     PDF unreadable, port unreachable). Treat as authoritative.

4. **Run the spool-side check above.**
   - Native `PrintTestPage` produces paper → mystery, capture the
     queued job (`Get-PrintJob`) and its status. Most likely the
     PDF itself is invalid; open `invoice_<order>.pdf` from
     `<sendOrderDir>\history\<order>\` and verify it renders.
   - Native `PrintTestPage` does **not** produce paper → printer or
     network is broken; not a connector issue.

5. **Pre-flight WARN at startup?**
   - `configured PrinterName=... uses port=WSD-*` → switch to the
     Standard TCP/IP equivalent of the same printer.
   - `configured PrinterName=... is NOT visible to the daemon` →
     printer is installed only for the interactive user; either
     reinstall machine-wide or switch the service to the user
     account that has it.

---

## Code map

```
internal/print/
  printer_windows.go              ← PrintPDF orchestrator + pre-flight
  printer_pdftoprinter_windows.go ← primary engine
  printer_acrobat_windows.go      ← fallback engine
  printer_stub.go                 ← non-Windows stub
  printers_windows.go             ← EnumPrintersW + GetDefaultPrinterW + WSD detection
  printers_stub.go                ← non-Windows stub

cmd/erp-connectord/app.go         ← startup printer enumeration + validation
internal/erp/hasavshevet/pdf_hook.go ← post-order dispatch (PDF → print → email)

.github/workflows/release-windows.yml ← bundles PDFtoPrinter binaries
assets/installer/erp-connector.iss    ← Inno Setup [Files] list
```

When changing print behavior, the test loop is:

1. `go build ./...`
2. `sc.exe stop erp-connectord`
3. Copy new `erp-connectord.exe` to `C:\Program Files\erp-connector\`
4. `sc.exe start erp-connectord`
5. Send a test order through the GUI / API
6. Watch `server.log` for the engine + exit lines
7. Walk to the printer and verify paper

There is no shortcut for step 7. The whole reason this document
exists is that **success in the log is not success at the printer**
unless the engine is PDFtoPrinter and the port is non-WSD.
