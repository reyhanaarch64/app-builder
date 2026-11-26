package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadHostResourceMetrics(t *testing.T) {
	procDir := t.TempDir()
	writeProc := func(stat string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(procDir, "stat"), []byte(stat), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(procDir, "meminfo"),
			[]byte("MemTotal: 1000 kB\nMemAvailable: 400 kB\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProc("cpu 100 0 50 850 0 0 0 0\n")
	first, counters, err := readHostResourceMetrics(procDir, hostCPUCounters{}, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if first.CPUPercent != 0 || first.MemoryPercent != 60 ||
		first.MemoryUsedBytes != 600*1024 || first.MemoryTotalBytes != 1000*1024 {
		t.Fatalf("unexpected first host sample: %+v", first)
	}
	writeProc("cpu 110 0 55 875 0 0 0 0\n")
	second, _, err := readHostResourceMetrics(procDir, counters, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if second.CPUPercent != 37.5 {
		t.Fatalf("CPU percent = %v, want 37.5", second.CPUPercent)
	}
}

func TestParseDockerStats(t *testing.T) {
	cpu, used, limit, err := parseDockerStats("12.5%|128MiB / 2GiB\n")
	if err != nil {
		t.Fatal(err)
	}
	if cpu != 12.5 || used != 128<<20 || limit != 2<<30 {
		t.Fatalf("unexpected Docker stats: cpu=%v used=%d limit=%d", cpu, used, limit)
	}
	if _, _, _, err := parseDockerStats("not stats"); err == nil {
		t.Fatal("expected invalid Docker stats to fail")
	}
}

func TestBuildMonitorRequiresAPIKeyInPrivateMode(t *testing.T) {
	db := testDB(t)
	buildID := "bld_0123456789abcdef0123456789abcdef"
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at,access_mode)
		VALUES(?, 'project-a', 'owner-a', 'building', 'debug', ?, 'private')`, buildID, now); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db: db, config: AppConfig{Mode: "private", Password: "monitor-secret"},
		sessions: map[string]session{},
		buildMetrics: map[string]BuildResourceMetrics{
			buildID: {
				CPUPercent: 18.5, PeakCPUPercent: 27.1, MemoryUsedBytes: 64 << 20,
				SampledAt: time.Now().UTC().Format(time.RFC3339Nano),
			},
		},
	}
	handler := server.authMiddleware(http.HandlerFunc(server.handleBuildMonitor))
	request := httptest.NewRequest(http.MethodGet, "/api/monitor/builds/"+buildID, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated monitor returned %d, want unauthorized", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/monitor/builds/bld_00000000000000000000000000000000", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("private build ID without API key returned %d, want unauthorized", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/monitor/builds/"+buildID, nil)
	request.Header.Set("X-API-Key", "wrong")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("wrong API key returned %d, want unauthorized", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/monitor/builds/"+buildID, nil)
	request.Header.Set("X-API-Key", "monitor-secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("valid API key returned %d: %s", response.Code, response.Body.String())
	}
	var result BuildMonitorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "building" || result.Metrics == nil ||
		result.Metrics.CPUPercent != 18.5 || result.Metrics.PeakCPUPercent != 27.1 {
		t.Fatalf("unexpected private build monitor result: %+v", result)
	}
}

func TestPublicBuildMonitorIsIDScopedAndOmitsResourceMetrics(t *testing.T) {
	db := testDB(t)
	buildID := "bld_abcdef0123456789abcdef0123456789"
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at,access_mode)
		VALUES(?, 'project-public', 'owner-public', 'queued', 'release', ?, 'public')`, buildID, now); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db, config: AppConfig{Mode: "public"}}
	request := httptest.NewRequest(http.MethodGet, "/api/monitor/builds/"+buildID, nil)
	response := httptest.NewRecorder()
	server.authMiddleware(http.HandlerFunc(server.handleBuildMonitor)).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("public build monitor returned %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "metrics") || strings.Contains(response.Body.String(), "project-public") {
		t.Fatalf("public monitor exposed private metrics or project data: %s", response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/monitor/builds/bld_00000000000000000000000000000000", nil)
	response = httptest.NewRecorder()
	server.authMiddleware(http.HandlerFunc(server.handleBuildMonitor)).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown public build returned %d, want not found", response.Code)
	}
}

func TestBuildLogMonitorStreamsLogsWithAPIKeyAndNoStatusPayload(t *testing.T) {
	db := testDB(t)
	buildID := "bld_1234567890abcdef1234567890abcdef"
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at,finished_at,access_mode)
		VALUES(?, 'project-a', 'owner-a', 'success', 'debug', ?, ?, 'private')`, buildID, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO build_logs(build_id,sequence,stream,message,created_at)
		VALUES(?,1,'stdout','Gradle task started',?),(?,2,'stderr','Compiler warning',?)`,
		buildID, now, buildID, now); err != nil {
		t.Fatal(err)
	}
	server := &Server{
		db: db, config: AppConfig{Mode: "private", Password: "monitor-secret"},
		sessions: map[string]session{},
	}
	handler := server.authMiddleware(http.HandlerFunc(server.handleBuildMonitor))
	request := httptest.NewRequest(http.MethodGet, "/api/monitor/builds/"+buildID+"/logs", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated log monitor returned %d, want unauthorized", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/monitor/builds/"+buildID+"/logs", nil)
	request.Header.Set("X-API-Key", "monitor-secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("log stream returned %d with content type %q: %s",
			response.Code, response.Header().Get("Content-Type"), body)
	}
	if !strings.Contains(body, "data: Gradle task started\n\n") ||
		!strings.Contains(body, "data: [stderr] Compiler warning\n\n") {
		t.Fatalf("stream is missing build log lines: %s", body)
	}
	if strings.Contains(body, "event: end") || strings.Contains(body, "data: success") {
		t.Fatalf("log-only stream exposed build status: %s", body)
	}
}

func TestPublicBuildLogMonitorOnlyStreamsPublicBuilds(t *testing.T) {
	db := testDB(t)
	now := time.Now().UTC().Format(time.RFC3339)
	publicID := "bld_abcdef0123456789abcdef0123456789"
	privateID := "bld_0123456789abcdef0123456789abcdef"
	for _, build := range []struct {
		id         string
		accessMode string
	}{
		{id: publicID, accessMode: "public"},
		{id: privateID, accessMode: "private"},
	} {
		if _, err := db.Exec(`INSERT INTO builds(id,project_id,owner_id,status,variant,created_at,finished_at,access_mode)
			VALUES(?, 'project', 'owner', 'success', 'debug', ?, ?, ?)`,
			build.id, now, now, build.accessMode); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{db: db, config: AppConfig{Mode: "public"}}
	handler := server.authMiddleware(http.HandlerFunc(server.handleBuildMonitor))
	request := httptest.NewRequest(http.MethodGet, "/api/monitor/builds/"+publicID+"/logs", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("public build log stream returned %d: %s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/api/monitor/builds/"+privateID+"/logs", nil)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("public monitor exposed private build logs with status %d", response.Code)
	}
}

func TestHostMonitorIsPrivateAndRequiresAuthentication(t *testing.T) {
	server := &Server{
		config:        AppConfig{Mode: "private", Password: "secret"},
		sessions:      map[string]session{},
		hostMetrics:   HostResourceMetrics{CPUPercent: 12, MemoryPercent: 35},
		hostMetricsOK: true,
	}
	handler := server.authMiddleware(http.HandlerFunc(server.handleHostMonitor))
	request := httptest.NewRequest(http.MethodGet, "/api/monitor/host", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("host metrics without API key returned %d, want unauthorized", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/monitor/host", nil)
	request.Header.Set("X-API-Key", "secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"cpuPercent":12`) {
		t.Fatalf("host metrics returned %d: %s", response.Code, response.Body.String())
	}

	public := &Server{config: AppConfig{Mode: "public"}}
	response = httptest.NewRecorder()
	public.handleHostMonitor(response, httptest.NewRequest(http.MethodGet, "/api/monitor/host", nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("public host metrics returned %d, want forbidden", response.Code)
	}
}
