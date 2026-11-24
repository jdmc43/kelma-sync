package handler

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jdmc43/kelma-sync/internal/storage"
)

const (
	maxMediaBytes                  = 100 << 20 // 100 MiB
	maxMediaDownloadPlanFiles      = 500
	maxMediaTarFiles               = 10_000
	maxMediaTarBytes               = 512 << 20 // 512 MiB of media payload
	mediaTarFetchWorkers           = 16
	maxConcurrentMediaTarDownloads = 5
)

// storageKey derives the R2 object key for a user's media file.
func storageKey(userID, filename string) string {
	return userID + "/" + filename
}

// HeadMedia reports whether the server has a media file.
func (h *Handler) HeadMedia(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	filename := r.PathValue("filename")
	var exists bool
	err := h.DB.QueryRow(r.Context(),
		`SELECT true FROM media WHERE user_id = $1 AND filename = $2 AND storage_key IS NOT NULL`,
		claims.UserID, filename,
	).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

type mediaDownloadPlanRequest struct {
	Filenames []string `json:"filenames"`
}

type mediaDownloadPlanItem struct {
	Filename     string `json:"filename"`
	URL          string `json:"url"`
	SizeBytes    int64  `json:"size_bytes"`
	RequiresAuth bool   `json:"requires_auth"`
}

type mediaDownloadPlanResponse struct {
	Files   []mediaDownloadPlanItem `json:"files"`
	Missing []string                `json:"missing"`
}

type mediaDownloadObject struct {
	index     int
	filename  string
	key       string
	sizeBytes int64
}

type mediaTarFetchResult struct {
	object mediaDownloadObject
	path   string
	err    error
}

// PostMediaDownloadPlan resolves up to 500 account-owned filenames at once.
// R2 deployments return short-lived direct URLs; filesystem and test stores
// fall back to the authenticated single-file endpoint.
func (h *Handler) PostMediaDownloadPlan(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var request mediaDownloadPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid media download plan")
		return
	}
	if len(request.Filenames) == 0 || len(request.Filenames) > maxMediaDownloadPlanFiles {
		writeError(w, http.StatusBadRequest, "bad_request", "media download plans require 1 to 500 filenames")
		return
	}

	filenames := make([]string, 0, len(request.Filenames))
	seen := make(map[string]struct{}, len(request.Filenames))
	for _, filename := range request.Filenames {
		if filename == "" || len(filename) > 1024 {
			writeError(w, http.StatusBadRequest, "bad_request", "media filenames must contain 1 to 1024 bytes")
			return
		}
		if _, duplicate := seen[filename]; !duplicate {
			seen[filename] = struct{}{}
			filenames = append(filenames, filename)
		}
	}

	claims := claimsFrom(r)
	rows, err := h.DB.Query(r.Context(), `
		SELECT filename, storage_key, size_bytes FROM media
		WHERE user_id = $1 AND filename = ANY($2::text[]) AND storage_key IS NOT NULL`,
		claims.UserID, filenames,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "media plan query failed")
		return
	}
	defer rows.Close()
	objects := make(map[string]mediaDownloadObject, len(filenames))
	for rows.Next() {
		var object mediaDownloadObject
		if err := rows.Scan(&object.filename, &object.key, &object.sizeBytes); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "media plan query failed")
			return
		}
		objects[object.filename] = object
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "media plan query failed")
		return
	}

	response := mediaDownloadPlanResponse{
		Files:   make([]mediaDownloadPlanItem, 0, len(objects)),
		Missing: make([]string, 0, len(filenames)-len(objects)),
	}
	for _, filename := range filenames {
		object, ok := objects[filename]
		if !ok {
			response.Missing = append(response.Missing, filename)
			continue
		}
		item := mediaDownloadPlanItem{
			Filename:     filename,
			URL:          "/v2/media/" + url.PathEscape(filename),
			SizeBytes:    object.sizeBytes,
			RequiresAuth: true,
		}
		response.Files = append(response.Files, item)
	}
	writeJSON(w, http.StatusOK, response)
}

// PostMediaDownloadTar streams a temporary uncompressed TAR for mass pulls.
// Loose objects remain canonical. Entry names are request indexes, allowing
// arbitrary account-owned filenames without placing paths in the archive.
func (h *Handler) PostMediaDownloadTar(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	var request mediaDownloadPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid media TAR request")
		return
	}
	if len(request.Filenames) == 0 || len(request.Filenames) > maxMediaTarFiles {
		writeError(w, http.StatusBadRequest, "bad_request", "media TAR requests require 1 to 10000 filenames")
		return
	}
	select {
	case h.mediaTarDownloads <- struct{}{}:
		defer func() { <-h.mediaTarDownloads }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "busy", "media TAR capacity is currently full")
		return
	}

	seen := make(map[string]struct{}, len(request.Filenames))
	for _, filename := range request.Filenames {
		if filename == "" || len(filename) > 1024 {
			writeError(w, http.StatusBadRequest, "bad_request", "media filenames must contain 1 to 1024 bytes")
			return
		}
		if _, duplicate := seen[filename]; duplicate {
			writeError(w, http.StatusBadRequest, "bad_request", "media TAR filenames must be unique")
			return
		}
		seen[filename] = struct{}{}
	}

	claims := claimsFrom(r)
	rows, err := h.DB.Query(r.Context(), `
		SELECT filename, storage_key, size_bytes FROM media
		WHERE user_id = $1 AND filename = ANY($2::text[]) AND storage_key IS NOT NULL`,
		claims.UserID, request.Filenames,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "media TAR query failed")
		return
	}
	defer rows.Close()
	byFilename := make(map[string]mediaDownloadObject, len(request.Filenames))
	for rows.Next() {
		var object mediaDownloadObject
		if err := rows.Scan(&object.filename, &object.key, &object.sizeBytes); err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "media TAR query failed")
			return
		}
		byFilename[object.filename] = object
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "media TAR query failed")
		return
	}

	objects := make([]mediaDownloadObject, 0, len(request.Filenames))
	var payloadBytes int64
	for index, filename := range request.Filenames {
		object, ok := byFilename[filename]
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "media TAR contains an unavailable file")
			return
		}
		if object.sizeBytes < 0 || object.sizeBytes > maxMediaBytes {
			writeError(w, http.StatusInternalServerError, "internal", "media TAR contains an invalid size")
			return
		}
		object.index = index
		objects = append(objects, object)
		payloadBytes += object.sizeBytes
		if payloadBytes > maxMediaTarBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "media TAR payload exceeds 512 MiB")
			return
		}
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	jobs := make(chan mediaDownloadObject)
	results := make(chan mediaTarFetchResult)
	workerCount := min(mediaTarFetchWorkers, len(objects))
	for range workerCount {
		go h.fetchMediaTarObjects(ctx, jobs, results)
	}
	go func() {
		defer close(jobs)
		for _, object := range objects {
			select {
			case jobs <- object:
			case <-ctx.Done():
				return
			}
		}
	}()

	first := <-results
	if first.err != nil {
		cancel()
		writeError(w, http.StatusInternalServerError, "internal", "media TAR storage read failed")
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="kelma-media.tar"`)
	w.Header().Set("Content-Length", strconv.FormatInt(mediaTarArchiveSize(objects), 10))
	w.Header().Set("X-Kelma-Media-Files", strconv.Itoa(len(objects)))
	w.Header().Set("X-Kelma-Media-Bytes", strconv.FormatInt(payloadBytes, 10))
	w.WriteHeader(http.StatusOK)

	tarWriter := tar.NewWriter(w)
	writeResult := func(result mediaTarFetchResult) error {
		if result.path != "" {
			defer os.Remove(result.path)
		}
		if result.err != nil {
			return result.err
		}
		file, err := os.Open(result.path)
		if err != nil {
			return err
		}
		defer file.Close()
		header := &tar.Header{
			Name: strconv.Itoa(result.object.index),
			Mode: 0o600,
			Size: result.object.sizeBytes,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		written, err := io.Copy(tarWriter, file)
		if err != nil {
			return err
		}
		if written != result.object.sizeBytes {
			return fmt.Errorf("media TAR size changed while streaming")
		}
		return nil
	}
	if err := writeResult(first); err != nil {
		cancel()
		log.Printf("media TAR stream failed: %v", err)
		return
	}
	for range len(objects) - 1 {
		result := <-results
		if err := writeResult(result); err != nil {
			cancel()
			log.Printf("media TAR stream failed: %v", err)
			return
		}
	}
	if err := tarWriter.Close(); err != nil {
		log.Printf("media TAR close failed: %v", err)
	}
}

func (h *Handler) fetchMediaTarObjects(
	ctx context.Context,
	jobs <-chan mediaDownloadObject,
	results chan<- mediaTarFetchResult,
) {
	for object := range jobs {
		result := mediaTarFetchResult{object: object}
		body, _, err := h.Storage.Get(ctx, object.key)
		if err == nil {
			var file *os.File
			file, err = os.CreateTemp("", "kelma-media-tar-*")
			if err == nil {
				result.path = file.Name()
				var written int64
				written, err = io.Copy(file, io.LimitReader(body, object.sizeBytes+1))
				if closeErr := file.Close(); err == nil {
					err = closeErr
				}
				if err == nil && written != object.sizeBytes {
					err = fmt.Errorf("media object size does not match metadata")
				}
			}
			if closeErr := body.Close(); err == nil {
				err = closeErr
			}
		}
		result.err = err
		if err != nil && result.path != "" {
			_ = os.Remove(result.path)
			result.path = ""
		}
		select {
		case results <- result:
		case <-ctx.Done():
			if result.path != "" {
				_ = os.Remove(result.path)
			}
			return
		}
	}
}

func mediaTarArchiveSize(objects []mediaDownloadObject) int64 {
	const block = int64(512)
	total := 2 * block
	for _, object := range objects {
		total += block + ((object.sizeBytes + block - 1) / block * block)
	}
	return total
}

// GetMedia streams a media file's bytes from R2.
func (h *Handler) GetMedia(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	filename := r.PathValue("filename")

	var key *string
	err := h.DB.QueryRow(r.Context(),
		`SELECT storage_key FROM media WHERE user_id = $1 AND filename = $2`,
		claims.UserID, filename,
	).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) || key == nil {
		writeError(w, http.StatusNotFound, "not_found", "media not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	body, contentType, err := h.Storage.Get(r.Context(), *key)
	if errors.Is(err, storage.ErrNotFound) {
		// The DB row exists but the blob is gone (e.g. dev restart wiped an
		// in-memory store). Return 404 so the client treats the file as absent
		// and re-uploads its local copy, instead of failing the whole sync.
		writeError(w, http.StatusNotFound, "not_found", "media blob missing")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "storage read failed")
		return
	}
	defer body.Close()

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// PutMedia uploads a media file to R2 and records its metadata. Idempotent.
func (h *Handler) PutMedia(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	filename := r.PathValue("filename")

	r.Body = http.MaxBytesReader(w, r.Body, maxMediaBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "media exceeds size limit")
		return
	}

	key := storageKey(claims.UserID, filename)
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if err := h.Storage.Put(r.Context(), key, byteReader(data), int64(len(data)), contentType); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "storage write failed")
		return
	}

	now := time.Now().UTC()
	_, err = h.DB.Exec(r.Context(),
		`INSERT INTO media (user_id, filename, size_bytes, storage_key, modified_at, last_client_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (user_id, filename) DO UPDATE SET
		     size_bytes  = EXCLUDED.size_bytes,
		     storage_key = EXCLUDED.storage_key,
		     modified_at = EXCLUDED.modified_at,
		     last_client_id = EXCLUDED.last_client_id`,
		claims.UserID, filename, len(data), key, now, claims.ClientID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	_, _ = h.DB.Exec(r.Context(),
		`DELETE FROM tombstones WHERE user_id = $1 AND type = 'media' AND resource_id = $2`,
		claims.UserID, filename)

	writeJSON(w, http.StatusCreated, map[string]any{
		"filename":   filename,
		"size_bytes": len(data),
	})
}

type adoptMediaRequest struct {
	SizeBytes int64 `json:"size_bytes"`
}

// PostMediaAdopt records an object already present in the shared bucket. It is
// used by the account mirror so metadata can converge without transferring the
// same blob between production and rolling.
func (h *Handler) PostMediaAdopt(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	filename := r.PathValue("filename")
	var request adoptMediaRequest
	if err := decode(r, &request); err != nil || request.SizeBytes < 0 || request.SizeBytes > maxMediaBytes {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid shared media size")
		return
	}
	key := storageKey(claims.UserID, filename)
	info, err := h.Storage.Stat(r.Context(), key)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "shared media object is unavailable")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "storage_unavailable", "shared media object could not be verified")
		return
	}
	if info.SizeBytes != request.SizeBytes {
		writeError(w, http.StatusConflict, "media_size_conflict", "shared media metadata does not match the object")
		return
	}
	now := time.Now().UTC()
	if _, err := h.DB.Exec(r.Context(), `
		INSERT INTO media(user_id,filename,size_bytes,storage_key,modified_at,last_client_id)
		VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(user_id,filename) DO UPDATE SET
		  size_bytes=EXCLUDED.size_bytes,storage_key=EXCLUDED.storage_key,
		  modified_at=EXCLUDED.modified_at,last_client_id=EXCLUDED.last_client_id`,
		claims.UserID, filename, request.SizeBytes, key, now, claims.ClientID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "shared media metadata could not be recorded")
		return
	}
	_, _ = h.DB.Exec(r.Context(),
		`DELETE FROM tombstones WHERE user_id=$1 AND type='media' AND resource_id=$2`,
		claims.UserID, filename)
	writeJSON(w, http.StatusOK, map[string]any{"filename": filename, "size_bytes": request.SizeBytes})
}

// DeleteMedia removes a media file and writes a tombstone.
func (h *Handler) DeleteMedia(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	filename := r.PathValue("filename")

	var key *string
	err := h.DB.QueryRow(r.Context(),
		`DELETE FROM media WHERE user_id = $1 AND filename = $2 RETURNING storage_key`,
		claims.UserID, filename,
	).Scan(&key)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	objectKey := storageKey(claims.UserID, filename)
	if key != nil {
		objectKey = *key
	}
	// Also remove an account-owned orphan. In shared-R2 mode rolling's storage
	// wrapper deliberately defers this physical delete; when its tombstone wins,
	// the mirror calls production and this deterministic key is deleted there.
	if err := h.Storage.Delete(r.Context(), objectKey); err != nil {
		writeError(w, http.StatusBadGateway, "storage_unavailable", "media object could not be deleted")
		return
	}
	if err := h.writeTombstone(r, claims.UserID, "media", filename, claims.ClientID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
