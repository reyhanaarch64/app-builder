package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseGitHubRepositoryURL(t *testing.T) {
	tests := []struct {
		url       string
		wantOwner string
		wantRepo  string
		wantError bool
	}{
		{url: "https://github.com/acme/android-app", wantOwner: "acme", wantRepo: "android-app"},
		{url: "https://github.com/acme/android_app.git/", wantOwner: "acme", wantRepo: "android_app"},
		{url: "http://github.com/acme/android-app", wantError: true},
		{url: "https://github.com.evil.test/acme/android-app", wantError: true},
		{url: "https://user@github.com/acme/android-app", wantError: true},
		{url: "https://github.com:443/acme/android-app", wantError: true},
		{url: "https://github.com/acme/android-app?ref=main", wantError: true},
		{url: "https://github.com/acme/android-app/tree/main", wantError: true},
		{url: "https://github.com/acme/%2e%2e", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.url, func(t *testing.T) {
			owner, repo, err := parseGitHubRepositoryURL(test.url)
			if test.wantError {
				if err == nil {
					t.Fatalf("expected invalid repository URL, got owner=%q repo=%q", owner, repo)
				}
				return
			}
			if err != nil || owner != test.wantOwner || repo != test.wantRepo {
				t.Fatalf("unexpected parse result owner=%q repo=%q err=%v", owner, repo, err)
			}
		})
	}
}

func TestGitHubImportRejectsInvalidRequestBeforeNetworkAccess(t *testing.T) {
	for _, repositoryURL := range []string{
		"http://127.0.0.1/",
		"https://github.com/acme/repo/tree/main",
		"https://github.com/acme/repo?archive=1",
	} {
		requestBody, err := json.Marshal(gitHubImportRequest{URL: repositoryURL})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/import/github", strings.NewReader(string(requestBody)))
		response := httptest.NewRecorder()
		(&Server{}).handleGitHubImport(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("expected invalid URL to be rejected, got %d: %s", response.Code, response.Body.String())
		}
	}
}

func TestActiveBuildsAreOwnerScoped(t *testing.T) {
	db := testDB(t)
	ownerA, ownerB := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, owner := range []string{ownerA, ownerB} {
		if _, err := db.Exec(`INSERT INTO owners(id,created_at,last_seen_at) VALUES(?,?,?)`, owner, "now", "now"); err != nil {
			t.Fatal(err)
		}
	}
	for _, build := range []struct {
		id, projectID, ownerID, status, variant string
	}{
		{"build-debug", "project-a", "public_" + ownerA, "building", "debug"},
		{"build-done", "project-b", "public_" + ownerA, "success", "release"},
		{"build-other-owner", "project-c", "public_" + ownerB, "queued", "release"},
	} {
		if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at)
			VALUES(?,?,?,?,?,?)`, build.id, build.projectID, build.ownerID, build.status, build.variant, "now"); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{db: db, config: AppConfig{Mode: "public"}}
	request := httptest.NewRequest(http.MethodGet, "/api/builds/active", nil)
	request.AddCookie(&http.Cookie{Name: "ide_owner", Value: ownerA})
	response := httptest.NewRecorder()
	server.handleActiveBuilds(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("expected active build list, got %d: %s", response.Code, response.Body.String())
	}
	var builds []Build
	if err := json.Unmarshal(response.Body.Bytes(), &builds); err != nil {
		t.Fatal(err)
	}
	if len(builds) != 1 || builds[0].ID != "build-debug" || builds[0].ProjectID != "project-a" ||
		builds[0].Variant != "debug" || builds[0].Status != "building" {
		t.Fatalf("unexpected active builds for owner: %+v", builds)
	}
}
