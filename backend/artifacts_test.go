package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactListingIsOwnerScopedAndIncludesAvailability(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	artifactDir := filepath.Join(dataDir, "artifacts")
	if err := os.MkdirAll(artifactDir, 0750); err != nil {
		t.Fatal(err)
	}
	ownerA, ownerB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, owner := range []string{ownerA, ownerB} {
		if _, err := db.Exec(`INSERT INTO owners(id,created_at,last_seen_at) VALUES(?,?,?)`, owner, now, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		id, owner, project string
	}{
		{"artifact-present", ownerA, "project-a"},
		{"artifact-missing", ownerA, "project-b"},
		{"artifact-other-owner", ownerB, "project-a"},
	} {
		if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at,artifact)
			VALUES(?,?,?,'success','debug',?,?)`, item.id, item.project, item.owner, now, item.id+".apk"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at,artifact)
		VALUES('library-build','project-library',?,'success','jar',?,'library-build.jar')`, ownerA, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "artifact-present.apk"), []byte("apk"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "library-build.jar"), []byte("jar"), 0640); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db, dataDir: dataDir}
	request := httptest.NewRequest(http.MethodGet, "/api/artifacts", nil)
	request.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerA})
	response := httptest.NewRecorder()
	server.handleArtifactRoutes(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("artifact listing returned %d: %s", response.Code, response.Body.String())
	}
	var artifacts []BuildArtifact
	if err := json.Unmarshal(response.Body.Bytes(), &artifacts); err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 3 {
		t.Fatalf("listed %d artifacts for owner, want three", len(artifacts))
	}
	byBuildID := make(map[string]BuildArtifact, len(artifacts))
	for _, artifact := range artifacts {
		byBuildID[artifact.BuildID] = artifact
	}
	if artifact := byBuildID["artifact-present"]; !artifact.Available || artifact.Size != 3 {
		t.Fatalf("unexpected present APK metadata: %+v", artifact)
	}
	if artifact := byBuildID["artifact-missing"]; artifact.Available {
		t.Fatalf("missing APK should remain visible but unavailable: %+v", artifact)
	}
	if artifact := byBuildID["library-build"]; artifact.ProjectType != "java-library" ||
		artifact.Filename != "library-build.jar" || !artifact.Available || artifact.Size != 3 {
		t.Fatalf("unexpected Java library artifact metadata: %+v", artifact)
	}
}

func TestArtifactDeleteRemovesOnlyOwnersAPKAndKeepsBuildHistory(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	artifactDir := filepath.Join(dataDir, "artifacts")
	if err := os.MkdirAll(artifactDir, 0750); err != nil {
		t.Fatal(err)
	}
	ownerA, ownerB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, owner := range []string{ownerA, ownerB} {
		if _, err := db.Exec(`INSERT INTO owners(id,created_at,last_seen_at) VALUES(?,?,?)`, owner, now, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ id, owner string }{
		{"artifact-owner-a", ownerA},
		{"artifact-owner-b", ownerB},
	} {
		if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at,artifact)
			VALUES(?, 'project-a', ?, 'success', 'debug', ?, ?)`, item.id, item.owner, now, item.id+".apk"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(artifactDir, item.id+".apk"), []byte("apk"), 0640); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{db: db, dataDir: dataDir}
	request := httptest.NewRequest(http.MethodDelete, "/api/artifacts/artifact-owner-b", nil)
	request.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerA})
	response := httptest.NewRecorder()
	server.handleArtifactRoutes(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-owner artifact delete returned %d, want not found", response.Code)
	}

	request = httptest.NewRequest(http.MethodDelete, "/api/artifacts/artifact-owner-a", nil)
	request.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerA})
	response = httptest.NewRecorder()
	server.handleArtifactRoutes(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"deleted":true`) {
		t.Fatalf("artifact delete returned %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(artifactDir, "artifact-owner-a.apk")); !os.IsNotExist(err) {
		t.Fatalf("deleted APK still exists: %v", err)
	}
	var artifact sql.NullString
	var status string
	if err := db.QueryRow(`SELECT status,artifact FROM builds WHERE id='artifact-owner-a'`).Scan(&status, &artifact); err != nil {
		t.Fatal(err)
	}
	if status != "success" || artifact.Valid {
		t.Fatalf("deleting an APK changed build history unexpectedly: status=%q artifact=%+v", status, artifact)
	}
	if _, err := os.Stat(filepath.Join(artifactDir, "artifact-owner-b.apk")); err != nil {
		t.Fatalf("other owner's APK was changed: %v", err)
	}
}
