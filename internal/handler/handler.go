package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jdmc43/kelma-sync/internal/auth"
	"github.com/jdmc43/kelma-sync/internal/inflight"
	"github.com/jdmc43/kelma-sync/internal/model"
	"github.com/jdmc43/kelma-sync/internal/storage"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	DB                    *pgxpool.Pool
	Storage               storage.Storage
	mediaTarDownloads     chan struct{}
	mediaTarJobsMu        sync.Mutex
	mediaTarJobs          map[string]preparedMediaTar
	mediaTarPreparedBytes int64
	now                   func() time.Time
}

func New(db *pgxpool.Pool, store storage.Storage) *Handler {
	return &Handler{
		DB:                db,
		Storage:           store,
		mediaTarDownloads: make(chan struct{}, maxConcurrentMediaTarDownloads),
		mediaTarJobs:      make(map[string]preparedMediaTar),
		now:               time.Now,
	}
}

// Routes registers all API routes on mux.
func (h *Handler) Routes(mux *http.ServeMux) {
	// Health
	mux.HandleFunc("GET /health", h.Health)

	// Auth
	mux.HandleFunc("POST /v2/auth/register", h.Register)
	mux.HandleFunc("POST /v2/auth/login", h.Login)
	mux.HandleFunc("POST /v2/auth/logout", h.requireAuth(h.Logout))

	// Sync manifest + exact account storage usage
	mux.HandleFunc("GET /v2/sync/identity", h.requireAuth(h.GetSyncIdentity))
	mux.HandleFunc("GET /v2/sync/manifest", h.requireAuth(h.GetManifest))
	mux.HandleFunc("GET /v2/usage", h.requireAuth(h.GetUsage))
	mux.HandleFunc("GET /v2/scheduler-profile", h.requireAuth(h.GetSchedulerProfile))
	mux.HandleFunc("PUT /v2/scheduler-profile", h.requireAuth(h.PutSchedulerProfile))
	mux.HandleFunc("GET /v2/study-day-policy", h.requireAuth(h.GetStudyDayPolicy))
	mux.HandleFunc("PUT /v2/study-day-policy", h.requireAuth(h.PutStudyDayPolicy))

	// Purpose-built read models for Kelma Immersion. These avoid downloading a
	// full collection manifest and then pulling every card/note record.
	mux.HandleFunc("GET /v2/summary/decks", h.requireAuth(h.GetDecksSummary))
	mux.HandleFunc("GET /v2/summary/decks/{name}/card-ids", h.requireAuth(h.GetDeckCardIDsSummary))
	mux.HandleFunc("GET /v2/summary/decks/{name}/card-states", h.requireAuth(h.GetDeckCardStatesSummary))
	mux.HandleFunc("GET /v2/summary/card-states", h.requireAuth(h.GetAllCardStatesSummary))
	mux.HandleFunc("GET /v2/summary/review-time", h.requireAuth(h.GetReviewTimeSummary))
	mux.HandleFunc("GET /v2/summary/review-days", h.requireAuth(h.GetReviewDaysSummary))
	mux.HandleFunc("POST /v2/summary/review-days", h.requireAuth(h.PostReviewDaysSummary))

	// One-request create path for a complete Immersion card and its media.
	mux.HandleFunc("POST /v2/immersion/cards", h.requireAuth(h.PostImmersionCard))

	// Notes
	mux.HandleFunc("GET /v2/notes/{guid}", h.requireAuth(h.GetNote))
	mux.HandleFunc("PUT /v2/notes/{guid}", h.requireAuth(h.PutNote))
	mux.HandleFunc("PUT /v2/notes/{guid}/mark", h.requireAuth(h.PutNoteMark))
	mux.HandleFunc("DELETE /v2/notes/{guid}", h.requireAuth(h.DeleteNote))

	// Cards
	mux.HandleFunc("GET /v2/cards/{card_id}", h.requireAuth(h.GetCard))
	mux.HandleFunc("PUT /v2/cards/{card_id}", h.requireAuth(h.PutCard))
	mux.HandleFunc("PUT /v2/cards/{card_id}/flag", h.requireAuth(h.PutCardFlag))
	mux.HandleFunc("DELETE /v2/cards/{card_id}", h.requireAuth(h.DeleteCard))

	// Notetypes
	mux.HandleFunc("GET /v2/notetypes/{notetype_id}", h.requireAuth(h.GetNotetype))
	mux.HandleFunc("PUT /v2/notetypes/{notetype_id}", h.requireAuth(h.PutNotetype))
	mux.HandleFunc("DELETE /v2/notetypes/{notetype_id}", h.requireAuth(h.DeleteNotetype))

	// Decks
	mux.HandleFunc("GET /v2/decks/{name}", h.requireAuth(h.GetDeck))
	mux.HandleFunc("PUT /v2/decks/{name}", h.requireAuth(h.PutDeck))
	mux.HandleFunc("DELETE /v2/decks/{name}", h.requireAuth(h.DeleteDeck))

	// Media
	mux.HandleFunc("POST /v2/media/download-plan", h.requireAuth(h.PostMediaDownloadPlan))
	mux.HandleFunc("POST /v2/media/download-tar", h.requireAuth(h.PostMediaDownloadTar))
	mux.HandleFunc("POST /v2/media/prepare-tar", h.requireAuth(h.PostMediaPrepareTar))
	mux.HandleFunc("GET /v2/media/prepared-tar/{job_id}", h.requireAuth(h.GetPreparedMediaTar))
	mux.HandleFunc("DELETE /v2/media/prepared-tar/{job_id}", h.requireAuth(h.DeletePreparedMediaTar))
	mux.HandleFunc("HEAD /v2/media/{filename}", h.requireAuth(h.HeadMedia))
	mux.HandleFunc("GET /v2/media/{filename}", h.requireAuth(h.GetMedia))
	mux.HandleFunc("PUT /v2/media/{filename}", h.requireAuth(h.PutMedia))
	mux.HandleFunc("POST /v2/media/adopt/{filename}", h.requireAuth(h.PostMediaAdopt))
	mux.HandleFunc("DELETE /v2/media/{filename}", h.requireAuth(h.DeleteMedia))

	// Internal (trusted Immersion backend only; disabled without secret)
	mux.HandleFunc("POST /v2/internal/token", requireInternal(h.InternalToken))
	mux.HandleFunc("POST /v2/internal/account-mirror-token", requireInternal(h.InternalAccountMirrorToken))
	mux.HandleFunc("POST /v2/internal/usage", requireInternal(h.InternalUsage))
	mux.HandleFunc("POST /v2/internal/account-deletion", requireInternal(h.InternalAccountDeletion))
	mux.HandleFunc("GET /v2/internal/projections", requireInternal(h.InternalProjectionHealth))

	// Batch
	mux.HandleFunc("POST /v2/batch/push", h.requireAuth(h.BatchPush))
	mux.HandleFunc("POST /v2/batch/pull", h.requireAuth(h.BatchPull))
	mux.HandleFunc("POST /v2/batch/delete", h.requireAuth(h.BatchDelete))
	mux.HandleFunc("POST /v2/sync/review-operations", h.requireAuth(h.PostReviewClientOperation))
	mux.HandleFunc("GET /v2/sync/review-operations/{operation_id}", h.requireAuth(h.GetReviewClientOperation))
}

// BrowserReviewRoutes registers rolling-only browser support that is not exposed
// by the normal production handler. Scheduling and sessions remain client-owned.
func (h *Handler) BrowserReviewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v2/review/imports/{filename}", h.requireAuth(h.PostReviewImport))
}

// statusRecorder captures the response status for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Middleware wraps a mux with panic recovery and access logging. A panic in any
// handler is recovered and returned as a 500 instead of crashing the process.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		defer func() {
			if rv := recover(); rv != nil {
				log.Printf("panic %s %s: %v\n%s", r.Method, r.URL.Path, rv, debug.Stack())
				// Best-effort 500 if nothing was written yet.
				defer func() { _ = recover() }()
				writeError(rec, http.StatusInternalServerError, "internal", "internal server error")
			}
			// Skip noisy health checks.
			if r.URL.Path != "/health" {
				log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// requireAuth wraps a handler, authenticating the request and injecting Claims
// into the request context before calling the inner handler.
func (h *Handler) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := auth.Authenticate(r.Context(), r, h.DB)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", err.Error())
			return
		}
		if writesAccountData(r) {
			// Registered before the handler takes any timestamp, so manifests
			// cannot hand out a cursor past this write's stamps until it ends.
			defer inflight.Writes.Begin(claims.UserID)()
		}
		next(w, r.WithContext(auth.WithClaims(r.Context(), claims)))
	}
}

// readOnlyPosts are POST routes that only read account data.
var readOnlyPosts = map[string]bool{
	"/v2/batch/pull": true, "/v2/media/download-plan": true,
	"/v2/media/download-tar": true, "/v2/media/prepare-tar": true,
}

func writesAccountData(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	case http.MethodPost:
		return !readOnlyPosts[r.URL.Path]
	}
	return true
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a standard error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, model.ErrorResponse{Error: code, Message: message})
}

// decode decodes the JSON request body into v.
func decode(r *http.Request, v any) error {
	d := json.NewDecoder(r.Body)
	// Preserve exact JSON numbers (Anki IDs are often > 2^53). If these become
	// float64, server-side checksums drift from Python/Rust checksums and every
	// sync looks like a conflict.
	d.UseNumber()
	d.DisallowUnknownFields()
	return d.Decode(v)
}

// claimsFrom extracts Claims from the request context (set by requireAuth).
func claimsFrom(r *http.Request) *auth.Claims {
	return auth.ClaimsFrom(r.Context())
}

// forceOverride reports whether the request carries the Force-Override header.
func forceOverride(r *http.Request) bool {
	return r.Header.Get("Force-Override") == "true"
}

// Handlers are implemented in separate files:
//   auth.go      — Register, Login, Logout
//   manifest.go  — GetManifest
//   summary.go   — purpose-built Kelma Immersion read models
//   immersion.go — one-request Kelma Immersion card creation
//   notes.go     — GetNote, PutNote, DeleteNote
//   cards.go     — GetCard, PutCard, DeleteCard
//   notetypes.go — GetNotetype, PutNotetype, DeleteNotetype
//   decks.go     — GetDeck, PutDeck, DeleteDeck
//   media.go     — HeadMedia, GetMedia, PutMedia, DeleteMedia
//   batch.go     — BatchPush, BatchPull
