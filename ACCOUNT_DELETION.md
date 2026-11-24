# Account deletion

KelmaSync owns synchronized collection data and media. Who owns the account
itself depends on the deployment:

- With **local accounts** (`KELMA_AUTH_MODE=local`, the default), KelmaSync is
  the account authority.
- With an **external account authority** (`KELMA_AUTH_MODE=immersion` and
  `KELMA_IMMERSION_LOGIN_URL`), that service authenticates people and runs the
  deletion workflow; KelmaSync has no separate user-facing account. This is how
  Kelma's hosted service works: one Kelma account for Kelma Review and Kelma
  Immersion.

## Ownership

- **The account authority** authenticates the person and owns the deletion
  workflow and any data it keeps itself.
- **KelmaSync** owns sync tokens, clients, decks, notes, cards, review history,
  scheduler state, tombstones, and account-prefixed media objects.
- **Clients** may retain local collections after signing out. After confirmed
  account deletion they must remove the deleted account's token, registry
  entry, database, and media cache from the device.

## Deletion saga

Cross-database and object-store deletion cannot be one ACID transaction. The
account authority runs a retry-safe saga:

1. Authenticate the request and require the exact account email plus `DELETE`.
2. Mark its own account record pending, so normal sign-in is rejected while the
   deletion stays retryable.
3. Call `POST /v2/internal/account-deletion` with the shared internal secret.
4. KelmaSync durably marks deletion pending and revokes every sync token in one
   PostgreSQL transaction.
5. KelmaSync removes pending media TARs and deletes the entire `<user-id>/`
   object prefix. Prefix deletion removes both tracked and orphaned blobs.
6. KelmaSync deletes the user row; foreign-key cascades erase all collection,
   review, scheduler, token, and client records. It then marks the deletion
   complete.
7. The account authority deletes its remaining account data and removes its
   authentication record last, invalidating every previously issued session.

If any step fails, the account remains marked pending and ordinary access stays
blocked. The same deletion request retries completed steps safely. The
authentication record is never removed before dependent services report success.

## Identity reuse

KelmaSync currently derives its user ID from normalized email for compatibility.
It stores a deletion marker with no email or content. A pending marker blocks
all token minting. A completed marker can be removed only after the account
authority positively verifies an active account again, allowing a person to
create a new, empty account later with the same email without restoring old
data.

A future identity migration should use an immutable random account ID issued by
the account authority rather than an email-derived ID.
