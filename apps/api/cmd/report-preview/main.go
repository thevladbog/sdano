// Command report-preview renders the report HTML template against a
// deterministic fixture so template/CSS work can be iterated on without a
// live database, S3, or Chrome (docs/09-pdf-report.md: "designers iterate
// on the template without touching Go"). Run via `make report-preview`.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"sdano.app/api/internal/report"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "report-preview:", err)
		os.Exit(1)
	}
}

func run() error {
	html, err := report.RenderHTML(report.PreviewFixture())
	if err != nil {
		return fmt.Errorf("rendering preview: %w", err)
	}

	path := "report-preview.html"
	if err := os.WriteFile(path, []byte(html), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	fmt.Println(abs)
	return nil
}
