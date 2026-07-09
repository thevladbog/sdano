# Sdano — Design Brief

## The story we start from

In a mid-sized city there is a cleaning company. It holds a municipal contract: keep the bus stops clean. Every morning several workers spread out along their routes — washing shelters, collecting trash, scrubbing off graffiti. The city pays for results and demands proof: didn't clean, or can't prove it — a fine or no payment.

How it looks today: a worker finishes a stop, pulls out a phone, and drops a couple of photos into a shared WhatsApp group. The chat holds hundreds of photos mixed with "I'm sick today", voice messages, and stickers. At the end of the month the owner's wife spends two evenings digging photos out by date and address to assemble the report for the city administration. When an inspector says "Lenina street was dirty on the 14th," archaeology begins — and most of the time the proof is never found, even though the work was done.

**Sdano exists to make those two evenings disappear.** The worker takes the same photo — but it already knows which stop it belongs to, when and where it was taken. The owner can see at any moment: 18 of 20 objects done today, two in progress. The city report is assembled with one click. A dispute with an inspector ends in ten seconds — here's the photo, here's the time, here are the coordinates.

The product formula: **"everything that happens to an object is recorded and provable."**

## Personas

### Alexey, 45 — field worker (mobile app)
- Cleans 15–20 bus stops per shift, moving by car or on foot.
- His phone is a cheap Android with a cracked screen. He works in gloves, outdoors: sun, rain, frost. Connectivity comes and goes.
- Not an "app person": WhatsApp, calls, a navigation app — that's it. Any complexity reads as mockery; any failure is a reason to go back to the chat group.
- Motivation: finish the shift with no complaints. The app must take seconds, not minutes. "Delivered — moving on."
- Fear: "I did the work but the photo didn't go through — and I'm the one blamed." Seeing that everything has been sent is critical to him.

### Sergey, 50 — business owner (admin panel, pays for the product)
- 12 workers, two municipal contracts. Half his day is in the car and on sites; he checks the admin panel from a laptop in the evening and from his phone during the day.
- Not technical. Excel is his ceiling. Values "seeing everything at once," hates settings.
- His questions to the screen: "Is everything done today? Where are the gaps? What do I show the client?"
- The buying moment: he sees the "Monthly report → PDF" button and pictures two freed-up evenings.

### Marina, 33 — manager/supervisor (admin panel, primary daily user)
- Assigns jobs, tracks completion, fields the client's calls.
- A confident PC user. Lives in the admin panel all day: statuses, overdue items, issues, assignments.
- Needs: navigation speed, filters, "what needs my attention right now."

## Key scenarios (design from these)

1. **Alexey's shift:** opens the app → today's route as a list → arrives at a stop (or scans the QR on it) → a 4-item checklist → "after" photo → a big "Sdano" button → next object. The full on-site cycle takes under a minute. In a basement / with no signal — exactly the same; back on the street everything uploads by itself.
2. **Marina's morning:** opens the dashboard → 20 objects: 14 green, 4 in progress, 2 red (overdue) → taps a red one → sees Alexey hasn't been there → calls him.
3. **Sergey's reporting day:** picks a contract and a period → "Generate report" → a PDF with all objects, dates, and photos → sends it to the administration. Two minutes instead of two evenings.
4. **A dispute:** the client calls — "Lenina street was dirty on the 14th" → Marina opens the object → the 14th, 9:41, a photo of a clean stop, a geotag → screenshot sent back.
5. **An issue along the way (slice 2):** Alexey notices a broken bench → "Report a problem" → photo + a couple of words → the issue lands with Marina. A week later Alexey is at that object on schedule — the app reminds him: "There's an open issue here — did you fix it?"

## Design principles

### Mobile app (Alexey)
- **Glove-friendly UI:** large touch targets (min 48dp, better bigger), the primary action is one big button at the bottom of the screen, reachable with a thumb.
- **Zero text input** in the main flow. Checkbox, photo, button. Comments are optional and never required.
- **Outdoors:** high contrast, large typography, sunlight readability. Dark theme is a nice bonus; contrast is an obligation.
- **Sync status is a first-class citizen.** Always visible: "Everything sent" / "3 photos waiting for network". This treats Alexey's core fear. Uploading never blocks work: delivered — move on, it will finish sending by itself. The offline state ("delivered offline, everything queued, keep going") must be designed into the main flow, not hidden in a settings corner — it is the heart of the product.
- **Object states via color and icon,** distinguishable without reading: done / in progress / overdue / has an open issue.
- **Forgiving by default:** retaking a photo or unchecking an item is easy and obvious, with no "are you sure?" dialogs at every step.
- Interface language mirrors the user's world: "Object", "Delivered", "Issue". No "tasks" or "submits".

### Admin panel (Sergey and Marina)
- **The dashboard answers "is everything okay today?" within 3 seconds** — before any click. Aggregates + red items on top.
- Two viewing modes: map (geographic picture) and list (operational work). Marina lives in the list; Sergey likes the map.
- **Photos are the primary content.** Large previews, instant lightbox, time and geo always adjacent. Never "click three times to see a photo."
- **The report is the product's storefront.** The report-generation screen and the PDF itself must look good enough that Sergey isn't embarrassed to send it to the city administration. The PDF is a design artifact too: cover, summary, per-object pages.
- Moderate information density: Marina needs speed, Sergey needs clarity. Filters and search — from day one; customization — never (at this stage).

### General
- **Brand tone:** workmanlike, dependable, no corporate gloss and no playfulness. A product for people who wash bus stops at 6 a.m. — respect for their work, zero condescension.
- Sdano = "delivered": a visual language of completion — checkmarks, stamps, an "accepted" state. The "Sdano!" moment in the mobile app can be a small celebration (a quick micro-animation), but fast.
- The **"СДАНО" stamp** is the brand device. It lives in the PDF report, the mobile success screen, and the admin's "done" status. A Latin **"SDANO"** twin version is required for international/open-source contexts.
- Aesthetic reference: honest utility at the level of good tools (Linear-grade clarity, but warmer and simpler; none of the gray enterprise gloom).
- Accessibility: WCAG AA contrast minimum; states never conveyed by color alone. Verify amber and green as text-on-white — both are borderline for small text.

### Account status states (platform level — see 12-platform-ops.md)
The organization's account can be in trial, active, or suspended state, and two of these are visible to users. These states carry business tension, so their tone matters as much as their layout:
- **Trial (admin):** a quiet indicator ("Trial — N days left"), informative, never nagging. No countdown drama; Sergey is evaluating, not being pressured.
- **Suspended (admin):** a persistent, calm banner: access is read-only, all data and past reports remain fully available and exportable. The message is factual, never punitive — "the account is paused; contact us to resume" — because the design must embody the product's rule that evidence is never held hostage. No red alarm styling: this is an administrative state, not an error.
- **Suspended (mobile, worker):** the worker is the innocent party in a billing matter between companies. A neutral full-screen state: "New work is paused by your organization — your completed work is safe and has been submitted." Queued pre-suspension work still uploads (with the normal sync indicators); nothing the worker did is lost or blamed on them. Never show billing language to the worker.

## What the designer does NOT need to do now

- Onboarding and registration (tenants are created manually).
- Pricing, billing, a landing page (the landing is a separate task later).
- Admin dark theme, customization, settings.

## Deliverables expected from design (by priority)

1. Mobile: job list → object/checklist → camera/photos → the "Sdano!" state → sync states, including the offline flow. The whole journey.
2. Admin: dashboard (list + map), object card with photos.
3. PDF report template.
4. App icon (simplified mark — the stamp outline + checkmark, no text; test at 48px next to WhatsApp) and a minimal Sdano logo lockup.
5. Account status states (lower priority, needed before first trial client): trial indicator, suspended banner (admin), suspended state (mobile).
