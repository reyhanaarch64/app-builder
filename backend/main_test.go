package main

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestAndroidBuilderImage(t *testing.T) {
	t.Setenv("ANDROID_BUILDER_IMAGE", "")
	if got := androidBuilderImage(); got != "android-web-ide-builder" {
		t.Fatalf("default builder image = %q, want %q", got, "android-web-ide-builder")
	}

	t.Setenv("ANDROID_BUILDER_IMAGE", " android-web-ide-builder:arm64 ")
	if got := androidBuilderImage(); got != "android-web-ide-builder:arm64" {
		t.Fatalf("configured builder image = %q, want %q", got, "android-web-ide-builder:arm64")
	}
}

func TestFrontendFilesRequireRevalidation(t *testing.T) {
	webDir := t.TempDir()
	for name, content := range map[string]string{
		"index.html": "app shell",
		"sw.js":      "service worker",
		"app.js":     "frontend asset",
	} {
		if err := os.WriteFile(filepath.Join(webDir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	handler := frontendFileServer(webDir)
	for _, path := range []string{"/", "/sw.js", "/app.js"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d, want %d", path, response.Code, http.StatusOK)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want %q", path, got, "no-cache")
		}
	}
}

func TestNotificationSettingsArePrivateAndDoNotExposeSMTPSecret(t *testing.T) {
	db := testDB(t)
	server := &Server{
		db: db,
		config: AppConfig{
			Mode: "private",
			SMTP: SMTPConfig{
				Host: "smtp.example.test", Port: 587, Username: "sender@example.test",
				Password: "smtp-secret", From: "sender@example.test",
			},
		},
	}

	request := httptest.NewRequest(http.MethodPut, "/api/notifications/settings",
		strings.NewReader(`{"recipient":"receiver@example.test","notifySuccess":true,"notifyFailure":false}`))
	response := httptest.NewRecorder()
	server.handleNotificationSettings(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("save notification settings returned %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "smtp-secret") || !strings.Contains(response.Body.String(), `"smtpConfigured":true`) {
		t.Fatalf("SMTP credentials leaked or configured state missing: %s", response.Body.String())
	}

	publicServer := &Server{db: db, config: AppConfig{Mode: "public"}}
	response = httptest.NewRecorder()
	publicServer.handleNotificationSettings(response, httptest.NewRequest(http.MethodGet, "/api/notifications/settings", nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("public mode settings returned %d, want forbidden", response.Code)
	}
}

func TestBuildNotificationQueueRespectsSettingsAndDeduplicates(t *testing.T) {
	db := testDB(t)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, build := range []struct {
		id     string
		status string
	}{
		{id: "success-1", status: "success"},
		{id: "failed-1", status: "failed"},
	} {
		if _, err := db.Exec(`INSERT INTO builds(id,project_id,status,variant,created_at,started_at,finished_at,error)
			VALUES(?,?,?,?,?,?,?,?)`, build.id, "project-1", build.status, "debug", now, now, now,
			map[string]string{"success": "", "failed": "Gradle build gagal"}[build.status]); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO build_logs(build_id,sequence,stream,message,created_at)
			VALUES(?,1,'stderr','Execution failed',?)`, build.id, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE notification_settings SET recipient='receiver@example.test',
		notify_success=1,notify_failure=0 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db}
	server.enqueueBuildNotification("success-1", "success")
	server.enqueueBuildNotification("success-1", "success")
	server.enqueueBuildNotification("failed-1", "failed")

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM email_outbox`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("queued %d messages, want one enabled deduplicated notification", count)
	}
	var recipient, subject, body, status string
	if err := db.QueryRow(`SELECT recipient,subject,body,status FROM email_outbox WHERE build_id='success-1'`).
		Scan(&recipient, &subject, &body, &status); err != nil {
		t.Fatal(err)
	}
	if recipient != "receiver@example.test" || !strings.Contains(subject, "success-1") ||
		!strings.Contains(body, "Project ID: project-1") || !strings.Contains(body, "Execution failed") || status != "pending" {
		t.Fatalf("unexpected queued notification: recipient=%q subject=%q body=%q status=%q", recipient, subject, body, status)
	}
}

func TestValidateAndroidProject(t *testing.T) {
	root := t.TempDir()
	if err := validateAndroidProject(root); err == nil || !strings.Contains(err.Error(), "settings.gradle") {
		t.Fatalf("expected missing settings.gradle error, got %v", err)
	}
	for _, path := range []string{
		"settings.gradle.kts",
		"app/build.gradle.kts",
		"app/src/main/AndroidManifest.xml",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("valid"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateAndroidProject(root); err != nil {
		t.Fatalf("expected valid single-module project, got %v", err)
	}
}

func TestExtractZipRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	archivePath := filepath.Join(root, "bad.zip")
	archive, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(archive)
	entry, err := writer.Create("../escaped.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("bad"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive.Close()
	if err := extractZip(archivePath, filepath.Join(root, "out")); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
}

func TestNormalizeManifestRejectsUnsafePathAndBadTextHash(t *testing.T) {
	content := []byte("settings")
	base := BuildManifest{
		ProjectID: "project-1", IdempotencyKey: "request",
		Files: []ManifestFile{{
			Path: "settings.gradle.kts", Kind: "text", Size: int64(len(content)),
			MIMEType: "text/plain", SHA256: hashBytes(content), Content: string(content),
		}},
	}
	unsafe := base
	unsafe.Files = append([]ManifestFile(nil), base.Files...)
	unsafe.Files[0].Path = "../settings.gradle.kts"
	if _, _, err := normalizeManifest(unsafe); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}
	mismatched := base
	mismatched.Files = append([]ManifestFile(nil), base.Files...)
	mismatched.Files[0].Content = "changed"
	if _, _, err := normalizeManifest(mismatched); err == nil {
		t.Fatal("expected text content with a mismatched hash to be rejected")
	}
}

func TestBuildListReturnsPersistedBuilds(t *testing.T) {
	db := testDB(t)
	ownerID := strings.Repeat("a", 64)
	_, err := db.Exec(`INSERT INTO owners(id,created_at,last_seen_at) VALUES(?,?,?)`,
		ownerID, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO builds (id,project_id,owner_id,status,variant,created_at)
		VALUES ('build-1','project-1',?,'building','debug','2026-01-01T00:00:00Z')`, ownerID)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db}
	request := httptest.NewRequest(http.MethodGet, "/api/builds?projectId=project-1", nil)
	request.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerID})
	response := httptest.NewRecorder()
	server.handleBuildList(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"build-1"`) ||
		!strings.Contains(response.Body.String(), `"status":"building"`) {
		t.Fatalf("unexpected build list response: %d %s", response.Code, response.Body.String())
	}
}

func TestBlobBuildPipelineIsOwnerScopedAndVerifiesContent(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	for _, directory := range []string{"workspaces", "blobs/sha256"} {
		if err := os.MkdirAll(filepath.Join(dataDir, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ownerA, ownerB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, owner := range []string{ownerA, ownerB} {
		if _, err := db.Exec(`INSERT INTO owners(id,created_at,last_seen_at) VALUES(?,?,?)`, owner, "now", "now"); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{db: db, dataDir: dataDir, config: AppConfig{SecureCookie: false}, building: map[string]bool{}}
	binary := []byte{0, 1, 2, 255, 17}
	textFiles := []struct{ path, content string }{
		{"settings.gradle.kts", `rootProject.name = "test"` + "\ninclude(\":app\")\n"},
		{"app/build.gradle.kts", "plugins { id(\"com.android.application\") }\n"},
		{"app/src/main/AndroidManifest.xml", `<manifest xmlns:android="http://schemas.android.com/apk/res/android"><application/></manifest>`},
	}
	manifest := BuildManifest{
		ProjectID: "project-1", IdempotencyKey: "request-a",
	}
	for _, file := range textFiles {
		content := []byte(file.content)
		manifest.Files = append(manifest.Files, ManifestFile{
			Path: file.path, Kind: "text", Size: int64(len(content)), MIMEType: "text/plain", SHA256: hashBytes(content), Content: file.content,
		})
	}
	blobHash := hashBytes(binary)
	manifest.Files = append(manifest.Files, ManifestFile{
		Path: "app/src/main/res/drawable/icon.png", Kind: "binary", Size: int64(len(binary)),
		MIMEType: "image/png", SHA256: blobHash,
	})
	prepare := func(owner, idempotencyKey string) (PrepareResponse, int) {
		t.Helper()
		copyManifest := manifest
		copyManifest.IdempotencyKey = idempotencyKey
		body, err := json.Marshal(copyManifest)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/builds/prepare", strings.NewReader(string(body)))
		request.AddCookie(&http.Cookie{Name: "ide_owner", Value: owner})
		response := httptest.NewRecorder()
		server.handleBuildPrepare(response, request)
		var result PrepareResponse
		if response.Code == http.StatusCreated || response.Code == http.StatusOK {
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
		}
		return result, response.Code
	}
	first, status := prepare(ownerA, "request-a")
	if status != http.StatusCreated || len(first.MissingBlobs) != 1 || first.MissingBlobs[0] != blobHash {
		t.Fatalf("unexpected first prepare: status=%d response=%+v", status, first)
	}
	second, status := prepare(ownerB, "request-b")
	if status != http.StatusCreated || len(second.MissingBlobs) != 1 {
		t.Fatalf("expected owner B to miss owner A's cached blob: status=%d response=%+v", status, second)
	}
	if _, err := db.Exec(`UPDATE build_manifests SET expires_at='2000-01-01T00:00:00Z' WHERE build_id=?`, second.Build.ID); err != nil {
		t.Fatal(err)
	}

	uploadURL := "/api/builds/" + first.Build.ID + "/blobs/" + blobHash
	foreignUpload := httptest.NewRequest(http.MethodPut, uploadURL, strings.NewReader(string(binary)))
	foreignUpload.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerB})
	foreignResponse := httptest.NewRecorder()
	server.handleBuildRoutes(foreignResponse, foreignUpload)
	if foreignResponse.Code != http.StatusNotFound {
		t.Fatalf("expected foreign owner upload to be hidden, got %d", foreignResponse.Code)
	}

	badUpload := httptest.NewRequest(http.MethodPut, uploadURL, strings.NewReader("wrong"))
	badUpload.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerA})
	badResponse := httptest.NewRecorder()
	server.handleBuildRoutes(badResponse, badUpload)
	if badResponse.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected mismatched hash to be rejected, got %d: %s", badResponse.Code, badResponse.Body.String())
	}
	goodUpload := httptest.NewRequest(http.MethodPut, uploadURL, strings.NewReader(string(binary)))
	goodUpload.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerA})
	goodResponse := httptest.NewRecorder()
	server.handleBuildRoutes(goodResponse, goodUpload)
	if goodResponse.Code != http.StatusNoContent {
		t.Fatalf("expected verified upload, got %d: %s", goodResponse.Code, goodResponse.Body.String())
	}

	server.startBuildFn = func(_, _ string) {}
	finalize := httptest.NewRequest(http.MethodPost, "/api/builds/"+first.Build.ID+"/finalize", nil)
	finalize.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerA})
	finalized := httptest.NewRecorder()
	server.handleBuildRoutes(finalized, finalize)
	if finalized.Code != http.StatusCreated {
		t.Fatalf("expected successful finalize, got %d: %s", finalized.Code, finalized.Body.String())
	}
	workspaceFile := filepath.Join(dataDir, "workspaces", first.Build.ID, "app/src/main/res/drawable/icon.png")
	if content, err := os.ReadFile(workspaceFile); err != nil || string(content) != string(binary) {
		t.Fatalf("assembled binary mismatch: content=%v err=%v", content, err)
	}
	var refCount int
	if err := db.QueryRow(`SELECT ref_count FROM blobs WHERE owner_id=? AND sha256=?`, ownerA, blobHash).Scan(&refCount); err != nil || refCount != 1 {
		t.Fatalf("expected one build ref, got refcount=%d err=%v", refCount, err)
	}
	if _, err := db.Exec(`UPDATE builds SET status='failed' WHERE id=?`, first.Build.ID); err != nil {
		t.Fatal(err)
	}
	reused, status := prepare(ownerA, "request-a-retry")
	if status != http.StatusCreated || len(reused.MissingBlobs) != 0 {
		t.Fatalf("expected same owner to reuse cached blob: status=%d response=%+v", status, reused)
	}
	finalizeReuse := httptest.NewRequest(http.MethodPost, "/api/builds/"+reused.Build.ID+"/finalize", nil)
	finalizeReuse.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerA})
	reusedResponse := httptest.NewRecorder()
	server.handleBuildRoutes(reusedResponse, finalizeReuse)
	if reusedResponse.Code != http.StatusCreated {
		t.Fatalf("expected cache-reusing build to finalize, got %d: %s", reusedResponse.Code, reusedResponse.Body.String())
	}
	if err := db.QueryRow(`SELECT ref_count FROM blobs WHERE owner_id=? AND sha256=?`, ownerA, blobHash).Scan(&refCount); err != nil || refCount != 2 {
		t.Fatalf("expected two build refs to retain the blob, got refcount=%d err=%v", refCount, err)
	}
	if err := server.releaseAllBuildBlobRefs(); err != nil {
		t.Fatal(err)
	}
	if err := server.collectExpiredBlobReservations(); err != nil {
		t.Fatal(err)
	}
	var expiredStatus string
	if err := db.QueryRow(`SELECT status FROM builds WHERE id=?`, second.Build.ID).Scan(&expiredStatus); err != nil || expiredStatus != "failed" {
		t.Fatalf("expected abandoned reservation to expire, got status=%q err=%v", expiredStatus, err)
	}
	if _, err := os.Stat(server.blobPath(ownerA, blobHash)); !os.IsNotExist(err) {
		t.Fatalf("expected unreferenced blob to be garbage collected, got %v", err)
	}
}

func TestOnlyOneActiveBuildPerProject(t *testing.T) {
	db := testDB(t)
	_, err := db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at)
		VALUES ('build-1','project-1','queued','debug','2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at)
		VALUES ('build-2','project-1','building','debug','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("expected active-build uniqueness constraint")
	}
	if _, err := db.Exec(`UPDATE builds SET status='failed' WHERE id='build-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at)
		VALUES ('build-2','project-1','queued','debug','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("expected retry after terminal status, got %v", err)
	}
}

func TestLoadConfigCreatesPrivateDefaultFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "IDE.config")
	config, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != "public" || config.Port != 8866 || config.Build.Parallel != 1 {
		t.Fatalf("unexpected default config: %+v", config)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("expected IDE.config mode 0600, got %04o", info.Mode().Perm())
	}
}

func TestSchemaRemovesLegacyBrowserPushTables(t *testing.T) {
	db := testDB(t)
	for _, table := range []string{"push_subscriptions", "push_outbox", "webpush_keys"} {
		if _, err := db.Exec("CREATE TABLE " + table + " (id INTEGER PRIMARY KEY)"); err != nil {
			t.Fatal(err)
		}
	}

	schema(db)

	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name IN ('push_subscriptions','push_outbox','webpush_keys')`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("schema retained %d legacy browser push tables", remaining)
	}
}

func TestLoadConfigRejectsPrivateModeWithoutPassword(t *testing.T) {
	path := filepath.Join(t.TempDir(), "IDE.config")
	content := `{"mode":"private","password":"","port":8866,"sessionTTLHours":168,"secureCookie":true,"build":{"maxUploadMB":50,"timeoutMinutes":30,"memoryMB":4096,"cpus":2,"parallel":1}}`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("expected private password validation error, got %v", err)
	}
}

func TestPrivateModeRequiresSessionButPublicModeDoesNot(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/builds", nil)
	privateServer := &Server{config: AppConfig{Mode: "private"}, sessions: make(map[string]session)}
	privateResponse := httptest.NewRecorder()
	privateServer.authMiddleware(handler).ServeHTTP(privateResponse, request)
	if privateResponse.Code != http.StatusUnauthorized {
		t.Fatalf("expected private endpoint to require session, got %d", privateResponse.Code)
	}
	privateResponse = httptest.NewRecorder()
	privateServer.authMiddleware(handler).ServeHTTP(privateResponse,
		httptest.NewRequest(http.MethodGet, "/api/auth/status", nil))
	if privateResponse.Code != http.StatusNoContent {
		t.Fatalf("expected auth status endpoint to remain public, got %d", privateResponse.Code)
	}

	publicServer := &Server{config: AppConfig{Mode: "public"}}
	publicResponse := httptest.NewRecorder()
	publicServer.authMiddleware(handler).ServeHTTP(publicResponse, request)
	if publicResponse.Code != http.StatusNoContent {
		t.Fatalf("expected public endpoint to stay open, got %d", publicResponse.Code)
	}
}

func TestPrivateLoginSetsSecureSessionCookie(t *testing.T) {
	server := &Server{
		config:   AppConfig{Mode: "private", Password: "strong-password", SessionTTLHours: 24, SecureCookie: true},
		sessions: make(map[string]session), loginFailures: make(map[string]loginAttempts),
	}
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"password":"strong-password"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.handleLogin(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected successful login, got %d: %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "ide_session" || !cookies[0].HttpOnly ||
		!cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected session cookie: %+v", cookies)
	}
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/builds", nil)
	sessionRequest.AddCookie(cookies[0])
	if !server.hasValidSession(sessionRequest) {
		t.Fatal("expected login session to authorize private API")
	}
}

func TestCleanupRejectsActiveBuildInPrivateMode(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	workspace := filepath.Join(dataDir, "workspaces", "active")
	if err := os.MkdirAll(workspace, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "keep.txt"), []byte("keep"), 0640); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at)
		VALUES ('active','project-1','building','debug',?)`, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db: db, dataDir: dataDir,
		config:   AppConfig{Mode: "private", Password: "password123", SessionTTLHours: 24},
		sessions: map[string]session{"valid-session": {expiresAt: time.Now().Add(time.Hour)}},
		building: make(map[string]bool),
	}
	body, _ := json.Marshal(CleanupRequest{Workspaces: true})
	unauthorized := httptest.NewRecorder()
	unauthorizedRequest := httptest.NewRequest(http.MethodPost, "/api/maintenance/cleanup", strings.NewReader(string(body)))
	server.authMiddleware(http.HandlerFunc(server.handleCleanup)).ServeHTTP(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized response, got %d", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/maintenance/cleanup", strings.NewReader(string(body)))
	request.AddCookie(&http.Cookie{Name: "ide_session", Value: "valid-session"})
	response := httptest.NewRecorder()
	server.authMiddleware(http.HandlerFunc(server.handleCleanup)).ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("expected active-build conflict, got %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(workspace, "keep.txt")); err != nil {
		t.Fatalf("cleanup removed workspace while build was active: %v", err)
	}
}

func TestRemoveTerminalArtifacts(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	artifactsDir := filepath.Join(dataDir, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "build-1.apk"), []byte("apk"), 0640); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at,artifact)
		VALUES ('build-1','project-1','success','debug','2026-01-01T00:00:00Z','build-1.apk')`)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db, dataDir: dataDir}
	if err := server.removeTerminalArtifacts(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(artifactsDir, "build-1.apk")); !os.IsNotExist(err) {
		t.Fatalf("expected APK to be deleted, got %v", err)
	}
	var artifact sql.NullString
	if err := db.QueryRow(`SELECT artifact FROM builds WHERE id='build-1'`).Scan(&artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Valid {
		t.Fatal("expected database artifact reference to be cleared")
	}
}

func TestCleanupRemovesSelectedBuildDataButKeepsHistory(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	workspace := filepath.Join(dataDir, "workspaces", "old-build")
	artifactsDir := filepath.Join(dataDir, "artifacts")
	if err := os.MkdirAll(workspace, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(artifactsDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "project.zip"), []byte("zip"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactsDir, "build-1.apk"), []byte("apk"), 0640); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at,artifact)
		VALUES ('build-1','project-1','success','debug','2026-01-01T00:00:00Z','build-1.apk')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO build_logs (build_id,sequence,stream,message,created_at)
		VALUES ('build-1',1,'system','done','2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db: db, dataDir: dataDir,
		cacheSizeFn: func() (int64, error) { return 0, nil },
	}
	body, _ := json.Marshal(CleanupRequest{Workspaces: true, Artifacts: true})
	request := httptest.NewRequest(http.MethodPost, "/api/maintenance/cleanup", strings.NewReader(string(body)))
	response := httptest.NewRecorder()
	server.handleCleanup(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected cleanup success, got %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace remains after cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artifactsDir, "build-1.apk")); !os.IsNotExist(err) {
		t.Fatalf("APK remains after cleanup: %v", err)
	}
	var buildCount, logCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM builds`).Scan(&buildCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM build_logs`).Scan(&logCount); err != nil {
		t.Fatal(err)
	}
	if buildCount != 1 || logCount != 1 {
		t.Fatalf("cleanup unexpectedly removed build history or logs: %d builds, %d logs", buildCount, logCount)
	}
}

func TestRunBuildRemovesWorkspaceOnValidationFailure(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	workspace := filepath.Join(dataDir, "workspaces", "bad-build")
	if err := os.MkdirAll(workspace, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "project.zip"), []byte("bad"), 0640); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at)
		VALUES ('bad-build','project-1','queued','debug','2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db, dataDir: dataDir, buildSlots: make(chan struct{}, 1), building: make(map[string]bool)}
	server.runBuild("bad-build", workspace)
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace remains after failed build: %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM builds WHERE id='bad-build'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("expected failed build status, got %s", status)
	}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	schema(db)
	return db
}
