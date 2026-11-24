package handler

import (
	"errors"
	"net/http"

	"github.com/jdmc43/kelma-sync/internal/studyday"
)

// GetStudyDayPolicy returns the account-wide timezone and rollover boundary.
func (h *Handler) GetStudyDayPolicy(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	policy, err := studyday.Get(r.Context(), h.DB, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

// PutStudyDayPolicy updates the account-wide policy with optimistic ownership.
func (h *Handler) PutStudyDayPolicy(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	var candidate studyday.Candidate
	if err := decode(r, &candidate); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	policy, err := studyday.Put(r.Context(), h.DB, claims.UserID, claims.ClientID, candidate)
	if errors.Is(err, studyday.ErrPolicyConflict) {
		current, loadErr := studyday.Get(r.Context(), h.DB, claims.UserID)
		if loadErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", loadErr.Error())
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "study_day_policy_conflict", "server": current, "client": candidate,
		})
		return
	}
	if errors.Is(err, studyday.ErrInvalidPolicy) {
		writeError(w, http.StatusBadRequest, "invalid_study_day_policy", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, policy)
}
