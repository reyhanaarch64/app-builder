package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const maxGitHubArchiveBytes = int64(250 << 20)

var (
	gitHubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	gitHubRepoPattern  = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

type gitHubImportRequest struct {
	URL string `json:"url"`
}

func parseGitHubRepositoryURL(raw string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", errors.New("URL GitHub tidak valid")
	}
	if parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") ||
		parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" ||
		parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(parsed.EscapedPath(), "%") {
		return "", "", errors.New("Masukkan URL repositori publik, misalnya https://github.com/owner/repo.")
	}
	path := strings.Trim(parsed.Path, "/")
	if strings.Contains(path, "//") {
		return "", "", errors.New("URL repositori GitHub tidak valid.")
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || !gitHubOwnerPattern.MatchString(parts[0]) {
		return "", "", errors.New("URL repositori GitHub tidak valid.")
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	if !gitHubRepoPattern.MatchString(repo) || repo == "." || repo == ".." {
		return "", "", errors.New("URL repositori GitHub tidak valid.")
	}
	return parts[0], repo, nil
}

func (s *Server) handleGitHubImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Metode HTTP tidak diizinkan.", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var request gitHubImportRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Permintaan impor dari GitHub tidak valid.", http.StatusBadRequest)
		return
	}
	owner, repo, err := parseGitHubRepositoryURL(request.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	requestURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/zipball/HEAD", owner, repo)
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 1 || req.URL.Scheme != "https" ||
				!strings.EqualFold(req.URL.Host, "codeload.github.com") ||
				req.URL.User != nil || req.URL.Port() != "" {
				return errors.New("redirect archive GitHub tidak valid")
			}
			return nil
		},
	}
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodGet, requestURL, nil)
	if err != nil {
		http.Error(w, "Gagal menyiapkan unduhan dari GitHub.", http.StatusInternalServerError)
		return
	}
	upstreamRequest.Header.Set("Accept", "application/vnd.github+json")
	upstreamRequest.Header.Set("User-Agent", "android-web-ide")
	response, err := client.Do(upstreamRequest)
	if err != nil {
		http.Error(w, "Gagal mengunduh repositori dari GitHub.", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusUnauthorized ||
		response.StatusCode == http.StatusForbidden {
		http.Error(w, "Repositori tidak ditemukan atau tidak bersifat publik.", http.StatusNotFound)
		return
	}
	if response.StatusCode != http.StatusOK {
		http.Error(w, "GitHub gagal menyediakan arsip repositori.", http.StatusBadGateway)
		return
	}
	if response.ContentLength > maxGitHubArchiveBytes {
		http.Error(w, "Arsip repositori melebihi batas 250 MB.", http.StatusRequestEntityTooLarge)
		return
	}

	archive, err := os.CreateTemp("", "android-web-ide-github-*.zip")
	if err != nil {
		http.Error(w, "Gagal menyiapkan arsip sementara.", http.StatusInternalServerError)
		return
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	size, err := io.Copy(archive, io.LimitReader(response.Body, maxGitHubArchiveBytes+1))
	if err != nil {
		archive.Close()
		http.Error(w, "Gagal menyimpan arsip dari GitHub.", http.StatusBadGateway)
		return
	}
	if size > maxGitHubArchiveBytes {
		archive.Close()
		http.Error(w, "Arsip repositori melebihi batas 250 MB.", http.StatusRequestEntityTooLarge)
		return
	}
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		archive.Close()
		http.Error(w, "Gagal membaca arsip sementara.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.zip"`, repo))
	w.Header().Set("Content-Length", fmt.Sprint(size))
	if _, err := io.Copy(w, archive); err != nil {
		archive.Close()
		return
	}
	if err := archive.Close(); err != nil {
		return
	}
}

func (s *Server) handleActiveBuilds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Metode HTTP tidak diizinkan.", http.StatusMethodNotAllowed)
		return
	}
	ownerID, err := s.requestOwner(w, r)
	if err != nil {
		http.Error(w, "Gagal membuat identitas peramban.", http.StatusInternalServerError)
		return
	}
	if _, err := s.db.Exec(`UPDATE builds SET updated_at=? WHERE owner_id=? AND access_mode='public'
		AND status IN ('preparing','assembling','queued','building')`,
		time.Now().UTC().Format(time.RFC3339), ownerID); err != nil {
		http.Error(w, "Gagal memperbarui aktivitas build.", http.StatusInternalServerError)
		return
	}
	rows, err := s.db.Query(`SELECT id,project_id,status,variant,created_at,started_at,finished_at,error,artifact
		FROM builds WHERE owner_id=? AND status IN ('preparing','assembling','queued','building')
		ORDER BY created_at DESC,id DESC`, ownerID)
	if err != nil {
		http.Error(w, "Gagal membaca build aktif.", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	builds := make([]Build, 0)
	for rows.Next() {
		var build Build
		var started, finished, buildError, artifact sql.NullString
		if err := rows.Scan(&build.ID, &build.ProjectID, &build.Status, &build.Variant, &build.CreatedAt,
			&started, &finished, &buildError, &artifact); err != nil {
			http.Error(w, "Gagal membaca build aktif.", http.StatusInternalServerError)
			return
		}
		build.StartedAt, build.FinishedAt, build.Error, build.Artifact =
			started.String, finished.String, buildError.String, artifact.String
		build.ProjectType = buildProjectType(build.Variant)
		builds = append(builds, build)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "Gagal membaca build aktif.", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, builds)
}
