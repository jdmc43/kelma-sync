package handler

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

const (
	maxPreparedMediaTarBytes     = 128 << 20
	maxPreparedMediaTarDiskBytes = 32 << 30
	preparedMediaTarLifetime     = 10 * time.Minute
)

type preparedMediaTar struct {
	userID        string
	path          string
	archiveBytes  int64
	payloadBytes  int64
	files         int
	expiresAt     time.Time
	reservedBytes int64
}

type preparedMediaTarResponse struct {
	JobID        string `json:"job_id"`
	ArchiveBytes int64  `json:"archive_bytes"`
	PayloadBytes int64  `json:"payload_bytes"`
	Files        int    `json:"files"`
	ExpiresAt    string `json:"expires_at"`
}

// PostMediaPrepareTar materializes one temporary 128 MiB archive. Clients may
// prepare several archives concurrently, but download them one at a time.
func (h *Handler) PostMediaPrepareTar(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	var request mediaDownloadPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid media TAR preparation request")
		return
	}
	if len(request.Filenames) == 0 || len(request.Filenames) > maxMediaTarFiles {
		writeError(w, http.StatusBadRequest, "bad_request", "media TAR preparations require 1 to 10000 filenames")
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
	select {
	case h.mediaTarDownloads <- struct{}{}:
		defer func() { <-h.mediaTarDownloads }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "busy", "media TAR preparation capacity is currently full")
		return
	}

	claims := claimsFrom(r)
	objects, payloadBytes, err := h.mediaTarObjects(r.Context(), claims.UserID, request.Filenames)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "media TAR preparation query failed")
		return
	}
	if len(objects) != len(request.Filenames) {
		writeError(w, http.StatusNotFound, "not_found", "media TAR contains an unavailable file")
		return
	}
	if payloadBytes > maxPreparedMediaTarBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "prepared media TAR payload exceeds 128 MiB")
		return
	}
	reservedBytes := mediaTarArchiveSize(objects)
	if !h.reservePreparedMediaTar(reservedBytes) {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusTooManyRequests, "busy", "prepared media TAR disk capacity is currently full")
		return
	}
	reserved := true
	defer func() {
		if reserved {
			h.releasePreparedMediaTar(reservedBytes)
		}
	}()

	archive, err := os.CreateTemp("", "kelma-prepared-media-*.tar")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "media TAR temporary storage failed")
		return
	}
	archivePath := archive.Name()
	if err := h.writePreparedMediaTar(r.Context(), archive, objects); err != nil {
		_ = archive.Close()
		_ = os.Remove(archivePath)
		writeError(w, http.StatusInternalServerError, "internal", "media TAR preparation failed")
		return
	}
	if err := archive.Close(); err != nil {
		_ = os.Remove(archivePath)
		writeError(w, http.StatusInternalServerError, "internal", "media TAR preparation failed")
		return
	}
	info, err := os.Stat(archivePath)
	if err != nil {
		_ = os.Remove(archivePath)
		writeError(w, http.StatusInternalServerError, "internal", "media TAR preparation failed")
		return
	}
	jobID, err := randomMediaTarJobID()
	if err != nil {
		_ = os.Remove(archivePath)
		writeError(w, http.StatusInternalServerError, "internal", "media TAR preparation failed")
		return
	}
	expiresAt := time.Now().UTC().Add(preparedMediaTarLifetime)
	job := preparedMediaTar{
		userID:        claims.UserID,
		path:          archivePath,
		archiveBytes:  info.Size(),
		payloadBytes:  payloadBytes,
		files:         len(objects),
		expiresAt:     expiresAt,
		reservedBytes: reservedBytes,
	}
	h.mediaTarJobsMu.Lock()
	h.mediaTarJobs[jobID] = job
	h.mediaTarJobsMu.Unlock()
	reserved = false
	time.AfterFunc(preparedMediaTarLifetime, func() { h.expirePreparedMediaTar(jobID) })
	writeJSON(w, http.StatusCreated, preparedMediaTarResponse{
		JobID:        jobID,
		ArchiveBytes: job.archiveBytes,
		PayloadBytes: payloadBytes,
		Files:        len(objects),
		ExpiresAt:    expiresAt.Format(time.RFC3339Nano),
	})
}

func (h *Handler) GetPreparedMediaTar(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	jobID := r.PathValue("job_id")
	h.mediaTarJobsMu.Lock()
	job, ok := h.mediaTarJobs[jobID]
	if ok && job.userID == claims.UserID && time.Now().Before(job.expiresAt) {
		delete(h.mediaTarJobs, jobID)
	} else {
		ok = false
	}
	h.mediaTarJobsMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "prepared media TAR not found")
		return
	}
	defer func() {
		_ = os.Remove(job.path)
		h.releasePreparedMediaTar(job.reservedBytes)
	}()
	archive, err := os.Open(job.path)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "prepared media TAR not found")
		return
	}
	defer archive.Close()
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", strconv.FormatInt(job.archiveBytes, 10))
	w.Header().Set("X-Kelma-Media-Files", strconv.Itoa(job.files))
	w.Header().Set("X-Kelma-Media-Bytes", strconv.FormatInt(job.payloadBytes, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, archive)
}

func (h *Handler) DeletePreparedMediaTar(w http.ResponseWriter, r *http.Request) {
	claims := claimsFrom(r)
	jobID := r.PathValue("job_id")
	h.mediaTarJobsMu.Lock()
	job, ok := h.mediaTarJobs[jobID]
	if ok && job.userID == claims.UserID {
		delete(h.mediaTarJobs, jobID)
	} else {
		ok = false
	}
	h.mediaTarJobsMu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "prepared media TAR not found")
		return
	}
	_ = os.Remove(job.path)
	h.releasePreparedMediaTar(job.reservedBytes)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) mediaTarObjects(
	ctx context.Context,
	userID string,
	filenames []string,
) ([]mediaDownloadObject, int64, error) {
	rows, err := h.DB.Query(ctx, `
		SELECT filename, storage_key, size_bytes FROM media
		WHERE user_id = $1 AND filename = ANY($2::text[]) AND storage_key IS NOT NULL`,
		userID, filenames,
	)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	byFilename := make(map[string]mediaDownloadObject, len(filenames))
	for rows.Next() {
		var object mediaDownloadObject
		if err := rows.Scan(&object.filename, &object.key, &object.sizeBytes); err != nil {
			return nil, 0, err
		}
		byFilename[object.filename] = object
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	objects := make([]mediaDownloadObject, 0, len(filenames))
	var payloadBytes int64
	for index, filename := range filenames {
		object, ok := byFilename[filename]
		if !ok {
			continue
		}
		if object.sizeBytes < 0 || object.sizeBytes > maxMediaBytes {
			return nil, 0, fmt.Errorf("invalid media size")
		}
		object.index = index
		objects = append(objects, object)
		payloadBytes += object.sizeBytes
	}
	return objects, payloadBytes, nil
}

func (h *Handler) writePreparedMediaTar(
	parent context.Context,
	archive *os.File,
	objects []mediaDownloadObject,
) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	jobs := make(chan mediaDownloadObject)
	results := make(chan mediaTarFetchResult)
	for range min(mediaTarFetchWorkers, len(objects)) {
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

	writer := tar.NewWriter(archive)
	for range len(objects) {
		result := <-results
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
		header := &tar.Header{Name: strconv.Itoa(result.object.index), Mode: 0o600, Size: result.object.sizeBytes}
		if err := writer.WriteHeader(header); err != nil {
			file.Close()
			return err
		}
		written, err := io.Copy(writer, file)
		file.Close()
		if err != nil {
			return err
		}
		if written != result.object.sizeBytes {
			return fmt.Errorf("media TAR size changed while preparing")
		}
	}
	return writer.Close()
}

func (h *Handler) expirePreparedMediaTar(jobID string) {
	h.mediaTarJobsMu.Lock()
	job, ok := h.mediaTarJobs[jobID]
	if ok {
		delete(h.mediaTarJobs, jobID)
	}
	h.mediaTarJobsMu.Unlock()
	if ok {
		_ = os.Remove(job.path)
		h.releasePreparedMediaTar(job.reservedBytes)
	}
}

func (h *Handler) reservePreparedMediaTar(bytes int64) bool {
	h.mediaTarJobsMu.Lock()
	defer h.mediaTarJobsMu.Unlock()
	if bytes < 0 || h.mediaTarPreparedBytes+bytes > maxPreparedMediaTarDiskBytes {
		return false
	}
	h.mediaTarPreparedBytes += bytes
	return true
}

func (h *Handler) releasePreparedMediaTar(bytes int64) {
	h.mediaTarJobsMu.Lock()
	h.mediaTarPreparedBytes = max(0, h.mediaTarPreparedBytes-bytes)
	h.mediaTarJobsMu.Unlock()
}

func randomMediaTarJobID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
