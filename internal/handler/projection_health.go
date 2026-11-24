package handler

import (
	"net/http"
	"time"
)

// projectionHealthResponse contains account-agnostic queue telemetry only. It
// deliberately excludes user IDs, logical card IDs, review IDs, and contents.
type projectionHealthResponse struct {
	Total                int64     `json:"total"`
	Pending              int64     `json:"pending"`
	Running              int64     `json:"running"`
	Succeeded            int64     `json:"succeeded"`
	Failed               int64     `json:"failed"`
	Superseded           int64     `json:"superseded"`
	Stale                int64     `json:"stale"`
	RetryExhausted       int64     `json:"retry_exhausted"`
	MaxAttemptCount      int64     `json:"max_attempt_count"`
	OldestPendingSeconds int64     `json:"oldest_pending_seconds"`
	OldestRunningSeconds int64     `json:"oldest_running_seconds"`
	OldestFailedSeconds  int64     `json:"oldest_failed_seconds"`
	OldestStaleSeconds   int64     `json:"oldest_stale_seconds"`
	GeneratedAt          time.Time `json:"generated_at"`
}

// InternalProjectionHealth exposes global queue health to trusted deployment
// and monitoring automation. The shared internal-secret middleware guards it.
func (h *Handler) InternalProjectionHealth(w http.ResponseWriter, r *http.Request) {
	var response projectionHealthResponse
	err := h.DB.QueryRow(r.Context(),
		`SELECT
		   COUNT(*),
		   COUNT(*) FILTER (WHERE status='pending'),
		   COUNT(*) FILTER (WHERE status='running'),
		   COUNT(*) FILTER (WHERE status='succeeded'),
		   COUNT(*) FILTER (WHERE status='failed'),
		   COUNT(*) FILTER (WHERE status='superseded'),
		   COUNT(*) FILTER (WHERE applied_history_hash IS DISTINCT FROM desired_history_hash
		                     OR applied_through_review_id IS DISTINCT FROM desired_through_review_id
		                     OR applied_profile_version IS DISTINCT FROM desired_profile_version),
		   COUNT(*) FILTER (WHERE status='failed' AND attempt_count>=5),
		   COALESCE(MAX(attempt_count),0),
		   COALESCE(EXTRACT(EPOCH FROM
		     (now()-MIN(updated_at) FILTER (WHERE status='pending'))),0)::bigint,
		   COALESCE(EXTRACT(EPOCH FROM
		     (now()-MIN(COALESCE(last_attempt_at,updated_at)) FILTER (WHERE status='running'))),0)::bigint,
		   COALESCE(EXTRACT(EPOCH FROM
		     (now()-MIN(updated_at) FILTER (WHERE status='failed'))),0)::bigint,
		   COALESCE(EXTRACT(EPOCH FROM
		     (now()-MIN(updated_at) FILTER (WHERE applied_history_hash IS DISTINCT FROM desired_history_hash
		       OR applied_through_review_id IS DISTINCT FROM desired_through_review_id
		       OR applied_profile_version IS DISTINCT FROM desired_profile_version))),0)::bigint
		 FROM scheduler_projections`,
	).Scan(
		&response.Total, &response.Pending, &response.Running,
		&response.Succeeded, &response.Failed, &response.Superseded,
		&response.Stale, &response.RetryExhausted, &response.MaxAttemptCount,
		&response.OldestPendingSeconds, &response.OldestRunningSeconds,
		&response.OldestFailedSeconds, &response.OldestStaleSeconds,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "projection health query failed")
		return
	}
	response.GeneratedAt = time.Now().UTC()
	writeJSON(w, http.StatusOK, response)
}
