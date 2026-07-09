# CLAUDE.md

Read `AGENTS.md` fully before making any change — it is the operational digest of this repo's rules. `docs/` is normative; `11-development-rules.md` is the full rulebook with rationale.

Non-negotiables (details in AGENTS.md):

- The type pipeline is law: SQL → sqlc → Go → huma → OpenAPI → orval → TS. Never hand-edit generated code — regenerate it.
- `tenant_id` from the authenticated principal on every domain query.
- Evidence (photos, executions, reports) is immutable and never silently dropped.
- Dependencies: latest stable, security-clean versions; verify current version and API via context7 before adding or upgrading; `govulncheck` and `npm audit` must stay green.
- Conventional commits; docs updated in the same PR as behavior changes.
