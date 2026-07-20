package pdf

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/cdproto/page"
)

// Generator produces PDF bytes from a pre-rendered HTML document via headless
// Chrome. The HTML always comes from the backend remote-template route now —
// the connector no longer ships its own invoice template.
type Generator struct {
	chromePath string
}

// NewGenerator creates a PDF generator. If chromePath is empty, chromedp
// will attempt to find Chrome automatically.
func NewGenerator(chromePath string) *Generator {
	return &Generator{chromePath: chromePath}
}

// GenerateFromHTML converts a self-contained HTML document (logo + fonts inlined
// as data URIs) to PDF bytes via headless Chrome.
func (g *Generator) GenerateFromHTML(ctx context.Context, htmlBytes []byte) ([]byte, error) {
	// Write HTML to a temp file and load via file:// URL.
	// Navigating to a data:text/html URI gives the page an opaque/null origin,
	// which causes Chrome to block embedded data: images from rendering in PDFs.
	tmpFile, err := os.CreateTemp("", "erp_invoice_*.html")
	if err != nil {
		return nil, fmt.Errorf("create temp html: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(htmlBytes); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("write temp html: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close temp html: %w", err)
	}

	// Build a valid file:// URL on both Windows and Unix.
	slashPath := filepath.ToSlash(tmpPath)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath // Windows: C:/foo → /C:/foo
	}
	fileURL := "file://" + slashPath

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
	)
	if g.chromePath != "" {
		opts = append(opts, chromedp.ExecPath(g.chromePath))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)
	defer allocCancel()

	taskCtx, taskCancel := chromedp.NewContext(allocCtx)
	defer taskCancel()

	var pdfBuf []byte
	if err := chromedp.Run(taskCtx,
		chromedp.Navigate(fileURL),
		chromedp.ActionFunc(func(ctx context.Context) error {
			buf, _, err := page.PrintToPDF().
				WithPaperWidth(8.27).   // A4 width in inches
				WithPaperHeight(11.69). // A4 height in inches
				WithMarginTop(0).       // margins handled by CSS @page
				WithMarginBottom(0).
				WithMarginLeft(0).
				WithMarginRight(0).
				WithPrintBackground(true).
				Do(ctx)
			if err != nil {
				return err
			}
			pdfBuf = buf
			return nil
		}),
	); err != nil {
		return nil, fmt.Errorf("chromedp pdf generation: %w", err)
	}

	return pdfBuf, nil
}
