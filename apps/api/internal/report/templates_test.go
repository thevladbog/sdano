package report_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"sdano.app/api/internal/report"
)

func TestRenderHTMLPreviewFixture(t *testing.T) {
	fixture := report.PreviewFixture()

	html, err := report.RenderHTML(fixture)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if html == "" {
		t.Fatal("RenderHTML returned empty output")
	}

	wants := []string{
		fixture.ShortID,
		"Отчёт о выполнении работ",
		"Пропущенные работы",
		"Фото не загружено",
		"Исполнитель",
		"Представитель заказчика",
		"@page",
		fixture.TenantName,
		fixture.ContractName,
		fixture.Objects[0].Name,
		fixture.Objects[1].Name,
		fixture.Missed[0].ObjectName,
		fixture.Missed[0].Date,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Errorf("rendered report missing %q", want)
		}
	}

	// A confirmed photo's data URI must reach the page verbatim inside the
	// <img> src — html/template's URL sanitizer defangs unrecognized
	// schemes (including "data:") into "#ZgotmplZ" unless the template
	// explicitly marks the value as a safe URL. If this regresses, every
	// evidence photo in the PDF silently turns into a broken image icon.
	foundDataURI := false
	for _, obj := range fixture.Objects {
		for _, job := range obj.Jobs {
			for _, p := range job.Photos {
				if !p.Missing && p.DataURI != "" {
					foundDataURI = true
					if !strings.Contains(html, p.DataURI) {
						t.Errorf("photo DataURI %q not found verbatim in rendered HTML (sanitizer likely defanged it)", p.DataURI)
					}
				}
			}
		}
	}
	if !foundDataURI {
		t.Fatal("fixture has no confirmed photo — test cannot verify DataURI passthrough")
	}
	if strings.Contains(html, "ZgotmplZ") {
		t.Error("rendered HTML contains ZgotmplZ — a URL was defanged by the template's safety sanitizer")
	}
}

func TestShortIDFor(t *testing.T) {
	id := uuid.MustParse("3f8a11c2-0000-0000-0000-000000000000")
	if got := report.ShortIDFor(id); got != "SD-3F8A11C2" {
		t.Errorf("ShortIDFor(%s) = %q, want SD-3F8A11C2", id, got)
	}
}
