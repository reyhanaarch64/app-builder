package main

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type BuildArtifact struct {
	BuildID     string `json:"buildId"`
	ProjectID   string `json:"projectId"`
	ProjectType string `json:"projectType"`
	Variant     string `json:"variant"`
	CreatedAt   string `json:"createdAt"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	Available   bool   `json:"available"`
}

func (s *Server) handleArtifactRoutes(w http.ResponseWriter, r *http.Request) {
	ownerID, err := s.requestOwner(w, r)
	if err != nil {
		http.Error(w, "Gagal membuat identitas peramban.", http.StatusInternalServerError)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/artifacts/"), "/")
	if r.URL.Path == "/api/artifacts" || rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "Metode HTTP tidak diizinkan.", http.StatusMethodNotAllowed)
			return
		}
		s.listArtifacts(w, ownerID)
		return
	}
	if strings.Contains(rest, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "Metode HTTP tidak diizinkan.", http.StatusMethodNotAllowed)
		return
	}
	s.deleteArtifact(w, ownerID, rest)
}

func (s *Server) listArtifacts(w http.ResponseWriter, ownerID string) {
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`UPDATE builds SET updated_at=? WHERE owner_id=? AND access_mode='public'
		AND status='success' AND artifact IS NOT NULL AND artifact!=''`, now, ownerID); err != nil {
		http.Error(w, "Gagal memperbarui aktivitas hasil build.", http.StatusInternalServerError)
		return
	}
	rows, err := s.db.Query(`SELECT id,project_id,variant,created_at,artifact,access_mode FROM builds
		WHERE owner_id=? AND status='success' AND artifact IS NOT NULL AND artifact!=''
		ORDER BY created_at DESC,id DESC`, ownerID)
	if err != nil {
		http.Error(w, "Gagal membaca daftar hasil build.", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	artifacts := make([]BuildArtifact, 0)
	for rows.Next() {
		var artifact BuildArtifact
		var mode string
		if err := rows.Scan(&artifact.BuildID, &artifact.ProjectID, &artifact.Variant,
			&artifact.CreatedAt, &artifact.Filename, &mode); err != nil {
			http.Error(w, "Gagal membaca metadata hasil build.", http.StatusInternalServerError)
			return
		}
		expectedFilename, err := artifactFilename(artifact.BuildID, artifact.Variant)
		if err != nil || artifact.Filename != expectedFilename || filepath.Base(artifact.Filename) != artifact.Filename {
			http.Error(w, "Metadata nama hasil build tidak valid.", http.StatusInternalServerError)
			return
		}
		artifact.ProjectType = buildProjectType(artifact.Variant)
		info, err := os.Stat(s.artifactPath(artifact.BuildID, mode, artifact.Variant))
		if errors.Is(err, os.ErrNotExist) {
			artifacts = append(artifacts, artifact)
			continue
		}
		if err != nil {
			http.Error(w, "Gagal membaca berkas hasil build.", http.StatusInternalServerError)
			return
		}
		if !info.Mode().IsRegular() {
			http.Error(w, "Berkas hasil build tidak valid.", http.StatusInternalServerError)
			return
		}
		artifact.Size = info.Size()
		artifact.Available = true
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "Gagal membaca daftar hasil build.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, artifacts)
}

func (s *Server) deleteArtifact(w http.ResponseWriter, ownerID, buildID string) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	var filename, mode, variant string
	err := s.db.QueryRow(`SELECT artifact,access_mode,variant FROM builds
		WHERE id=? AND owner_id=? AND status='success'`, buildID, ownerID).Scan(&filename, &mode, &variant)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Hasil build tidak ditemukan.", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "Gagal membaca metadata hasil build.", http.StatusInternalServerError)
		return
	}
	if filename == "" {
		http.Error(w, "Hasil build tidak ditemukan.", http.StatusNotFound)
		return
	}
	expectedFilename, filenameErr := artifactFilename(buildID, variant)
	if filenameErr != nil || filename != expectedFilename || filepath.Base(filename) != filename {
		http.Error(w, "Metadata nama hasil build tidak valid.", http.StatusInternalServerError)
		return
	}
	path := s.artifactPath(buildID, mode, variant)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "Gagal menghapus hasil build.", http.StatusInternalServerError)
		return
	}
	result, err := s.db.Exec(`UPDATE builds SET artifact=NULL WHERE id=? AND owner_id=?
		AND status='success' AND artifact=?`, buildID, ownerID, filename)
	if err != nil {
		http.Error(w, "Hasil build dihapus, tetapi metadata gagal diperbarui.", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "Hasil build dihapus, tetapi metadata gagal diperbarui.", http.StatusInternalServerError)
		return
	}
	if mode == "public" {
		if _, err := s.db.Exec(`UPDATE builds SET updated_at=? WHERE id=?`,
			time.Now().UTC().Format(time.RFC3339), buildID); err != nil {
			http.Error(w, "Hasil build dihapus, tetapi aktivitas gagal diperbarui.", http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
