# Kelma FSRS-6 architecture

Status: implementation design

This document defines scheduling and optimization across KelmaSync, Kelma
clients, KelmaDesktop compatibility, and Kelma Immersion. It supersedes the
older assumption that uploaded card scheduling snapshots are shared truth.

## Goals

- Treat immutable review history as the scheduling source of truth.
- Publish an FSRS-6 server projection for legacy KelmaDesktop and Kelma
  Immersion without making that projection authoritative for modern clients.
- Keep production scheduling inside the Go KelmaSync process.
- Keep optimization user-initiated and local to a capable client.
- Make pending, failed, stale, and unchanged projections distinguishable.
- Validate Go and Kotlin behavior against a pinned independent Python oracle.
- Preserve permissive licensing and upstream attribution.

## Non-goals

- Running Python in production.
- Automatically optimizing accounts in the background.
- Synchronizing device-local queues, burying state, or study ordering. Per-deck
  New/day and Reviews/day limits remain ordinary synchronized deck metadata.
- Treating a due date, interval, or FSRS memory snapshot as review truth.
- Executing arbitrary Anki custom-scheduler code.

## Components

### KelmaSync

KelmaSync contains an internal Go FSRS-6 package under `internal/fsrs`. The
package begins as a focused adaptation of the MIT-licensed
`open-spaced-repetition/go-fsrs` v4.0.0 source at commit
`61159b9b3891b1279b63206afef4086d7ef92992`. The package is changed where
necessary to match the pinned Python scheduler and Kelma's deterministic replay
contract.

It is internal rather than a separate module because its API, history identity,
projection metadata, and failure handling are KelmaSync implementation details.
Future upstream fixes are reviewed and selectively merged.

KelmaSync:

- stores immutable reviews;
- stores account scheduler profiles;
- validates profiles uploaded by clients;
- replays affected card histories through the internal scheduler;
- publishes the compatibility result in `card.scheduling`;
- derives cloud due counts and Immersion state from the applied projection;
- records projection progress and failure independently of scheduling output.

### `kelma-fsrs-v6`

`kelma-fsrs-v6` is a separate Apache-2.0 Kotlin Multiplatform library. It will provide:

- the FSRS-6 scheduler and memory-state replay in common Kotlin;
- deterministic scheduling with configurable steps, retention, and maximum
  interval;
- a local optimizer in a later implementation stage;
- JVM, Android, iOS, macOS, and Linux support;
- parity tests against the same Python fixtures used by KelmaSync.

Kelma clients use this library for device-local scheduling. Local queues remain
independent of the server compatibility projection.

### Python reference tooling

Python is development and release tooling only:

- `py-fsrs==6.3.1` is the scheduling oracle.
- `fsrs-optimizer==6.5.0` is the optimizer oracle.
- A pinned container generates immutable JSON fixtures.
- Routine Go and Kotlin tests consume committed fixtures and require no network
  or Python installation.

Python is not deployed in the production sync path.

## Authoritative data and projections

The authority order is:

1. Immutable review events.
2. A versioned scheduler profile chosen by the user.
3. Derived local or server scheduling projections.

A review event remains valid if every scheduling projection is deleted. A
projection must be reproducible from review history, profile configuration, and
scheduler version.

Modern Kotlin clients upload review facts but not their local due dates, FSRS
memory state, queue counters, or daily quota-consumption state. Configured
per-deck New/day and Reviews/day limits synchronize independently as deck
metadata. Legacy scheduling payloads may still be accepted during migration,
but they do not override a server-managed projection.

## Canonical card identity and review order

Collection-local numeric card IDs are not portable. Replay maps each review to
its logical card using:

```text
(account, note_guid, card_ordinal)
```

Reviews for a logical card are ordered by:

1. review timestamp;
2. immutable review ID as the tie-breaker.

Re-sending the same ID and immutable content is idempotent. Reusing an ID with
different immutable content is a review conflict. An out-of-order accepted
review invalidates incremental state and causes complete replay of that card.
A synchronized schedule-reset cutoff does not delete any review. Replay ignores
reviews at or below the monotonic cutoff and starts from a new card; if no later
review exists, the compatibility projection is New.

The canonical history hash is computed from a versioned encoding of the reset
cutoff plus the sorted immutable fields required for replay. It excludes card text, templates, current
scheduling, server timestamps, and mutable deck placement. The hash format is
versioned before use as a persistence or API contract.

## FSRS-6 server profile

The initial default profile is:

- parameters:
  `[0.212, 1.2931, 2.3065, 8.2956, 6.4133, 0.8334, 3.0194, 0.001, 1.8722, 0.1666, 0.796, 1.4835, 0.0614, 0.2629, 1.6483, 0.6014, 1.8729, 0.5425, 0.0912, 0.0658, 0.1542]`;
- desired retention: `0.90`;
- maximum interval: `36500` days;
- learning steps: `1m, 10m`;
- relearning steps: `10m`;
- short-term scheduling enabled;
- fuzzing disabled.

Fuzzing remains disabled so replay is deterministic and Go/Kotlin projections
can be compared exactly apart from documented floating-point tolerances.

### User control

Parameter and retention ownership are independent:

```text
parameter_source: default | client_optimized | manual
retention_source: default | client_optimized | manual
```

Desired retention is allowed from `0.70` through `0.99`. Higher retention means
more reviews. A manually selected retention does not disable parameter
optimization, and manually entered parameters do not prevent a user from
requesting an optimized-retention recommendation.

Editing the 21 parameters sets `parameter_source=manual`. Editing retention sets
`retention_source=manual`. Delayed optimization results never overwrite manual
values.

The server initially stores one profile per account. Per-deck profiles and
preset assignments are a later extension. Device-local clients may already use
per-deck settings without uploading them as the account server profile.

### Profile identity

Each profile version records at least:

- monotonically increasing profile version;
- FSRS algorithm version;
- scheduler implementation and version;
- 21 parameters and parameter source;
- desired retention and retention source;
- maximum interval;
- learning and relearning steps;
- short-term and fuzzing flags;
- configuration hash;
- optimizer name/version when applicable;
- optimization history hash and through-review ID when applicable;
- creator client and creation timestamp.

The configuration hash includes every value capable of changing a projection.

## Explicit local optimization

Optimization never starts automatically. The user presses **Optimize** in a
capable client.

The workflow is:

1. Sync confirmed review history.
2. Freeze a local history snapshot.
3. Verify that the history is suitable for optimization.
4. Run the local optimizer without changing the active profile.
5. Show the candidate parameters, candidate retention, history size, optimizer
   version, and quality metrics.
6. Let the user cancel or press **Apply optimized settings**.
7. Upload the candidate with its base profile version and history provenance.
8. Keep the previous profile active until the server accepts the candidate.
9. Reproject server cards after profile activation.

An interrupted, cancelled, or failed local run has no server effect.

Initial suitability guidance is at least 1,000 qualifying cross-day reviews
across at least 100 cards, with both recalled and forgotten outcomes. These are
conservative product thresholds, not an FSRS mathematical constant, and may be
changed after validation. A client must explain insufficient data rather than
silently returning defaults.

Desktop is the preferred environment for large optimization jobs. Mobile
optimization is explicit, cancellable, and should warn about battery use.

### Candidate validation

KelmaSync does not trust a client implementation merely because it is a Kelma
client. It validates:

- authenticated account and client identity;
- supported algorithm and schema versions;
- exactly 21 finite parameters;
- every parameter within the pinned official bounds;
- retention in `0.70..0.99`;
- a known history prefix/hash when optimization provenance is supplied;
- optimistic `base_profile_version` ownership;
- finite and structurally valid metrics.

Metrics are diagnostic and do not grant additional authority. A valid manual
profile may omit optimizer provenance.

## Profile API

KelmaSync exposes the profile independently from the collection batch API:

```text
GET /v2/scheduler-profile
```

Returns the active profile, version, source metadata, and reprojection summary.

```text
PUT /v2/scheduler-profile
```

Accepts a complete candidate plus `base_profile_version`. Identical retries are
idempotent. If another device changed the profile from the supplied base,
KelmaSync returns `409` with the current and attempted profiles.

A client then offers the user the same explicit conflict choices used for other
user-owned settings: retain server, replace with local, or rerun optimization
from the current profile/history.

Resetting to FSRS-6 defaults and requesting local optimization are separate UI
actions.

## Server projection lifecycle

A scheduling payload and the status of producing it are separate data.
For each logical card, KelmaSync records:

- desired history hash and through-review ID;
- applied history hash and through-review ID;
- desired and applied profile versions;
- status;
- job ID and attempt count;
- last sanitized error code;
- last attempt and next retry timestamps.

Statuses are:

```text
pending | running | succeeded | failed | superseded
```

Profile-candidate workflows may additionally report
`completed_no_improvement`; that is a client optimization result, not a server
projection failure.

### New review

When a review is accepted, the same database transaction updates desired
projection identity and creates or supersedes an idempotent projection job. The
review transaction does not claim that scheduling was updated merely because
the old scheduling JSON remains present.

### Success

The worker re-reads the current desired identity, replays canonical history,
and atomically:

- writes the new legacy `card.scheduling` projection;
- copies desired history/profile identity to applied identity;
- marks the projection `succeeded`;
- updates the card modification stream used by legacy clients and summaries.

A byte-for-byte identical scheduling result is still a successful regeneration
because the applied history and profile identity advance.

### Failure

Failure preserves the last applied scheduling payload and marks it stale:

```text
desired identity != applied identity
status = failed
```

Clients and conflict logic must not interpret this as “nothing changed.” The
sanitized error code is sync-visible; stack traces and sensitive details remain
in server logs. Jobs use bounded exponential retry and remain manually
retryable.

### Supersession and compare-and-swap

A job result is published only if its expected job ID, desired history identity,
and desired profile version are still current. Otherwise that job is
superseded and exits without publication while the per-card row continues to
show the newer job's current state. This prevents an old replay or profile job
from overwriting newer work.

### Execution

Projection jobs run inside the KelmaSync Go deployment. A database-backed queue
survives process restarts. Workers claim jobs with database locking, process
bounded batches, and may safely retry after interruption. No Python service or
external scheduling API is involved.

## Legacy scheduling and conflict ownership

Once an account/card is projection-managed, the server owns its legacy
`card.scheduling` compatibility field. A client scheduling upload cannot win by
having a newer timestamp. Content changes such as note identity, card ordinal,
or deck movement remain independent from scheduling projection ownership.

Migration may temporarily retain opaque uploaded scheduling for cards with no
usable review history. The record must identify that origin explicitly; it is
not mislabeled as an FSRS-6 server projection. After a server projection is
successfully created, subsequent legacy scheduling uploads are ignored or
stored only as diagnostics.

Old KelmaDesktop versions will consume the published server scheduling field.
Future clients can explicitly select local scheduling, server FSRS, or legacy
snapshot behavior. Modern Kotlin clients always derive local scheduling from
history and local policy.

## Summaries, due counts, and daily state

Kelma Immersion and server due summaries use only successfully applied server
projections. A stale projection may continue to appear with its last applied
due value, but the summary model must expose or internally count stale/failed
state so operations can distinguish incomplete projection from no change.

Daily study counts are derived from immutable review history for server
reporting. Admission limits, counters, ordering, and burying remain client
behavior, while the account's versioned IANA timezone and rollover hour define
one shared DST-aware civil-day identity. Legacy `study_days` payloads may remain
supported during migration but do not become scheduler truth for Kotlin clients.

## Python parity contract

The committed fixture metadata includes:

- generator package/version and source commit;
- generator container digest;
- algorithm version;
- exact parameters and configuration;
- fixture schema version and SHA-256;
- UTC input events and expected outputs.

Coverage includes:

- all ratings and first-review outcomes;
- complete learning, review, and relearning trajectories;
- same-day and fractional-day reviews;
- completed-24-hour and UTC-calendar-boundary cases;
- Python rounding boundaries;
- early and overdue reviews;
- custom parameter and retention profiles;
- minimum and maximum interval behavior;
- empty learning/relearning steps;
- out-of-order input normalized to canonical order;
- duplicate-event idempotency;
- long deterministic stress histories.

Kelma's canonical elapsed-day and rounding behavior must match the pinned
Python scheduler. Any intentional divergence is documented in fixture metadata
and tests rather than hidden behind a broad tolerance. Floating-point checks use
small documented absolute and relative tolerances on JVM and Kotlin/Native.

A dependency or algorithm upgrade creates a new fixture file and migration
plan. Existing fixtures are not silently rewritten.

## Security and privacy

Local optimization uses review history already present on the client. Profile
uploads contain parameters, retention, optimizer/provenance metadata, and
quality metrics; they contain no note fields, templates, card rendering, media,
passwords, or tokens.

Server logs and projection errors exclude card contents and credentials.
History/profile hashes are account-scoped and are not cross-account tracking
identifiers.

## Licensing and provenance

KelmaSync carries a root Apache License 2.0 `LICENSE` and a `NOTICE` file. The repository
also receives `THIRD_PARTY_NOTICES.md`, and `internal/fsrs/UPSTREAM.md` records
source files, exact upstream commit, upstream license, and intentional changes.
The Open Spaced Repetition MIT copyright and license text are retained.

`kelma-fsrs-v6` has its own Apache License 2.0 and provenance documentation. Python
packages are development dependencies only; their pinned MIT and BSD-3-Clause
notices remain in fixture-generation documentation and container metadata.

No generated or project documentation includes tool attribution unrelated to
copyright and source provenance.

## Rollout

1. Add KelmaSync licensing and upstream provenance files.
2. Add pinned Python fixture generation and commit the first oracle fixture.
3. Add the internal Go FSRS package and make parity tests pass.
4. Add scheduler-profile and projection-status persistence migrations.
5. Add profile validation and conflict-safe profile APIs.
6. Add the durable in-process projection worker.
7. Reproject test accounts and compare legacy scheduling and Immersion output.
8. Backfill production accounts in bounded batches while exposing progress.
9. Create `kelma-fsrs-v6` and make its scheduler pass the same fixtures.
10. Add explicit local optimization only after Kotlin scheduler parity and
    persistence are stable.

Steps 1–6 are implemented. Steps 7–10 remain deployment, cross-client, Kotlin,
and optimization work.

Backfill never deletes review history. A card retains its prior scheduling
payload until a successful FSRS-6 projection replaces it. Rollback disables new
job claiming and profile activation while retaining reviews, profiles, job
state, and the last applied projections.

## Acceptance criteria

- Replaying identical history/profile inputs is deterministic.
- Go matches pinned Python fixtures, including time and rounding edges.
- Kotlin matches the same fixtures on JVM and Native.
- Duplicate and out-of-order reviews produce the canonical projection.
- A failed job is visible as stale/failed and cannot appear unchanged.
- A superseded job cannot publish.
- Manual profile values cannot be overwritten by delayed optimization output.
- Profile conflicts require explicit ownership resolution.
- Profile changes trigger complete affected-card reprojection.
- Legacy scheduling uploads cannot overwrite a managed server projection.
- Immersion and due summaries identify incomplete projection state.
- Production has no Python runtime or external scheduler dependency.
