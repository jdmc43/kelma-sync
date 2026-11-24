# KelmaSync v2 — Design Document

The versioned FSRS-6 profile, local optimization, server projection, and
failure/conflict model are specified in [FSRS_DESIGN.md](FSRS_DESIGN.md). That
document supersedes earlier shared-scheduling assumptions in this overview.

## Core principle

A note is an independent item. Sync is the process of keeping independent items
consistent across devices. Conflicts are rare and should be surfaced clearly, not
silently resolved.

---

## What the server stores

One canonical record per note, keyed by `guid`:

- `guid` — stable identity, survives moves between decks and devices
- `fields` — the note content (array of strings, one per notetype field)
- `tags` — list of strings
- `notetype_id` + `notetype_name` — what template renders this note
- `deck` — where the note lives
- `modified_at` — authoritative UTC server timestamp of the last accepted write
- `client_modified_at` — source collection mtime, normalized to UTC for newest-wins
- `checksum` — hash of fields + tags, for fast equality checks
- `client_id` — which client last wrote this record (for display in conflict UI)

The server also stores:

- **Scheduling projections** — derived per card from immutable review history
  and a versioned FSRS-6 profile. The server publishes a compatibility
  projection for legacy KelmaDesktop and Kelma Immersion; modern clients may
  independently derive local scheduling. Projection status is stored
  separately so failed work cannot appear unchanged.
- **Card creation time** — one immutable nullable timestamp. New clients send
  the exact source time, plausible positive Anki millisecond IDs backfill legacy
  rows, and unverifiable legacy cards remain Unknown rather than receiving a
  migration or modification date.
- **Card study state** — `active` or `suspended` is durable synchronized user
  intent with its own modification clock. It is independent from both card
  content and derived scheduling, so projection jobs cannot unsuspend cards.
- **Schedule reset** — a monotonic per-card review-id cutoff is synchronized
  independently. Reviews remain immutable, but FSRS replay ignores facts at or
  below the cutoff and projects the card as New until another review arrives.
- **Manual due date** — a nullable (`0` when absent) per-card UTC due-date
  override is synchronized with its own newest-wins clock. It does not alter
  FSRS memory state, and a later review or reset clears it.
- **Review history** — Anki `revlog` rows are append-only and keyed by their
  millisecond review id. `(note_guid, card_ord)` remaps collection-local card
  ids on download, preserving complete statistics and FSRS training history.
  Review history, not a scheduling snapshot, is authoritative.
- **Study-day policy** — one versioned account-level IANA timezone and local
  rollover hour, exposed independently from collection sync so Review,
  Immersion, and future clients classify immutable event timestamps into the
  same DST-aware civil study day. Queue admission and ordering remain client
  behavior; only the day-boundary policy is shared.
- **Legacy daily counters** — accepted during migration for older clients.
  Server reporting derives study activity from review history under the shared
  study-day policy.
- **Media files** — stored as blobs keyed by filename. A note's fields may
  reference media filenames; the server ensures referenced files are present.
  Initial downloads resolve up to 500 account-owned names in one authenticated
  plan and use short-lived, scoped R2 URLs, removing one server authentication,
  database lookup, and proxy hop per blob while clients retain bounded transfer
  concurrency. Deletion is reference-counted: a file is removed only when no
  note references it. Deduplication is out of scope for v2.
- **Notetype definitions** — stored per user, synced the same way as notes:
  manifest exchange by checksum, conflict surfaced and resolved by the client
  (force local / accept server / edit and make canonical). When a notetype
  changes in a way that affects existing notes (field rename, reorder, removal),
  the server flags affected notes as requiring re-review by the user.

---

## Protocol

Modelled on CardDAV semantics: clients exchange lightweight manifests, then
fetch or push only what differs. No global counters, no binary blobs, no full
collection state on every sync.

### 1. Manifest exchange

```
GET /v2/notes?since=<iso-timestamp>
```

Returns a list of `{ guid, checksum, modified_at }` for every note changed on
the server since `since`. On first sync, omit `since` to get all notes.

Client compares this list to its local `{ guid, checksum }` index and
classifies each note as:

- **same checksum** → in sync, skip
- **server only** → pull from server
- **client only** → push to server
- **both changed** → conflict (see below)

### 2. Pull

```
GET /v2/notes/:guid
```

Returns the full note record. Client writes it locally.

### 3. Push

```
PUT /v2/notes/:guid
Body: { fields, tags, notetype_id, notetype_name, deck, client_modified_at }
```

Server accepts the write and updates `modified_at` to now. Returns the saved
record.

If the note has changed on the server since the client last saw it (checksums
diverge), the server returns `409 Conflict` with both the server's current
record and the client's attempted write in the response body. The client
decides what to do (see Conflict resolution below).

### 4. Delete

```
DELETE /v2/notes/:guid
```

Soft-delete: server marks the note deleted with a tombstone, so other clients
know to remove it rather than re-pushing it.

---

## Conflict resolution

A conflict occurs when both the client and server have changed the same note
since the last sync. The server never silently resolves this — it always returns
`409` and hands control to the client.

The client has three options, all explicit:

### Option A — Force local (client wins)
Client pushes its version with a `Force-Override: true` header. Server accepts
unconditionally and overwrites. Use when you are certain the local copy is
correct.

### Option B — Accept server (server wins)
Client discards its local changes and writes the server's record locally. Use
when the server copy is correct.

### Option C — Edit and make canonical
Client presents both versions to the user side by side. User edits a merged
version. Client pushes that merged version with `Force-Override: true`. This
becomes the new canonical record on the server and propagates to all other
clients on their next sync.

In all three cases the client has the last say. The server never picks a winner
on its own. A two-source client such as KelmaDesktop may apply newest-wins when
`client_modified_at` identifies one strictly newer copy; tied or unknown times
remain an explicit conflict.

---

## Authentication

```
POST /v2/auth/login
Body: { username, password }
Returns: { token }
```

Token passed as `Authorization: Bearer <token>` on all subsequent requests.
Tokens are long-lived (refreshed silently). No per-request re-auth.

Each client registers a `client_id` on first sync — a stable, human-readable
label (e.g. `"iPhone 15"`, `"MacBook — Anki plugin"`). The server stores this
alongside every record it accepts, so the conflict UI can show "last edited on
iPhone 15" rather than a raw timestamp.

---

## Pruning / history

The server keeps only the **current state** of each note — no note revision
history in the hot path. Anki review history is the intentional exception:
`revlog` is itself user data used by statistics and FSRS, and is retained
append-only just as native Anki sync retains it.

Optionally, an async audit log can record each accepted write as an append-only
event `{ guid, fields, tags, timestamp, client_id }`. This log can be pruned on
a schedule (e.g. keep 90 days) without affecting sync correctness. It is never
read during normal sync — only for debugging and manual recovery.

---

## What this is not

- Not a CRDT. Notes are replaced whole, not merged character-by-character.
  Field-level merging is the user's job in Option C.
- Not event-sourced. Current state is the source of truth; the audit log is
  optional and additive.
- Not the Anki wire protocol. Existing clients (plugin, desktop, mobile) that
  speak the Anki protocol continue to use `kelma_sync` (v1) until they are
  ported to this protocol.

---

## Deck sync

Notes carry the deck **name** (stable, human-readable, e.g. `Japanese::Vocab`).
Deck metadata and opaque legacy config sync separately as a flat key-value
record per deck name, using the same manifest/checksum/conflict flow as notes
and notetypes. Deck New/day and Reviews/day limits synchronize in the deck configuration as
`newLimit` and `reviewLimit`; other device-local deck study Options do not become
shared scheduling truth. The account study-day timezone and rollover hour are
also shared because all clients need one civil-day identity for limits,
statistics, and streaks.
The account server FSRS profile has the separate ownership and conflict model in
[FSRS_DESIGN.md](FSRS_DESIGN.md). No deck ids are required for reconciliation.

## Tombstone cleanup

Tombstones are kept **forever**. A tombstone is a tiny record `{ guid,
deleted_at }`. Storage cost is negligible. A client that hasn't synced in an
arbitrary amount of time will still receive correct deletes on reconnect.
No background jobs, no prune windows, no per-client acknowledgement tracking.
