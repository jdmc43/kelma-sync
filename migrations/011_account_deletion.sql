-- 011_account_deletion.sql
--
-- A deletion marker blocks token minting while cross-service account erasure is
-- in progress. Completed markers are retained until the account authority
-- verifies a newly active account with the same email, at which point the
-- marker is removed before a clean sync account can be created.

CREATE TABLE account_deletions (
    user_id      TEXT        PRIMARY KEY,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ
);
