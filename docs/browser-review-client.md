# Browser-owned review client

KelmaSync remains authoritative for synchronized collection facts while the
browser derives FSRS schedules, queues, limits, study days, interval previews,
and statistics locally. The browser reads through the identity, manifest,
batch-pull, scheduler-profile, and study-day-policy APIs. It appends immutable
reviews and review retractions through the batch API.

`review_client_operations_v1` advertises typed, scheduler-independent card and
note actions at `POST /v2/sync/review-operations`, with account-scoped receipt
recovery at `GET /v2/sync/review-operations/:operation_id`. These operations
accept canonical card identity, not queue or schedule authority. Edit, copy, and
delete are guarded by the synchronized note checksum. Bury stays device-local;
Undo stays an append-only review retraction.

Each accepted action and its compact receipt commit under the same account lock
and transaction. Exact operation-ID retries return the stored receipt, while an
ID reused with different content conflicts. Receipt lookup is account scoped.
No active handler reads or writes the historical `web_review_sessions`,
`web_review_presentations`, `web_review_attempts`, or `web_review_operations`
tables.

The `POST /v2/review/imports/:filename` endpoint performs bounded,
additive `.apkg` imports under the same account lock. Import is independent of
browser session state and never consults the historical web-review tables.

Migration 026 adds the browser operation receipts without rewriting canonical
history. Earlier web-review migrations and their dormant tables remain unchanged
so a coordinated image rollback is possible; they are not a parallel runtime
path.
