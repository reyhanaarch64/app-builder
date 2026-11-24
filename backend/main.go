package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/subtle"
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
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Build struct {
	ID         string `json:"id"`
	ProjectID  string `json:"projectId"`
	Status     string `json:"status"`
	Variant    string `json:"variant"`
	CreatedAt  string `json:"createdAt"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Error      string `json:"error,omitempty"`
	Artifact   string `json:"artifact,omitempty"`
}

type Server struct {
	db            *sql.DB
	dataDir       string
	hostDataDir   string
	cleanupToken  string
	buildSlots    chan struct{}
	cacheSizeFn   func() (int64, error)
	buildMu       sync.Mutex
	maintenanceMu sync.Mutex
	building      map[string]bool
}

func main() {
	dataDir := env("DATA_DIR", "/root/android-web-ide/data")
	hostDataDir := env("HOST_DATA_DIR", dataDir)
	webDir := env("WEB_DIR", "/root/android-web-ide/frontend/dist")
	port := env("PORT", "8866")
	if err := os.MkdirAll(filepath.Join(dataDir, "workspaces"), 0750); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "artifacts"), 0750); err != nil {
		log.Fatal(err)
	}
	cleanupToken, err := loadOrCreateCleanupToken(dataDir)
	if err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "ide.db"))
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	schema(db)

	s := &Server{
		db: db, dataDir: dataDir, hostDataDir: hostDataDir,
		cleanupToken: cleanupToken, buildSlots: make(chan struct{}, 1), building: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/builds", s.handleBuildCollection)
	mux.HandleFunc("/api/builds/", s.handleBuildRoutes)
	mux.HandleFunc("/api/maintenance/storage", s.handleStorage)
	mux.HandleFunc("/api/maintenance/cleanup", s.handleCleanup)
	mux.Handle("/", http.FileServer(http.Dir(webDir)))

	log.Printf("android web ide listening on http://0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, withCORS(mux)))
}

func schema(db *sql.DB) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS builds (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		variant TEXT NOT NULL,
		created_at TEXT NOT NULL,
		started_at TEXT,
		finished_at TEXT,
		error TEXT,
		artifact TEXT
	); CREATE TABLE IF NOT EXISTS build_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		build_id TEXT NOT NULL,
		sequence INTEGER NOT NULL,
		stream TEXT NOT NULL,
		message TEXT NOT NULL,
		created_at TEXT NOT NULL
	);`)
	if err != nil {
		log.Fatal(err)
	}
	var projectIDColumn int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('builds') WHERE name='project_id'`).Scan(&projectIDColumn); err != nil {
		log.Fatal(err)
	}
	if projectIDColumn == 0 {
		if _, err := db.Exec(`ALTER TABLE builds ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`); err != nil {
			log.Fatal(err)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS builds_one_active_per_project
		ON builds(project_id) WHERE project_id != '' AND status IN ('queued','building')`); err != nil {
		log.Fatal(err)
	}
}

func loadOrCreateCleanupToken(dataDir string) (string, error) {
	if token := os.Getenv("CLEANUP_TOKEN"); token != "" {
		return token, nil
	}
	path := filepath.Join(dataDir, "cleanup.token")
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tokenBytes)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return "", readErr
		}
		token = strings.TrimSpace(string(existing))
		if token == "" {
			return "", errors.New("cleanup token file kosong")
		}
	} else {
		if _, err := file.WriteString(token + "\n"); err != nil {
			file.Close()
			os.Remove(path)
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		return "", err
	}
	return token, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Cleanup-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "android-web-ide"})
}

type StorageStats struct {
	WorkspaceBytes   int64 `json:"workspaceBytes"`
	ArtifactBytes    int64 `json:"artifactBytes"`
	DatabaseBytes    int64 `json:"databaseBytes"`
	GradleCacheBytes int64 `json:"gradleCacheBytes"`
	BuildHistory     int64 `json:"buildHistory"`
	ActiveBuilds     int64 `json:"activeBuilds"`
}

type CleanupRequest struct {
	Workspaces  bool `json:"workspaces"`
	Artifacts   bool `json:"artifacts"`
	History     bool `json:"history"`
	GradleCache bool `json:"gradleCache"`
}

func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizedCleanupRequest(r) {
		http.Error(w, "cleanup token tidak valid", http.StatusUnauthorized)
		return
	}
	stats, err := s.storageStats()
	if err != nil {
		http.Error(w, "gagal membaca penggunaan storage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizedCleanupRequest(r) {
		http.Error(w, "cleanup token tidak valid", http.StatusUnauthorized)
		return
	}
	var request CleanupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil {
		http.Error(w, "request cleanup tidak valid", http.StatusBadRequest)
		return
	}
	if !request.Workspaces && !request.Artifacts && !request.History && !request.GradleCache {
		http.Error(w, "pilih setidaknya satu jenis data untuk dibersihkan", http.StatusBadRequest)
		return
	}

	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	var active int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE status IN ('queued','building')`).Scan(&active); err != nil {
		http.Error(w, "gagal memeriksa build aktif", http.StatusInternalServerError)
		return
	}
	s.buildMu.Lock()
	runners := len(s.building)
	s.buildMu.Unlock()
	if active > 0 || runners > 0 {
		http.Error(w, "cleanup ditunda sampai semua build selesai", http.StatusConflict)
		return
	}

	before, err := s.storageStats()
	if err != nil {
		http.Error(w, "gagal membaca penggunaan storage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if request.History {
		request.Workspaces, request.Artifacts = true, true
	}
	if request.Workspaces {
		if err := removeDirectoryContents(filepath.Join(s.dataDir, "workspaces")); err != nil {
			http.Error(w, "gagal menghapus workspace: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if request.Artifacts {
		if err := s.removeTerminalArtifacts(); err != nil {
			http.Error(w, "gagal menghapus APK: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if request.History {
		if _, err := s.db.Exec(`DELETE FROM build_logs`); err != nil {
			http.Error(w, "gagal menghapus log build", http.StatusInternalServerError)
			return
		}
		if _, err := s.db.Exec(`DELETE FROM builds`); err != nil {
			http.Error(w, "gagal menghapus riwayat build", http.StatusInternalServerError)
			return
		}
		if _, err := s.db.Exec(`VACUUM`); err != nil {
			http.Error(w, "gagal merapikan database: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if request.GradleCache {
		if err := clearGradleCache(); err != nil {
			http.Error(w, "gagal menghapus cache Gradle: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	after, err := s.storageStats()
	if err != nil {
		http.Error(w, "cleanup selesai, tetapi gagal membaca penggunaan storage: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"removed": map[string]int64{
			"workspaceBytes":   before.WorkspaceBytes - after.WorkspaceBytes,
			"artifactBytes":    before.ArtifactBytes - after.ArtifactBytes,
			"databaseBytes":    before.DatabaseBytes - after.DatabaseBytes,
			"gradleCacheBytes": before.GradleCacheBytes - after.GradleCacheBytes,
		},
		"storage": after,
	})
}

func (s *Server) authorizedCleanupRequest(r *http.Request) bool {
	provided := r.Header.Get("X-Cleanup-Token")
	return len(provided) == len(s.cleanupToken) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(s.cleanupToken)) == 1
}

func (s *Server) storageStats() (StorageStats, error) {
	var stats StorageStats
	var err error
	stats.WorkspaceBytes, err = directorySize(filepath.Join(s.dataDir, "workspaces"))
	if err != nil {
		return stats, err
	}
	stats.ArtifactBytes, err = directorySize(filepath.Join(s.dataDir, "artifacts"))
	if err != nil {
		return stats, err
	}
	for _, path := range []string{
		filepath.Join(s.dataDir, "ide.db"),
		filepath.Join(s.dataDir, "ide.db-wal"),
		filepath.Join(s.dataDir, "ide.db-shm"),
	} {
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			stats.DatabaseBytes += info.Size()
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return stats, statErr
		}
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM builds`).Scan(&stats.BuildHistory); err != nil {
		return stats, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE status IN ('queued','building')`).Scan(&stats.ActiveBuilds); err != nil {
		return stats, err
	}
	cacheSizeFn := s.cacheSizeFn
	if cacheSizeFn == nil {
		cacheSizeFn = gradleCacheSize
	}
	stats.GradleCacheBytes, err = cacheSizeFn()
	return stats, err
}

func directorySize(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func removeDirectoryContents(root string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) removeTerminalArtifacts() error {
	rows, err := s.db.Query(`SELECT id,artifact FROM builds WHERE status NOT IN ('queued','building') AND artifact IS NOT NULL`)
	if err != nil {
		return err
	}
	type artifactRecord struct{ id, filename string }
	var artifacts []artifactRecord
	for rows.Next() {
		var item artifactRecord
		if err := rows.Scan(&item.id, &item.filename); err != nil {
			rows.Close()
			return err
		}
		artifacts = append(artifacts, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range artifacts {
		if item.filename != item.id+".apk" || filepath.Base(item.filename) != item.filename {
			return fmt.Errorf("nama artifact build %s tidak valid", item.id)
		}
		if err := os.Remove(filepath.Join(s.dataDir, "artifacts", item.filename)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := s.db.Exec(`UPDATE builds SET artifact=NULL WHERE id=?`, item.id); err != nil {
			return err
		}
	}
	return removeDirectoryContents(filepath.Join(s.dataDir, "artifacts"))
}

func gradleCacheSize() (int64, error) {
	found, err := gradleCacheExists()
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	out, err := exec.Command("docker", "run", "--rm", "-v", "androidide_gradle:/cache", "--entrypoint", "du",
		"android-web-ide-builder", "-sb", "/cache").Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, errors.New("output ukuran cache Gradle kosong")
	}
	var size int64
	if _, err := fmt.Sscan(fields[0], &size); err != nil {
		return 0, err
	}
	return size, nil
}

func clearGradleCache() error {
	found, err := gradleCacheExists()
	if err != nil || !found {
		return err
	}
	out, err := exec.Command("docker", "volume", "rm", "androidide_gradle").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func gradleCacheExists() (bool, error) {
	out, err := exec.Command("docker", "volume", "ls", "--format", "{{.Name}}", "--filter", "name=androidide_gradle").Output()
	if err != nil {
		return false, err
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name == "androidide_gradle" {
			return true, nil
		}
	}
	return false, nil
}

func (s *Server) handleBuildCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleBuildList(w, r)
	case http.MethodPost:
		s.handleBuildCreate(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleBuildList(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("projectId")
	if !validProjectID(projectID) {
		http.Error(w, "projectId tidak valid", http.StatusBadRequest)
		return
	}
	rows, err := s.db.Query(`SELECT id,project_id,status,variant,created_at,started_at,finished_at,error,artifact
		FROM builds WHERE project_id=? ORDER BY created_at DESC,id DESC LIMIT 20`, projectID)
	if err != nil {
		http.Error(w, "gagal membaca riwayat build", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	builds := make([]Build, 0)
	for rows.Next() {
		var b Build
		var started, finished, errMsg, artifact sql.NullString
		if err := rows.Scan(&b.ID, &b.ProjectID, &b.Status, &b.Variant, &b.CreatedAt, &started, &finished, &errMsg, &artifact); err != nil {
			http.Error(w, "gagal membaca riwayat build", http.StatusInternalServerError)
			return
		}
		b.StartedAt, b.FinishedAt, b.Error, b.Artifact = started.String, finished.String, errMsg.String, artifact.String
		builds = append(builds, b)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "gagal membaca riwayat build", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(builds)
}

func (s *Server) handleBuildCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "ZIP project terlalu besar (maks 50 MB)", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "multipart upload gagal", http.StatusBadRequest)
		}
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	projectID := r.FormValue("projectId")
	if !validProjectID(projectID) {
		http.Error(w, "projectId wajib berupa ID project yang valid", http.StatusBadRequest)
		return
	}
	s.maintenanceMu.Lock()
	defer s.maintenanceMu.Unlock()
	if b, err := s.getActiveBuild(projectID); err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": "project ini masih punya build aktif", "build": b})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "gagal memeriksa build aktif", http.StatusInternalServerError)
		return
	}
	file, header, err := r.FormFile("project")
	if err != nil {
		http.Error(w, "field project wajib berupa ZIP", 400)
		return
	}
	defer file.Close()
	if !strings.HasSuffix(strings.ToLower(header.Filename), ".zip") {
		http.Error(w, "file harus berformat ZIP", 400)
		return
	}

	id := fmt.Sprintf("bld_%d", time.Now().UnixNano())
	workspace := filepath.Join(s.dataDir, "workspaces", id)
	if err := os.MkdirAll(workspace, 0750); err != nil {
		http.Error(w, "gagal membuat workspace", 500)
		return
	}
	zipPath := filepath.Join(workspace, "project.zip")
	dst, err := os.Create(zipPath)
	if err != nil {
		http.Error(w, "gagal menyimpan ZIP", 500)
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		http.Error(w, "gagal menerima ZIP", 500)
		return
	}
	dst.Close()
	if err := extractZip(zipPath, workspace); err != nil {
		os.RemoveAll(workspace)
		http.Error(w, "ZIP tidak valid: "+err.Error(), 400)
		return
	}
	projectRoot, err := findProjectRoot(workspace)
	if err != nil {
		os.RemoveAll(workspace)
		http.Error(w, "struktur project tidak valid: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateAndroidProject(projectRoot); err != nil {
		os.RemoveAll(workspace)
		http.Error(w, "project tidak valid: "+err.Error(), http.StatusBadRequest)
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := s.db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at) VALUES (?,?,?,?,?)`,
		id, projectID, "queued", "debug", now); err != nil {
		os.RemoveAll(workspace)
		if b, activeErr := s.getActiveBuild(projectID); activeErr == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": "project ini masih punya build aktif", "build": b})
			return
		}
		http.Error(w, "gagal membuat job build", 500)
		return
	}
	s.addLog(id, "system", "Build job dibuat")
	go s.runBuild(id, workspace)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(Build{ID: id, ProjectID: projectID, Status: "queued", Variant: "debug", CreatedAt: now})
}

var projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validProjectID(id string) bool { return projectIDPattern.MatchString(id) }

func (s *Server) getActiveBuild(projectID string) (Build, error) {
	var b Build
	var started, finished, errMsg, artifact sql.NullString
	err := s.db.QueryRow(`SELECT id,project_id,status,variant,created_at,started_at,finished_at,error,artifact
		FROM builds WHERE project_id=? AND status IN ('queued','building') ORDER BY created_at DESC,id DESC LIMIT 1`, projectID).
		Scan(&b.ID, &b.ProjectID, &b.Status, &b.Variant, &b.CreatedAt, &started, &finished, &errMsg, &artifact)
	if err != nil {
		return b, err
	}
	b.StartedAt, b.FinishedAt, b.Error, b.Artifact = started.String, finished.String, errMsg.String, artifact.String
	return b, nil
}

func (s *Server) handleBuildRoutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/builds/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "build ID required", 400)
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method == http.MethodDelete {
			s.db.Exec(`UPDATE builds SET status='canceled',finished_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), id)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		build, err := s.getBuild(id)
		if err != nil {
			http.Error(w, "build tidak ditemukan", 404)
			return
		}
		json.NewEncoder(w).Encode(build)
	case "logs":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		if _, err := s.getBuild(id); err != nil {
			http.Error(w, "build tidak ditemukan", http.StatusNotFound)
			return
		}
		s.streamLogs(w, r, id)
	case "artifact":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		build, err := s.getBuild(id)
		if err != nil || build.Artifact == "" {
			http.Error(w, "artifact belum tersedia", 404)
			return
		}
		http.ServeFile(w, r, filepath.Join(s.dataDir, "artifacts", build.Artifact))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) getBuild(id string) (Build, error) {
	var b Build
	var started, finished, errMsg, artifact sql.NullString
	err := s.db.QueryRow(`SELECT id,project_id,status,variant,created_at,started_at,finished_at,error,artifact FROM builds WHERE id=?`, id).
		Scan(&b.ID, &b.ProjectID, &b.Status, &b.Variant, &b.CreatedAt, &started, &finished, &errMsg, &artifact)
	if err != nil {
		return b, err
	}
	b.StartedAt, b.FinishedAt, b.Error, b.Artifact = started.String, finished.String, errMsg.String, artifact.String
	return b, nil
}

func (s *Server) addLog(id, stream, message string) {
	s.db.Exec(`INSERT INTO build_logs(build_id,sequence,stream,message,created_at)
		SELECT ?,COALESCE(MAX(sequence),0)+1,?,?,? FROM build_logs WHERE build_id=?`,
		id, stream, strings.TrimSuffix(message, "\n"), time.Now().UTC().Format(time.RFC3339), id)
}

func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE unsupported", 500)
		return
	}
	last := 0
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		rows, err := s.db.Query(`SELECT sequence,stream,message,created_at FROM build_logs WHERE build_id=? AND sequence>? ORDER BY sequence`, id, last)
		if err == nil {
			for rows.Next() {
				var seq int
				var stream, msg, created string
				rows.Scan(&seq, &stream, &msg, &created)
				payload, _ := json.Marshal(map[string]any{"sequence": seq, "stream": stream, "message": msg, "timestamp": created})
				fmt.Fprintf(w, "data: %s\n\n", payload)
				flusher.Flush()
				last = seq
			}
			rows.Close()
		}
		build, _ := s.getBuild(id)
		if build.Status == "success" || build.Status == "failed" || build.Status == "canceled" {
			fmt.Fprintf(w, "event: end\ndata: %s\n\n", build.Status)
			flusher.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func extractZip(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	destAbs, _ := filepath.Abs(dest)
	var totalUncompressed uint64
	if len(r.File) > 20000 {
		return errors.New("jumlah entry ZIP melebihi batas 20.000")
	}
	for _, f := range r.File {
		if strings.Contains(f.Name, `\`) || filepath.IsAbs(f.Name) {
			return errors.New("path entry ZIP tidak valid")
		}
		clean := filepath.Clean(filepath.FromSlash(f.Name))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return errors.New("path traversal terdeteksi")
		}
		if f.UncompressedSize64 > 50<<20 {
			return errors.New("ukuran satu file ZIP melebihi 50 MB")
		}
		totalUncompressed += f.UncompressedSize64
		if totalUncompressed > 250<<20 {
			return errors.New("total ukuran project setelah diekstrak melebihi 250 MB")
		}
		target := filepath.Join(destAbs, clean)
		if !strings.HasPrefix(filepath.Clean(target), destAbs+string(filepath.Separator)) {
			return errors.New("path traversal terdeteksi")
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0750); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
		if err == nil {
			_, err = io.Copy(out, rc)
		}
		rc.Close()
		if out != nil {
			out.Close()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func validateAndroidProject(root string) error {
	if !fileExists(filepath.Join(root, "settings.gradle")) && !fileExists(filepath.Join(root, "settings.gradle.kts")) {
		return errors.New("settings.gradle atau settings.gradle.kts tidak ditemukan")
	}
	if !fileExists(filepath.Join(root, "app", "build.gradle")) && !fileExists(filepath.Join(root, "app", "build.gradle.kts")) {
		return errors.New("module app/build.gradle atau app/build.gradle.kts tidak ditemukan; MVP hanya mendukung project single-module :app")
	}
	if !fileExists(filepath.Join(root, "app", "src", "main", "AndroidManifest.xml")) {
		return errors.New("app/src/main/AndroidManifest.xml tidak ditemukan")
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (s *Server) runBuild(id, workspace string) {
	defer func() {
		if err := os.RemoveAll(workspace); err != nil {
			log.Printf("failed to remove build workspace %s: %v", id, err)
		}
	}()
	s.buildMu.Lock()
	if s.building[id] {
		s.buildMu.Unlock()
		return
	}
	s.building[id] = true
	s.buildMu.Unlock()
	defer func() { s.buildMu.Lock(); delete(s.building, id); s.buildMu.Unlock() }()

	s.buildSlots <- struct{}{}
	defer func() { <-s.buildSlots }()

	now := time.Now().UTC().Format(time.RFC3339)
	s.db.Exec(`UPDATE builds SET status='building',started_at=? WHERE id=?`, now, id)
	s.addLog(id, "system", "Menjalankan Docker build container")

	projectRoot := workspace
	if root, err := findProjectRoot(workspace); err == nil {
		projectRoot = root
	} else {
		s.failBuild(id, err.Error())
		return
	}
	relativeRoot, err := filepath.Rel(s.dataDir, projectRoot)
	if err != nil || relativeRoot == ".." || strings.HasPrefix(relativeRoot, ".."+string(filepath.Separator)) {
		s.failBuild(id, "workspace build berada di luar DATA_DIR")
		return
	}
	hostProjectRoot := filepath.Join(s.hostDataDir, relativeRoot)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--memory=4g", "--cpus=2", "--pids-limit=256",
		"-v", hostProjectRoot+":/project",
		"-v", "androidide_gradle:/root/.gradle",
		"android-web-ide-builder",
		"/bin/bash", "-lc", "if [ -x ./gradlew ]; then ./gradlew --no-daemon assembleDebug; elif [ -f ./gradlew ]; then chmod +x ./gradlew && ./gradlew --no-daemon assembleDebug; else gradle --no-daemon assembleDebug; fi")
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		s.failBuild(id, "gagal menjalankan Docker: "+err.Error())
		return
	}
	go s.copyLogs(id, "stdout", stdout)
	go s.copyLogs(id, "stderr", stderr)
	err = cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		s.failBuild(id, "build timeout 30 menit")
		return
	}
	if err != nil {
		s.failBuild(id, "Gradle build gagal")
		return
	}

	apk, err := findAPK(projectRoot)
	if err != nil {
		s.failBuild(id, err.Error())
		return
	}
	filename := id + ".apk"
	artifact := filepath.Join(s.dataDir, "artifacts", filename)
	input, _ := os.Open(apk)
	output, err := os.Create(artifact)
	if err != nil {
		s.failBuild(id, "gagal menyimpan APK")
		return
	}
	io.Copy(output, input)
	input.Close()
	output.Close()
	s.addLog(id, "system", "APK berhasil dibuild")
	s.db.Exec(`UPDATE builds SET status='success',finished_at=?,artifact=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), filename, id)
}

func (s *Server) copyLogs(id, stream string, r io.ReadCloser) {
	buf := make([]byte, 4096)
	var line strings.Builder
	for {
		n, err := r.Read(buf)
		if n > 0 {
			line.Write(buf[:n])
			for {
				text := line.String()
				i := strings.IndexByte(text, '\n')
				if i < 0 {
					break
				}
				s.addLog(id, stream, text[:i])
				line.Reset()
				line.WriteString(text[i+1:])
			}
		}
		if err != nil {
			break
		}
	}
	if text := strings.TrimSpace(line.String()); text != "" {
		s.addLog(id, stream, text)
	}
	r.Close()
}

func (s *Server) failBuild(id, message string) {
	s.addLog(id, "system", "Build gagal: "+message)
	s.db.Exec(`UPDATE builds SET status='failed',finished_at=?,error=? WHERE id=?`,
		time.Now().UTC().Format(time.RFC3339), message, id)
}

func findProjectRoot(workspace string) (string, error) {
	for _, candidate := range []string{workspace, filepath.Join(workspace, "app"), filepath.Join(workspace, "project")} {
		if _, err := os.Stat(filepath.Join(candidate, "settings.gradle")); err == nil {
			return candidate, nil
		}
		if _, err := os.Stat(filepath.Join(candidate, "settings.gradle.kts")); err == nil {
			return candidate, nil
		}
	}
	var found string
	filepath.Walk(workspace, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() &&
			(info.Name() == "settings.gradle" || info.Name() == "settings.gradle.kts") {
			found = filepath.Dir(path)
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("project tidak memiliki settings.gradle atau settings.gradle.kts")
	}
	return found, nil
}

func findAPK(root string) (string, error) {
	var apk string
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.HasSuffix(strings.ToLower(info.Name()), ".apk") &&
			strings.Contains(filepath.ToSlash(path), "/build/outputs/apk/") {
			apk = path
		}
		return nil
	})
	if apk == "" {
		return "", fmt.Errorf("APK tidak ditemukan setelah Gradle build")
	}
	return apk, nil
}
