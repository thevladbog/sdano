# Sdano — PDF Report Specification

The report is the product's storefront: the artifact the owner sends to the municipality, and the reason he pays. It must look like a document a serious contractor produces — not a software printout. This spec covers structure, rendering, and generation mechanics.

## Purpose and audience

- **Primary reader:** the client's inspector/administrator (municipality, property owner). Skims the summary, spot-checks objects, files the document.
- **Secondary reader:** the contractor's owner, defending payment or contesting a fine. Needs to find one object on one date in seconds.
- **Tertiary use:** an attachment in a formal dispute. Every photo must carry its evidence metadata (time, coordinates) *on the page*, not just in the database.

## Document structure

### 1. Cover page
- Sdano "SDANO/СДАНО" stamp (locale-dependent), muted placement — the brand is present, not shouting.
- Title: "Work completion report" / «Отчёт о выполнении работ».
- Contract name and client name, reporting period, contractor (tenant) name.
- Generation date and a **report ID** (short hash) — referenced in disputes ("see report SD-3F8A, page 12").

### 2. Summary page (the inspector's page)
- Headline numbers: objects under contract, planned jobs in period, completed, completion rate, missed (with dates), issues created/resolved (slice 2+).
- A compact per-object table: object name/address, planned count, completed count, missed dates if any. One line per object, sorted by address.
- A period bar chart (completions per day) — one, small, honest. No dashboard cosplay.

### 3. Per-object sections
For each object, ordered by address:
- Header: object name, address, coordinates, small static map thumbnail (optional, phase 2 of the template).
- A row per completed job: **date, completion time (device time), worker's display name, checklist result (n/n items), photo grid.**
- Photos: before/after pairs side by side where both exist; each photo captioned with capture time and coordinates. 4–6 photos per page max — evidence must be legible, not thumbnail confetti.
- Missed jobs listed explicitly with dates — hiding them would destroy the report's credibility as a dispute artifact, and the honest gap builds trust with the inspector.

### 4. Issues appendix (slice 2+)
Issues in the period: created → resolved timeline, resolution photos, the "resolved during planned visit on {date}" note where linked.

### 5. Closing page
Signature blocks ("Contractor" / "Client representative") — the document is designed to be printed and signed, because that is how municipal acceptance actually works. Page footer throughout: report ID, period, page n/m.

## Design requirements

- Follows the brand system: Report Paper background tone, Asphalt text, Work Green accents, the stamp on cover and closing page. Restrained — this is an official document, not a pitch deck.
- Print-safe: A4, works in grayscale (statuses readable without color), embedded fonts, no edge-to-edge bleeds.
- Locale: RU template first (the paying client's world — «Акт», «Объект», «Выполнено»); EN template later for the OSS/international story. Template strings externalized from day one.
- Photo treatment: fixed aspect containers, no stretching; EXIF orientation respected; capture metadata printed in a small caption strip under each photo.

## Generation mechanics

- **Pipeline:** report request → 202 → a worker job renders an HTML template (Go `html/template`) with data from one aggregate SQL query per section → headless Chrome (chromedp) prints to PDF → PDF uploaded to S3 → report row stamped `ready`.
- **Photos in the render:** the renderer downloads originals from S3 and embeds **downscaled** versions (target ~1200px long edge, ~80 quality) — a month of full-size photos would produce a 500 MB PDF. Originals remain untouched in S3; the PDF references their evidence metadata.
- **Size budget:** target <30 MB for a monthly single-contract report (~20 objects × 26 days × 2 photos ≈ 1,000 photos ⇒ pagination matters: photo grids, not one photo per page).
- **Time budget:** tens of seconds to low minutes is acceptable (async + polling per API spec). Chrome instance pooled/reused; one render at a time initially (single-tenant-scale reality).
- **Determinism:** a generated report is immutable. Regenerating the same period creates a *new* report row and PDF — the old one referenced in correspondence must never silently change (legal artifact discipline, same as photos).
- **Failure handling:** render errors mark the report `failed` with a reason; partial PDFs are never uploaded. Missing photos (unconfirmed uploads) render as an explicit "photo not uploaded" placeholder rather than being skipped — again, honesty is the product.

## Template architecture

```
apps/api/internal/report/
  templates/
    report.html        # layout shell, page CSS (@page rules, headers/footers)
    cover.html
    summary.html
    object_section.html
    issues.html
  assets/              # embedded fonts, stamp SVGs (RU/EN)
```
- CSS Paged Media (`@page`, `page-break-*`) drives pagination; tested against Chrome's print engine only (we control the renderer version via the Docker image — no cross-browser lottery).
- A `make report-preview` dev target renders a template with fixture data to PDF locally — designers iterate on the template without touching Go.

## Open questions (for the first client conversation)

- Does the municipality mandate a specific act form (КС-2-like or a regional template)? If yes: our report may become an *appendix* to their form — which changes nothing structurally but changes the cover.
- Photo volume per report: do they want every photo, or per-object samples with "full archive available on request"?
- Do they need a machine-readable companion (XLSX summary) alongside the PDF? (Cheap to add: same aggregate query → xlsx.)
