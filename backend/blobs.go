package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const reservationTTL = time.Hour
const maxProjectBytes = 250 << 20
const maxManifestBytes = 50 << 20

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type ManifestFile struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Size     int64  `json:"size"`
	MIMEType string `json:"mimeType"`
	SHA256   string `json:"sha256"`
	Content  string `json:"content,omitempty"`
}

type BuildManifest struct {
	ProjectID      string         `json:"projectId"`
	IdempotencyKey string         `json:"idempotencyKey"`
	Files          []ManifestFile `json:"files"`
}

type PrepareResponse struct {
	Build        Build    `json:"build"`
	MissingBlobs []string `json:"missingBlobs"`
}

func (s *Server) ensureOwner(w http.ResponseWriter, r *http.Request) (string, error) {
	s.ownerMu.Lock()
	defer s.ownerMu.Unlock()
	if cookie, err := r.Cookie("ide_owner"); err == nil && sha256Pattern.MatchString(cookie.Value) {
		var id string
		err = s.db.QueryRow(`SELECT id FROM owners WHERE id=?`, cookie.Value).Scan(&id)
		if err == nil {
			_, err = s.db.Exec(`UPDATE owners SET last_seen_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), id)
			if err != nil {
				return "", err
			}
			return id, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}

	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT INTO owners(id,created_at,last_seen_at) VALUES(?,?,?)`, id, now, now); err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name: "ide_owner", Value: id, Path: "/", Expires: time.Now().AddDate(10, 0, 0),
		MaxAge: 315360000, HttpOnly: true, Secure: s.config.SecureCookie,
		SameSite: http.SameSiteStrictMode,
	})
	return id, nil
}

func (s *Server) handleBuildPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxManifestBytes)
	var manifest BuildManifest
	if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
		http.Error(w, "manifest tidak valid", http.StatusBadRequest)
		return
	}
	normalized, totalBytes, err := normalizeManifest(manifest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if totalBytes > maxProjectBytes {
		http.Error(w, "ukuran project melebihi 250 MB", http.StatusRequestEntityTooLarge)
		return
	}
	manifestJSON, err := json.Marshal(normalized)
	if err != nil {
		http.Error(w, "gagal memproses manifest", http.StatusInternalServerError)
		return
	}
	hash := sha256.Sum256(manifestJSON)
	manifestHash := hex.EncodeToString(hash[:])
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	ownerID, err := s.ensureOwner(w, r)
	if err != nil {
		http.Error(w, "gagal membuat identitas browser", http.StatusInternalServerError)
		return
	}
	if existing, err := s.getBuildByIdempotency(ownerID, normalized.IdempotencyKey); err == nil {
		if existing.manifestHash != manifestHash {
			http.Error(w, "idempotency key sudah dipakai untuk manifest berbeda", http.StatusConflict)
			return
		}
		missing, err := s.missingBlobs(ownerID, existing.build.ID, normalized)
		if err != nil {
			http.Error(w, "gagal memeriksa binary cache", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, PrepareResponse{Build: existing.build, MissingBlobs: missing})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "gagal memeriksa build idempotent", http.StatusInternalServerError)
		return
	}
	if active, err := s.getActiveBuild(ownerID, normalized.ProjectID); err == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "project ini masih punya build aktif", "build": active})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "gagal memeriksa build aktif", http.StatusInternalServerError)
		return
	}

	id, err := newBuildID()
	if err != nil {
		http.Error(w, "gagal membuat ID build", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	createdAt := now.Format(time.RFC3339)
	tx, err := s.db.Begin()
	if err != nil {
		http.Error(w, "gagal membuat reservasi build", http.StatusInternalServerError)
		return
	}
	_, err = tx.Exec(`INSERT INTO builds(id,project_id,owner_id,idempotency_key,manifest_hash,status,variant,created_at,updated_at)
		VALUES(?,?,?,?,?,'preparing','debug',?,?)`,
		id, normalized.ProjectID, ownerID, normalized.IdempotencyKey, manifestHash, createdAt, createdAt)
	if err == nil {
		_, err = tx.Exec(`INSERT INTO build_manifests(build_id,manifest,expires_at) VALUES(?,?,?)`,
			id, string(manifestJSON), now.Add(reservationTTL).Format(time.RFC3339))
	}
	if err == nil {
		err = insertBlobReservations(tx, id, ownerID, normalized, now.Add(reservationTTL))
	}
	if err != nil {
		tx.Rollback()
		if active, activeErr := s.getActiveBuild(ownerID, normalized.ProjectID); activeErr == nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "project ini masih punya build aktif", "build": active})
			return
		}
		http.Error(w, "gagal menyimpan reservasi build", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "gagal menyimpan reservasi build", http.StatusInternalServerError)
		return
	}
	missing, err := s.missingBlobs(ownerID, id, normalized)
	if err != nil {
		http.Error(w, "gagal memeriksa binary cache", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, PrepareResponse{
		Build:        Build{ID: id, ProjectID: normalized.ProjectID, Status: "preparing", Variant: "debug", CreatedAt: createdAt},
		MissingBlobs: missing,
	})
}

func insertBlobReservations(tx *sql.Tx, buildID, ownerID string, manifest BuildManifest, expiresAt time.Time) error {
	hashes := map[string]bool{}
	for _, file := range manifest.Files {
		if file.Kind == "binary" {
			hashes[file.SHA256] = true
		}
	}
	for hash := range hashes {
		if _, err := tx.Exec(`INSERT INTO blob_reservations(build_id,owner_id,sha256,expires_at) VALUES(?,?,?,?)`,
			buildID, ownerID, hash, expiresAt.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}

func normalizeManifest(manifest BuildManifest) (BuildManifest, int64, error) {
	if !validProjectID(manifest.ProjectID) || !idempotencyPattern.MatchString(manifest.IdempotencyKey) {
		return manifest, 0, errors.New("projectId atau idempotencyKey tidak valid")
	}
	if len(manifest.Files) == 0 || len(manifest.Files) > 20000 {
		return manifest, 0, errors.New("manifest harus berisi 1 sampai 20.000 file")
	}
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	paths := make(map[string]bool, len(manifest.Files))
	hashSizes := make(map[string]int64, len(manifest.Files))
	var totalBytes int64
	for i := range manifest.Files {
		file := &manifest.Files[i]
		if !validManifestPath(file.Path) || paths[file.Path] {
			return manifest, 0, fmt.Errorf("path manifest tidak valid atau duplikat: %s", file.Path)
		}
		paths[file.Path] = true
		if file.Size < 0 || file.Size > 50<<20 || !sha256Pattern.MatchString(file.SHA256) {
			return manifest, 0, fmt.Errorf("ukuran atau SHA-256 tidak valid untuk %s", file.Path)
		}
		if previousSize, exists := hashSizes[file.SHA256]; exists && previousSize != file.Size {
			return manifest, 0, fmt.Errorf("ukuran hash tidak konsisten untuk %s", file.Path)
		}
		hashSizes[file.SHA256] = file.Size
		if file.MIMEType == "" || len(file.MIMEType) > 255 {
			return manifest, 0, fmt.Errorf("mimeType tidak valid untuk %s", file.Path)
		}
		if file.Kind == "text" {
			data := []byte(file.Content)
			if int64(len(data)) != file.Size || hashBytes(data) != file.SHA256 {
				return manifest, 0, fmt.Errorf("ukuran atau SHA-256 teks tidak cocok untuk %s", file.Path)
			}
		} else if file.Kind == "binary" {
			if file.Content != "" {
				return manifest, 0, fmt.Errorf("file binary tidak boleh mengirim content: %s", file.Path)
			}
		} else {
			return manifest, 0, fmt.Errorf("kind file tidak valid: %s", file.Path)
		}
		totalBytes += file.Size
	}
	return manifest, totalBytes, nil
}

func validManifestPath(path string) bool {
	if path == "" || strings.Contains(path, "\\") || strings.HasPrefix(path, "/") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func newBuildID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "bld_" + hex.EncodeToString(random), nil
}

func hashBytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (s *Server) getBuildByIdempotency(ownerID, key string) (struct {
	build        Build
	manifestHash string
}, error) {
	var result struct {
		build        Build
		manifestHash string
	}
	var started, finished, errMsg, artifact sql.NullString
	err := s.db.QueryRow(`SELECT id,project_id,status,variant,created_at,started_at,finished_at,error,artifact,manifest_hash
		FROM builds WHERE owner_id=? AND idempotency_key=?`, ownerID, key).
		Scan(&result.build.ID, &result.build.ProjectID, &result.build.Status, &result.build.Variant, &result.build.CreatedAt,
			&started, &finished, &errMsg, &artifact, &result.manifestHash)
	if err != nil {
		return result, err
	}
	result.build.StartedAt, result.build.FinishedAt = started.String, finished.String
	result.build.Error, result.build.Artifact = errMsg.String, artifact.String
	return result, nil
}

func (s *Server) missingBlobs(ownerID, buildID string, manifest BuildManifest) ([]string, error) {
	seen := map[string]bool{}
	missing := make([]string, 0)
	for _, file := range manifest.Files {
		if file.Kind != "binary" || seen[file.SHA256] {
			continue
		}
		seen[file.SHA256] = true
		var size int64
		err := s.db.QueryRow(`SELECT size FROM blobs WHERE owner_id=? AND sha256=?`, ownerID, file.SHA256).Scan(&size)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && size != file.Size) {
			missing = append(missing, file.SHA256)
			continue
		}
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(s.blobPath(ownerID, file.SHA256))
		if errors.Is(err, os.ErrNotExist) || (err == nil && (!info.Mode().IsRegular() || info.Size() != file.Size)) {
			missing = append(missing, file.SHA256)
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	return missing, nil
}

func (s *Server) blobPath(ownerID, hash string) string {
	return filepath.Join(s.dataDir, "blobs", "sha256", ownerID, hash[:2], hash)
}

func (s *Server) handleBuildBlobUpload(w http.ResponseWriter, r *http.Request, ownerID, buildID, hash string) {
	if r.Method != http.MethodPut || !sha256Pattern.MatchString(hash) {
		http.Error(w, "request upload binary tidak valid", http.StatusBadRequest)
		return
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	var status, manifestJSON string
	var expiresAt string
	err := s.db.QueryRow(`SELECT b.status,m.manifest,m.expires_at FROM builds b
		JOIN build_manifests m ON m.build_id=b.id WHERE b.id=? AND b.owner_id=?`,
		buildID, ownerID).Scan(&status, &manifestJSON, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "gagal mencari reservasi build", http.StatusInternalServerError)
		return
	}
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || time.Now().After(expiry) || status != "preparing" {
		http.Error(w, "reservasi build sudah tidak aktif", http.StatusConflict)
		return
	}
	var manifest BuildManifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		http.Error(w, "manifest build rusak", http.StatusInternalServerError)
		return
	}
	var expectedSize int64 = -1
	for _, file := range manifest.Files {
		if file.Kind == "binary" && file.SHA256 == hash {
			expectedSize = file.Size
			break
		}
	}
	if expectedSize < 0 {
		http.Error(w, "hash tidak terdapat dalam manifest build", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, expectedSize+1)
	tempDir := filepath.Join(s.dataDir, "workspaces", buildID)
	if err := os.MkdirAll(tempDir, 0700); err != nil {
		http.Error(w, "gagal menyiapkan upload", http.StatusInternalServerError)
		return
	}
	temp, err := os.CreateTemp(tempDir, "blob-upload-*")
	if err != nil {
		http.Error(w, "gagal menyiapkan upload", http.StatusInternalServerError)
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, digest), r.Body)
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil {
		http.Error(w, "gagal menerima binary", http.StatusBadRequest)
		return
	}
	if written != expectedSize || hex.EncodeToString(digest.Sum(nil)) != hash {
		http.Error(w, "ukuran atau SHA-256 binary tidak cocok", http.StatusUnprocessableEntity)
		return
	}
	target := s.blobPath(ownerID, hash)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		http.Error(w, "gagal membuat blob store", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT INTO blobs(owner_id,sha256,size,ref_count,created_at)
		VALUES(?,?,?,0,?) ON CONFLICT(owner_id,sha256) DO NOTHING`, ownerID, hash, written, now); err != nil {
		http.Error(w, "gagal menyimpan metadata binary", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tempName, target); err != nil {
		http.Error(w, "gagal menyimpan binary", http.StatusInternalServerError)
		return
	}
	expires := time.Now().Add(reservationTTL).UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE build_manifests SET expires_at=? WHERE build_id=?`, expires, buildID); err != nil {
		http.Error(w, "gagal memperpanjang reservation build", http.StatusInternalServerError)
		return
	}
	if _, err := s.db.Exec(`UPDATE blob_reservations SET expires_at=? WHERE build_id=? AND sha256=?`,
		expires, buildID, hash); err != nil {
		http.Error(w, "gagal memperpanjang reservation binary", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBuildFinalize(w http.ResponseWriter, r *http.Request, ownerID, buildID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	var expiresAt string
	if err := s.db.QueryRow(`SELECT m.expires_at FROM build_manifests m
		JOIN builds b ON b.id=m.build_id WHERE m.build_id=? AND b.owner_id=?`, buildID, ownerID).Scan(&expiresAt); err != nil {
		if build, buildErr := s.getBuildForOwner(ownerID, buildID); buildErr == nil &&
			(build.Status == "queued" || build.Status == "building" || build.Status == "success" ||
				build.Status == "failed" || build.Status == "canceled") {
			writeJSON(w, http.StatusOK, build)
			return
		}
		http.NotFound(w, r)
		return
	}
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || time.Now().After(expiry) {
		http.Error(w, "reservasi build sudah kedaluwarsa", http.StatusConflict)
		return
	}
	var projectID, manifestJSON string
	result, err := s.db.Exec(`UPDATE builds SET status='assembling',updated_at=?
		WHERE id=? AND owner_id=? AND status='preparing'`,
		time.Now().UTC().Format(time.RFC3339), buildID, ownerID)
	if err != nil {
		http.Error(w, "gagal mengunci reservasi build", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "reservasi build tidak ditemukan atau sudah dipakai", http.StatusConflict)
		return
	}
	if err := s.db.QueryRow(`SELECT project_id FROM builds WHERE id=? AND owner_id=?`, buildID, ownerID).Scan(&projectID); err != nil {
		s.failBuild(buildID, "reservasi build tidak ditemukan")
		http.Error(w, "reservasi build tidak ditemukan", http.StatusNotFound)
		return
	}
	if err := s.db.QueryRow(`SELECT manifest FROM build_manifests WHERE build_id=?`, buildID).Scan(&manifestJSON); err != nil {
		s.failBuild(buildID, "manifest build tidak ditemukan")
		http.Error(w, "manifest build tidak ditemukan", http.StatusNotFound)
		return
	}
	var manifest BuildManifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		s.failBuild(buildID, "manifest build rusak")
		http.Error(w, "manifest build rusak", http.StatusInternalServerError)
		return
	}
	missing, err := s.missingBlobs(ownerID, buildID, manifest)
	if err != nil {
		s.failBuild(buildID, "gagal memeriksa binary cache")
		http.Error(w, "gagal memeriksa binary cache", http.StatusInternalServerError)
		return
	}
	if len(missing) > 0 {
		s.failBuild(buildID, "binary project belum lengkap")
		http.Error(w, "masih ada binary yang belum diupload", http.StatusConflict)
		return
	}
	workspace := filepath.Join(s.dataDir, "workspaces", buildID)
	if err := os.MkdirAll(workspace, 0750); err != nil {
		s.failBuild(buildID, "gagal membuat workspace")
		http.Error(w, "gagal membuat workspace", http.StatusInternalServerError)
		return
	}
	for _, file := range manifest.Files {
		var data []byte
		if file.Kind == "text" {
			data = []byte(file.Content)
		} else {
			source, err := os.Open(s.blobPath(ownerID, file.SHA256))
			if err != nil {
				os.RemoveAll(workspace)
				s.failBuild(buildID, "binary cache tidak bisa dibaca")
				http.Error(w, "binary cache tidak bisa dibaca", http.StatusInternalServerError)
				return
			}
			data, err = io.ReadAll(io.LimitReader(source, file.Size+1))
			source.Close()
			if err != nil || int64(len(data)) != file.Size || hashBytes(data) != file.SHA256 {
				os.RemoveAll(workspace)
				s.failBuild(buildID, "binary cache gagal diverifikasi")
				http.Error(w, "binary cache gagal diverifikasi", http.StatusUnprocessableEntity)
				return
			}
		}
		target := filepath.Join(workspace, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			os.RemoveAll(workspace)
			s.failBuild(buildID, "gagal membuat struktur project")
			http.Error(w, "gagal membuat struktur project", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(target, data, 0640); err != nil {
			os.RemoveAll(workspace)
			s.failBuild(buildID, "gagal merakit project")
			http.Error(w, "gagal merakit project", http.StatusInternalServerError)
			return
		}
	}
	if err := validateAndroidProject(workspace); err != nil {
		os.RemoveAll(workspace)
		s.failBuild(buildID, err.Error())
		http.Error(w, "project tidak valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.commitBuildBlobRefs(buildID, ownerID, manifest); err != nil {
		os.RemoveAll(workspace)
		s.failBuild(buildID, "gagal menyimpan referensi binary")
		http.Error(w, "gagal menyimpan referensi binary", http.StatusInternalServerError)
		return
	}
	s.addLog(buildID, "system", "Project dirakit dari manifest dan binary cache")
	s.startBuild(buildID, workspace)
	build, err := s.getBuild(buildID)
	if err != nil {
		http.Error(w, "build dibuat tetapi gagal membaca status", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, build)
}

func (s *Server) commitBuildBlobRefs(buildID, ownerID string, manifest BuildManifest) error {
	hashes := map[string]bool{}
	for _, file := range manifest.Files {
		if file.Kind == "binary" {
			hashes[file.SHA256] = true
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for hash := range hashes {
		result, err := tx.Exec(`UPDATE blobs SET ref_count=ref_count+1 WHERE owner_id=? AND sha256=?`, ownerID, hash)
		if err != nil {
			tx.Rollback()
			return err
		}
		count, err := result.RowsAffected()
		if err != nil || count != 1 {
			tx.Rollback()
			return fmt.Errorf("blob %s tidak ditemukan", hash)
		}
		if _, err := tx.Exec(`INSERT INTO build_blob_refs(build_id,owner_id,sha256,counted) VALUES(?,?,?,1)`,
			buildID, ownerID, hash); err != nil {
			tx.Rollback()
			return err
		}
	}
	result, err := tx.Exec(`UPDATE builds SET status='queued',updated_at=? WHERE id=? AND owner_id=? AND status='assembling'`,
		now, buildID, ownerID)
	if err != nil {
		tx.Rollback()
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		tx.Rollback()
		return errors.New("reservasi build tidak lagi aktif")
	}
	if _, err := tx.Exec(`DELETE FROM build_manifests WHERE build_id=?`, buildID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM blob_reservations WHERE build_id=?`, buildID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Server) releaseAllBuildBlobRefs() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE blobs SET ref_count=ref_count-(SELECT COUNT(*) FROM build_blob_refs r
		WHERE r.owner_id=blobs.owner_id AND r.sha256=blobs.sha256 AND r.counted=1)`); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM build_blob_refs`); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Server) runBlobGC() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.maintenanceMu.Lock()
		if err := s.collectExpiredBlobReservations(); err != nil {
			fmt.Fprintf(os.Stderr, "blob cache GC failed: %v\n", err)
		}
		s.maintenanceMu.Unlock()
	}
}

func (s *Server) collectExpiredBlobReservations() error {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.db.Query(`SELECT b.id FROM builds b JOIN build_manifests m ON m.build_id=b.id
		WHERE b.status IN ('preparing','assembling') AND m.expires_at<?`, now)
	if err != nil {
		return err
	}
	var expired []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, id)
	}
	rows.Close()
	for _, id := range expired {
		if _, err := s.db.Exec(`UPDATE builds SET status='failed',finished_at=?,updated_at=?,error='reservation expired'
			WHERE id=? AND status IN ('preparing','assembling')`, now, now, id); err != nil {
			return err
		}
		if _, err := s.db.Exec(`DELETE FROM build_manifests WHERE build_id=?`, id); err != nil {
			return err
		}
		if _, err := s.db.Exec(`DELETE FROM blob_reservations WHERE build_id=?`, id); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(s.dataDir, "workspaces", id)); err != nil {
			return err
		}
	}
	rows, err = s.db.Query(`SELECT owner_id,sha256 FROM blobs b WHERE ref_count<=0
		AND NOT EXISTS (SELECT 1 FROM blob_reservations r WHERE r.owner_id=b.owner_id AND r.sha256=b.sha256)`)
	if err != nil {
		return err
	}
	type blobRecord struct{ ownerID, hash string }
	var unreferenced []blobRecord
	for rows.Next() {
		var blob blobRecord
		if err := rows.Scan(&blob.ownerID, &blob.hash); err != nil {
			rows.Close()
			return err
		}
		unreferenced = append(unreferenced, blob)
	}
	rows.Close()
	for _, blob := range unreferenced {
		if err := os.Remove(s.blobPath(blob.ownerID, blob.hash)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := s.db.Exec(`DELETE FROM blobs WHERE owner_id=? AND sha256=? AND ref_count<=0`, blob.ownerID, blob.hash); err != nil {
			return err
		}
	}
	return nil
}
