package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jdmc43/kelma-sync/internal/auth"
	"github.com/jdmc43/kelma-sync/internal/storage"
)

func TestMirrorTokenWorksWithoutAccountAdministrationPrivileges(t *testing.T) {
	t.Setenv("KELMA_INTERNAL_SECRET", "test-internal-secret")
	_, mux, pool := testServer(t)
	defer pool.Close()
	_ = loginDemo(t, mux, "existing-account")
	ctx := context.Background()
	role := pgx.Identifier{fmt.Sprintf("mirror_token_test_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE ROLE "+role+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := pool.Exec(ctx, "DROP OWNED BY "+role+"; DROP ROLE "+role); err != nil {
			t.Error(err)
		}
	}()
	if _, err := pool.Exec(ctx, "GRANT USAGE ON SCHEMA public TO "+role+
		"; GRANT SELECT ON users,account_deletions,clients,tokens TO "+role+
		"; GRANT INSERT,UPDATE ON clients TO "+role+
		"; GRANT INSERT ON tokens TO "+role); err != nil {
		t.Fatal(err)
	}
	config := pool.Config().Copy()
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE "+role)
		return err
	}
	restricted, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer restricted.Close()
	var canDelete bool
	if err := restricted.QueryRow(ctx, "SELECT has_table_privilege(current_user,'account_deletions','DELETE')").Scan(&canDelete); err != nil || canDelete {
		t.Fatal("fixture must not have account-deletion write access", err)
	}
	h := New(restricted, storage.NewMemory())
	limitedMux := http.NewServeMux()
	h.Routes(limitedMux)
	response := reqInternalJSON(t, limitedMux, http.MethodPost, "/v2/internal/account-mirror-token", map[string]any{"email": "demo"})
	if response.Code != http.StatusOK {
		t.Fatalf("restricted mirror token status=%d", response.Code)
	}
	var token internalTokenResponse
	if json.Unmarshal(response.Body.Bytes(), &token) != nil || token.UserID != auth.UIDForEmail("demo") || token.HKey == "" {
		t.Fatal("invalid scoped token response")
	}
	identity := reqJSON(t, limitedMux, http.MethodGet, "/v2/sync/identity", nil, token.HKey)
	if identity.Code != http.StatusOK {
		t.Fatalf("minted token did not authenticate: status=%d", identity.Code)
	}
}

func TestMirrorTokenDoesNotClearAccountDeletionMarkers(t *testing.T) {
	t.Setenv("KELMA_INTERNAL_SECRET", "test-internal-secret")
	_, mux, pool := testServer(t)
	defer pool.Close()
	_ = loginDemo(t, mux, "existing-account")
	ctx := context.Background()
	userID := auth.UIDForEmail("demo")
	for _, completed := range []bool{false, true} {
		if _, err := pool.Exec(ctx, `INSERT INTO account_deletions(user_id,completed_at)
			VALUES ($1,CASE WHEN $2 THEN now() ELSE NULL END)
			ON CONFLICT(user_id) DO UPDATE SET completed_at=EXCLUDED.completed_at`, userID, completed); err != nil {
			t.Fatal(err)
		}
		response := reqInternalJSON(t, mux, http.MethodPost, "/v2/internal/account-mirror-token", map[string]any{"email": "demo"})
		if response.Code != http.StatusConflict {
			t.Fatalf("deletion marker did not block token (completed=%v): status=%d", completed, response.Code)
		}
		var markers, clients int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM account_deletions WHERE user_id=$1", userID).Scan(&markers); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM clients WHERE user_id=$1 AND label='kelma-account-mirror'", userID).Scan(&clients); err != nil {
			t.Fatal(err)
		}
		if markers != 1 || clients != 0 {
			t.Fatal("mirror provisioning changed account state or created a client")
		}
	}
}
