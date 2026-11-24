# KelmaSync v2 — API

REST over HTTPS. All request and response bodies are JSON. All endpoints except
`/v2/auth/*` require `Authorization: Bearer <token>`.

---

The browser-owned review sync contract is documented in
[`docs/browser-review-client.md`](docs/browser-review-client.md).

## Auth

### Register
```
POST /v2/auth/register
Body: { username, password }
Response 201: { user_id }
```

### Login
```
POST /v2/auth/login
Body: { username, password, client_label }
Response 200: { token, client_id }
```

`client_label` is the human-readable device name ("iPhone 15"). If a client
with that label already exists for the user, the existing `client_id` is
returned and `last_seen` is updated. Otherwise a new client record is created.

### Logout
```
POST /v2/auth/logout
Response 204
```

Revokes the token used to make the request.

### Internal account deletion

An external account authority (when one is configured) permanently erases an
account's KelmaSync data through the shared-secret internal API:

```http
POST /v2/internal/account-deletion
x-internal-secret: <KELMA_INTERNAL_SECRET>
Body: { "email": string, "confirm": "DELETE" }
Response 200: { "ok": true, "alreadyDeleted": boolean }
```

The operation revokes all sync tokens, deletes pending media archives and the
complete account media prefix (including orphaned blobs), then deletes the user
row and all cascading collection data. It is idempotent. A storage failure
returns 502 and leaves token minting blocked until a retry succeeds. See
[`ACCOUNT_DELETION.md`](ACCOUNT_DELETION.md) for the cross-service workflow.

---

## Sync

The sync flow is always:

1. Client calls `GET /v2/sync/manifest` to find out what changed on the server.
2. Client compares to its local state, classifies each item.
3. Client pulls items the server has that it doesn't (or that differ).
4. Client pushes items it has that the server doesn't (or that differ).
5. Conflicts (both sides changed) are held for user resolution.

### Authenticated sync identity
```
GET /v2/sync/identity
Response 200: { "user_id": string, "client_id": string }
```

This allows an account-scoped mirror to fail closed unless both bearer tokens
resolve to the configured account. The route does not expose another account or
token.

### Manifest
```
GET /v2/sync/manifest?since=<iso-timestamp>
Response 200:
{
  "capabilities": ["card_flag_intents_v1", "review_retractions_v1", "review_client_operations_v1"],
  "notes":     [{ "guid": string, "checksum": string, "modified_at": string, "client_modified_at": string,
                    "mark": { "marked": boolean, "intent_id": string, "modified_at": string,
                              "client_modified_at": string, "last_client_id": string } }],
  "cards":     [{ "card_id": number, "checksum": string, "modified_at": string, "client_modified_at": string,
                   "study_state": "active" | "suspended",
                   "study_state_modified_at": string,
                   "study_state_client_modified_at": string,
                   "schedule_reset_through_review_id": number,
                   "schedule_reset_modified_at": string,
                   "schedule_reset_client_modified_at": string,
                   "due_date_override_ms": number,
                   "due_date_override_modified_at": string,
                   "due_date_override_client_modified_at": string,
                   "projection_status": string,
                   "desired_history_hash": string, "desired_through_review_id": number,
                   "desired_profile_version": number, "applied_history_hash": string,
                   "applied_through_review_id": number, "applied_profile_version": number }],
  "reviews":   [{ "review_id":   number, "checksum": string, "deck_name": string, "modified_at": string }],
  "study_days":[{ "day": number, "deck_name": string, "new_studied": number, "review_studied": number, "learning_studied": number, "milliseconds_studied": number, "modified_at": string }],
  "notetypes": [{ "notetype_id": number, "checksum": string, "modified_at": string, "client_modified_at": string }],
  "decks":     [{ "name":        string, "checksum": string, "modified_at": string, "client_modified_at": string }],
  "media":     [{ "filename":    string, "size_bytes": number, "modified_at": string, "last_client_id": string }],
  "tombstones":[{ "type": string, "resource_id": string, "deleted_at": string,
                    "last_client_id": string, "note_guid": string, "card_ord": number }],
  "server_time": string   -- use as `since` on the next sync
}
```

Omit `since` on first sync to receive everything. `modified_at` is when the
server accepted the write; `client_modified_at` is the source collection's
mtime and lets two-source clients select a uniquely newer copy. `server_time`
is the timestamp the server used to filter this response; the client stores it
and passes it as `since` next time. Every timestamp is serialized as RFC3339
UTC (`...Z`). Clients also use `server_time` as their authoritative UTC clock;
a source clock more than five minutes in the future is clamped by the server
and must not win client-side reconciliation. Portable manifests include
`last_client_id` so a two-endpoint account mirror can suppress its own echoed
writes. Future card tombstones also include the logical note/ordinal identity.

### Storage usage
```
GET /v2/usage
Response 200:
{
  "used_bytes": number,
  "media_bytes": number,
  "content_bytes": number
}
```

Returns the authenticated account's exact KelmaSync storage usage. This is an
account-wide server total, independent of the requesting client's local files.

### FSRS-6 scheduler profile

```
GET /v2/scheduler-profile
Response 200: {
  "profile": {
    "version": number,
    "algorithm": "fsrs-6",
    "scheduler_implementation": "kelma-go-fsrs",
    "scheduler_version": string,
    "parameters": [21 numbers],
    "parameter_source": "default" | "client_optimized" | "manual",
    "desired_retention": number,
    "retention_source": "default" | "client_optimized" | "manual",
    "maximum_interval": number,
    "learning_steps_seconds": [number],
    "relearning_steps_seconds": [number],
    "enable_short_term": true,
    "enable_fuzzing": false,
    "config_hash": string
  },
  "projections": {
    "pending": number, "running": number, "succeeded": number,
    "failed": number, "superseded": number, "stale": number
  }
}
```

Version `0` is the implicit pinned FSRS-6 default profile.

```
PUT /v2/scheduler-profile
Body: {
  "base_profile_version": number,
  "idempotency_key": string,
  "parameters": [21 numbers],
  "parameter_source": "default" | "client_optimized" | "manual",
  "desired_retention": number,
  "retention_source": "default" | "client_optimized" | "manual",
  "maximum_interval": number,
  "learning_steps_seconds": [number],
  "relearning_steps_seconds": [number],
  "enable_short_term": true,
  "enable_fuzzing": false,
  "optimizer": string,
  "optimizer_version": string,
  "optimization_history_hash": string,
  "optimization_through_review_id": number,
  "quality_metrics": object
}
Response 200: same shape as GET
Response 409: { "error": "scheduler_profile_conflict", "server": {...}, "client": {...} }
```

Optimization fields are required only when either source is
`client_optimized`. The history hash is SHA-256 over qualifying reviews through
the supplied review ID, ordered by `(review_id,note_guid,card_ord)`. Each row is
encoded as UTF-8 `review_id + "\\n" + byte_length(note_guid) + ":" + note_guid
+ "\\n" + card_ord + "\\n" + review_checksum + "\\n"`. Profile writes use
optimistic base-version ownership and idempotency keys. Acceptance marks
affected server projections pending; it does not upload or overwrite
device-local scheduling policy.

### Study-day policy

```
GET /v2/study-day-policy
Response 200: {
  "version": number,
  "timezone_id": string,
  "day_start_hour": number,
  "idempotency_key": string,
  "modified_at": string,
  "last_client_id": string
}
```

This small account-level endpoint is the canonical source for Review, Immersion,
and other clients that need to classify activity into the same civil study day.
`timezone_id` is an IANA timezone and `day_start_hour` is an integer from 0
through 23. Clients must apply timezone rules at the event instant, so daylight
saving transitions do not turn the setting into a fixed UTC offset. Version `0`
is the implicit `UTC`/04:00 default; the first timezone-aware client should
replace it with the user's system timezone.

```
PUT /v2/study-day-policy
Body: {
  "base_version": number,
  "timezone_id": string,
  "day_start_hour": number,
  "idempotency_key": string
}
Response 200: same shape as GET
Response 409: { "error": "study_day_policy_conflict", "server": {...}, "client": {...} }
```

Writes use optimistic base-version ownership and idempotency keys. A successful
write is immediately visible account-wide through the GET endpoint; clients do
not need to pull the collection manifest.

### Kelma Immersion summaries

These read models are intentionally separate from the sync manifest. They return
only the projection the website needs and never require a follow-up collection
pull.

```
GET /v2/summary/decks
Response 200: { "decks": [{ "name": string, "card_count": number }] }
```

```
GET /v2/summary/decks/:name/card-ids
Response 200: { "deck_name": string, "card_ids": [number] }
```

```
GET /v2/summary/decks/:name/card-states
GET /v2/summary/card-states
Response 200: {
  "decks": [{ "name": string, "card_count": number }],
  "cards": [{
    "card_id": number, "note_guid": string, "deck_name": string,
    "type": number, "queue": number, "ivl": number,
    "projection_status": string, "fields": [string]
  }]
}
```

The deck-scoped response leaves `decks` empty because the requested deck name is
already in the URL. The account-wide response includes deck rows.

```
GET /v2/summary/review-time?since_ms=<epoch ms>&offset_minutes=<-840..840>
Response 200: { "days": [{ "deck_name": string, "day": "YYYY-MM-DD", "reviews": number, "seconds": number }] }
```

Review time per deck per day from the review history, in the learner's own
calendar days (`offset_minutes` ahead of UTC), for Kelma Immersion's progress:
reviews count on the day they were done, from any client. Each answer counts at
most 60 s; undone (retracted) reviews are left out. `since_ms` defaults to, and
cannot go earlier than, 400 days ago.

```
GET /v2/summary/review-days?offset_minutes=<-840..840>
Response 200: {
  "timezone_id": string, "day_start_hour": number, "today": "YYYY-MM-DD",
  "days": [{ "day": "YYYY-MM-DD", "due": number, "arrived": number, "done": number, "reviews": number }],
  "first_known": [{ "card_id": number, "day": "YYYY-MM-DD" }]
}
```

Reviews per study day for Kelma's review streak, rebuilt from the review
history for the last 400 days: `due` cards were due when the day began (overdue
ones included), `arrived` of them came due that day, `done` of them were answered
that day, and `reviews` counts every answer. A card's due day after its last
answer of a day is that day plus the answer's interval (negative intervals are
learning-step seconds); a reset (manual, interval 0) makes it new. A card's
current due date is the one Kelma Review and the website use: its FSRS
projection's (`scheduling._kelma_fsrs.due_at_millis`) when that is up to date
with its history, a due date set by hand since its last answer, or none after a
schedule reset (else its last answer's interval). A card
answered before that due day (Kelma Review and the website work due dates out
again from the whole history, so they can serve a card a day early) counts as
due and done the day it was answered. Cards deleted now are left out; a card
suspended now counts only on the day it was suspended, as handled (due and done,
or a new card started) when it was due or new that day — suspending it mid-review
used up its place in the day. Undone answers don't count. `first_known` is the
day each existing card was first answered into review (graduated), from its whole
history (not cards reset to new since: forgotten). Days follow the account's study-day policy (`timezone_id`,
`day_start_hour`); without one, 4 am at `offset_minutes` (`timezone_id` empty).

```
POST /v2/summary/review-days
Body: { "offset_minutes": number, "groups": { "<name>": ["<deck>", ...] } }   (≤ 20 groups, ≤ 50 decks each)
Response 200: the GET response plus
  "groups": [{ "name": string, "decks": [string], "review_limit": number, "new_limit": number,
               "days": [{ "day", "due", "served", "done", "new_served", "new_done", "reviews" }] }]
```

Each group (Kelma: one per language, with its decks; subdecks included) as
Kelma Review allots its day: first `new_served` new cards within the decks'
new-card limits (each deck's own, and the group deck's over all of it), which
also count against the review limit (Kelma Review's default); then `served` due
cards in what is left of the review limits; `done` due cards and `new_done` new
cards answered that day. Only decks answered in the 14 days up to a day count
(the decks the learner reviews). Limits: the deck config's `newLimit` and
`reviewLimit` (Kelma Review syncs them), else 20 and 200 — each day with the
limits it had (`deck_limit_history`: a day before a change keeps the earlier
limits; a change during a day counts for that day). `review_limit` and
`new_limit` on a group are the current ones.

### Kelma Immersion card creation

```
POST /v2/immersion/cards
Body: {
  "deck_name": string,
  "deck_config": object,
  "create_deck": boolean,
  "notetype": { "notetype_id": number, "name": string, "definition": object },
  "note": { "guid": string, "fields": [string], "tags": [string] },
  "card": { "card_id": number, "ord": number, "scheduling": object, "created_at": string? },
  "media_files": [{ "filename": string, "data_base64": string }],
  "client_modified_at": string
}
Response 201: {
  "deck_name": string, "notetype_id": number, "note_guid": string,
  "card_id": number, "media_uploaded": number
}
```

This is a create-only, idempotent fast path. It uploads the card and media in one
HTTP request, creates missing deck/notetype dependencies, and commits the
content records in one database transaction. `card.created_at` carries the
original Immersion card timestamp when available. Reposting the same `note.guid`
returns the existing logical card instead of creating a duplicate, and can fill a
previously unknown creation timestamp without replacing one already stored.

---

## Notes

### Get one
```
GET /v2/notes/:guid
Response 200:
{
  "guid":                string,
  "notetype_id":         number,
  "fields":              string[],
  "tags":                string[],
  "checksum":            string,
  "modified_at":         string,
  "client_modified_at":  string,
  "last_client_id":      string,
  "last_client_label":   string,
  "mark": { "marked": boolean, "intent_id": string, "modified_at": string,
            "client_modified_at": string, "last_client_id": string } | omitted
}
Response 404: { "error": "not found" }
```

### Push one
```
PUT /v2/notes/:guid
Headers (optional): Force-Override: true
Body:
{
  "notetype_id":         number,
  "fields":              string[],
  "tags":                string[],
  "client_modified_at":  string
}
Response 200: { ...saved record }
Response 409:
{
  "error": "conflict",
  "server": { ...server record },
  "client": { ...what you sent }
}
```

`409` is returned when the note exists on the server and its checksum differs
from what the client last saw. A base that differs only by a typed Mark/Unmark
is accepted so the content edit and mark can converge. `Force-Override: true`
bypasses content conflict resolution, but never overwrites independently
versioned mark state. Once typed intent exists, whole-note writes from older
clients preserve its current case-insensitive `marked` state. They may still
create or update an unversioned marked note before typed intent is established.

### Push typed Mark/Unmark intent
```
PUT /v2/notes/:guid/mark
Body:
{
  "marked":             boolean,
  "intent_id":          string,  -- canonical UUID, stable across retries
  "client_modified_at": string   -- RFC3339, no more than five minutes ahead
}
Response 200:
{
  "guid": string, "accepted": true, "applied": boolean,
  "mark": { "marked": boolean, "intent_id": string,
            "modified_at": string, "client_modified_at": string,
            "last_client_id": string }
}
```

The winner is the greatest `(client_modified_at, intent_id)` tuple. An older
valid intent is acknowledged with `applied: false` and the current winner so a
durable outbox can converge. Reusing the current intent ID with different
state or time returns `409 mark_intent_conflict`. The operation changes only
the canonical `marked` tag, note checksum, and mark version; it does not advance
the whole-note content clock.

### Delete one
```
DELETE /v2/notes/:guid
Response 204
```

Writes a tombstone. Idempotent.

---

## Cards

### Get one
```
GET /v2/cards/:card_id
Response 200:
{
  "card_id":             number,
  "note_guid":           string,
  "deck_name":           string,
  "ord":                 number,
  "scheduling":          object,
  "created_at":          string | null,
  "study_state":         "active" | "suspended",
  "study_state_modified_at": string,
  "study_state_client_modified_at": string,
  "schedule_reset_through_review_id": number,
  "schedule_reset_modified_at": string,
  "schedule_reset_client_modified_at": string,
  "due_date_override_ms": number,
  "due_date_override_modified_at": string,
  "due_date_override_client_modified_at": string,
  "modified_at":         string,
  "client_modified_at":  string,
  "last_client_id":      string,
  "last_client_label":   string,
  "projection": {
    "status": "pending" | "running" | "succeeded" | "failed" | "superseded",
    "desired_history_hash": string,
    "desired_through_review_id": number,
    "desired_profile_version": number,
    "applied_history_hash": string,
    "applied_through_review_id": number,
    "applied_profile_version": number,
    "last_error_code": string
  }
}
```

`projection` is absent until the logical card has qualifying immutable review
history. Desired/applied differences mean the compatibility scheduling payload
is stale, even if its JSON has not changed.

### Push one
```
PUT /v2/cards/:card_id
Body:
{
  "note_guid":           string,
  "deck_name":           string,
  "ord":                 number,
  "scheduling":          object,
  "created_at":          string,                            // optional, immutable
  "study_state":         "active" | "suspended",          // optional on legacy clients
  "study_state_client_modified_at": string,                 // recommended when study_state is sent
  "schedule_reset_through_review_id": number,               // optional, monotonic review cutoff
  "schedule_reset_client_modified_at": string,              // recommended when reset is sent
  "due_date_override_ms": number,                            // optional; 0 clears the override
  "due_date_override_client_modified_at": string,            // recommended when due override is sent
  "client_modified_at":  string
}
Response 200: { ...saved record }
```

`created_at` is immutable after the first known value. Omitting it preserves the
stored value. Plausible positive Anki millisecond card IDs are backfilled as
creation times; unverifiable legacy and Kelma-native cards return `null`.

Card content, `study_state`, schedule reset, and manual due date use independent
timestamps. Omitting an independent field preserves its current value. A schedule reset
is a monotonic cutoff: reviews at or below `schedule_reset_through_review_id`
remain immutable history but no longer contribute to the card projection.
The FSRS worker projects the card as New when no later reviews exist and never
changes `study_state` or the due-date override. A manual due date controls only
the next queue date; it does not alter FSRS memory state. `0` clears it, and an
accepted later review or schedule reset clears it automatically.

Before a card has a managed server projection, its opaque legacy scheduling payload follows the same
newest-wins behavior. After a successful FSRS-6 projection, clients may still
move/update card content but cannot overwrite server-owned compatibility
scheduling. The next pull returns the managed scheduling payload.

### Delete one
```
DELETE /v2/cards/:card_id
Response 204
```

---

## Review history

Review history uses Anki's append-only `revlog` semantics. Rows are transferred
through the batch endpoints and identified by their millisecond `review_id`.
They are never deleted by note/card tombstones (matching native Anki sync).

A full review record is:
```
{
  "review_id": number,
  "source_card_id": number,
  "note_guid": string,
  "card_ord": number,
  "deck_name": string,
  "ease": number,
  "interval": number,
  "last_interval": number,
  "factor": number,
  "taken_millis": number,
  "review_kind": number,
  "checksum": string,
  "modified_at": string
}
```

`source_card_id` is diagnostic/fallback metadata. Card ids are local to each
collection, so clients normally map each row through `(note_guid, card_ord)`
before inserting it. Re-sending identical immutable content is idempotent. If
the same `review_id` carries different immutable content, batch push returns a
review conflict rather than silently losing history.

Legacy daily quota consumption is stored outside `revlog` in collection-relative
deck counters. `study_days` carries their portable epoch-day snapshots. Same-day
server counters merge monotonically so an older device cannot reopen a quota
already consumed on another device; configured New/day and Reviews/day limits
live in deck metadata as described below.

---

## Notetypes

### Get one
```
GET /v2/notetypes/:notetype_id
Response 200:
{
  "notetype_id":         number,
  "name":                string,
  "definition":          object,
  "checksum":            string,
  "modified_at":         string,
  "client_modified_at":  string,
  "last_client_id":      string,
  "last_client_label":   string
}
```

### Push one
```
PUT /v2/notetypes/:notetype_id
Headers (optional): Force-Override: true
Body:
{
  "name":                string,
  "definition":          object,
  "client_modified_at":  string
}
Response 200: { ...saved record }
Response 409:
{
  "error": "conflict",
  "server": { ...server record },
  "client": { ...what you sent }
}
```

Same conflict semantics as notes. `Force-Override: true` overwrites
unconditionally.

### Delete one
```
DELETE /v2/notetypes/:notetype_id
Response 204
```

---

## Decks

### Get one
```
GET /v2/decks/:name
Response 200:
{
  "name":                string,
  "config":              object,
  "checksum":            string,
  "modified_at":         string,
  "client_modified_at":  string,
  "last_client_id":      string,
  "last_client_label":   string
}
```

`:name` is URL-encoded (e.g. `Japanese%3A%3AVocab`). Top-level integer
`config.newLimit` and `config.reviewLimit` values in `0..9999` are the shared
per-deck New/day and Reviews/day limits. Clients updating them preserve unknown
configuration keys.

### Push one
```
PUT /v2/decks/:name
Headers (optional): Force-Override: true
Body:
{
  "config":              object,
  "client_modified_at":  string
}
Response 200: { ...saved record }
Response 409: { "error": "conflict", "server": {...}, "client": {...} }
```

### Delete one
```
DELETE /v2/decks/:name
Response 204
```

---

## Media

### Check presence
```
HEAD /v2/media/:filename
Response 200   -- server has it
Response 404   -- server does not have it
```

### Plan bulk downloads
```
POST /v2/media/download-plan
Body: { "filenames": [string] }  // 1–500 distinct or repeated names
Response 200: {
  "files": [{
    "filename": string,
    "url": string,
    "size_bytes": number,
    "requires_auth": boolean
  }],
  "missing": [string]
}
```

All deployments return the ordinary KelmaSync media URL with `requires_auth: true`;
R2 credentials and object URLs remain server-side. The response preserves requested
filename order after deduplication. Ordinary clients should use at most 64 concurrent
authenticated KelmaSync downloads and verify every completed response against
`size_bytes`. A missing name remains an integrity error unless the client has durable
bytes it can re-upload.

### Pipeline prepared mass-download TARs

```http
POST /v2/media/prepare-tar
Body: { "filenames": [string] }  // 1–10,000 unique names, at most 128 MiB payload
Response 201: {
  "job_id": string,
  "archive_bytes": number,
  "payload_bytes": number,
  "files": number,
  "expires_at": timestamp
}

GET /v2/media/prepared-tar/:job_id
Response 200: <uncompressed POSIX TAR>

DELETE /v2/media/prepared-tar/:job_id
Response 204
```

Mass clients prepare up to five 128 MiB TARs concurrently and start downloading
as soon as any preparation finishes, while keeping exactly one archive GET active.
The server atomically claims a job at GET start and deletes its TAR when that
response completes or disconnects; the client's DELETE is a best-effort acknowledgement.
Prepared jobs that are never opened are account-scoped, expire after 10 minutes,
and are also bounded by a global 32 GiB temporary-disk reservation. Loose R2 objects remain canonical. A server
restart or cancellation can discard prepared archives without affecting media.

### Stream a compatibility mass-download TAR
```
POST /v2/media/download-tar
Body: { "filenames": [string] }  // 1–10,000 unique names, at most 512 MiB payload
Response 200: <uncompressed POSIX TAR>
Content-Type: application/x-tar
X-Kelma-Media-Files: number
X-Kelma-Media-Bytes: number
```

This authenticated compatibility path supports clients that do not implement prepared TAR jobs. A server
admits at most five concurrent TAR streams and returns HTTP 429 with `Retry-After`
when that capacity is full. Clients should begin with two streams, rise toward five
after successful batches, and halve toward one after transient failures. The server
concurrently prefetches account-owned loose objects from storage into
bounded temporary files, streams one TAR response, and deletes each temporary
file after writing it. It never stores an archive in R2 or changes ordinary
per-file upload/download/delete behavior. TAR entry names are zero-based decimal
indexes into the request's filename array; clients must reject duplicates,
unexpected indexes, size mismatches, invalid checksums, and incomplete archives.
Clients should partition requests at 512 MiB, stage each verified entry as it
arrives, and resume a failed pull from staged files. Servers without this route
return 404/405, allowing clients to retain the download-plan fallback.

### Download one
```
GET /v2/media/:filename
Response 200: <binary>
Content-Type: set to the file's actual MIME type
```

This endpoint remains the authenticated compatibility/fallback path.

### Upload
```
PUT /v2/media/:filename
Content-Type: application/octet-stream
Body: <binary>
Response 201: { "filename": string, "size_bytes": number }
```

Idempotent. Re-uploading the same filename overwrites the stored file.

### Adopt an existing shared object
```
POST /v2/media/adopt/:filename
Body: { "size_bytes": number }
Response 200: { "filename": string, "size_bytes": number }
```

For deployments sharing one R2 bucket, this performs an object `HEAD`, verifies
the exact size, and records account metadata without transferring the blob.

### Delete
```
DELETE /v2/media/:filename
Response 204
```

Decrements `ref_count`. If `ref_count` reaches zero the file is deleted from
object storage. Writes a tombstone.

---

## Batch endpoints

Single-item pushes are fine for conflict resolution, but initial sync of a
large collection needs batching.

### Batch push
```
POST /v2/batch/push
Body:
{
  "notes":     [{ "guid": string, ...note body }],
  "note_marks":[{ "guid": string, "marked": boolean, "intent_id": string,
                    "client_modified_at": string }],
  "cards":     [{ "card_id": number, ...card body }],
  "reviews":   [{ "review_id": number, ...review-history fields }],
  "study_days":[{ "day": number, "deck_name": string, ...daily counters }],
  "notetypes": [{ "notetype_id": number, ...notetype body }],
  "decks":     [{ "name": string, ...deck body }]
}
Response 200:
{
  "accepted": { "notes": number, "note_marks": number, "cards": number, "reviews": number, "study_days": number, "notetypes": number, "decks": number },
  "conflicts": {
    "notes":     [{ "guid": string, "server": {...}, "client": {...} }],
    "note_marks":[{ "guid": string, "server": {...}, "client": {...} }],
    "reviews":   [{ "review_id": number, "server": {...}, "client": {...} }],
    "notetypes": [{ "notetype_id": number, "server": {...}, "client": {...} }],
    "decks":     [{ "name": string, "server": {...}, "client": {...} }]
  },
  "note_marks": [{ "guid": string, "accepted": true, "applied": boolean, "mark": {...} }]
}
```

Cards never appear in conflicts (timestamp wins silently). Review rows conflict
only on an impossible/ambiguous reused id; notes, notetypes, and decks retain
their normal user-resolution behavior. Typed note marks use the same stable
intent ordering as the single-item endpoint; valid stale intents are accepted
and return the current winner. The remaining rows are accepted.
Newly accepted rating reviews atomically mark their logical card's FSRS-6
server projection pending.
`Force-Override: true` on a batch push accepts all items unconditionally.

### Batch pull
```
POST /v2/batch/pull
Body:
{
  "notes":     [string],    -- guids
  "cards":     [number],    -- card_ids
  "card_keys": [{ "note_guid": string, "ord": number }],
  "reviews":   [number],    -- review_ids
  "study_days":[{ "day": number, "deck_name": string }],
  "notetypes": [number],    -- notetype_ids
  "decks":     [string],    -- names
  "media":     [string],    -- filenames
  "tombstones":[{ "type": string, "resource_id": string }],
  "tombstone_card_keys": [{ "note_guid": string, "ord": number }]
}
Response 200:
{
  "notes":      [...],
  "cards":      [...],
  "reviews":    [...],
  "study_days": [...],
  "notetypes":  [...],
  "decks":      [...],
  "media":      [...],
  "tombstones": [...]
}
```

The combined pull request is limited to 12,000 identifiers; account mirroring
uses much smaller batches.

### Batch delete
```
POST /v2/batch/delete
Body:
{
  "notes":     [string],
  "cards":     [number],
  "notetypes": [number],
  "decks":     [string]
}
Response 200:
{
  "requested": { "notes": number, "cards": number, "notetypes": number, "decks": number },
  "deleted":   { "notes": number, "cards": number, "notetypes": number, "decks": number }
}
```

The delete and matching tombstone writes are transactional. A request is
limited to 12,000 resources; clients use 3,000-item batches and separately
require explicit approval for large deletion plans.

---

## Scheduler-independent review operations

Authenticated sync clients can apply typed review actions without creating a
server review session or presentation:

```text
POST /v2/sync/review-operations
Body: {
  "operation_id": UUID,
  "presentation_id": UUID,
  "kind": "set_card_flag" | "set_note_marked" | "suspend_card" |
          "suspend_note" | "set_card_due_date" | "reset_card" |
          "create_note_copy" | "edit_note" | "delete_note",
  "source_card_id": number,
  "note_guid": string,
  "card_ord": number,
  "base_note_checksum": string,
  "payload": object
}

GET /v2/sync/review-operations/:operation_id
```

`source_card_id` is a nonzero signed 64-bit integer; legacy imported card IDs
may be negative. The source identities must resolve to one card in the
authenticated account. Edit, copy, and delete additionally require the current
note checksum. Payloads
use closed kind-specific shapes and strict field/tag bounds. Bury remains
device-local, and Undo remains an append-only review
retraction, so neither is accepted here.

The operation ID is the idempotency key. KelmaSync locks the account, applies the
canonical note/card intent and stores its compact receipt in one transaction.
An exact retry returns that receipt; reusing the ID with different content is a
conflict. Receipt lookup is account scoped. These operations do not calculate a
queue or accept a browser-computed schedule.

---

## Trusted projection monitoring

```
GET /v2/internal/projections
Header: x-internal-secret: <deployment secret>
Response 200: {
  "total": number,
  "pending": number,
  "running": number,
  "succeeded": number,
  "failed": number,
  "superseded": number,
  "stale": number,
  "retry_exhausted": number,
  "max_attempt_count": number,
  "oldest_pending_seconds": number,
  "oldest_running_seconds": number,
  "oldest_failed_seconds": number,
  "oldest_stale_seconds": number,
  "generated_at": string
}
```

This aggregate endpoint is disabled when `KELMA_INTERNAL_SECRET` is unset and
never exposes account, card, review, or content identifiers.

---

## Error shape

All error responses follow:
```json
{ "error": "short machine-readable string", "message": "human detail" }
```

Common status codes:
- `400` — malformed request body
- `401` — missing or invalid token
- `403` — token valid but action not permitted
- `404` — resource not found
- `409` — conflict (note / notetype / deck only)
- `413` — media file too large
- `429` — rate limited
- `500` — server error
