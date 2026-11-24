package handler

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/auth"
)

type internalAccountDeletionRequest struct {
	Email   string `json:"email"`
	Confirm string `json:"confirm"`
}

type internalAccountDeletionResponse struct {
	OK             bool `json:"ok"`
	AlreadyDeleted bool `json:"alreadyDeleted"`
}

// InternalAccountDeletion permanently erases one shared Kelma account's sync
// data. The Immersion account service is the account authority and calls this
// endpoint as one step of its deletion saga. The operation is idempotent.
func (h *Handler) InternalAccountDeletion(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var request internalAccountDeletionRequest
	if err := decode(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account deletion request")
		return
	}
	email := strings.TrimSpace(strings.ToLower(request.Email))
	if email == "" || request.Confirm != "DELETE" {
		writeError(w, http.StatusBadRequest, "bad_request", "email and deletion confirmation required")
		return
	}
	userID := auth.UIDForEmail(email)

	alreadyDeleted, err := h.beginAccountDeletion(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not begin account deletion")
		return
	}
	if alreadyDeleted {
		writeJSON(w, http.StatusOK, internalAccountDeletionResponse{OK: true, AlreadyDeleted: true})
		return
	}

	// Tokens are revoked before blob removal, and pending prepared archives are
	// removed immediately. A failed blob deletion leaves the durable marker in
	// the pending state so logins stay blocked and the authority can safely retry.
	h.deletePreparedMediaTarsForAccount(userID)
	if err := h.Storage.DeletePrefix(r.Context(), userID+"/"); err != nil {
		writeError(w, http.StatusBadGateway, "storage_deletion_failed", "could not delete account media")
		return
	}
	if err := h.completeAccountDeletion(r.Context(), userID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not complete account deletion")
		return
	}
	writeJSON(w, http.StatusOK, internalAccountDeletionResponse{OK: true})
}

// beginAccountDeletion records the durable gate and revokes every token in one
// transaction. It returns true when an earlier request already completed.
func (h *Handler) beginAccountDeletion(ctx context.Context, userID string) (bool, error) {
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var completedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT completed_at FROM account_deletions WHERE user_id = $1 FOR UPDATE
	`, userID).Scan(&completedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err = tx.Exec(ctx, `
			INSERT INTO account_deletions (user_id, requested_at) VALUES ($1, now())
		`, userID); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	case completedAt != nil:
		return true, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM tokens WHERE user_id = $1`, userID); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

func (h *Handler) completeAccountDeletion(ctx context.Context, userID string) error {
	tx, err := h.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `
		UPDATE account_deletions SET completed_at = now() WHERE user_id = $1
	`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// accountActivationAllowed protects a deletion in progress from token minting.
// A completed marker may be cleared only after the external account authority
// has positively verified that the account is active again (for example after
// the person creates a new account using the same email address).
func (h *Handler) accountActivationAllowed(
	ctx context.Context,
	userID string,
	authorityVerified bool,
) (bool, error) {
	if authorityVerified {
		if _, err := h.DB.Exec(ctx, `
			DELETE FROM account_deletions
			WHERE user_id = $1 AND completed_at IS NOT NULL
		`, userID); err != nil {
			return false, err
		}
	}
	var blocked bool
	if err := h.DB.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM account_deletions WHERE user_id = $1)
	`, userID).Scan(&blocked); err != nil {
		return false, err
	}
	return !blocked, nil
}

func (h *Handler) deletePreparedMediaTarsForAccount(userID string) {
	h.mediaTarJobsMu.Lock()
	jobs := make([]preparedMediaTar, 0)
	for jobID, job := range h.mediaTarJobs {
		if job.userID == userID {
			delete(h.mediaTarJobs, jobID)
			jobs = append(jobs, job)
		}
	}
	h.mediaTarJobsMu.Unlock()
	for _, job := range jobs {
		_ = os.Remove(job.path)
		h.releasePreparedMediaTar(job.reservedBytes)
	}
}
