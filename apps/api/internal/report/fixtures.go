package report

import "time"

// onePxGifDataURI is a fully valid, tiny (1x1 transparent) GIF encoded as a
// data URI — enough for the template/browser to actually decode and paint
// something, unlike an opaque placeholder string.
const onePxGifDataURI = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBTAA7"

// PreviewFixture returns a deterministic ReportData used by `make
// report-preview` and templates_test.go. Every timestamp is fixed so the
// rendered HTML — and any PDF diffed from it later — never wiggles between
// runs. Numbers are internally consistent: 2 objects, 3 completed jobs, 3
// photos (one Missing), 1 missed row; Planned=4, Done=3, Missed=1,
// CompletionPct=75.
func PreviewFixture() ReportData {
	periodFrom := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodTo := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	generatedAt := time.Date(2026, 7, 1, 9, 15, 0, 0, time.UTC)

	return ReportData{
		ShortID:      "SD-3F8A11C2",
		TenantName:   "ООО «Чистый Город»",
		ContractName: "Договор №14-2026 на содержание территорий",
		ClientName:   "Администрация г. Мурома",
		PeriodFrom:   periodFrom,
		PeriodTo:     periodTo,
		GeneratedAt:  generatedAt,
		Summary: SummaryData{
			ObjectCount:   2,
			Planned:       4,
			Done:          3,
			Missed:        1,
			CompletionPct: 75,
			PerObject: []SummaryRow{
				{Name: "Остановка «Central Park»", Address: "ул. Ленина, 10", Planned: 2, Done: 2, Missed: 0},
				{Name: "Сквер Победы", Address: "ул. Мира, 5", Planned: 2, Done: 1, Missed: 1},
			},
		},
		Objects: []ObjectSection{
			{
				Name:    "Остановка «Central Park»",
				Address: "ул. Ленина, 10",
				Jobs: []JobRow{
					{
						Date: "05.06.2026", FinishedAt: "08:42", WorkerName: "Алексей Петров",
						CheckedItems: 3, TotalItems: 3,
						Photos: []PhotoCell{
							{DataURI: onePxGifDataURI, Caption: "08:41 · 55.75800, 37.61730"},
						},
					},
					{
						Date: "12.06.2026", FinishedAt: "09:05", WorkerName: "Алексей Петров",
						CheckedItems: 2, TotalItems: 3,
						Photos: []PhotoCell{
							{Missing: true},
						},
					},
				},
			},
			{
				Name:    "Сквер Победы",
				Address: "ул. Мира, 5",
				Jobs: []JobRow{
					{
						Date: "20.06.2026", FinishedAt: "14:10", WorkerName: "Мария Иванова",
						CheckedItems: 4, TotalItems: 4,
						Photos: []PhotoCell{
							{DataURI: onePxGifDataURI, Caption: "14:08 · 55.79500, 37.61200"},
						},
					},
				},
			},
		},
		Missed: []MissedRow{
			{ObjectName: "Сквер Победы", Date: "19.06.2026"},
		},
	}
}
