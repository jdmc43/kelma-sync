package handler

import (
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/jdmc43/kelma-sync/internal/ankiimport"
)

var reviewImportSlots = make(chan struct{}, 2)

// PostReviewImport imports one authenticated Anki deck package. It is exposed
// only with BrowserReviewRoutes, never by the normal production handler.
func (h *Handler) PostReviewImport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	select {
	case reviewImportSlots <- struct{}{}:
		defer func() { <-reviewImportSlots }()
	default:
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "review_import_busy", "deck import capacity is currently full")
		return
	}
	filename := strings.TrimSpace(r.PathValue("filename"))
	if filename == "" || len([]byte(filename)) > 255 || filepath.Base(filename) != filename ||
		!strings.EqualFold(filepath.Ext(filename), ".apkg") || strings.ContainsAny(filename, "\\/\r\n\x00") {
		writeError(w, http.StatusBadRequest, "bad_review_import", "select one .apkg deck package")
		return
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (contentType != "application/octet-stream" && contentType != "application/vnd.anki") {
		writeError(w, http.StatusUnsupportedMediaType, "bad_review_import", "an Anki package upload is required")
		return
	}
	if r.ContentLength > ankiimport.MaxPackageBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "review_import_too_large", "Anki package exceeds the web import limit")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, ankiimport.MaxPackageBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "review_import_too_large", "Anki package exceeds the web import limit")
		return
	}
	collection, err := ankiimport.ReadContext(r.Context(), data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_review_import", "the Anki package is invalid or unsupported")
		return
	}
	result, err := h.mergeReviewImport(r.Context(), claimsFrom(r), collection)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "review_import_unavailable", "the Anki package could not be imported")
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
