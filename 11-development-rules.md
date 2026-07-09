# Sdano — Development Rules & Conventions

The engineering rulebook. AGENTS.md is the operational digest of this document for AI agents; this is the full version with rationale. Rules exist to protect two things: **evidence integrity** (the product's legal core) and **the solo developer's velocity** (the project's survival condition).

## 1. Repository & branching

- Trunk-based: `main` is always deployable. Short-lived feature branches, squash-merged.
- Branch naming: `feat/...`, `fix/...`, `chore/...`, `docs/...`.
- Conventional commits; the squash commit message is the changelog entry.
- No long-lived branches, no develop branch, no git-flow — one developer, one trunk.
- Every PR (even self-merged) gets a description: what, why, and "docs updated?" A future contributor — or the OSS crowd after the pivot — reads history, not memory.

## 2. Documentation discipline

- `docs/` is normative, not descriptive. Code that contradicts a doc is a bug in one of them — fix whichever is wrong **in the same PR**.
- Architectural decisions that reverse a documented one get a short note in the relevant doc ("Changed on DATE: ... because ...") — cheap ADR without ADR ceremony.
- The "deliberate non-features" list (AGENTS.md) is the project's immune system against scope creep — additions to it are welcome, silent violations are not.

## 3. Go (API)

- Layout: `apps/api/internal/{domain}/` packages (objects, workorders, photos, reports, auth); `cmd/api/main.go` wires everything. No `pkg/`, no hexagonal ceremony — internal packages with clear ownership.
- Handlers are huma operations: typed input/output structs, validation via struct tags. Business logic lives in the domain package, not in handlers.
- **SQL only via sqlc.** Queries in `db/queries/{domain}.sql`. If a query is hard to express in sqlc, write the SQL anyway (sqlc handles most things); dropping to raw pgx is allowed only with a comment explaining why.
- Transactions: domain functions accept a `pgx.Tx`-compatible querier where atomicity matters (execution upsert + items is one transaction).
- Errors: wrap with context at every boundary; API errors map to RFC 7807 with stable slugs. Slugs are API contract — renaming one is a breaking change.
- Context first arg everywhere; no goroutines without a clear owner and cancellation path (the report worker is the pattern: one worker, one queue, graceful shutdown).
- Style: gofmt + golangci-lint (config in repo) — zero warnings policy; disagreement with a linter = change the config in a dedicated PR, not an inline nolint.

## 4. TypeScript (admin & mobile)

- Strict mode, no `any` (an `unknown` you narrow is fine). ESLint + Prettier, zero warnings.
- API access **only** through orval-generated clients from `packages/api-client`. A hand-written fetch to our API in app code is a review blocker.
- State: server state via TanStack Query (admin) / the local SQLite read model (mobile). No Redux-class global stores until a concrete need is documented.
- Mobile: every user action follows the pattern *apply locally → enqueue outbox job → render from local state*. Network status never gates a user action; it only gates the flusher.
- UI strings via the i18n layer from day one (RU is the first locale, EN skeleton exists) — even for "temporary" screens, because temporary screens go to the demo.

## 5. Database

- Migrations: plain SQL, additive, backward-compatible (deploy runs migrations before the new binary; a rollback must not need a down-migration in anger). `down` files exist and work, verified in CI against a scratch DB.
- Naming: snake_case, singular table names, `{table}_id` FKs, `created_at` on everything, `deleted_at` only where history matters.
- Every new domain table: `tenant_id NOT NULL REFERENCES tenant(id)` + it appears in the WHERE of every query. Reviewer's first check.
- No triggers for business logic (debuggability), CHECK constraints for invariants — the photo single-parent CHECK is the pattern.
- Seed/fixture data lives in `db/seed/` as SQL or a small Go tool; `make seed-demo` must always work — it's a sales instrument, not a dev convenience.

## 6. Testing policy (opinionated, budget-aware)

Priority order — test money-and-evidence, skip ceremony:

1. **Idempotency properties** (server): replaying any mobile mutation sequence converges. This is the product's core guarantee; property-style tests, run in CI against real Postgres (testcontainers or the compose DB).
2. **Outbox state machine** (mobile): coalescing, backoff, photo pipeline transitions, kill-and-restart recovery. Pure-logic unit tests, no device needed.
3. **Report data queries**: the aggregate queries feeding the PDF, against fixtures — a wrong number in a municipal report is a client-trust incident.
4. **Auth flows**: invite claim, token rotation, revocation.
5. Everything else: test when it breaks (every bugfix ships with a regression test), not preemptively.

No coverage targets. A missing test for critical paths blocks merge; a missing test for a CRUD list endpoint does not.

## 7. Security & data rules

- Secrets only in env; `.env.example` lists every variable with a fake value. Adding a config value = adding it to `.env.example` in the same PR.
- Tokens/codes: stored hashed, logged never. Log lines are reviewed for PII with the same seriousness as query WHERE clauses for tenant_id.
- Presigned URL lifetimes: 15 min PUT / 5 min GET — changing these is a security decision, not a convenience tweak.
- The production S3 credential has no DELETE permission (evidence protection); local/dev MinIO may differ. Code must therefore never *rely* on being able to delete objects.
- Dependencies: renovate/dependabot on; a new dependency needs a sentence of justification in the PR ("no stdlib/existing-dep way to do X reasonably").

## 8. AI-assisted development

AI agents (Claude Code and peers) are first-class contributors here, under human review. Ground rules:

- Agents read AGENTS.md; humans keep it current — it is the single most leveraged file in the repo. When an agent repeatedly makes the same mistake, the fix is an AGENTS.md edit, not a repeated correction.
- Agent output is reviewed with the same checklist as human code (see §9) — *especially* golden rules 1–4, where an agent's plausible-looking shortcut (a non-idempotent endpoint, a missing tenant_id) is exactly the class of bug that slips through.
- Agents may update docs, and are encouraged to propose additions to the "deliberate non-features" list when they detect themselves about to "improve" one.
- Generated code (sqlc, orval) is never hand-edited by agents or humans — the check is mechanical (CI drift check), the rule is absolute.
- Large agent-driven changes land as a series of small PRs following the vertical-slice roadmap, not as one mega-commit — reviewability is the constraint, not the agent's context window.

## 9. Review checklist (every PR, including your own)

1. Tenant isolation: every new/changed query filters by tenant_id.
2. Idempotency: mobile-facing mutations are replayable upserts.
3. Evidence: no code path silently drops or mutates photos/executions/reports.
4. Offline: mobile features work in airplane mode; new actions go through the outbox.
5. Types: pipeline regenerated, no hand-edited generated files, CI drift checks green.
6. Migrations: additive, reversible, backward-compatible.
7. Docs: updated if any contract or behavior changed.
8. Secrets/PII: nothing new logged, nothing new collected, `.env.example` current.
9. Deps: no new heavy dependency without written justification.
10. Strings: user-facing text externalized, RU wording uses the client's vocabulary.

## 10. Definition of done (duplicated in AGENTS.md — keep in sync)

Works offline (if mobile) → generated code committed → CI green → docs current → isolation/PII/deps intact → migration additive and reversible.
