package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jdmc43/kelma-sync/internal/auth"
)

// A self-hosted server works without any external service: unless an operator
// selects another account authority, accounts are local and can be registered.
func TestSelfHostedServerDefaultsToLocalAccounts(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	t.Setenv("KELMA_AUTH_MODE", "")
	t.Setenv("KELMA_IMMERSION_LOGIN_URL", "")

	rr := reqJSON(t, mux, "POST", "/v2/auth/register",
		map[string]string{"username": "maria@home.example", "password": "correct horse"}, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rr.Code, rr.Body.String())
	}
	rr = reqJSON(t, mux, "POST", "/v2/auth/login", map[string]string{
		"username": "maria@home.example", "password": "correct horse", "client_label": "Kelma Review",
	}, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "token") {
		t.Fatalf("login: %d %s", rr.Code, rr.Body.String())
	}
}

// Selecting an external authority without configuring it fails closed and does
// not leak internal error details, and never falls back to a project-operated
// login service.
func TestExternalAccountAuthorityMustBeConfigured(t *testing.T) {
	_, mux, pool := testServer(t)
	defer pool.Close()
	t.Setenv("KELMA_AUTH_MODE", "immersion")
	t.Setenv("KELMA_IMMERSION_LOGIN_URL", "")

	if got := auth.ImmersionLoginURL(); got != "" {
		t.Fatalf("public builds must not default to a login service, got %q", got)
	}
	if _, err := auth.VerifyViaImmersion(context.Background(), "a@b.c", "x"); err == nil {
		t.Fatal("verification without a configured authority must fail")
	}
	rr := reqJSON(t, mux, "POST", "/v2/auth/register",
		map[string]string{"username": "maria@home.example", "password": "correct horse"}, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("register with an external authority: %d %s", rr.Code, rr.Body.String())
	}
	rr = reqJSON(t, mux, "POST", "/v2/auth/login", map[string]string{
		"username": "maria@home.example", "password": "correct horse", "client_label": "Kelma Review",
	}, "")
	if rr.Code != http.StatusBadGateway || strings.Contains(rr.Body.String(), "KELMA_IMMERSION_LOGIN_URL") {
		t.Fatalf("login without an authority: %d %s", rr.Code, rr.Body.String())
	}
}
