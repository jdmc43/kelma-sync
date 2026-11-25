package handler

import (
	"errors"
	"net/http"

	"github.com/jdmc43/kelma-sync/internal/projection"
)

type projectionSummary struct {
	Pending    int64 `json:"pending"`
	Running    int64 `json:"running"`
	Succeeded  int64 `json:"succeeded"`
	Failed     int64 `json:"failed"`
	Superseded int64 `json:"superseded"`
	Stale      int64 `json:"stale"`
}

type schedulerProfileResponse struct {
	Profile     projection.Profile `json:"profile"`
	Projections projectionSummary  `json:"projections"`
}

func (h *Handler) GetSchedulerProfile(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	profile, err := projection.ActiveProfile(r.Context(), h.DB, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	summary, err := h.projectionSummary(r, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, schedulerProfileResponse{Profile: profile, Projections: summary})
}

func (h *Handler) PutSchedulerProfile(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	var candidate projection.Candidate
	if err := decode(r, &candidate); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	profile, err := projection.PutProfile(r.Context(), h.DB, claims.UserID, claims.ClientID, candidate)
	if errors.Is(err, projection.ErrProfileConflict) {
		current, loadErr := projection.ActiveProfile(r.Context(), h.DB, claims.UserID)
		if loadErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", loadErr.Error())
			return
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "scheduler_profile_conflict", "server": current, "client": candidate,
		})
		return
	}
	if errors.Is(err, projection.ErrHistoryMismatch) {
		writeError(w, http.StatusConflict, "optimization_history_mismatch", err.Error())
		return
	}
	if errors.Is(err, projection.ErrInvalidProfile) {
		writeError(w, http.StatusBadRequest, "invalid_scheduler_profile", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	summary, err := h.projectionSummary(r, claims.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, schedulerProfileResponse{Profile: profile, Projections: summary})
}

func (h *Handler) projectionSummary(r *http.Request, userID string) (projectionSummary, error) {
	var summary projectionSummary
	err := h.DB.QueryRow(r.Context(),
		`SELECT
		   COUNT(*) FILTER (WHERE status='pending'),
		   COUNT(*) FILTER (WHERE status='running'),
		   COUNT(*) FILTER (WHERE status='succeeded'),
		   COUNT(*) FILTER (WHERE status='failed'),
		   COUNT(*) FILTER (WHERE status='superseded'),
		   COUNT(*) FILTER (WHERE status<>'superseded' AND (applied_history_hash IS DISTINCT FROM desired_history_hash
		                     OR applied_through_review_id IS DISTINCT FROM desired_through_review_id
		                     OR applied_profile_version IS DISTINCT FROM desired_profile_version))
		 FROM scheduler_projections WHERE user_id=$1`, userID,
	).Scan(
		&summary.Pending, &summary.Running, &summary.Succeeded,
		&summary.Failed, &summary.Superseded, &summary.Stale,
	)
	return summary, err
}
