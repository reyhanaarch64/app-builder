package main

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type APKArtifact struct {
	BuildID   string `json:"buildId"`
	ProjectID string `json:"projectId"`
	Variant   string `json:"variant"`
	CreatedAt string `json:"createdAt"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	Available bool   `json:"available"`
}

func (s *Server) handleArtifactRoutes(w http.ResponseWriter, r *http.Request) {
	ownerID, err := s.ensureOwner(w, r)
	if err != nil {
		http.Error(w, "gagal membuat identitas browser", http.StatusInternalServerError)
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/artifacts/"), "/")
	if r.URL.Path == "/api/artifacts" || rest == "" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.deleteArtifact(w, ownerID, rest)
}

func (s *Server) listArtifacts(w http.ResponseWriter, ownerID string) {
	rows, err := s.db.Query(`SELECT id,project_id,variant,created_at,artifact FROM builds
		WHERE owner_id=? AND status='success' AND artifact IS NOT NULL AND artifact!=''
		ORDER BY created_at DESC,id DESC`, ownerID)
	if err != nil {
		http.Error(w, "gagal membaca daftar APK", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	artifacts := make([]APKArtifact, 0)
	for rows.Next() {
		var artifact APKArtifact
		if err := rows.Scan(&artifact.BuildID, &artifact.ProjectID, &artifact.Variant,
			&artifact.CreatedAt, &artifact.Filename); err != nil {
			http.Error(w, "gagal membaca metadata APK", http.StatusInternalServerError)
			return
		}
		if artifact.Filename != artifact.BuildID+".apk" || filepath.Base(artifact.Filename) != artifact.Filename {
			http.Error(w, "metadata nama APK tidak valid", http.StatusInternalServerError)
			return
		}
		info, err := os.Stat(filepath.Join(s.dataDir, "artifacts", artifact.Filename))
		if errors.Is(err, os.ErrNotExist) {
			artifacts = append(artifacts, artifact)
			continue
		}
		if err != nil {
			http.Error(w, "gagal membaca file APK", http.StatusInternalServerError)
			return
		}
		if !info.Mode().IsRegular() {
			http.Error(w, "file APK tidak valid", http.StatusInternalServerError)
			return
		}
		artifact.Size = info.Size()
		artifact.Available = true
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "gagal membaca daftar APK", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, artifacts)
}

func (s *Server) deleteArtifact(w http.ResponseWriter, ownerID, buildID string) {
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	var filename string
	err := s.db.QueryRow(`SELECT artifact FROM builds
		WHERE id=? AND owner_id=? AND status='success'`, buildID, ownerID).Scan(&filename)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "artifact tidak ditemukan", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "gagal membaca metadata APK", http.StatusInternalServerError)
		return
	}
	if filename == "" {
		http.Error(w, "artifact tidak ditemukan", http.StatusNotFound)
		return
	}
	if filename != buildID+".apk" || filepath.Base(filename) != filename {
		http.Error(w, "metadata nama APK tidak valid", http.StatusInternalServerError)
		return
	}
	path := filepath.Join(s.dataDir, "artifacts", filename)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		http.Error(w, "gagal menghapus APK", http.StatusInternalServerError)
		return
	}
	result, err := s.db.Exec(`UPDATE builds SET artifact=NULL WHERE id=? AND owner_id=?
		AND status='success' AND artifact=?`, buildID, ownerID, filename)
	if err != nil {
		http.Error(w, "APK dihapus, tetapi metadata gagal diperbarui", http.StatusInternalServerError)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		http.Error(w, "APK dihapus, tetapi metadata gagal diperbarui", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}
