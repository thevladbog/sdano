# Sdano — Roadmap in Vertical Slices

Principle: proper architecture from day one (foundations are expensive to redo), but functionality ships in vertical slices. Every slice ends with a working end-to-end scenario. The backlog after slice 1 is driven by live client feedback, not imagination.

## Slice 0 — Foundation (week 1)

- Nx monorepo; skeletons for apps/api, apps/admin, apps/mobile.
- Postgres + migrations, sqlc, huma with /docs (Scalar), CI (lint, test, build).
- docker-compose: api + postgres + minio + caddy — one command to bring up.
- Auth: admin sign-in, worker creation, invite code, worker sign-in.
- Multitenancy in the schema (tenant_id) from day one.

Output: an "empty" but living product, deployable to a VPS.

## Slice 1 — Planned loop, the demo slice (weeks 2–5)

**Mobile (worker):**
- Sign in with a code → today's jobs grouped by object.
- Object → checklist (3–5 items) → photos (before/after) → submit.
- Photos: automatic timestamp + geotag; upload via presigned URL.
- Offline queue: shoot without network — uploads when the network returns; visible sync states.

**Admin (owner/manager):**
- List/map of objects with today's statuses (done / pending / overdue).
- Object card: execution history, photos with time and geo.
- **"Report for period" button → PDF** (objects, dates, photos). The demo's money slide.

**Deliberately cut from the slice:** checklist builder UI (templates entered directly into the DB), registration, payments, push, editing, roles beyond admin/worker.

**Milestone: demo to a live client (the cleaning company), weeks 5–6.**
Demo prep: load 10 real bus stops of the client's city into the tenant (public data) with a few of our own photos — the owner must see the streets he actually cleans.

## Slice 2 — Worker-reported issues (weeks 7–10, adjusted by demo feedback)

- Mobile: "Report a problem" on an object (photo + description) → an issue is created.
- Admin: issue feed, statuses (open → assigned → resolved), assignment.
- **Loop linking:** closing an issue within a planned visit — when executing a work_order on an object with open issues, the mobile app offers to mark them resolved; issue_resolution references the work_execution.
- Issues appear in the PDF report as a separate section.

## Slice 3 — QR and the builder (weeks 11–16)

- Object QR codes: the worker scans → the object's checklist opens instantly (plus proof of physical presence).
- Checklist builder in the admin (with template versioning).
- QR sticker generation and printing from the admin.

## Slice 4 — Public QR (on client demand / as premium)

- A "Something wrong? Report it" sticker → a web form with no app install, the object baked into the code → an issue in the system.
- Moderation/anti-spam (rate limits, captcha).
- Sold carefully: as a "for tenders" option — openness toward the client means political points for the municipality. Note: public reports create documented work for the contractor — not everyone wants that.

## Parallel track — sales and content

- Before code: set the demo date with client #1 (a commitment).
- After the demo: 3–5 conversations with adjacent contractors (mowing, snow, playgrounds).
- Build-in-public articles in English (Dev.to + Medium): the photo offline queue, the sqlc+huma type pipeline, hand-rolled auth in 2026, PDF reports via headless Chrome. Development doubles as tech-brand content before launch.

## Checkpoints (duplicated from the concept doc)

- **Month 3–4:** ≥2–3 businesses actually using it (even for free). If not → revisit the approach.
- **Month 8:** ≥5 paying clients or ≥$300 MRR. If not → open-source pivot (license: AGPL vs open-core — decide then).
