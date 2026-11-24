package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCancelBuildStopsRunnerAndReleasesBlobReferences(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	ownerID := strings.Repeat("a", 64)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO owners(id,created_at,last_seen_at) VALUES(?,?,?)`, ownerID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at)
		VALUES('build-cancel','project-1',?,'building','debug',?)`, ownerID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO blobs(owner_id,sha256,size,ref_count,created_at)
		VALUES(?,?,3,1,?)`, ownerID, strings.Repeat("b", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO build_blob_refs(build_id,owner_id,sha256,counted)
		VALUES('build-cancel',?,?,1)`, ownerID, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(dataDir, "workspaces", "build-cancel")
	if err := os.MkdirAll(workspace, 0750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{
		db: db, dataDir: dataDir,
		building: map[string]bool{"build-cancel": true},
		buildCancels: map[string]context.CancelFunc{
			"build-cancel": cancel,
		},
	}
	request := httptest.NewRequest(http.MethodDelete, "/api/builds/build-cancel", nil)
	request.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerID})
	response := httptest.NewRecorder()
	server.handleBuildRoutes(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("cancel returned %d: %s", response.Code, response.Body.String())
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("build process context was not canceled")
	}
	cancel()

	var status string
	if err := db.QueryRow(`SELECT status FROM builds WHERE id='build-cancel'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "canceled" {
		t.Fatalf("build status = %q, want canceled", status)
	}
	var refCount int
	if err := db.QueryRow(`SELECT ref_count FROM blobs WHERE owner_id=? AND sha256=?`,
		ownerID, strings.Repeat("b", 64)).Scan(&refCount); err != nil {
		t.Fatal(err)
	}
	if refCount != 0 {
		t.Fatalf("blob refcount = %d after cancellation, want zero", refCount)
	}
	var refs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM build_blob_refs WHERE build_id='build-cancel'`).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != 0 {
		t.Fatalf("build retained %d blob refs after cancellation", refs)
	}
}

func TestCancelBuildIsIdempotentButRejectsFinishedBuild(t *testing.T) {
	db := testDB(t)
	ownerID := strings.Repeat("a", 64)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO owners(id,created_at,last_seen_at) VALUES(?,?,?)`, ownerID, now, now); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"canceled", "success"} {
		if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at)
			VALUES(?,?,?,?,'debug',?)`, "build-"+status, "project-1", ownerID, status, now); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{db: db, dataDir: t.TempDir()}
	request := func(id string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/builds/"+id, nil)
		req.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerID})
		server.handleBuildRoutes(recorder, req)
		return recorder
	}
	if response := request("build-canceled"); response.Code != http.StatusNoContent {
		t.Fatalf("repeat cancel returned %d, want no content", response.Code)
	}
	if response := request("build-success"); response.Code != http.StatusConflict {
		t.Fatalf("cancel finished build returned %d, want conflict", response.Code)
	}
}

func TestCancelQueuedBuildRemovesWorkspaceBeforeRunnerStarts(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	ownerID := strings.Repeat("a", 64)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO owners(id,created_at,last_seen_at) VALUES(?,?,?)`, ownerID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at)
		VALUES('build-queued','project-1',?,'queued','debug',?)`, ownerID, now); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(dataDir, "workspaces", "build-queued")
	if err := os.MkdirAll(workspace, 0750); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db, dataDir: dataDir}
	request := httptest.NewRequest(http.MethodDelete, "/api/builds/build-queued", nil)
	request.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerID})
	response := httptest.NewRecorder()
	server.handleBuildRoutes(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("cancel queued build returned %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("queued workspace still exists after cancel: %v", err)
	}
}
