# Sdano — Design Concept Review (v1)

Overall verdict: **accepted as the foundation for slice 1**, with four requested changes. The concept clearly reflects the brief — sync states as a dedicated section, the big bottom "Sdano" button, the stamped PDF. This is close to a working design system already.

## What's strong (keep)

1. **The "СДАНО" stamp.** Of the four logo directions, the stamp is the only element with real character: it's physical, it comes from the client's world of acts and seals, and it embodies the brief's "visual language of completion." Its appearance on the PDF report is the best detail in the whole concept.
2. **Direction A (phone + stamp) as the primary lockup** — "proof in your pocket." Directions B (pin + checklist) and C (camera + check) are competent but generic: they could belong to any field-service SaaS. D works as a secondary composition for report/marketing contexts.
3. **Semantic palette.** Work Green / Signal Amber / Issue Red map exactly onto done / in progress / overdue. The color naming (Asphalt, Report Paper) shows product thinking.
4. **Mobile flow reads end to end** — route → object → checklist → photos → "Sdano!" with upload confirmation. The success screen listing "3 photos sent / everything synced" is precisely the treatment for the worker's core fear.
5. **Admin dashboard passes the 3-second test:** 14 / 4 / 2 aggregates on top, red visible immediately. The "Call worker" action on the object card is a great detail — it is literally Marina's scenario from the brief.
6. **The PDF concept** (cover, summary, per-object pages, stamp) is client-ready in spirit.

## Requested changes

### 1. Commit to the stamp as a system, not just a logo option
Adopt direction A as the primary lockup and extract the stamp as a standalone brand device used across: the PDF report, the mobile "Sdano!" success state, and the admin "done" status. Provide the stamp in its three drawn variants (rectangular, round with check, round outline) with usage guidance.

### 2. Latin twin of the stamp
The product targets the Russian market first, but documentation, the repository, and future open-source life are in English. Produce an **"SDANO"** Latin version of the stamp alongside "СДАНО" (the Latin word conveniently matches in length and rhythm). Russian version in the RU product; Latin in the README, GitHub, and English-language articles. Half an hour now versus a rebrand later.

### 3. Simplify the app icon
The stamp with text inside a dark square will turn to mush at real home-screen sizes. Split it: the full stamp for stores and marketing; the icon itself — a simplified mark (stamp outline + checkmark, no text). Please provide a 48px test render next to WhatsApp and a maps app — that is where Alexey will look for it.

### 4. Design the offline state into the main flow
The sync-status section at the bottom shows the thinking is there, but the flow itself must include the offline journey: what does "Sdano!" look like with no network? The required state: "delivered offline — everything queued — keep going, it will upload by itself." This is the heart of the product and cannot live only in a status bar. Add: the offline variant of the success screen, the queued-photos indicator on the route list, and the moment the queue flushes (subtle, non-blocking).

## Smaller notes

- **Contrast audit for Amber and Green.** Signal Amber (#F59E0B) as text on white almost certainly fails WCAG AA — fine as a badge fill with dark text, not as text color. Work Green (#16A34A) is borderline for small text on white. Fix the rules in the guide now (when each color may be text vs fill) rather than catching it screen by screen later.
- **Before/after photo pair.** The checklist screen currently labels photos "Photo after" only; the before/after pair is the core of the evidence. Add the "before" capture step or an explicit paired layout.
- **States beyond color.** Done / in progress / overdue / has-open-issue must be distinguishable by icon or shape as well as color (accessibility + sunlight).
- Keep the "Сдано!" micro-celebration fast (<400ms) and skippable by the next tap — Alexey does this 20 times a day.

## Deliverables for the next iteration

1. Stamp system: RU + EN versions, three variants, usage rules.
2. Simplified app icon + 48px test render.
3. Offline flow screens (success state, queue indicator, flush moment).
4. Contrast-fixed palette rules (text vs fill usage per color).
5. Before/after photo capture step in the checklist flow.
