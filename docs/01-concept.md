# Sdano — Product Concept

> Everything that happens to an object is recorded and provable.

## In one paragraph

Sdano is a photo-evidence and reporting platform for contractors who service physical objects (bus stops, building entrances, playgrounds, grounds). A field worker completes a job against a checklist and confirms it with photos carrying a geotag and timestamp; the business owner sees the live picture across all objects and generates a client-ready report with one click. Sdano replaces the chaos of WhatsApp chats, paper sheets, and manual end-of-month report assembly.

## The problem

Small contractors holding municipal and corporate contracts (bus stop cleaning, grass mowing, snow removal, playground maintenance, entrance cleaning) must **prove that work was performed** — otherwise they don't get paid, or get fined.

How it works today:
- the worker sends before/after photos to a WhatsApp group;
- photos are mixed with stickers, sick notes, and personal messages;
- at the end of the month a manager (often the owner) manually digs photos out by date and address to assemble the client report;
- when a dispute arises ("it was dirty on the 14th"), evidence is found by archaeology — and often not found at all.

Sdano's competitor is not other software but the "somehow, in the chat" habit. Enterprise solutions (SafetyCulture and peers) are expensive and bloated; the low end of the market is empty.

## The core framing

We sell **proof of work for the client, not employee surveillance**. Surveillance is a "nice to have"; contract reporting is a "must". The report is directly tied to getting paid, which makes "Export contract report for a period as PDF" the primary reason to pay.

## Two loops of work

The object (bus stop, entrance, playground) is the central entity. Two types of events happen to it:

1. **Planned work.** Schedule → work order with a checklist → execution with photos → inclusion in the report. The primary loop; the pain is confirmed by a real prospective client.
2. **Issues (incidents).** A discovered problem (by a worker on rounds, later by a resident via QR code) → issue → assignment → resolution with photo evidence.

The key link that sets Sdano apart from plain ticketing systems: **an issue can be closed within a planned visit**. The worker was going to the object anyway — they fix the problem while there; the system simply links the issue to the planned work execution. This reflects contractor reality ("I'll be there tomorrow anyway"), which classic service desks don't model.

Both loops are in the data model from day one; the UI ships them in slices (planned loop first).

## Target segment

**Entry wedge:** small contracting companies (5–50 field workers) with contracts requiring photo evidence:
- bus stop and urban territory cleaning;
- entrance cleaning / property management companies;
- grass mowing, winter snow removal;
- playground maintenance, waste removal, facade washing.

**Who pays:** the owner/director. **Who uses it daily:** the field worker (mobile app) and the manager/supervisor (web admin).

The first target client is a real cleaning company that services bus stops and currently reports via WhatsApp photos.

## Value proposition by role

| Role | Pain | What Sdano provides |
|---|---|---|
| Owner | Client reports take days to assemble; disputes are lost | One-click report; every photo with geo and time is a legal argument |
| Manager | "Who was where, what's done?" — calls and chats | Live map/list of objects with today's statuses |
| Worker | Photos get lost; "prove you were there" | Scan QR / open object → checklist → photo → done; works offline |
| Client (municipality) | Contractor opacity | Structured reports; later — a public "Report a problem" QR |

## Differentiators

1. **Offline-first.** Field conditions: basements, city outskirts, cheap Android phones. Photos and checklists work without connectivity; sync happens when the network returns, with visible states like "3 photos waiting to upload".
2. **The report is the product.** A PDF per contract per period: objects, dates, photos, geo. The thing customers pay for.
3. **Linked loops.** Issues closed within planned visits.
4. **QR codes on objects.** For the worker — instant checklist opening and proof of physical presence; later, for residents — a "Something wrong? Report it" web form with no app install (the object is baked into the code). The product physically present in the city.
5. **Self-hosted / data residency.** One docker-compose deploys anywhere: a Russian cloud (152-FZ compliance), the client's own server, a European cloud. For municipal contractors, "your data on your server" is sometimes a requirement, not a perk.
6. **PII minimalism.** A worker has no profile: a name and an invite code. What isn't in the database can't leak and doesn't need defending.

## Business model

- B2B subscription per organization: ~$30–80/month (tiered by number of objects and/or workers — to be refined after first sales).
- First clients onboarded manually (tenant creation, object entry); payment by invoice.
- Target economics: 20 clients × ~$50 = ~$1,000/month side income.

## Strategy and pivot criteria

**Plan:** a 6–8 month commercial attempt. If it doesn't take off — pivot to open source (portfolio, community, content: guaranteed upside even on commercial failure).

**Criteria fixed in advance:**
- Checkpoint, month 3–4: are there ≥2–3 businesses actually using the product (even for free)? If not even free users exist — a warning sign; revisit the approach earlier.
- Pivot point, month 8: if there are not ≥5 paying clients or ≥$300 MRR — pivot to open source, no regrets.

**Discipline for the pivot:** code is written as if public from day one — secrets only in env, decent READMEs and commit history, no hardcoded values. The pivot should cost a week of tidying, not a month of excavation.

**License at pivot time:** decision deferred until the pivot; candidates are AGPL (protection against "a cloud provider just hosts it") or open-core (open core, paid PDF reports/SSO). MIT — only if the commercial track is definitively closed.

## The name

**Sdano** (Russian "сдано" — "handed over / delivered") — a state of completion: the object is delivered, the work is handed to the client. A word from the worker's daily ritual. Two syllables, unambiguous to read in any language, domain sdano.app. Backup brand — Prinyato (prinyato.app) — registered as a reserve.

Brand mark: the "СДАНО" stamp, with a Latin **"SDANO"** twin version for international/open-source contexts (README, GitHub, English articles).

## First milestone

**A demo to a live client within 4–6 weeks** of development start: a vertical slice of the planned loop (worker mobile app → geotagged photo → admin panel → PDF report), populated with real bus stops from the client's city. The demo date is set before the first commit.
