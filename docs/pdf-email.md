# PDF Generation, Auto-Print & Email

## Overview

After each successful `sendOrder`, the daemon can automatically:
1. **Generate** a PDF invoice from the order data using headless Chrome.
2. **Print** the PDF to a local printer. For the engine ranking
   (`PDFtoPrinter` → Adobe → SumatraPDF), Windows session-0 pitfalls,
   and the runbook for "did not print" reports, see **[`printing.md`](printing.md)**.
3. **Email** the PDF as an attachment via SMTP.

All three are optional and independently toggled in config.

---

## Configuration

Set in `%PROGRAMDATA%\digi-erp-connector\config.yaml` (or via the GUI → **PDF & Email Settings…**):

```yaml
pdf:
  # Company branding (shown on every invoice)
  companyName:    "My Company Ltd."
  companyAddress: "123 Main St, Tel Aviv"
  companyPhone:   "03-1234567"
  companyFax:     "03-7654321"
  companyEmail:   "office@mycompany.co.il"
  logoPath:       'C:\images\logo.png'   # PNG / JPEG / GIF / WebP / BMP
  footerHTML:     "Thanks for your order!"

  # PDF engine
  chromePath:     ""   # auto-detected if empty
  sumatraPdfPath: ""   # auto-detected if empty

  # Print
  printAfterOrder: true
  printerName:     ""  # empty = Windows default printer

  # Email
  emailAfterOrder: true

smtp:
  host:        "smtp.gmail.com"
  port:        587          # default 587
  user:        "me@gmail.com"
  fromAddress: "me@gmail.com"
  useTLS:      true
  # password: stored in OS secrets (Windows DPAPI) — set via GUI, never in YAML
```

### SMTP password

The SMTP password is **never stored in the YAML file**. It is encrypted via Windows DPAPI
(`secrets/` package) and stored under the key `smtp_password`. Set it once via the
GUI's "SMTP Password" field and click Save; it persists across restarts.

---

## PDF engine requirements

| Component | Purpose | Auto-detected locations |
|-----------|---------|------------------------|
| **Chrome / Chromium** | HTML → PDF rendering | `Program Files`, `Program Files (x86)`, `LocalAppData`, `PATH`, exe dir |
| **PDFtoPrinter / Acrobat / SumatraPDF** | Silent printing | see [`printing.md`](printing.md) |

Set `chromePath` explicitly in config if auto-detection fails. Print
engines are detected at runtime; bundled `PDFtoPrinter.exe` is the
primary engine.

---

## PDF content

The invoice PDF is rendered from an RTL Hebrew HTML template (`internal/pdf/templates/invoice.html`)
embedded at compile time. It includes:

- Company header: name, address, phone, fax, email, logo
- Document number and date
- Customer name, phone, company
- Line items table: SKU, description, quantity, unit price, discount, total
- Totals block: before discount, discount %, after discount, VAT (17%), total due
- Footer (custom HTML or default thank-you text)

Font: **NotoSansHebrew** (embedded as a base64 data URI — no external font loading required).

### Logo rendering

The logo is read from `logoPath` at generation time, base64-encoded, and embedded as a
`data:` URI in the HTML. Any common image format is supported (MIME type is detected from
file content, not extension).

**Important:** The logo field in the HTML template must be typed `template.URL` (not `string`)
to bypass Go's `html/template` URL safety filter, which silently replaces `data:` URIs with
`#ZgotmplZ`. See `internal/pdf/template.go → InvoiceData.LogoDataURI`.

---

## Chrome rendering approach

The HTML is written to a temporary file (`os.TempDir()/erp_invoice_*.html`) and Chrome
navigates to it via `file://` URL. **Do not** navigate Chrome to a `data:text/html` URI:
Chrome treats that as an opaque/null origin and blocks embedded `data:` images from
rendering in printed PDFs.

```
renderInvoiceHTML(data)
  └─ os.CreateTemp → erp_invoice_*.html
       └─ chromedp.Navigate("file:///C:/Users/.../erp_invoice_*.html")
            └─ page.PrintToPDF() → []byte
                 └─ os.Remove(temp file)
```

---

## Post-order hook flow

`PDFPostOrderHook.AfterOrder()` is called by the order worker after `has.exe` succeeds:

```
Sender.ProcessOrder()
  └─ PDFPostOrderHook.AfterOrder(req, result)
       ├─ Load logo → data: URI (logs path/size/mime on success, error on failure)
       ├─ Build InvoiceData from order + config
       ├─ Generator.Generate(ctx, data) → pdfBytes
       ├─ Save PDF  → SendOrderDir/history/<orderNum>/invoice_<orderNum>.pdf
       ├─ [if printAfterOrder] PrintPDF (engine: PDFtoPrinter → Adobe → Sumatra; see printing.md)
       └─ [if emailAfterOrder] email.Sender.SendInvoice → SMTP with PDF attachment
```

The hook is **non-fatal by default**: print and email failures are logged as warnings
but do not fail the order itself.

---

## Test Print (GUI)

The **PDF & Email Settings** dialog has a **Test Print** button that:

1. Reads all branding fields from the dialog (does not require Save first).
2. Checks the logo file: logs `path / size / MIME type` and shows the result in the
   status bar (`logo OK: image/png (83292 B)` or `logo ERROR: <reason>`).
3. Generates a sample invoice with dummy Hebrew data.
4. **Saves a copy** to `%PROGRAMDATA%\digi-erp-connector\test_print_YYYYMMDD_HHMMSS.pdf`
   so the output can be opened and inspected without a printer.
5. Sends to the configured printer via SumatraPDF.
6. Shows final status: `Print OK | logo OK: image/png (83292 B) | Saved: C:\ProgramData\...`

The saved PDFs accumulate in `%PROGRAMDATA%\digi-erp-connector\` and can be deleted manually.

---

## Observability

PDF hook log lines written to `%PROGRAMDATA%\digi-erp-connector\server.log`:

```
[INFO]  logo loaded: path=C:\images\logo.png size=83292 mime=image/png
[INFO]  PDF generated for order 1000295 (142350 bytes)
[INFO]  PDF saved to C:\digiorders\history\1000295\invoice_1000295.pdf
[OK]    PDF printed for order 1000295
[OK]    PDF emailed to customer@example.com for order 1000295
```

Failure cases:
```
[WARN]  cannot read logo file: path=C:\images\logo.png err=open ...: The system cannot find the file specified.
[WARN]  print failed for order 1000295: SumatraPDF not found; install it or set the path in config
[WARN]  email failed for order 1000295: dial tcp: connection refused
```

---

## Known pitfalls

| Symptom | Cause | Fix |
|---------|-------|-----|
| Logo missing in PDF | `LogoDataURI` field typed as `string` — Go template filter replaces `data:` with `#ZgotmplZ` | Must be `template.URL` |
| Logo missing in PDF | Chrome navigated to `data:text/html` URI (opaque origin blocks images) | Use `file://` temp file |
| Logo wrong format | MIME type hardcoded as `image/png` but file is JPEG | Use `http.DetectContentType` |
| No logs in `server.log` | GUI `logSvc` falls back to stderr (discarded in GUI mode) | Check status bar instead; GUI log is in `ui.log` |
| Print engine = SumatraPDF / Adobe instead of PDFtoPrinter | `PDFtoPrinter.exe` + `qpdf29.dll` + `resource.dat` missing from install dir | Re-deploy via the installer, or drop the three files into `C:\Program Files\digi-erp-connector\` manually (see `printing.md`) |
| Daemon log says "PDF printed" but no paper | SumatraPDF "exit 0 silent failure", or WSD-port printer used from session 0 | See decision tree in `printing.md` |
