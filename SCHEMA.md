# KelmaSync v2 — Database Schema

Postgres. One database per deployment, one schema per user (or row-level
security with a `user_id` column — TBD based on scale). Examples below use
a single shared schema with `user_id` on every table.

---

## Users and clients

```sql
CREATE TABLE users (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username    TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE clients (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label       TEXT NOT NULL,  -- "iPhone 15", "MacBook — Anki plugin"
    last_seen   TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    client_id   UUID NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

---

## Notetypes

```sql
CREATE TABLE notetypes (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    notetype_id     BIGINT NOT NULL,   -- Anki's local id, for client mapping
    name            TEXT NOT NULL,
    -- full definition as JSON: fields, templates, css, config
    definition      JSONB NOT NULL,
    checksum        TEXT NOT NULL,     -- hash of definition
    modified_at     TIMESTAMPTZ NOT NULL,
    client_modified_at TIMESTAMPTZ NOT NULL,
    last_client_id  UUID REFERENCES clients(id),
    UNIQUE (user_id, notetype_id)
);
```

---

## Decks

```sql
CREATE TABLE decks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,   -- full path, e.g. "Japanese::Vocab"
    -- scheduler config: new/day, review limits, etc. as JSON
    config      JSONB NOT NULL DEFAULT '{}',
    checksum    TEXT NOT NULL,   -- hash of config
    modified_at TIMESTAMPTZ NOT NULL,
    client_modified_at TIMESTAMPTZ NOT NULL,
    last_client_id UUID REFERENCES clients(id),
    UNIQUE (user_id, name)
);
```

---

### Deck limit history

```sql
-- One row per change of a deck's daily limits (config newLimit / reviewLimit),
-- with the limits in effect before it; written by the trigger decks_limit_history
-- (any client's write, renames carried over). Review days score each past day
-- with the limits it had, so raising a limit doesn't rewrite earlier days.
CREATE TABLE deck_limit_history (
    id          BIGSERIAL PRIMARY KEY,
    user_id     TEXT        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    deck_name   TEXT        NOT NULL,
    limits      JSONB       NOT NULL,   -- {"newLimit": …, "reviewLimit": …} before the change
    changed_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

## Notes

```sql
CREATE TABLE notes (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    guid        TEXT NOT NULL,
    notetype_id BIGINT NOT NULL,   -- references notetypes.notetype_id
    -- fields stored as ordered JSON array, one entry per notetype field
    fields      JSONB NOT NULL,
    tags        TEXT[] NOT NULL DEFAULT '{}',
    checksum    TEXT NOT NULL,     -- hash of fields + tags
    modified_at TIMESTAMPTZ NOT NULL,
    client_modified_at TIMESTAMPTZ NOT NULL,
    last_client_id UUID REFERENCES clients(id),
    UNIQUE (user_id, guid)
);
```

---

## Cards

```sql
CREATE TABLE cards (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    card_id     BIGINT NOT NULL,   -- Anki's local card id, for client mapping
    note_guid   TEXT NOT NULL,     -- references notes.guid
    deck_name   TEXT NOT NULL,     -- references decks.name
    ord         INT NOT NULL,      -- template index within the notetype
    -- scheduling state as JSON: due, interval, factor, reps, lapses, type, queue
    scheduling  JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ,
    study_state TEXT NOT NULL DEFAULT 'active'
        CHECK (study_state IN ('active', 'suspended')),
    study_state_modified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    study_state_client_modified_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    study_state_client_id UUID REFERENCES clients(id),
    schedule_reset_through_review_id BIGINT NOT NULL DEFAULT 0,
    schedule_reset_modified_at TIMESTAMPTZ NOT NULL
        DEFAULT '1970-01-01T00:00:00Z',
    schedule_reset_client_modified_at TIMESTAMPTZ NOT NULL
        DEFAULT '1970-01-01T00:00:00Z',
    schedule_reset_client_id UUID REFERENCES clients(id),
    due_date_override_ms BIGINT NOT NULL DEFAULT 0
        CHECK (due_date_override_ms >= 0 AND due_date_override_ms % 86400000 = 0),
    due_date_override_modified_at TIMESTAMPTZ NOT NULL
        DEFAULT '1970-01-01T00:00:00Z',
    due_date_override_client_modified_at TIMESTAMPTZ NOT NULL
        DEFAULT '1970-01-01T00:00:00Z',
    due_date_override_client_id UUID REFERENCES clients(id),
    modified_at TIMESTAMPTZ NOT NULL,
    client_modified_at TIMESTAMPTZ NOT NULL,
    last_client_id UUID REFERENCES clients(id),
    UNIQUE (user_id, note_guid, ord)
);
```

`created_at` is the immutable original card-creation time. It is nullable so
legacy cards without trustworthy source metadata remain Unknown; migration 010
backfills plausible positive Anki millisecond IDs without fabricating dates for
negative or arbitrary IDs.

`study_state` is durable synchronized user intent. Its independent timestamps
prevent server FSRS projection writes to `scheduling` from overwriting or
reordering suspension changes. `schedule_reset_through_review_id` is a monotonic,
independently synchronized projection cutoff: history remains stored, while
FSRS replay starts after the cutoff. `due_date_override_ms` is independently
synchronized user intent (`0` means no override). It changes only queue timing,
not FSRS memory state, and a later review or schedule reset automatically clears it.

---

## Review history

```sql
CREATE TABLE reviews (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    review_id       BIGINT NOT NULL,
    source_card_id  BIGINT NOT NULL DEFAULT 0,
    note_guid       TEXT NOT NULL DEFAULT '',
    card_ord        INT NOT NULL DEFAULT 0,
    deck_name       TEXT NOT NULL DEFAULT '',
    ease            SMALLINT NOT NULL,
    interval        INT NOT NULL,
    last_interval   INT NOT NULL,
    factor          INT NOT NULL,
    taken_millis    INT NOT NULL,
    review_kind     SMALLINT NOT NULL,
    checksum        TEXT NOT NULL,
    modified_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_client_id  UUID REFERENCES clients(id),
    UNIQUE (user_id, review_id)
);
```

Review rows are append-only, like native Anki sync. `source_card_id` is retained
as a fallback, but clients map history through `(note_guid, card_ord)` because
card ids differ between collections.

## FSRS-6 profiles and projections

`scheduler_profiles` stores immutable, account-wide profile versions uploaded
through explicit user action. Version `0` is the implicit pinned default and is
not stored. Each row records the complete 21-parameter configuration, ownership
sources, optimizer provenance when applicable, and a configuration hash.

`scheduler_projections` is keyed by `(user_id, note_guid, card_ord)`. It stores
separate desired and applied history/profile identities plus durable job state:

```text
desired_history_hash / desired_through_review_id / desired_profile_version
applied_history_hash / applied_through_review_id / applied_profile_version
status / job_id / attempt_count / last_error_code / retry timestamps
```

Accepting a new rating review atomically updates desired identity and marks the
projection pending. The in-process Go worker publishes `cards.scheduling` and
advances applied identity in one transaction. Failed jobs preserve the previous
scheduling JSON while remaining explicitly stale.

See `migrations/005_fsrs_projections.sql` for the complete constraints and
indexes and `FSRS_DESIGN.md` for ownership and replay semantics.

## Study-day policy

```sql
CREATE TABLE study_day_policies (
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    version         BIGINT NOT NULL CHECK (version > 0),
    timezone_id     TEXT NOT NULL,
    day_start_hour  SMALLINT NOT NULL CHECK (day_start_hour BETWEEN 0 AND 23),
    idempotency_key TEXT NOT NULL,
    modified_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_client_id  UUID REFERENCES clients(id),
    PRIMARY KEY (user_id, version),
    UNIQUE (user_id, idempotency_key)
);
```

Versions are append-only. The highest version is the active account-wide
DST-aware civil-day policy. An absent row has the implicit UTC/04:00 version-0
default so old accounts remain readable until a timezone-aware client publishes
the user's system timezone.

## Daily study counters

```sql
CREATE TABLE study_days (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    day                   BIGINT NOT NULL,
    deck_name             TEXT NOT NULL,
    new_studied           INT NOT NULL DEFAULT 0,
    review_studied        INT NOT NULL DEFAULT 0,
    learning_studied      INT NOT NULL DEFAULT 0,
    milliseconds_studied  BIGINT NOT NULL DEFAULT 0,
    modified_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_client_id        UUID REFERENCES clients(id),
    UNIQUE (user_id, day, deck_name)
);
```

`day` is a portable epoch-day. Clients translate between it and Anki's
collection-relative deck day using the collection creation day (`crt`).

---

## Media

```sql
CREATE TABLE media (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filename    TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL,
    -- storage key in object store (S3/R2); NULL if not yet uploaded
    storage_key TEXT,
    -- reference count: number of notes whose fields mention this filename
    ref_count   INT NOT NULL DEFAULT 0,
    modified_at TIMESTAMPTZ NOT NULL,
    UNIQUE (user_id, filename)
);
```

---

## Tombstones

```sql
CREATE TABLE tombstones (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- which resource type was deleted
    type        TEXT NOT NULL CHECK (type IN ('note', 'card', 'deck', 'notetype', 'media')),
    -- the stable identifier for the deleted resource
    -- (guid for notes, card_id for cards, name for decks, notetype_id for notetypes, filename for media)
    resource_id TEXT NOT NULL,
    deleted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_client_id UUID REFERENCES clients(id),
    UNIQUE (user_id, type, resource_id)
);
```

---

## Indexes

```sql
-- manifest queries: "give me everything changed since <timestamp>"
CREATE INDEX idx_notes_user_modified      ON notes      (user_id, modified_at);
CREATE INDEX idx_cards_user_modified      ON cards      (user_id, modified_at);
CREATE INDEX idx_reviews_user_modified    ON reviews    (user_id, modified_at);
CREATE INDEX idx_study_days_user_modified ON study_days (user_id, modified_at);
CREATE INDEX idx_notetypes_user_modified  ON notetypes  (user_id, modified_at);
CREATE INDEX idx_decks_user_modified      ON decks      (user_id, modified_at);
CREATE INDEX idx_tombstones_user_modified ON tombstones (user_id, deleted_at);

-- individual record lookups
CREATE INDEX idx_notes_guid       ON notes  (user_id, guid);
CREATE INDEX idx_cards_card_id    ON cards  (user_id, card_id);
CREATE INDEX idx_cards_note_guid  ON cards  (user_id, note_guid);
CREATE INDEX idx_media_filename   ON media  (user_id, filename);
```

---

## Notes on design choices

- **JSONB for fields and scheduling** — note fields are an ordered array whose
  length and meaning are defined by the notetype. Card scheduling remains JSONB
  for Anki compatibility, but a successfully managed payload is a derived
  server FSRS-6 projection rather than opaque shared truth.
- **`modified_at` vs `client_modified_at`** — `modified_at` is the server
  timestamp of the last accepted write (used for manifest filtering).
  `client_modified_at` is what the client reported and remains relevant for
  card content reconciliation. It cannot overwrite a managed server FSRS-6
  scheduling projection.
- **`notetype_id` as BIGINT not UUID** — Anki assigns notetype ids as
  millisecond timestamps locally. Storing the original id avoids a mapping layer
  on the client.
- **`deck_name` on cards, not a foreign key** — decks are identified by name.
  Storing the name directly on each card avoids a join and keeps card records
  self-contained for sync.
- **Append-only reviews** — Anki does not synchronize revlog deletion/undo.
  Review ids are immutable millisecond ids; an id reused with different content
  is surfaced as a conflict instead of silently dropping either history row.
- **Portable daily counters** — revlog rows power statistics, while Anki's daily
  limits use separate deck counters. Epoch-day snapshots keep those limits in
  sync even when collections have different creation timestamps.
- **Media `ref_count`** — incremented when a note is pushed whose fields
  reference the filename, decremented on note update/delete. When `ref_count`
  reaches zero the storage object can be deleted. Not enforced as a DB
  constraint; maintained by application logic.
