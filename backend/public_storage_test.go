package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublicBuildLimitAndPerBuildBinaryCache(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	server := &Server{db: db, dataDir: dataDir, config: AppConfig{Mode: "public"}}

	tooLarge := BuildManifest{
		ProjectID: "too-large", IdempotencyKey: "too-large", Files: []ManifestFile{{
			Path: "asset.bin", Kind: "binary", Size: maxPublicProjectBytes + 1,
			MIMEType: "application/octet-stream", SHA256: strings.Repeat("a", 64),
		}},
	}
	response := prepareManifest(t, server, nil, tooLarge)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized public project returned %d: %s", response.Code, response.Body.String())
	}
	var buildCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM builds`).Scan(&buildCount); err != nil {
		t.Fatal(err)
	}
	if buildCount != 0 {
		t.Fatalf("oversized request created %d builds", buildCount)
	}
	privateServer := &Server{db: db, dataDir: dataDir, config: AppConfig{Mode: "private"}}
	privateManifest := tooLarge
	privateManifest.ProjectID = "private-large"
	privateManifest.IdempotencyKey = "private-large"
	response = prepareManifest(t, privateServer, nil, privateManifest)
	if response.Code != http.StatusCreated {
		t.Fatalf("private project larger than 10 MiB was rejected: %d %s", response.Code, response.Body.String())
	}

	content := []byte("asset")
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	manifest := func(projectID, key string) BuildManifest {
		return BuildManifest{
			ProjectID: projectID, IdempotencyKey: key,
			Files: []ManifestFile{{
				Path: "asset.bin", Kind: "binary", Size: int64(len(content)),
				MIMEType: "application/octet-stream", SHA256: hash,
			}},
		}
	}
	firstResponse := prepareManifest(t, server, nil, manifest("project-a", "first"))
	if firstResponse.Code != http.StatusCreated {
		t.Fatalf("first prepare returned %d: %s", firstResponse.Code, firstResponse.Body.String())
	}
	firstCookie := firstResponse.Result().Cookies()[0]
	var publicOwnerRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM owners WHERE id=?`, firstCookie.Value).Scan(&publicOwnerRows); err != nil {
		t.Fatal(err)
	}
	if publicOwnerRows != 0 {
		t.Fatal("public visitor identity was persisted in the private owner registry")
	}
	var first PrepareResponse
	if err := json.Unmarshal(firstResponse.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}

	upload := httptest.NewRequest(http.MethodPut,
		"/api/builds/"+first.Build.ID+"/blobs/"+hash, bytes.NewReader(content))
	upload.AddCookie(firstCookie)
	uploadResponse := httptest.NewRecorder()
	server.handleBuildRoutes(uploadResponse, upload)
	if uploadResponse.Code != http.StatusNoContent {
		t.Fatalf("public binary upload returned %d: %s", uploadResponse.Code, uploadResponse.Body.String())
	}

	secondResponse := prepareManifest(t, server, firstCookie, manifest("project-b", "second"))
	if secondResponse.Code != http.StatusCreated {
		t.Fatalf("second prepare returned %d: %s", secondResponse.Code, secondResponse.Body.String())
	}
	var second PrepareResponse
	if err := json.Unmarshal(secondResponse.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.MissingBlobs) != 1 || second.MissingBlobs[0] != hash {
		t.Fatalf("second public build reused the previous build's cache: missing=%v", second.MissingBlobs)
	}
	var firstBlobOwner, secondBlobOwner string
	if err := db.QueryRow(`SELECT blob_owner_id FROM builds WHERE id=?`, first.Build.ID).Scan(&firstBlobOwner); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT blob_owner_id FROM builds WHERE id=?`, second.Build.ID).Scan(&secondBlobOwner); err != nil {
		t.Fatal(err)
	}
	if firstBlobOwner == secondBlobOwner || !strings.HasPrefix(firstBlobOwner, "public_build_") ||
		!strings.HasPrefix(secondBlobOwner, "public_build_") {
		t.Fatalf("public builds do not have isolated blob owners: %q, %q", firstBlobOwner, secondBlobOwner)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "blobs", "sha256", firstBlobOwner, hash[:2], hash)); !os.IsNotExist(err) {
		t.Fatalf("public blob escaped into private cache: %v", err)
	}
}

func TestPublicIdleCleanupLeavesPrivateBuildData(t *testing.T) {
	db := testDB(t)
	dataDir := t.TempDir()
	server := &Server{db: db, dataDir: dataDir}
	now := time.Now().UTC()
	old := now.Add(-publicIdleTTL - time.Minute).Format(time.RFC3339)
	recent := now.Add(-time.Minute).Format(time.RFC3339)
	publicBuildID := "bld_" + strings.Repeat("a", 32)
	privateBuildID := "bld_" + strings.Repeat("b", 32)
	recentBuildID := "bld_" + strings.Repeat("c", 32)
	publicBlobOwner := "public_build_" + publicBuildID

	for _, build := range []struct {
		id, mode, owner, blobOwner string
	}{
		{publicBuildID, "public", "public_owner", publicBlobOwner},
		{privateBuildID, "private", strings.Repeat("b", 64), strings.Repeat("b", 64)},
	} {
		if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at,updated_at,artifact,access_mode,blob_owner_id)
			VALUES(?,?,?,'success','debug',?,?,?, ?,?)`,
			build.id, "project", build.owner, old, old, build.id+".apk", build.mode, build.blobOwner); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO build_logs(build_id,sequence,stream,message,created_at)
			VALUES(?,1,'system','done',?)`, build.id, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at,updated_at,access_mode,blob_owner_id)
		VALUES(?,'project','recent-owner','success','debug',?,?,'public',?)`,
		recentBuildID, recent, recent, "public_build_"+recentBuildID); err != nil {
		t.Fatal(err)
	}

	publicWorkspace := server.workspacePath(publicBuildID, "public")
	privateWorkspace := server.workspacePath(privateBuildID, "private")
	for _, path := range []string{
		filepath.Join(publicWorkspace, "project.txt"),
		filepath.Join(privateWorkspace, "project.txt"),
		server.artifactPath(publicBuildID, "public"),
		server.artifactPath(privateBuildID, "private"),
		filepath.Join(dataDir, "public", "gradle", publicBuildID, "cache.bin"),
		filepath.Join(dataDir, "public", "gradle", privateBuildID, "private-cache.bin"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	hash := strings.Repeat("c", 64)
	if _, err := db.Exec(`INSERT INTO blobs(owner_id,sha256,size,ref_count,created_at)
		VALUES(?,?,4,1,?)`, publicBlobOwner, hash, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO build_blob_refs(build_id,owner_id,sha256,counted)
		VALUES(?,?,?,1)`, publicBuildID, publicBlobOwner, hash); err != nil {
		t.Fatal(err)
	}
	publicBlob := server.blobPath(publicBlobOwner, hash)
	if err := os.MkdirAll(filepath.Dir(publicBlob), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicBlob, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := server.cleanupExpiredPublicBuilds(now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{publicBuildID, privateBuildID, recentBuildID} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM builds WHERE id=?`, id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 1
		if id == publicBuildID {
			want = 0
		}
		if count != want {
			t.Fatalf("build %s count = %d, want %d", id, count, want)
		}
	}
	for _, path := range []string{
		filepath.Join(publicWorkspace, "project.txt"),
		server.artifactPath(publicBuildID, "public"),
		filepath.Join(dataDir, "public", "gradle", publicBuildID),
		publicBlob,
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("expired public file remains at %s: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(privateWorkspace, "project.txt"),
		server.artifactPath(privateBuildID, "private"),
		filepath.Join(dataDir, "public", "gradle", privateBuildID, "private-cache.bin"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("private file was removed at %s: %v", path, err)
		}
	}
}

func TestPublicZIPExpansionLimitIsEnforced(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "oversized.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	entry, err := writer.Create("asset.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(bytes.Repeat([]byte{0}, int(maxPublicProjectBytes+1))); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	err = extractZip(archivePath, filepath.Join(t.TempDir(), "expanded"), uint64(maxPublicProjectBytes))
	if err == nil || !strings.Contains(err.Error(), "10 MiB") {
		t.Fatalf("public ZIP expansion limit error = %v", err)
	}
}

func prepareManifest(t *testing.T, server *Server, cookie *http.Cookie, manifest BuildManifest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/builds/prepare", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	server.handleBuildPrepare(response, request)
	return response
}
