package report

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

// safeURL marks a string as a trusted URL so html/template embeds it
// verbatim in an <img src="..."> attribute. html/template's default URL
// sanitizer (html/template/url.go, isSafeURL) only allows http/https/mailto
// schemes and defangs everything else — including "data:" — to
// "#ZgotmplZ". The photo data URIs rendered here are built server-side from
// S3-confirmed evidence (PhotoLoader), never from unescaped user input, so
// trusting them is safe; without this, every evidence photo in the PDF
// would silently become a broken-image icon.
func safeURL(s string) template.URL {
	return template.URL(s) //nolint:gosec // server-built data: URI, not user input
}

// formatDate renders a time.Time as "02.01.2006" for the cover/closing
// pages. Per-job and per-photo strings are already pre-formatted by data.go
// (dates 02.01.2006, times 15:04); this covers the top-level ReportData
// fields (PeriodFrom, PeriodTo, GeneratedAt) that stay time.Time so the
// template — not data.go — owns their on-page formatting.
func formatDate(t time.Time) string {
	// .UTC() so the printed date never drifts with the render host's local
	// timezone (see data.go's identical rationale for FinishedAt/captions).
	return t.UTC().Format("02.01.2006")
}

var reportTmpl = template.Must(template.New("report.html").Funcs(template.FuncMap{
	"safeURL":    safeURL,
	"formatDate": formatDate,
}).ParseFS(templateFS, "templates/*.html"))

// RenderHTML executes the embedded template set against d and returns the
// full HTML document.
func RenderHTML(d ReportData) (string, error) {
	var buf bytes.Buffer
	if err := reportTmpl.ExecuteTemplate(&buf, "report.html", d); err != nil {
		return "", fmt.Errorf("rendering report template: %w", err)
	}
	return buf.String(), nil
}
