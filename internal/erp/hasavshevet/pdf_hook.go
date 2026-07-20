package hasavshevet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
	"github.com/digitrade-e/digi-erp-connector/internal/email"
	"github.com/digitrade-e/digi-erp-connector/internal/logger"
	"github.com/digitrade-e/digi-erp-connector/internal/pdf"
	"github.com/digitrade-e/digi-erp-connector/internal/print"
)

// connectorVersion is reported as the User-Agent suffix to the backend so the
// admin can distinguish connector instances in token usage telemetry.
const connectorVersion = "0.1.0"

// PDFPostOrderHook fetches the rendered HTML from the backend, converts it to
// PDF locally via chromedp, then optionally prints and/or emails the result.
type PDFPostOrderHook struct {
	cfg       config.Config
	pdfGen    *pdf.Generator
	emailSend *email.Sender // nil if email not configured
	log       logger.LoggerService
}

// NewPDFPostOrderHook creates a hook that handles post-order PDF operations.
func NewPDFPostOrderHook(cfg config.Config, pdfGen *pdf.Generator, emailSend *email.Sender, log logger.LoggerService) *PDFPostOrderHook {
	return &PDFPostOrderHook{
		cfg:       cfg,
		pdfGen:    pdfGen,
		emailSend: emailSend,
		log:       log,
	}
}

// AfterOrder fetches the pre-rendered HTML for this order from the backend,
// converts it to PDF, then dispatches print + email side-effects. Failure is
// non-fatal — the order itself was already written to the ERP successfully.
func (h *PDFPostOrderHook) AfterOrder(ctx context.Context, req OrderRequest, result *OrderResult) error {
	orderNum := fmt.Sprintf("%d", result.OrderNumber)

	h.log.Info(fmt.Sprintf(
		"AfterOrder invoked: order=%s documentType=%q UseRemoteTemplate=%v PrintAfterOrder=%v EmailAfterOrder=%v hasCustomerEmail=%v tokenCount=%d",
		orderNum, req.DocumentType, h.cfg.PDF.UseRemoteTemplate,
		h.cfg.PDF.PrintAfterOrder, h.cfg.PDF.EmailAfterOrder,
		req.CustomerEmail != "", len(h.cfg.PDF.RemoteTokens),
	))

	if !h.cfg.PDF.UseRemoteTemplate {
		h.log.Warn(fmt.Sprintf(
			"UseRemoteTemplate is false — print/email skipped for order %s. Local template support was removed; enable UseRemoteTemplate and configure RemoteTokens.",
			orderNum,
		))
		return nil
	}

	token := lookupRemoteToken(h.cfg.PDF.RemoteTokens, req.DocumentType)
	if token == "" {
		h.log.Warn(fmt.Sprintf(
			"no remote token configured for documentType=%s — print/email skipped for order %s",
			req.DocumentType, orderNum,
		))
		return nil
	}

	pdfBytes, err := h.fetchRemoteHTMLAndRenderPDF(ctx, token, req, orderNum)
	if err != nil {
		h.log.Error(fmt.Sprintf(
			"remote template fetch/render failed for order %s (token=%s) — print/email skipped",
			orderNum, pdf.MaskToken(token),
		), err)
		return nil
	}

	h.log.Success(fmt.Sprintf(
		"remote template rendered for order %s (%d bytes, token=%s)",
		orderNum, len(pdfBytes), pdf.MaskToken(token),
	))
	return h.dispatchPDF(ctx, orderNum, pdfBytes, req.CustomerEmail)
}

// fetchRemoteHTMLAndRenderPDF asks the backend for the rendered HTML, then runs
// the existing chromedp HTML→PDF pipeline. The token never appears in errors.
//
// documentNumber is the user-visible order number (e.g. "1000011" — the
// externalOrderId / Hasavshevet order number from result.OrderNumber). The
// backend resolves this via History.orderExtId. Passing req.HistoryID (an
// internal UUID) here would 404 because the backend has no row keyed by that.
func (h *PDFPostOrderHook) fetchRemoteHTMLAndRenderPDF(ctx context.Context, token string, req OrderRequest, documentNumber string) ([]byte, error) {
	timeout := time.Duration(h.cfg.PDF.RemoteTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	fetcher := pdf.NewRemoteFetcher(
		h.cfg.PDF.RemoteTemplateBaseURL,
		timeout,
		"digi-erp-connector/"+connectorVersion,
	)

	fetchCtx, fetchCancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer fetchCancel()

	h.log.Info(fmt.Sprintf(
		"remote fetch: documentType=%q documentNumber=%s userExtId=%s baseURL=%q",
		req.DocumentType, documentNumber, req.UserExtID, h.cfg.PDF.RemoteTemplateBaseURL,
	))

	htmlBytes, err := fetcher.Fetch(
		fetchCtx,
		token,
		strings.ToLower(req.DocumentType),
		documentNumber,
		req.UserExtID,
	)
	if err != nil {
		return nil, fmt.Errorf("remote fetch: %w", err)
	}

	pdfCtx, pdfCancel := context.WithTimeout(ctx, 60*time.Second)
	defer pdfCancel()
	pdfBytes, err := h.pdfGen.GenerateFromHTML(pdfCtx, htmlBytes)
	if err != nil {
		return nil, fmt.Errorf("html→pdf: %w", err)
	}
	return pdfBytes, nil
}

// dispatchPDF saves the PDF to the order's history dir and optionally prints
// and/or emails it.
func (h *PDFPostOrderHook) dispatchPDF(ctx context.Context, orderNum string, pdfBytes []byte, customerEmail string) error {
	historyDir := filepath.Join(h.cfg.SendOrderDir, "history", orderNum)
	pdfPath := filepath.Join(historyDir, fmt.Sprintf("invoice_%s.pdf", orderNum))

	h.log.Info(fmt.Sprintf(
		"dispatchPDF start: order=%s pdfBytes=%d historyDir=%q PrintAfterOrder=%v EmailAfterOrder=%v emailSenderConfigured=%v PrinterName=%q SumatraPDFPath=%q",
		orderNum, len(pdfBytes), historyDir,
		h.cfg.PDF.PrintAfterOrder, h.cfg.PDF.EmailAfterOrder,
		h.emailSend != nil, h.cfg.PDF.PrinterName, h.cfg.PDF.SumatraPDFPath,
	))

	pdfWritten := false
	if err := os.MkdirAll(historyDir, 0o755); err != nil {
		h.log.Warn(fmt.Sprintf("failed to create history dir %q: %v", historyDir, err))
	} else {
		if err := os.WriteFile(pdfPath, pdfBytes, 0o644); err != nil {
			h.log.Warn(fmt.Sprintf("failed to save PDF to history: %v", err))
		} else {
			pdfWritten = true
			h.log.Info(fmt.Sprintf("PDF saved to %s", pdfPath))
		}
	}

	if h.cfg.PDF.PrintAfterOrder {
		if !pdfWritten {
			h.log.Warn(fmt.Sprintf("print skipped for order %s: PDF was not written to %s", orderNum, pdfPath))
		} else {
			h.log.Info(fmt.Sprintf("calling print.PrintPDF for order %s: path=%s printer=%q sumatra=%q", orderNum, pdfPath, h.cfg.PDF.PrinterName, h.cfg.PDF.SumatraPDFPath))
			if err := print.PrintPDF(ctx, pdfPath, h.cfg.PDF.PrinterName, h.cfg.PDF.SumatraPDFPath, h.log); err != nil {
				h.log.Warn(fmt.Sprintf("print failed for order %s: %v", orderNum, err))
			} else {
				h.log.Success(fmt.Sprintf("PDF printed for order %s", orderNum))
			}
		}
	} else {
		h.log.Info(fmt.Sprintf("print skipped for order %s: PrintAfterOrder=false in config", orderNum))
	}

	if h.cfg.PDF.EmailAfterOrder && h.emailSend != nil {
		if customerEmail == "" {
			h.log.Warn(fmt.Sprintf("email after order enabled but no customer email for order %s", orderNum))
		} else {
			if err := h.emailSend.SendInvoice(ctx, customerEmail, pdfBytes, orderNum); err != nil {
				h.log.Warn(fmt.Sprintf("email failed for order %s: %v", orderNum, err))
			} else {
				h.log.Success(fmt.Sprintf("PDF emailed to %s for order %s", customerEmail, orderNum))
			}
		}
	} else {
		h.log.Info(fmt.Sprintf("email skipped for order %s: EmailAfterOrder=%v emailSenderConfigured=%v", orderNum, h.cfg.PDF.EmailAfterOrder, h.emailSend != nil))
	}

	return nil
}

// lookupRemoteToken returns the token for the given documentType, trying both
// the original case and lowercase form. Backend documentTypes are lowercase
// (e.g. "order"); the OrderRequest.DocumentType from Hasavshevet is uppercase
// (e.g. "ORDER") — so the operator may have stored the token under either.
func lookupRemoteToken(tokens map[string]string, documentType string) string {
	if len(tokens) == 0 || documentType == "" {
		return ""
	}
	if t, ok := tokens[documentType]; ok && t != "" {
		return t
	}
	if t, ok := tokens[strings.ToLower(documentType)]; ok && t != "" {
		return t
	}
	if t, ok := tokens[strings.ToUpper(documentType)]; ok && t != "" {
		return t
	}
	return ""
}
