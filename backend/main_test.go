package main

import (
	"archive/zip"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	schema(db)
	_, err = db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at)
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
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	schema(db)
	_, err = db.Exec(`INSERT INTO builds (id,project_id,status,variant,created_at)
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
