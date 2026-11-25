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
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const reservationTTL = time.Hour
const publicIdleTTL = 5 * time.Minute
const maxProjectBytes = 250 << 20
const maxPublicProjectBytes = 10 << 20
const maxManifestBytes = 50 << 20
const maxKeystoreBytes = 32 << 20

var sha256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var idempotencyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var buildIDPattern = regexp.MustCompile(`^bld_[a-f0-9]{32}$`)

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
	ProjectType    string         `json:"projectType,omitempty"`
	Variant        string         `json:"variant,omitempty"`
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

func (s *Server) requestOwner(w http.ResponseWriter, r *http.Request) (string, error) {
	if s.config.Mode == "public" {
		if cookie, err := r.Cookie("ide_owner"); err == nil && sha256Pattern.MatchString(cookie.Value) {
			return "public_" + cookie.Value, nil
		}
		random := make([]byte, 32)
		if _, err := rand.Read(random); err != nil {
			return "", err
		}
		id := hex.EncodeToString(random)
		http.SetCookie(w, &http.Cookie{
			Name: "ide_owner", Value: id, Path: "/", Expires: time.Now().AddDate(10, 0, 0),
			MaxAge: 315360000, HttpOnly: true, Secure: s.config.SecureCookie,
			SameSite: http.SameSiteStrictMode,
		})
		return "public_" + id, nil
	}
	ownerID, err := s.ensureOwner(w, r)
	if err != nil {
		return "", err
	}
	return ownerID, nil
}

func (s *Server) handleBuildPrepare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Metode HTTP tidak diizinkan.", http.StatusMethodNotAllowed)
		return
	}
	maxBytes := int64(maxManifestBytes)
	if s.config.Mode == "public" {
		maxBytes = 64 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	var manifest BuildManifest
	if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
		http.Error(w, "Manifest tidak valid.", http.StatusBadRequest)
		return
	}
	normalized, totalBytes, err := normalizeManifest(manifest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	maxProjectSize := int64(maxProjectBytes)
	if s.config.Mode == "public" {
		maxProjectSize = maxPublicProjectBytes
	}
	if totalBytes > maxProjectSize {
		if s.config.Mode == "public" {
			http.Error(w, "Ukuran proyek dalam mode publik melebihi 10 MiB setelah diekstrak.", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Ukuran proyek melebihi batas 250 MB.", http.StatusRequestEntityTooLarge)
		return
	}
	manifestJSON, err := json.Marshal(normalized)
	if err != nil {
		http.Error(w, "Gagal memproses manifest.", http.StatusInternalServerError)
		return
	}
	hash := sha256.Sum256(manifestJSON)
	manifestHash := hex.EncodeToString(hash[:])
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	ownerID, err := s.requestOwner(w, r)
	if err != nil {
		http.Error(w, "Gagal membuat identitas peramban.", http.StatusInternalServerError)
		return
	}
	if existing, err := s.getBuildByIdempotency(ownerID, normalized.IdempotencyKey); err == nil {
		if existing.manifestHash != manifestHash {
			http.Error(w, "Kunci idempotensi telah digunakan untuk manifest yang berbeda.", http.StatusConflict)
			return
		}
		_, blobOwnerID, err := s.buildStorage(existing.build.ID)
		if err != nil {
			http.Error(w, "Gagal membaca cache berkas biner build.", http.StatusInternalServerError)
			return
		}
		missing, err := s.missingBlobs(blobOwnerID, existing.build.ID, normalized)
		if err != nil {
			http.Error(w, "Gagal memeriksa cache berkas biner.", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, PrepareResponse{Build: existing.build, MissingBlobs: missing})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Gagal memeriksa build idempoten.", http.StatusInternalServerError)
		return
	}
	if active, err := s.getActiveBuild(ownerID, normalized.ProjectID); err == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "Proyek ini masih memiliki build aktif.", "build": active})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Gagal memeriksa build aktif.", http.StatusInternalServerError)
		return
	}

	id, err := newBuildID()
	if err != nil {
		http.Error(w, "Gagal membuat ID build.", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	createdAt := now.Format(time.RFC3339)
	blobOwnerID := ownerID
	if s.config.Mode == "public" {
		blobOwnerID = "public_build_" + id
	}
	tx, err := s.db.Begin()
	if err != nil {
		http.Error(w, "Gagal membuat reservasi build.", http.StatusInternalServerError)
		return
	}
	_, err = tx.Exec(`INSERT INTO builds(id,project_id,owner_id,idempotency_key,manifest_hash,status,variant,created_at,updated_at,access_mode,blob_owner_id)
		VALUES(?,?,?,?,?,'preparing',?,?,?,?,?)`,
		id, normalized.ProjectID, ownerID, normalized.IdempotencyKey, manifestHash, normalized.Variant, createdAt, createdAt, s.config.Mode, blobOwnerID)
	expiresAt := now.Add(reservationTTL)
	if s.config.Mode == "public" {
		expiresAt = now.Add(publicIdleTTL)
	}
	if err == nil {
		_, err = tx.Exec(`INSERT INTO build_manifests(build_id,manifest,expires_at) VALUES(?,?,?)`,
			id, string(manifestJSON), expiresAt.Format(time.RFC3339))
	}
	if err == nil {
		err = insertBlobReservations(tx, id, blobOwnerID, normalized, expiresAt)
	}
	if err != nil {
		tx.Rollback()
		if active, activeErr := s.getActiveBuild(ownerID, normalized.ProjectID); activeErr == nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "Proyek ini masih memiliki build aktif.", "build": active})
			return
		}
		http.Error(w, "Gagal menyimpan reservasi build.", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "Gagal menyimpan reservasi build.", http.StatusInternalServerError)
		return
	}
	missing, err := s.missingBlobs(blobOwnerID, id, normalized)
	if err != nil {
		http.Error(w, "Gagal memeriksa cache berkas biner.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, PrepareResponse{
		Build:        Build{ID: id, ProjectID: normalized.ProjectID, ProjectType: normalized.ProjectType, Status: "preparing", Variant: normalized.Variant, CreatedAt: createdAt},
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
	if manifest.ProjectType == "" {
		manifest.ProjectType = "android"
	}
	switch manifest.ProjectType {
	case "android":
		if manifest.Variant == "" {
			manifest.Variant = "debug"
		}
		if manifest.Variant != "debug" && manifest.Variant != "release" {
			return manifest, 0, errors.New("variant build android harus debug atau release")
		}
	case "java-library":
		if manifest.Variant == "" {
			manifest.Variant = "jar"
		}
		if manifest.Variant != "jar" && manifest.Variant != "fat-jar" {
			return manifest, 0, errors.New("variant build java library harus jar atau fat-jar")
		}
	default:
		return manifest, 0, errors.New("Jenis proyek tidak didukung.")
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

func validBuildID(id string) bool { return buildIDPattern.MatchString(id) }

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
	result.build.ProjectType = buildProjectType(result.build.Variant)
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
	if strings.HasPrefix(ownerID, "public_build_") {
		return filepath.Join(s.dataDir, "public", "blobs", "sha256", ownerID, hash[:2], hash)
	}
	return filepath.Join(s.dataDir, "blobs", "sha256", ownerID, hash[:2], hash)
}

func (s *Server) handleBuildBlobUpload(w http.ResponseWriter, r *http.Request, ownerID, buildID, hash string) {
	if r.Method != http.MethodPut || !sha256Pattern.MatchString(hash) {
		http.Error(w, "Permintaan unggah berkas biner tidak valid.", http.StatusBadRequest)
		return
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	var status, manifestJSON, accessMode, blobOwnerID, browserOwnerID string
	var expiresAt string
	err := s.db.QueryRow(`SELECT b.status,m.manifest,m.expires_at,b.access_mode,b.blob_owner_id,b.owner_id FROM builds b
		JOIN build_manifests m ON m.build_id=b.id WHERE b.id=? AND b.owner_id=?`,
		buildID, ownerID).Scan(&status, &manifestJSON, &expiresAt, &accessMode, &blobOwnerID, &browserOwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Gagal mencari reservasi build.", http.StatusInternalServerError)
		return
	}
	if blobOwnerID == "" {
		blobOwnerID = browserOwnerID
	}
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || time.Now().After(expiry) || status != "preparing" {
		http.Error(w, "Reservasi build sudah tidak aktif.", http.StatusConflict)
		return
	}
	var manifest BuildManifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		http.Error(w, "Manifest build rusak.", http.StatusInternalServerError)
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
		http.Error(w, "Hash tidak terdapat dalam manifest build.", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, expectedSize+1)
	tempDir := s.workspacePath(buildID, accessMode)
	if err := os.MkdirAll(tempDir, 0700); err != nil {
		http.Error(w, "Gagal menyiapkan proses unggah.", http.StatusInternalServerError)
		return
	}
	temp, err := os.CreateTemp(tempDir, "blob-upload-*")
	if err != nil {
		http.Error(w, "Gagal menyiapkan proses unggah.", http.StatusInternalServerError)
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, digest), r.Body)
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil {
		http.Error(w, "Gagal menerima berkas biner.", http.StatusBadRequest)
		return
	}
	if written != expectedSize || hex.EncodeToString(digest.Sum(nil)) != hash {
		http.Error(w, "Ukuran atau SHA-256 berkas biner tidak cocok.", http.StatusUnprocessableEntity)
		return
	}
	target := s.blobPath(blobOwnerID, hash)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		http.Error(w, "Gagal membuat penyimpanan blob.", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT INTO blobs(owner_id,sha256,size,ref_count,created_at)
		VALUES(?,?,?,0,?) ON CONFLICT(owner_id,sha256) DO NOTHING`, blobOwnerID, hash, written, now); err != nil {
		http.Error(w, "Gagal menyimpan metadata berkas biner.", http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tempName, target); err != nil {
		http.Error(w, "Gagal menyimpan berkas biner.", http.StatusInternalServerError)
		return
	}
	expiration := time.Now().Add(reservationTTL)
	if accessMode == "public" {
		expiration = time.Now().Add(publicIdleTTL)
	}
	expires := expiration.UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE build_manifests SET expires_at=? WHERE build_id=?`, expires, buildID); err != nil {
		http.Error(w, "Gagal memperpanjang reservasi build.", http.StatusInternalServerError)
		return
	}
	if _, err := s.db.Exec(`UPDATE blob_reservations SET expires_at=? WHERE build_id=? AND sha256=?`,
		expires, buildID, hash); err != nil {
		http.Error(w, "Gagal memperpanjang reservasi berkas biner.", http.StatusInternalServerError)
		return
	}
	if _, err := s.db.Exec(`UPDATE builds SET updated_at=? WHERE id=? AND access_mode='public'`,
		time.Now().UTC().Format(time.RFC3339), buildID); err != nil {
		http.Error(w, "Gagal memperbarui aktivitas build.", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBuildFinalize(w http.ResponseWriter, r *http.Request, ownerID, buildID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Metode HTTP tidak diizinkan.", http.StatusMethodNotAllowed)
		return
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	var expiresAt, variant, status, accessMode, blobOwnerID, browserOwnerID string
	if err := s.db.QueryRow(`SELECT m.expires_at,b.variant,b.status,b.access_mode,b.blob_owner_id,b.owner_id FROM build_manifests m
		JOIN builds b ON b.id=m.build_id WHERE m.build_id=? AND b.owner_id=?`, buildID, ownerID).
		Scan(&expiresAt, &variant, &status, &accessMode, &blobOwnerID, &browserOwnerID); err != nil {
		if build, buildErr := s.getBuildForOwner(ownerID, buildID); buildErr == nil &&
			(build.Status == "queued" || build.Status == "building" || build.Status == "success" ||
				build.Status == "failed" || build.Status == "canceled") {
			writeJSON(w, http.StatusOK, build)
			return
		}
		http.NotFound(w, r)
		return
	}
	if blobOwnerID == "" {
		blobOwnerID = browserOwnerID
	}
	if status != "preparing" {
		if build, err := s.getBuildForOwner(ownerID, buildID); err == nil &&
			(build.Status == "queued" || build.Status == "building" || build.Status == "success" ||
				build.Status == "failed" || build.Status == "canceled") {
			writeJSON(w, http.StatusOK, build)
			return
		}
		http.Error(w, "Reservasi build tidak ditemukan atau sudah digunakan.", http.StatusConflict)
		return
	}
	expiry, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || time.Now().After(expiry) {
		http.Error(w, "Reservasi build telah kedaluwarsa.", http.StatusConflict)
		return
	}
	signing, err := s.parseReleaseSigning(w, r, buildID, variant)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "Berkas keystore melebihi batas 32 MB.", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	signingTransferred := false
	if signing != nil {
		defer func() {
			if !signingTransferred {
				s.cleanupReleaseSigning(signing)
			}
		}()
	}
	var projectID, manifestJSON string
	result, err := s.db.Exec(`UPDATE builds SET status='assembling',updated_at=?
		WHERE id=? AND owner_id=? AND status='preparing'`,
		time.Now().UTC().Format(time.RFC3339), buildID, ownerID)
	if err != nil {
		http.Error(w, "Gagal mengunci reservasi build.", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "Reservasi build tidak ditemukan atau sudah digunakan.", http.StatusConflict)
		return
	}
	if err := s.db.QueryRow(`SELECT project_id FROM builds WHERE id=? AND owner_id=?`, buildID, ownerID).Scan(&projectID); err != nil {
		s.failBuild(buildID, "reservasi build tidak ditemukan")
		http.Error(w, "Reservasi build tidak ditemukan.", http.StatusNotFound)
		return
	}
	if err := s.db.QueryRow(`SELECT manifest FROM build_manifests WHERE build_id=?`, buildID).Scan(&manifestJSON); err != nil {
		s.failBuild(buildID, "manifest build tidak ditemukan")
		http.Error(w, "Manifest build tidak ditemukan.", http.StatusNotFound)
		return
	}
	var manifest BuildManifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		s.failBuild(buildID, "manifest build rusak")
		http.Error(w, "Manifest build rusak.", http.StatusInternalServerError)
		return
	}
	missing, err := s.missingBlobs(blobOwnerID, buildID, manifest)
	if err != nil {
		s.failBuild(buildID, "gagal memeriksa binary cache")
		http.Error(w, "Gagal memeriksa cache berkas biner.", http.StatusInternalServerError)
		return
	}
	if len(missing) > 0 {
		s.failBuild(buildID, "binary project belum lengkap")
		http.Error(w, "Masih ada berkas biner yang belum diunggah.", http.StatusConflict)
		return
	}
	workspace := s.workspacePath(buildID, accessMode)
	if err := os.MkdirAll(workspace, 0750); err != nil {
		s.failBuild(buildID, "gagal membuat workspace")
		http.Error(w, "Gagal membuat ruang kerja.", http.StatusInternalServerError)
		return
	}
	for _, file := range manifest.Files {
		var data []byte
		if file.Kind == "text" {
			data = []byte(file.Content)
		} else {
			source, err := os.Open(s.blobPath(blobOwnerID, file.SHA256))
			if err != nil {
				os.RemoveAll(workspace)
				s.failBuild(buildID, "binary cache tidak bisa dibaca")
				http.Error(w, "Cache berkas biner tidak dapat dibaca.", http.StatusInternalServerError)
				return
			}
			data, err = io.ReadAll(io.LimitReader(source, file.Size+1))
			source.Close()
			if err != nil || int64(len(data)) != file.Size || hashBytes(data) != file.SHA256 {
				os.RemoveAll(workspace)
				s.failBuild(buildID, "binary cache gagal diverifikasi")
				http.Error(w, "Verifikasi cache berkas biner gagal.", http.StatusUnprocessableEntity)
				return
			}
		}
		target := filepath.Join(workspace, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			os.RemoveAll(workspace)
			s.failBuild(buildID, "gagal membuat struktur project")
			http.Error(w, "Gagal membuat struktur proyek.", http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(target, data, 0640); err != nil {
			os.RemoveAll(workspace)
			s.failBuild(buildID, "gagal merakit project")
			http.Error(w, "Gagal menyusun proyek.", http.StatusInternalServerError)
			return
		}
	}
	if err := validateProjectForBuild(workspace, manifest.ProjectType); err != nil {
		os.RemoveAll(workspace)
		s.failBuild(buildID, err.Error())
		http.Error(w, "Proyek tidak valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.commitBuildBlobRefs(buildID, blobOwnerID, manifest); err != nil {
		os.RemoveAll(workspace)
		s.failBuild(buildID, "gagal menyimpan referensi binary")
		http.Error(w, "Gagal menyimpan referensi berkas biner.", http.StatusInternalServerError)
		return
	}
	s.addLog(buildID, "system", "Project dirakit dari manifest dan binary cache")
	s.startBuild(buildID, workspace, signing)
	signingTransferred = signing != nil
	build, err := s.getBuild(buildID)
	if err != nil {
		http.Error(w, "Build berhasil dibuat, tetapi statusnya gagal dibaca.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, build)
}

type releaseSigning struct {
	keyStorePath      string
	storePasswordPath string
	keyPasswordPath   string
	keyAlias          string
}

func (s *Server) parseReleaseSigning(w http.ResponseWriter, r *http.Request, buildID, variant string) (*releaseSigning, error) {
	if variant != "release" {
		if r.ContentLength != 0 {
			return nil, errors.New("opsi tanda tangan hanya berlaku untuk build release")
		}
		return nil, nil
	}
	if r.ContentLength == 0 {
		return nil, nil
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return nil, errors.New("data tanda tangan harus berupa multipart/form-data")
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxKeystoreBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return nil, err
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	} else {
		return nil, errors.New("form data keystore tidak valid")
	}
	files := r.MultipartForm.File["keystore"]
	keyAlias := strings.TrimSpace(r.FormValue("keyAlias"))
	storePassword := r.FormValue("storePassword")
	keyPassword := r.FormValue("keyPassword")
	hasSigningData := len(files) > 0 || keyAlias != "" || storePassword != "" || keyPassword != ""
	if !hasSigningData {
		return nil, nil
	}
	if len(files) != 1 || keyAlias == "" || storePassword == "" || keyPassword == "" {
		return nil, errors.New("keystore, alias, store password, dan key password wajib diisi")
	}
	if files[0].Size <= 0 || files[0].Size > maxKeystoreBytes {
		return nil, errors.New("ukuran keystore harus antara 1 byte dan 32 MB")
	}
	if len(keyAlias) > 256 || len(storePassword) > 4096 || len(keyPassword) > 4096 ||
		strings.ContainsAny(keyAlias+storePassword+keyPassword, "\x00\r\n") {
		return nil, errors.New("alias atau password keystore tidak valid")
	}

	signingDir := filepath.Join(s.dataDir, "signing")
	if err := os.MkdirAll(signingDir, 0700); err != nil {
		return nil, fmt.Errorf("gagal menyiapkan file keystore")
	}
	if err := os.Chmod(signingDir, 0700); err != nil {
		return nil, errors.New("gagal mengamankan direktori keystore")
	}
	temp, err := os.CreateTemp(signingDir, "keystore-upload-*")
	if err != nil {
		return nil, errors.New("gagal menyiapkan file keystore")
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	file, err := files[0].Open()
	if err != nil {
		temp.Close()
		return nil, errors.New("gagal membaca file keystore")
	}
	written, copyErr := io.Copy(temp, io.LimitReader(file, maxKeystoreBytes+1))
	file.Close()
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil || written != files[0].Size || written > maxKeystoreBytes {
		return nil, errors.New("gagal menerima file keystore")
	}

	keyStorePath := filepath.Join(signingDir, buildID+".keystore")
	if err := os.Rename(tempPath, keyStorePath); err != nil {
		return nil, errors.New("gagal menyimpan file keystore sementara")
	}
	storePasswordPath := filepath.Join(signingDir, buildID+".storepass")
	keyPasswordPath := filepath.Join(signingDir, buildID+".keypass")
	if err := writeSigningSecret(storePasswordPath, storePassword); err != nil {
		os.Remove(keyStorePath)
		os.Remove(storePasswordPath)
		return nil, errors.New("gagal menyimpan password keystore sementara")
	}
	if err := writeSigningSecret(keyPasswordPath, keyPassword); err != nil {
		os.Remove(keyStorePath)
		os.Remove(storePasswordPath)
		os.Remove(keyPasswordPath)
		return nil, errors.New("gagal menyimpan password keystore sementara")
	}
	return &releaseSigning{
		keyStorePath: keyStorePath, storePasswordPath: storePasswordPath,
		keyPasswordPath: keyPasswordPath, keyAlias: keyAlias,
	}, nil
}

func writeSigningSecret(path, value string) error {
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

func (s *Server) cleanupReleaseSigning(signing *releaseSigning) {
	if signing == nil {
		return
	}
	for _, path := range []string{signing.keyStorePath, signing.storePasswordPath, signing.keyPasswordPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("failed to remove temporary signing file: %v", err)
		}
	}
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

func releaseBuildBlobRefsTx(tx *sql.Tx, buildID string) error {
	if _, err := tx.Exec(`UPDATE blobs SET ref_count=ref_count-(SELECT COUNT(*) FROM build_blob_refs r
		WHERE r.build_id=? AND r.owner_id=blobs.owner_id AND r.sha256=blobs.sha256 AND r.counted=1)
		WHERE EXISTS (SELECT 1 FROM build_blob_refs r WHERE r.build_id=? AND r.owner_id=blobs.owner_id
		AND r.sha256=blobs.sha256 AND r.counted=1)`, buildID, buildID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM build_blob_refs WHERE build_id=?`, buildID)
	return err
}

func (s *Server) runBlobGC() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.maintenanceMu.Lock()
		if err := s.cleanupExpiredPublicBuilds(time.Now().UTC()); err != nil {
			log.Printf("public build cleanup failed: %v", err)
		}
		if err := s.collectExpiredBlobReservations(); err != nil {
			fmt.Fprintf(os.Stderr, "blob cache GC failed: %v\n", err)
		}
		s.maintenanceMu.Unlock()
	}
}

func (s *Server) cleanupPublicBuildCache(buildID string) error {
	mode, blobOwnerID, err := s.buildStorage(buildID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && mode != "public") {
		return nil
	}
	if err != nil {
		return err
	}
	if !validBuildID(buildID) || blobOwnerID != "public_build_"+buildID {
		return fmt.Errorf("public build %s has invalid blob owner", buildID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := releaseBuildBlobRefsTx(tx, buildID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM blob_reservations WHERE build_id=?`, buildID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM build_manifests WHERE build_id=?`, buildID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM blobs WHERE owner_id=?`, blobOwnerID); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, directory := range []string{
		filepath.Join(s.dataDir, "public", "blobs", "sha256", blobOwnerID),
		filepath.Join(s.dataDir, "public", "gradle", buildID),
	} {
		if err := os.RemoveAll(directory); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) cleanupExpiredPublicBuilds(now time.Time) error {
	cutoff := now.Add(-publicIdleTTL).UTC().Format(time.RFC3339)
	rows, err := s.db.Query(`SELECT id,status FROM builds WHERE access_mode='public' AND updated_at!='' AND updated_at<?`, cutoff)
	if err != nil {
		return err
	}
	type expiredBuild struct{ id, status string }
	var expired []expiredBuild
	for rows.Next() {
		var build expiredBuild
		if err := rows.Scan(&build.id, &build.status); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, build)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	cleaned := false
	for _, build := range expired {
		if !validBuildID(build.id) {
			return fmt.Errorf("invalid public build id %q", build.id)
		}
		s.buildMu.Lock()
		running := s.building[build.id]
		s.buildMu.Unlock()
		if running {
			continue
		}
		if build.status == "queued" || build.status == "building" {
			if build.status == "building" {
				containerName := "android-ide-build-" + build.id
				out, err := exec.Command("docker", "ps", "-aq", "--filter", "name=^"+containerName+"$").Output()
				if err != nil {
					return fmt.Errorf("check abandoned public build container %s: %w", build.id, err)
				}
				if strings.TrimSpace(string(out)) != "" {
					if out, err := exec.Command("docker", "rm", "-f", containerName).CombinedOutput(); err != nil {
						return fmt.Errorf("stop abandoned public build container %s: %s: %w", build.id, strings.TrimSpace(string(out)), err)
					}
				}
			}
			if _, err := s.db.Exec(`UPDATE builds SET status='failed',finished_at=?,updated_at=?,error='public build expired after inactivity'
				WHERE id=? AND status IN ('queued','building')`,
				now.Format(time.RFC3339), now.Format(time.RFC3339), build.id); err != nil {
				return err
			}
		}
		if err := s.removeExpiredPublicBuild(build.id); err != nil {
			return err
		}
		cleaned = true
	}
	if cleaned {
		if _, err := s.db.Exec(`VACUUM`); err != nil {
			return fmt.Errorf("compact build database after public cleanup: %w", err)
		}
	}
	return nil
}

func (s *Server) removeExpiredPublicBuild(buildID string) error {
	if !validBuildID(buildID) {
		return fmt.Errorf("invalid public build id %q", buildID)
	}
	var mode, artifact, variant string
	if err := s.db.QueryRow(`SELECT access_mode,COALESCE(artifact,''),variant FROM builds WHERE id=?`, buildID).
		Scan(&mode, &artifact, &variant); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	} else if mode != "public" {
		return fmt.Errorf("refusing to expire non-public build %s", buildID)
	}
	if artifact != "" {
		expectedFilename, err := artifactFilename(buildID, variant)
		if err != nil || artifact != expectedFilename || filepath.Base(artifact) != artifact {
			return fmt.Errorf("invalid artifact path for public build %s", buildID)
		}
		if err := os.Remove(s.artifactPath(buildID, mode, variant)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(s.workspacePath(buildID, mode)); err != nil {
		return err
	}
	if err := s.cleanupPublicBuildCache(buildID); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for _, query := range []string{
		`DELETE FROM build_logs WHERE build_id=?`,
		`DELETE FROM build_blob_refs WHERE build_id=?`,
		`DELETE FROM blob_reservations WHERE build_id=?`,
		`DELETE FROM build_manifests WHERE build_id=?`,
		`DELETE FROM builds WHERE id=? AND access_mode='public'`,
	} {
		if _, err := tx.Exec(query, buildID); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Server) collectExpiredBlobReservations() error {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.db.Query(`SELECT b.id,b.access_mode FROM builds b JOIN build_manifests m ON m.build_id=b.id
		WHERE b.status IN ('preparing','assembling') AND m.expires_at<?`, now)
	if err != nil {
		return err
	}
	type expiredReservation struct{ id, mode string }
	var expired []expiredReservation
	for rows.Next() {
		var item expiredReservation
		if err := rows.Scan(&item.id, &item.mode); err != nil {
			rows.Close()
			return err
		}
		expired = append(expired, item)
	}
	rows.Close()
	for _, item := range expired {
		id := item.id
		result, err := s.db.Exec(`UPDATE builds SET status='failed',finished_at=?,updated_at=?,error='reservation expired'
			WHERE id=? AND status IN ('preparing','assembling')`, now, now, id)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed > 0 {
			s.enqueueBuildNotification(id, "failed")
		}
		if _, err := s.db.Exec(`DELETE FROM build_manifests WHERE build_id=?`, id); err != nil {
			return err
		}
		if _, err := s.db.Exec(`DELETE FROM blob_reservations WHERE build_id=?`, id); err != nil {
			return err
		}
		if err := os.RemoveAll(s.workspacePath(id, item.mode)); err != nil {
			return err
		}
		if item.mode == "public" {
			if err := s.cleanupPublicBuildCache(id); err != nil {
				return err
			}
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
