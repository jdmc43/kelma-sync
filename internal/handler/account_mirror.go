package handler

import (
	"net/http"
	"strings"

	"github.com/jdmc43/kelma-sync/internal/auth"
)

type syncIdentityResponse struct {
	UserID   string `json:"user_id"`
	ClientID string `json:"client_id"`
}

// GetSyncIdentity lets a narrowly scoped mirror prove that both bearer tokens
// belong to the same expected account before it transfers any records.
func (h *Handler) GetSyncIdentity(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	writeJSON(w, http.StatusOK, syncIdentityResponse{UserID: claims.UserID, ClientID: claims.ClientID})
}

type accountMirrorTokenRequest struct {
	Email string `json:"email"`
}

// InternalAccountMirrorToken creates a dedicated, revocable account token.
// Unlike InternalToken it never creates a user and never shares the web client's
// identity, allowing mirror-origin writes to be identified in manifests.
func (h *Handler) InternalAccountMirrorToken(w http.ResponseWriter, r *http.Request) {
	var request accountMirrorTokenRequest
	if err := decode(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid account mirror token request")
		return
	}
	email := strings.ToLower(strings.TrimSpace(request.Email))
	userID := auth.UIDForEmail(email)
	var storedEmail string
	if email == "" || h.DB.QueryRow(r.Context(),
		`SELECT username FROM users WHERE id=$1`, userID).Scan(&storedEmail) != nil ||
		strings.ToLower(strings.TrimSpace(storedEmail)) != email {
		writeError(w, http.StatusNotFound, "not_found", "account mirror target unavailable")
		return
	}
	// Provisioning a mirror token is not an account-authority login or reactivation.
	allowed, err := h.accountActivationAllowed(r.Context(), userID, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "account state lookup failed")
		return
	}
	if !allowed {
		writeError(w, http.StatusConflict, "account_deletion_pending", "account deletion is in progress")
		return
	}
	var clientID string
	if err := h.DB.QueryRow(r.Context(), `
		INSERT INTO clients(user_id,label,last_seen) VALUES($1,'kelma-account-mirror',now())
		ON CONFLICT(user_id,label) DO UPDATE SET last_seen=now() RETURNING id`, userID).Scan(&clientID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "account mirror client unavailable")
		return
	}
	raw, hashed, err := auth.GenerateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "account mirror token unavailable")
		return
	}
	if _, err := h.DB.Exec(r.Context(), `
		INSERT INTO tokens(user_id,client_id,token_hash,expires_at)
		VALUES($1,$2,$3,now()+interval '1 year')`, userID, clientID, hashed); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "account mirror token unavailable")
		return
	}
	writeJSON(w, http.StatusOK, internalTokenResponse{HKey: raw, UserID: userID})
}
