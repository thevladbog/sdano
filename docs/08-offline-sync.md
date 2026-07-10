# Sdano — Offline Sync Specification

The riskiest technical component of the product, designed before any UI is built. Goal: the worker's app is **fully functional with zero connectivity for an entire shift**, and the worker always knows the sync state without thinking about it.

## Model: local truth + replayable outbox

Three layers on the device:

1. **Working set (read model).** The `GET /worker/today` payload persisted into SQLite: objects, work orders, denormalized checklists, known open issues (slice 2). Refreshed opportunistically (app start, pull-to-refresh, successful queue flush) — never blocking work.
2. **Local mutations.** The worker's actions applied immediately to local SQLite state (optimistic, no spinners): item checked, photo captured, execution finished. The UI always renders local state.
3. **Outbox (write model).** Every mutation appended as a job in an `outbox` table. A background flusher drains it when the network allows.

The server is the source of truth *after* sync; the device is the source of truth *until* sync. Because all server writes are full-state idempotent upserts keyed by client UUIDs, the outbox needs no diffing and tolerates any amount of replay.

## Outbox schema (device SQLite)

```sql
CREATE TABLE outbox (
  seq         INTEGER PRIMARY KEY AUTOINCREMENT,  -- flush order
  kind        TEXT NOT NULL,     -- 'execution_upsert' | 'photo_presign' | 'photo_upload' | 'photo_confirm' | 'issue_upsert' ...
  entity_id   TEXT NOT NULL,     -- the client UUID
  payload     TEXT NOT NULL,     -- JSON snapshot at enqueue time (superseded by later snapshots of same entity)
  attempts    INTEGER NOT NULL DEFAULT 0,
  next_try_at INTEGER,           -- unix ms; exponential backoff
  state       TEXT NOT NULL DEFAULT 'pending'  -- 'pending' | 'inflight' | 'done' | 'blocked'
);
```

**Coalescing rule:** for `execution_upsert`, only the latest snapshot per entity matters (full-state upsert). On enqueue, earlier pending jobs for the same `entity_id` and `kind` are marked done and replaced. This keeps the queue short even if the worker toggles a checkbox twenty times.

## Photo pipeline (the hard part)

A photo travels through a small state machine, tracked in a local `photo_local` table:

```
captured → presigned → uploading → uploaded → confirmed
```

- **captured:** JPEG written to app-private storage, row created with client UUID, EXIF (time, geo) extracted immediately and stored in SQLite (never trust re-reading the file later).
- **presigned:** `POST /photos/presign` succeeded; `upload_url` + expiry stored.
- **uploading → uploaded:** PUT of bytes to S3. Interrupted PUT = retry the same PUT (S3 PUTs are atomic — a partial upload never becomes a visible object). Expired presign = go back to `presigned` via a fresh presign call (same key, safe).
- **confirmed:** `POST /photos/{id}/confirm` succeeded; local file becomes deletable (kept until confirmed + N days as paranoia buffer, storage permitting).

Three outbox job kinds map to the three network steps; each is independently retryable and idempotent. Photos upload **oldest-first, one or two in parallel max** — field networks are slow, and finishing photo 1 fully beats having 8 photos at 50%.

**Storage budget:** a shift is ~20 objects × 2–4 photos × ~3 MB ≈ 250 MB worst case. Compress on capture (target 1600px long edge, ~85 quality ≈ 400–700 KB) → a full offline shift fits in <60 MB. Compression parameters live in remote config later; hardcoded initially.

## Flusher behavior

- Triggers: connectivity regained (NetInfo), app foregrounded, new job enqueued while online, manual pull-to-refresh.
- Order: strictly by `seq`, except photo byte-uploads which run in their own lane (they're big; a 5 MB upload must not block a 2 KB execution upsert behind it). Two lanes: **metadata lane** (upserts, presigns, confirms — sequential) and **bytes lane** (S3 PUTs, ≤2 parallel).
- Backoff: 2^attempts seconds, cap 5 min, jittered. `attempts` resets on connectivity change (a new network deserves a fresh try).
- **iOS reality:** no reliance on background execution. Flush runs in foreground; on backgrounding, in-flight PUTs continue via URLSession background transfer where available, but correctness never depends on it. The UX message is honest: "open the app when back in coverage and everything finishes itself."

## Failure taxonomy

| Failure | Handling |
|---|---|
| No network / timeout | Backoff, retry forever. Never surfaces as an error to the worker — it's a *state* ("waiting for network"), not a problem. |
| 401 (token revoked) | Stop the queue (`blocked`), route to sign-in. Outbox survives; flushes after re-auth. |
| 403 `tenant-suspended` | Work performed **before** suspension still flushes (the server accepts it — see 07-api-spec). The boundary is `tenant.suspended_at`, set only by the `sdano-ops tenant suspend` CLI command (12-platform-ops.md): an outbox job's `device_finished_at` must be strictly before that timestamp to be accepted once suspended. Jobs for work attempted after suspension are impossible by construction: the app enters read-only mode on the first suspended response and disables new work. The outbox is preserved either way. |
| 409 / validation 4xx | Should be impossible (idempotent upserts) — treated as a bug: job marked `blocked`, error logged to crash reporting, red "needs attention" badge. Never silently dropped: **evidence is never lost silently.** |
| 5xx | Backoff and retry — server's problem, not the worker's. |
| Presign expired | Re-presign, same key. |
| Device storage full | Warn before capture (threshold check), never after — losing a shot the worker thinks was taken is the worst failure in the product. |
| App killed mid-flush | `inflight` jobs older than a lease timeout revert to `pending` on next start. All server ops idempotent ⇒ double-send is harmless. |
| Device clock wrong | Both device and server times are stored (see data model); reports use device time, disputes can show both. The app nudges if device time differs from server time by >5 min on a successful sync. |

## Sync state UX contract (binding for design)

The worker-visible model is exactly three states, computed from the outbox:

1. **"Всё отправлено" / all synced** — outbox empty, last flush ok.
2. **"N фото ждут сети" / waiting** — pending jobs exist, no permanent errors. Calm color; this is *normal*, not a warning.
3. **"Требует внимания" / needs attention** — a `blocked` job exists. The only state the worker must act on (re-login or contact the manager).

Placement: a persistent, quiet status strip on the route screen + a per-object queued indicator. The "Sdano!" success screen shows the offline variant explicitly: stamped success + "queued, will upload automatically." Success of the *work* is never conditional on success of the *upload*.

## What is deliberately NOT built

- **No generic sync framework, no CRDTs.** The domain has one writer per entity (the worker owns their execution); last-write-wins on full snapshots is provably sufficient. Revisit only if multi-worker executions become real (open question #1 in the data model).
- **No server→device incremental sync.** The working set is small (a day's route); full refresh of `GET /worker/today` is cheap and unbeatably simple. Delta sync is a scaling optimization for a problem we don't have.
- **No conflict UI.** There is nothing to merge; the worker never sees the word "conflict."

## Test plan (the component earns real tests)

- Unit: outbox coalescing, backoff math, photo state machine transitions.
- Integration (device): airplane-mode shift simulation — 20 objects offline, then flush; kill-app-mid-flush; presign expiry; storage-full path.
- Server: idempotency property tests — any prefix of the outbox replayed any number of times in any interleaving of the two lanes converges to the same DB state.
- Chaos script in CI: proxy that drops/delays/duplicates requests between a headless client and the API; assert convergence.

This component is also the primary build-in-public content: "An offline-first photo evidence queue without a sync framework" is the flagship article of the series.
