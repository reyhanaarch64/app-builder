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

func TestBuildListReturnsPersistedBuilds(t *testing.T) {
	db := testDB(t)
	_, err := db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at)
		VALUES ('build-1','project-1','building','debug','2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db}
	request := httptest.NewRequest(http.MethodGet, "/api/builds?projectId=project-1", nil)
	response := httptest.NewRecorder()
	server.handleBuildList(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"build-1"`) ||
		!strings.Contains(response.Body.String(), `"status":"building"`) {
		t.Fatalf("unexpected build list response: %d %s", response.Code, response.Body.String())
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

func TestCleanupTokenIsPersistentAndPrivate(t *testing.T) {
	t.Setenv("CLEANUP_TOKEN", "")
	dataDir := t.TempDir()
	first, err := loadOrCreateCleanupToken(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadOrCreateCleanupToken(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(first) != 64 {
		t.Fatal("cleanup token was not persisted correctly")
	}
	info, err := os.Stat(filepath.Join(dataDir, "cleanup.token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("expected private token file mode 0600, got %04o", info.Mode().Perm())
	}
}

func TestCleanupRequiresTokenAndDoesNotTouchActiveBuild(t *testing.T) {
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
	server := &Server{db: db, dataDir: dataDir, cleanupToken: "secret"}
	body, _ := json.Marshal(CleanupRequest{Workspaces: true})
	unauthorized := httptest.NewRecorder()
	unauthorizedRequest := httptest.NewRequest(http.MethodPost, "/api/maintenance/cleanup", strings.NewReader(string(body)))
	server.handleCleanup(unauthorized, unauthorizedRequest)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized response, got %d", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/maintenance/cleanup", strings.NewReader(string(body)))
	request.Header.Set("X-Cleanup-Token", "secret")
	response := httptest.NewRecorder()
	server.handleCleanup(response, request)
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
		db: db, dataDir: dataDir, cleanupToken: "secret",
		cacheSizeFn: func() (int64, error) { return 0, nil },
	}
	body, _ := json.Marshal(CleanupRequest{Workspaces: true, Artifacts: true})
	request := httptest.NewRequest(http.MethodPost, "/api/maintenance/cleanup", strings.NewReader(string(body)))
	request.Header.Set("X-Cleanup-Token", "secret")
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
