package report

import (
	"context"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// renderTimeout bounds a single PDF render (docs/09: "tens of seconds to
// low minutes is acceptable") — headless Chrome wedged on a bad render must
// fail loudly rather than hang the report worker forever.
const renderTimeout = 120 * time.Second

// A4 in inches — the paper size PrintToPDFParams expects (docs/09:
// "Print-safe: A4").
const (
	pdfPaperWidthIn  = 8.27
	pdfPaperHeightIn = 11.69
)

// PDFRenderer turns a fully-rendered HTML document into PDF bytes. Deliberately
// knows nothing about ReportData, sqlc, or S3 — task 4's worker composes
// RenderHTML → PDFRenderer.RenderPDF → store.Put. Injected so the worker is
// testable without a live Chrome instance.
type PDFRenderer interface {
	RenderPDF(ctx context.Context, html string) ([]byte, error)
}

// ChromeRenderer implements PDFRenderer against a headless Chrome instance
// reachable over the Chrome DevTools Protocol — the compose headless-shell
// service (docs/09 generation mechanics; deploy/docker-compose.yml).
type ChromeRenderer struct {
	cdpURL string
}

// NewChromeRenderer builds a ChromeRenderer that dials cdpURL fresh for
// every render. cdpURL is the browser's stable HTTP debugging endpoint
// (e.g. "http://headless-shell:9222") — see the URL-resolution note on
// RenderPDF for why this must NOT be chromedp.NoModifyURL'd.
func NewChromeRenderer(cdpURL string) *ChromeRenderer {
	return &ChromeRenderer{cdpURL: cdpURL}
}

// RenderPDF navigates a fresh headless-Chrome tab to a data: URL holding
// html and prints it to PDF (A4, background graphics enabled — docs/09:
// "Print-safe: A4 ... no edge-to-edge bleeds").
//
// cdpURL / NewRemoteAllocator resolution (verified against the installed
// chromedp v0.15.1 source, allocate.go's NewRemoteAllocator/modifyURL —
// context7 docs alone under-specify this): NewRemoteAllocator is called
// WITHOUT chromedp.NoModifyURL, on purpose. When the URL passed to it does
// not already contain "/devtools/browser/", chromedp's *default*
// modifyURLFunc resolves it itself by GETting
// "http://<host>:<port>/json/version" and reading "webSocketDebuggerUrl"
// from the response — exactly the shape of a stable config value like
// "http://headless-shell:9222" (the compose service host:port), and exactly
// how the chromedp/docker-headless-shell image documents connecting to it.
// chromedp.NoModifyURL disables that resolution entirely and would hand the
// literal "http://headless-shell:9222" string straight to the websocket
// dialer — not a valid devtools websocket endpoint — failing every render.
// Because Allocate() (and therefore this resolution) runs fresh on every
// RenderPDF call rather than once at construction time, this also tolerates
// the headless-shell container restarting between renders and handing out a
// new debugger URL.
func (r *ChromeRenderer) RenderPDF(ctx context.Context, html string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()

	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, r.cdpURL)
	defer cancelAlloc()
	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()

	var pdf []byte
	err := chromedp.Run(taskCtx,
		chromedp.Navigate("data:text/html;base64,"+base64.StdEncoding.EncodeToString([]byte(html))),
		chromedp.WaitReady("body"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			pdf, _, err = page.PrintToPDF().WithPrintBackground(true).
				WithPaperWidth(pdfPaperWidthIn).WithPaperHeight(pdfPaperHeightIn).Do(ctx)
			return err
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("chrome render: %w", err)
	}
	return pdf, nil
}
