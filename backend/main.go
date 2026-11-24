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
	"net"
	"net/http"
	"net/url"
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

type BuildConfig struct {
	MaxUploadMB    int `json:"maxUploadMB"`
	TimeoutMinutes int `json:"timeoutMinutes"`
	MemoryMB       int `json:"memoryMB"`
	CPUs           int `json:"cpus"`
	Parallel       int `json:"parallel"`
}

type SMTPConfig struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	From     string `json:"from"`
}

type AppConfig struct {
	Mode            string      `json:"mode"`
	Password        string      `json:"password"`
	Port            int         `json:"port"`
	SessionTTLHours int         `json:"sessionTTLHours"`
	SecureCookie    bool        `json:"secureCookie"`
	Build           BuildConfig `json:"build"`
	SMTP            SMTPConfig  `json:"smtp"`
	VAPIDSubject    string      `json:"vapidSubject"`
}

type session struct {
	expiresAt time.Time
}

type loginAttempts struct {
	times []time.Time
}

type Server struct {
	db            *sql.DB
	dataDir       string
	hostDataDir   string
	config        AppConfig
	buildSlots    chan struct{}
	cacheSizeFn   func() (int64, error)
	buildMu       sync.Mutex
	maintenanceMu sync.Mutex
	building      map[string]bool
	buildCancels  map[string]context.CancelFunc
	startBuildFn  func(string, string)
	ownerMu       sync.Mutex
	sessionMu     sync.Mutex
	sessions      map[string]session
	loginMu       sync.Mutex
	loginFailures map[string]loginAttempts
}

func main() {
	dataDir := env("DATA_DIR", "/root/android-web-ide/data")
	hostDataDir := env("HOST_DATA_DIR", dataDir)
	webDir := env("WEB_DIR", "/root/android-web-ide/frontend/dist")
	configPath := env("IDE_CONFIG", filepath.Join(dataDir, "IDE.config"))
	if err := os.MkdirAll(filepath.Join(dataDir, "workspaces"), 0750); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "artifacts"), 0750); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "blobs"), 0700); err != nil {
		log.Fatal(err)
	}
	config, err := loadConfig(configPath)
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
		db: db, dataDir: dataDir, hostDataDir: hostDataDir, config: config,
		buildSlots: make(chan struct{}, config.Build.Parallel), building: map[string]bool{},
		sessions: make(map[string]session), loginFailures: make(map[string]loginAttempts),
	}
	go s.runBlobGC()
	go s.runPushOutbox()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/auth/status", s.handleAuthStatus)
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)
	mux.HandleFunc("/api/notifications/settings", s.handleNotificationSettings)
	mux.HandleFunc("/api/notifications/test", s.handleNotificationTest)
	mux.HandleFunc("/api/notifications/push/key", s.handlePushVAPIDKey)
	mux.HandleFunc("/api/notifications/push/subscription", s.handlePushSubscription)
	mux.HandleFunc("/api/notifications/push/test", s.handlePushTest)
	mux.HandleFunc("/api/artifacts", s.handleArtifactRoutes)
	mux.HandleFunc("/api/artifacts/", s.handleArtifactRoutes)
	mux.HandleFunc("/api/builds", s.handleBuildCollection)
	mux.HandleFunc("/api/builds/prepare", s.handleBuildPrepare)
	mux.HandleFunc("/api/builds/", s.handleBuildRoutes)
	mux.HandleFunc("/api/maintenance/storage", s.handleStorage)
	mux.HandleFunc("/api/maintenance/cleanup", s.handleCleanup)
	mux.Handle("/", http.FileServer(http.Dir(webDir)))

	go s.runEmailOutbox()
	log.Printf("android web ide mode=%s listening on http://0.0.0.0:%d", config.Mode, config.Port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", config.Port), s.authMiddleware(mux)))
}

func schema(db *sql.DB) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS builds (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL DEFAULT '',
		owner_id TEXT NOT NULL DEFAULT '',
		idempotency_key TEXT NOT NULL DEFAULT '',
		manifest_hash TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '',
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
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS notification_settings (
		id INTEGER PRIMARY KEY CHECK(id=1),
		recipient TEXT NOT NULL DEFAULT '',
		notify_success INTEGER NOT NULL DEFAULT 1,
		notify_failure INTEGER NOT NULL DEFAULT 1,
		updated_at TEXT NOT NULL
	); INSERT OR IGNORE INTO notification_settings(id,recipient,notify_success,notify_failure,updated_at)
		VALUES(1,'',1,1,''); CREATE TABLE IF NOT EXISTS email_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		build_id TEXT NOT NULL,
		event TEXT NOT NULL,
		recipient TEXT NOT NULL,
		subject TEXT NOT NULL,
		body TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at TEXT NOT NULL,
		last_error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		sent_at TEXT NOT NULL DEFAULT '',
		UNIQUE(build_id,event)
	); CREATE INDEX IF NOT EXISTS email_outbox_due ON email_outbox(status,next_attempt_at,id);
	CREATE TABLE IF NOT EXISTS push_subscriptions (
		endpoint TEXT PRIMARY KEY,
		owner_id TEXT NOT NULL,
		subscription TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	); CREATE INDEX IF NOT EXISTS push_subscriptions_owner ON push_subscriptions(owner_id);
	CREATE TABLE IF NOT EXISTS push_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		build_id TEXT NOT NULL,
		event TEXT NOT NULL,
		endpoint TEXT NOT NULL,
		subscription TEXT NOT NULL,
		payload TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at TEXT NOT NULL,
		last_error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		sent_at TEXT NOT NULL DEFAULT '',
		UNIQUE(build_id,event,endpoint)
	); CREATE INDEX IF NOT EXISTS push_outbox_due ON push_outbox(status,next_attempt_at,id);
	CREATE TABLE IF NOT EXISTS webpush_keys (
		id INTEGER PRIMARY KEY CHECK(id=1),
		public_key TEXT NOT NULL,
		private_key TEXT NOT NULL
	)`); err != nil {
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
	for column, definition := range map[string]string{
		"owner_id":        "TEXT NOT NULL DEFAULT ''",
		"idempotency_key": "TEXT NOT NULL DEFAULT ''",
		"manifest_hash":   "TEXT NOT NULL DEFAULT ''",
		"updated_at":      "TEXT NOT NULL DEFAULT ''",
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('builds') WHERE name=?`, column).Scan(&count); err != nil {
			log.Fatal(err)
		}
		if count == 0 {
			if _, err := db.Exec(`ALTER TABLE builds ADD COLUMN ` + column + ` ` + definition); err != nil {
				log.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS owners (
		id TEXT PRIMARY KEY,
		created_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL
	); CREATE TABLE IF NOT EXISTS blobs (
		owner_id TEXT NOT NULL,
		sha256 TEXT NOT NULL,
		size INTEGER NOT NULL,
		ref_count INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		PRIMARY KEY(owner_id,sha256)
	); CREATE TABLE IF NOT EXISTS build_manifests (
		build_id TEXT PRIMARY KEY,
		manifest TEXT NOT NULL,
		expires_at TEXT NOT NULL
	); CREATE TABLE IF NOT EXISTS blob_reservations (
		build_id TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		sha256 TEXT NOT NULL,
		expires_at TEXT NOT NULL,
		PRIMARY KEY(build_id,sha256)
	); CREATE TABLE IF NOT EXISTS build_blob_refs (
		build_id TEXT NOT NULL,
		owner_id TEXT NOT NULL,
		sha256 TEXT NOT NULL,
		counted INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(build_id,sha256)
	);`); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS builds_one_active_per_project`); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS builds_one_active_per_owner_project
		ON builds(owner_id,project_id) WHERE project_id != '' AND status IN ('preparing','assembling','queued','building')`); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS builds_one_request_per_owner
		ON builds(owner_id,idempotency_key) WHERE idempotency_key != ''`); err != nil {
		log.Fatal(err)
	}
}

func defaultConfig() AppConfig {
	return AppConfig{
		Mode: "public", Port: 8866, SessionTTLHours: 168, SecureCookie: true,
		Build: BuildConfig{MaxUploadMB: 50, TimeoutMinutes: 30, MemoryMB: 4096, CPUs: 2, Parallel: 1},
		SMTP:  SMTPConfig{Port: 587}, VAPIDSubject: vapidSubscriber,
	}
}

func loadConfig(path string) (AppConfig, error) {
	config := defaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data, err = json.MarshalIndent(config, "", "  ")
		if err != nil {
			return config, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			return config, err
		}
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			return config, err
		}
		log.Printf("created default public config at %s", path)
		return config, nil
	}
	if err != nil {
		return config, err
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("invalid IDE.config: %w", err)
	}
	if config.Mode != "public" && config.Mode != "private" {
		return config, errors.New(`IDE.config "mode" must be "public" or "private"`)
	}
	if config.Port < 1 || config.Port > 65535 {
		return config, errors.New("IDE.config port must be between 1 and 65535")
	}
	if config.SessionTTLHours < 1 || config.SessionTTLHours > 8760 {
		return config, errors.New("IDE.config sessionTTLHours must be between 1 and 8760")
	}
	if config.Mode == "private" && len([]rune(config.Password)) < 8 {
		return config, errors.New("private mode requires a password with at least 8 characters")
	}
	if config.Build.MaxUploadMB < 1 || config.Build.MaxUploadMB > 1024 ||
		config.Build.TimeoutMinutes < 1 || config.Build.TimeoutMinutes > 240 ||
		config.Build.MemoryMB < 512 || config.Build.MemoryMB > 65536 ||
		config.Build.CPUs < 1 || config.Build.CPUs > 64 ||
		config.Build.Parallel < 1 || config.Build.Parallel > 16 {
		return config, errors.New("IDE.config build limits are outside supported ranges")
	}
	if config.SMTP.Port < 0 || config.SMTP.Port > 65535 {
		return config, errors.New("IDE.config smtp port must be 0 or between 1 and 65535")
	}
	if config.VAPIDSubject == "" {
		config.VAPIDSubject = vapidSubscriber
	}
	subject, err := url.Parse(config.VAPIDSubject)
	if err != nil || strings.ContainsAny(config.VAPIDSubject, "\r\n") ||
		(subject.Scheme != "mailto" && subject.Scheme != "https") ||
		(subject.Scheme == "mailto" && subject.Opaque == "") ||
		(subject.Scheme == "https" && subject.Hostname() == "") {
		return config, errors.New("IDE.config vapidSubject must be a mailto: or HTTPS URL")
	}
	if err := os.Chmod(path, 0600); err != nil {
		return config, err
	}
	return config, nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "service": "android-web-ide"})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isAPI := strings.HasPrefix(r.URL.Path, "/api/")
		if isAPI {
			w.Header().Set("Cache-Control", "no-store")
		}
		if s.config.Mode == "private" && isAPI &&
			r.URL.Path != "/api/auth/status" && r.URL.Path != "/api/auth/login" &&
			r.URL.Path != "/api/auth/logout" && r.URL.Path != "/api/health" &&
			!s.hasValidSession(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.ensureOwner(w, r); err != nil {
		http.Error(w, "gagal membuat identitas browser", http.StatusInternalServerError)
		return
	}
	authenticated := s.config.Mode == "public" || s.hasValidSession(r)
	writeJSON(w, http.StatusOK, map[string]any{"mode": s.config.Mode, "authenticated": authenticated})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.config.Mode != "private" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "private mode is disabled"})
		return
	}
	ip := clientIP(r)
	if s.isLoginRateLimited(ip) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "terlalu banyak percobaan login; coba lagi dalam 15 menit"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		s.recordLoginFailure(ip)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "request login tidak valid"})
		return
	}
	if len(body.Password) != len(s.config.Password) ||
		subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.config.Password)) != 1 {
		s.recordLoginFailure(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "password salah"})
		return
	}
	s.clearLoginFailures(ip)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "gagal membuat session"})
		return
	}
	token := hex.EncodeToString(tokenBytes)
	expiresAt := time.Now().Add(time.Duration(s.config.SessionTTLHours) * time.Hour)
	s.sessionMu.Lock()
	s.sessions[token] = session{expiresAt: expiresAt}
	s.sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "ide_session", Value: token, Path: "/", Expires: expiresAt,
		MaxAge: int(time.Until(expiresAt).Seconds()), HttpOnly: true,
		Secure: s.config.SecureCookie, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie("ide_session"); err == nil {
		s.sessionMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessionMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: "ide_session", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.config.SecureCookie, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

func (s *Server) hasValidSession(r *http.Request) bool {
	cookie, err := r.Cookie("ide_session")
	if err != nil {
		return false
	}
	now := time.Now()
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	for token, session := range s.sessions {
		if !now.Before(session.expiresAt) {
			delete(s.sessions, token)
		}
	}
	session, ok := s.sessions[cookie.Value]
	return ok && now.Before(session.expiresAt)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) isLoginRateLimited(ip string) bool {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	now := time.Now()
	attempts := s.loginFailures[ip]
	recent := attempts.times[:0]
	for _, attempt := range attempts.times {
		if now.Sub(attempt) < 15*time.Minute {
			recent = append(recent, attempt)
		}
	}
	attempts.times = recent
	s.loginFailures[ip] = attempts
	return len(recent) >= 5
}

func (s *Server) recordLoginFailure(ip string) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	attempts := s.loginFailures[ip]
	attempts.times = append(attempts.times, time.Now())
	s.loginFailures[ip] = attempts
}

func (s *Server) clearLoginFailures(ip string) {
	s.loginMu.Lock()
	delete(s.loginFailures, ip)
	s.loginMu.Unlock()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

type StorageStats struct {
	WorkspaceBytes   int64 `json:"workspaceBytes"`
	ArtifactBytes    int64 `json:"artifactBytes"`
	BlobBytes        int64 `json:"blobBytes"`
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
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE status IN ('preparing','assembling','queued','building')`).Scan(&active); err != nil {
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
		if err := s.releaseAllBuildBlobRefs(); err != nil {
			http.Error(w, "gagal melepas referensi binary cache: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.collectExpiredBlobReservations(); err != nil {
			http.Error(w, "gagal membersihkan binary cache: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if _, err := s.db.Exec(`DELETE FROM build_manifests`); err != nil {
			http.Error(w, "gagal menghapus manifest build", http.StatusInternalServerError)
			return
		}
		if _, err := s.db.Exec(`DELETE FROM blob_reservations`); err != nil {
			http.Error(w, "gagal menghapus reservation binary", http.StatusInternalServerError)
			return
		}
		if err := removeDirectoryContents(filepath.Join(s.dataDir, "blobs", "sha256")); err != nil {
			http.Error(w, "gagal menghapus blob cache: "+err.Error(), http.StatusInternalServerError)
			return
		}
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
			"blobBytes":        before.BlobBytes - after.BlobBytes,
			"databaseBytes":    before.DatabaseBytes - after.DatabaseBytes,
			"gradleCacheBytes": before.GradleCacheBytes - after.GradleCacheBytes,
		},
		"storage": after,
	})
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
	stats.BlobBytes, err = directorySize(filepath.Join(s.dataDir, "blobs", "sha256"))
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
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM builds WHERE status IN ('preparing','assembling','queued','building')`).Scan(&stats.ActiveBuilds); err != nil {
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
		androidBuilderImage(), "-sb", "/cache").Output()
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
	ownerID, err := s.ensureOwner(w, r)
	if err != nil {
		http.Error(w, "gagal membuat identitas browser", http.StatusInternalServerError)
		return
	}
	rows, err := s.db.Query(`SELECT id,project_id,status,variant,created_at,started_at,finished_at,error,artifact
		FROM builds WHERE owner_id=? AND project_id=? ORDER BY created_at DESC,id DESC LIMIT 20`, ownerID, projectID)
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
	maxUploadBytes := int64(s.config.Build.MaxUploadMB) << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, fmt.Sprintf("ZIP project terlalu besar (maks %d MB)", s.config.Build.MaxUploadMB), http.StatusRequestEntityTooLarge)
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
	ownerID, err := s.ensureOwner(w, r)
	if err != nil {
		http.Error(w, "gagal membuat identitas browser", 500)
		return
	}
	if b, err := s.getActiveBuild(ownerID, projectID); err == nil {
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
	if _, err := s.db.Exec(`INSERT INTO builds (id,project_id,owner_id,status,variant,created_at,updated_at) VALUES (?,?,?,?,?,?,?)`,
		id, projectID, ownerID, "queued", "debug", now, now); err != nil {
		os.RemoveAll(workspace)
		if b, activeErr := s.getActiveBuild(ownerID, projectID); activeErr == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": "project ini masih punya build aktif", "build": b})
			return
		}
		http.Error(w, "gagal membuat job build", 500)
		return
	}
	s.addLog(id, "system", "Build job dibuat")
	s.startBuild(id, workspace)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(Build{ID: id, ProjectID: projectID, Status: "queued", Variant: "debug", CreatedAt: now})
}

func (s *Server) startBuild(id, workspace string) {
	if s.startBuildFn != nil {
		s.startBuildFn(id, workspace)
		return
	}
	go s.runBuild(id, workspace)
}

var projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validProjectID(id string) bool { return projectIDPattern.MatchString(id) }

func (s *Server) getActiveBuild(ownerID, projectID string) (Build, error) {
	var b Build
	var started, finished, errMsg, artifact sql.NullString
	err := s.db.QueryRow(`SELECT id,project_id,status,variant,created_at,started_at,finished_at,error,artifact
		FROM builds WHERE owner_id=? AND project_id=? AND status IN ('preparing','assembling','queued','building') ORDER BY created_at DESC,id DESC LIMIT 1`, ownerID, projectID).
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
	ownerID, err := s.ensureOwner(w, r)
	if err != nil {
		http.Error(w, "gagal membuat identitas browser", http.StatusInternalServerError)
		return
	}
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch action {
	case "blobs":
		if len(parts) != 3 {
			http.Error(w, "hash binary wajib diisi", http.StatusBadRequest)
			return
		}
		s.handleBuildBlobUpload(w, r, ownerID, id, parts[2])
	case "finalize":
		s.handleBuildFinalize(w, r, ownerID, id)
	case "":
		if r.Method == http.MethodDelete {
			s.maintenanceMu.Lock()
			build, err := s.getBuildForOwner(ownerID, id)
			if err != nil {
				s.maintenanceMu.Unlock()
				http.NotFound(w, r)
				return
			}
			if build.Status == "canceled" {
				s.maintenanceMu.Unlock()
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if build.Status != "preparing" && build.Status != "assembling" &&
				build.Status != "queued" && build.Status != "building" {
				s.maintenanceMu.Unlock()
				http.Error(w, "build sudah selesai dan tidak bisa dibatalkan", http.StatusConflict)
				return
			}
			now := time.Now().UTC().Format(time.RFC3339)
			tx, err := s.db.Begin()
			if err != nil {
				s.maintenanceMu.Unlock()
				http.Error(w, "gagal membatalkan build", http.StatusInternalServerError)
				return
			}
			result, err := tx.Exec(`UPDATE builds SET status='canceled',finished_at=?,updated_at=?
				WHERE id=? AND owner_id=? AND status IN ('preparing','assembling','queued','building')`,
				now, now, id, ownerID)
			if err != nil {
				tx.Rollback()
				s.maintenanceMu.Unlock()
				http.Error(w, "gagal membatalkan build", http.StatusInternalServerError)
				return
			}
			changed, err := result.RowsAffected()
			if err != nil || changed == 0 {
				tx.Rollback()
				s.maintenanceMu.Unlock()
				http.Error(w, "build sudah selesai dan tidak bisa dibatalkan", http.StatusConflict)
				return
			}
			if _, err := tx.Exec(`DELETE FROM build_manifests WHERE build_id=?`, id); err != nil {
				tx.Rollback()
				s.maintenanceMu.Unlock()
				http.Error(w, "build dibatalkan, tetapi manifest gagal dihapus", http.StatusInternalServerError)
				return
			}
			if _, err := tx.Exec(`DELETE FROM blob_reservations WHERE build_id=?`, id); err != nil {
				tx.Rollback()
				s.maintenanceMu.Unlock()
				http.Error(w, "build dibatalkan, tetapi reservasi binary gagal dihapus", http.StatusInternalServerError)
				return
			}
			if err := releaseBuildBlobRefsTx(tx, id); err != nil {
				tx.Rollback()
				s.maintenanceMu.Unlock()
				http.Error(w, "build dibatalkan, tetapi referensi binary gagal dilepas", http.StatusInternalServerError)
				return
			}
			if err := tx.Commit(); err != nil {
				s.maintenanceMu.Unlock()
				http.Error(w, "gagal menyelesaikan pembatalan build", http.StatusInternalServerError)
				return
			}
			s.maintenanceMu.Unlock()
			if !s.cancelRunningBuild(id) {
				if err := os.RemoveAll(filepath.Join(s.dataDir, "workspaces", id)); err != nil {
					log.Printf("failed to remove canceled build workspace %s: %v", id, err)
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		build, err := s.getBuildForOwner(ownerID, id)
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
		if _, err := s.getBuildForOwner(ownerID, id); err != nil {
			http.Error(w, "build tidak ditemukan", http.StatusNotFound)
			return
		}
		s.streamLogs(w, r, id)
	case "artifact":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		build, err := s.getBuildForOwner(ownerID, id)
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

func (s *Server) getBuildForOwner(ownerID, id string) (Build, error) {
	var b Build
	var started, finished, errMsg, artifact sql.NullString
	err := s.db.QueryRow(`SELECT id,project_id,status,variant,created_at,started_at,finished_at,error,artifact
		FROM builds WHERE id=? AND owner_id=?`, id, ownerID).
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(s.config.Build.TimeoutMinutes)*time.Minute)
	defer cancel()
	s.buildMu.Lock()
	if s.buildCancels == nil {
		s.buildCancels = make(map[string]context.CancelFunc)
	}
	s.buildCancels[id] = cancel
	s.buildMu.Unlock()
	defer func() {
		s.buildMu.Lock()
		delete(s.buildCancels, id)
		s.buildMu.Unlock()
	}()
	select {
	case s.buildSlots <- struct{}{}:
		defer func() { <-s.buildSlots }()
	case <-ctx.Done():
		if ctx.Err() == context.DeadlineExceeded {
			s.failBuild(id, fmt.Sprintf("build timeout %d menit", s.config.Build.TimeoutMinutes))
		}
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.Exec(`UPDATE builds SET status='building',started_at=? WHERE id=? AND status='queued'`, now, id)
	if err != nil {
		s.failBuild(id, "gagal memulai worker build")
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		s.failBuild(id, "gagal memulai worker build")
		return
	}
	if changed == 0 {
		return
	}
	s.addLog(id, "system", "Menjalankan Docker build container")
	containerName := "android-ide-build-" + id
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm",
		"--name", containerName,
		fmt.Sprintf("--memory=%dm", s.config.Build.MemoryMB),
		fmt.Sprintf("--cpus=%d", s.config.Build.CPUs), "--pids-limit=256",
		"-v", hostProjectRoot+":/project",
		"-v", "androidide_gradle:/root/.gradle",
		androidBuilderImage(),
		"/bin/bash", "-lc", "if [ -x ./gradlew ]; then ./gradlew --no-daemon assembleDebug; elif [ -f ./gradlew ]; then chmod +x ./gradlew && ./gradlew --no-daemon assembleDebug; else gradle --no-daemon assembleDebug; fi")
	cmd.Cancel = func() error {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := exec.CommandContext(stopCtx, "docker", "stop", "--time", "2", containerName).Run(); err != nil {
			return err
		}
		return os.ErrProcessDone
	}
	cmd.WaitDelay = 5 * time.Second
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		if ctx.Err() == context.Canceled {
			return
		}
		s.failBuild(id, "gagal menjalankan Docker: "+err.Error())
		return
	}
	var logWriters sync.WaitGroup
	logWriters.Add(2)
	go func() {
		defer logWriters.Done()
		s.copyLogs(id, "stdout", stdout)
	}()
	go func() {
		defer logWriters.Done()
		s.copyLogs(id, "stderr", stderr)
	}()
	err = cmd.Wait()
	logWriters.Wait()
	if ctx.Err() == context.Canceled {
		s.addLog(id, "system", "Build dibatalkan")
		return
	}
	if ctx.Err() == context.DeadlineExceeded {
		s.failBuild(id, fmt.Sprintf("build timeout %d menit", s.config.Build.TimeoutMinutes))
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
	if err := os.MkdirAll(filepath.Dir(artifact), 0750); err != nil {
		s.failBuild(id, "gagal membuat direktori artifact")
		return
	}
	input, _ := os.Open(apk)
	output, err := os.Create(artifact)
	if err != nil {
		s.failBuild(id, "gagal menyimpan APK")
		return
	}
	io.Copy(output, input)
	input.Close()
	output.Close()
	if ctx.Err() == context.Canceled {
		os.Remove(artifact)
		s.addLog(id, "system", "Build dibatalkan")
		return
	}
	s.addLog(id, "system", "APK berhasil dibuild")
	result, err = s.db.Exec(`UPDATE builds SET status='success',finished_at=?,artifact=?
		WHERE id=? AND status='building'`,
		time.Now().UTC().Format(time.RFC3339), filename, id)
	if err != nil {
		log.Printf("failed to save success status for build %s: %v", id, err)
		os.Remove(artifact)
		return
	}
	changed, err = result.RowsAffected()
	if err != nil || changed == 0 {
		os.Remove(artifact)
		return
	}
	s.enqueueBuildNotification(id, "success")
}

func (s *Server) cancelRunningBuild(id string) bool {
	s.buildMu.Lock()
	cancel := s.buildCancels[id]
	s.buildMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
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

func androidBuilderImage() string {
	image := strings.TrimSpace(os.Getenv("ANDROID_BUILDER_IMAGE"))
	if image == "" {
		return "android-web-ide-builder"
	}
	return image
}

func (s *Server) failBuild(id, message string) {
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := s.db.Exec(`UPDATE builds SET status='failed',finished_at=?,updated_at=?,error=?
		WHERE id=? AND status IN ('preparing','assembling','queued','building')`,
		now, now, message, id)
	if err != nil {
		log.Printf("failed to save failure status for build %s: %v", id, err)
		return
	}
	changed, err := result.RowsAffected()
	if err != nil {
		log.Printf("failed to confirm failure status for build %s: %v", id, err)
		return
	}
	if changed == 0 {
		return
	}
	s.addLog(id, "system", "Build gagal: "+message)
	s.enqueueBuildNotification(id, "failed")
	s.db.Exec(`DELETE FROM build_manifests WHERE build_id=?`, id)
	s.db.Exec(`DELETE FROM blob_reservations WHERE build_id=?`, id)
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
