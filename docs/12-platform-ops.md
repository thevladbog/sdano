# Sdano — Platform Operations (Operator Admin & Billing)

The third, previously implicit level of the system. Levels: (1) worker — mobile app, (2) tenant staff — admin panel, (3) **platform operator** — the developer running Sdano as a business. This document defines level 3.

## Guiding principle

**No operator web UI until manual operations measurably eat evenings.** The operator audience is one person with SSH access and SQL skills; a beautiful back-office for that audience is procrastination wearing a product costume. The progression:

1. **Phase A (clients 1–10):** CLI + DB fields + a weekly Telegram digest. This phase is specified fully below.
2. **Phase B (clients ~10–30):** a minimal `/ops` web section (read-mostly: tenant list, health, storage) behind separate auth — built only when Phase A demonstrably hurts.
3. **Phase C (self-serve):** registration, payment provider, automated dunning — only after repeatable demand is proven. Explicitly out of scope now.

## Tenant lifecycle (schema — needed NOW, it affects app behavior)

```sql
CREATE TYPE tenant_status AS ENUM ('trial', 'active', 'suspended', 'archived');

ALTER TABLE tenant ADD COLUMN status        tenant_status NOT NULL DEFAULT 'trial';
ALTER TABLE tenant ADD COLUMN trial_ends_at timestamptz;
ALTER TABLE tenant ADD COLUMN plan_note     text;          -- human-readable: "50$/mo, 20 objects, agreed 2026-08"
ALTER TABLE tenant ADD COLUMN billed_until  date;          -- covered-by-payment horizon
ALTER TABLE tenant ADD COLUMN ops_note      text;          -- operator's free-form notes
```

Status semantics — **enforced in one middleware, not scattered across handlers:**

| Status | Worker mobile | Staff admin | Reports |
|---|---|---|---|
| trial | full | full | full (watermark "trial" on PDF — optional, decide later) |
| active | full | full | full |
| **suspended** | **read-only: sees history, cannot start/submit new work** (outbox flush of already-performed work IS accepted — work done before suspension must not be lost) | **read-only: sees everything, exports everything** | can generate reports for past periods |
| archived | 401 | 401 | operator-only export |

**The evidence rule extends to billing:** suspension never holds already-collected evidence hostage. A non-paying client loses the ability to create new work, never the ability to read their photos and old reports. Rationale: (a) legal murkiness of withholding dispute evidence, (b) reputation in a small market where contractors all know each other, (c) it is simply the product's own discipline applied consistently. Deleting data happens only at `archived`, only manually, only after an export offer.

Nuance worth the emphasis: a worker's outbox may contain legitimately performed work at the moment of suspension (offline shift). The API accepts outbox flushes for executions whose `device_finished_at` precedes the suspension timestamp — evidence of performed work is never rejected on billing grounds.

**Auth endpoints and archived tenants:** the `/auth/*` routes are public, so the status middleware above never runs on them. Login, refresh, and worker-claim therefore enforce `archived` directly in the auth service and return `401 tenant-archived` rather than minting tokens for a dead tenant. `suspended`/`trial`/`active` tenants still authenticate normally; their per-request access is gated by the middleware (a suspended tenant is read-only, not locked out). A deactivated worker likewise cannot claim an invite — the resulting device token could never authenticate — so the claim fails with `invite-code-invalid`.

## Operator CLI (`sdano-ops`)

A small Go binary in the same repo (`cmd/ops/`), run via SSH on the VPS (or locally against prod DSN through an SSH tunnel). No network surface of its own = no attack surface. Commands, Phase A set:

```
sdano-ops tenant create --name "ЧистоГрад" --trial-days 30
    → creates tenant + first admin user, prints credentials + invite instructions

sdano-ops tenant list
    → table: name, status, workers (active/total), executions last 7d,
      photos count / storage GB, billed_until, trial_ends_at

sdano-ops tenant suspend --id <uuid> [--note "..."]
sdano-ops tenant activate --id <uuid>
sdano-ops tenant set-billing --id <uuid> --billed-until 2026-09-01 [--plan-note "..."]
    → omitting --plan-note keeps the existing note (the audit row records
      only what was actually applied)

sdano-ops tenant archive ...
    → phase-A deferred: not yet in the CLI. The archived-status semantics
      in this document still apply once it ships.

sdano-ops stats
    → platform totals: tenants by status, executions/day trend, storage, top tenants by usage

sdano-ops export-tenant <id> --out ./export/
    → full data export (JSON + photo manifest) — the archived-tenant obligation,
      also the GDPR/152-FZ "give me my data" answer
```

Implementation notes: reuses the domain packages and sqlc queries (no parallel logic); every mutating command writes an `ops_audit` row (what, when, note) — the operator is not above the audit discipline. Tenant ids are passed as `--id` flags rather than positional arguments: the CLI is stdlib `flag` only (no cobra), and `flag` stops parsing at the first positional token, so a uniform all-flags style is the simple correct shape.

```sql
CREATE TABLE ops_audit (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action text NOT NULL, tenant_id uuid, detail jsonb,
    performed_at timestamptz NOT NULL DEFAULT now()
);
```

## Health monitoring = churn radar

The weekly digest (a cron container posting to a Telegram bot) is the single most important operator tool — it replaces dashboards:

```
Sdano weekly:
  ЧистоГрад     active   billed to 01.09  ▂▅▇▆▅ 132 exec/wk   4 workers   ⚠ none
  ГорСервис     trial    ends in 9 days   ▁▁▂▁▁ 6 exec/wk     1 worker    ⚠ low usage
  Благоустрой   active   billed to 15.07  ▇▇▇▇▇ 210 exec/wk   7 workers   ⚠ billing due in 7d
```

Three signals per tenant: **usage trend** (executions/week — the real health metric; a client who stops uploading photos has already churned, the invoice just doesn't know yet), **billing horizon** (billed_until approaching = send invoice), **trial expiry**. This digest IS the billing reminder system in Phase A — no dunning automation.

## Billing, Phase A (deliberately manual)

- Price agreed per client (anchor: $30–80/mo by objects/workers), recorded in `plan_note`.
- Invoice issued manually (RU legal entity/self-employment mechanics — a business decision outside this doc; likely «самозанятость» or ИП at this scale, consult an accountant before the first invoice).
- Payment received → `sdano-ops tenant set-billing --billed-until +1 month/quarter`. Quarterly prepay discount encouraged — fewer invoices, better cash flow, stronger commitment signal.
- Grace behavior: digest warns 7 days before `billed_until`; suspension is a **manual decision** (a call first — at 20 clients every relationship is personal), never automatic.

**What we deliberately do NOT build now:** payment provider integration (RU: ЮKassa/Тинькофф — Phase C; international: merchant-of-record like Paddle — only relevant post-OSS-pivot or foreign expansion), metered billing, plan-limit enforcement in code (limits live in `plan_note` and human conversation; a client 3 objects over the agreed limit is a sales conversation, not a 403).

## Platform-admin access model

- Phase A: **no platform-admin role in the web app at all.** The operator surface is the CLI over SSH plus read-only SQL. The web app knows only tenant-scoped principals — the simplest possible security story and one less thing to get wrong.
- Phase B (when /ops web arrives): a separate `platform_operator` principal, separate session issuer, ideally behind Tailscale/VPN rather than the public internet, always read-mostly.

## Interaction with the OSS pivot

Everything here survives the pivot cleanly: self-hosters get `sdano-ops` as their instance-management tool (tenant creation, exports), the lifecycle enum stays (a self-hosted instance still wants trial/suspend semantics for ITS clients), and the absent billing integration becomes a feature — nothing SaaS-specific to rip out. `plan_note`-style manual billing is exactly how a self-hosting contractor would run it anyway.

## Additions to other documents

- **06-data-model:** tenant lifecycle columns + ops_audit table (this doc is the rationale; DDL to be merged there).
- **07-api-spec:** the status-enforcement middleware (one place, table above as the contract).
- **08-offline-sync:** the suspension-vs-outbox nuance (accept pre-suspension work).
- **AGENTS.md, deliberate non-features:** "no payment integration, no plan-limit enforcement in code, no operator web UI (Phase A)".
