package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type HostResourceMetrics struct {
	CPUPercent           float64 `json:"cpuPercent"`
	MemoryUsedBytes      uint64  `json:"memoryUsedBytes"`
	MemoryTotalBytes     uint64  `json:"memoryTotalBytes"`
	MemoryAvailableBytes uint64  `json:"memoryAvailableBytes"`
	MemoryPercent        float64 `json:"memoryPercent"`
	SampledAt            string  `json:"sampledAt"`
}

type BuildResourceMetrics struct {
	CPUPercent          float64 `json:"cpuPercent"`
	MemoryUsedBytes     uint64  `json:"memoryUsedBytes"`
	MemoryLimitBytes    uint64  `json:"memoryLimitBytes"`
	MemoryPercent       float64 `json:"memoryPercent"`
	PeakCPUPercent      float64 `json:"peakCpuPercent"`
	PeakMemoryUsedBytes uint64  `json:"peakMemoryUsedBytes"`
	SampledAt           string  `json:"sampledAt"`
}

type BuildMonitorResponse struct {
	BuildID    string                `json:"buildId"`
	Status     string                `json:"status"`
	Variant    string                `json:"variant"`
	CreatedAt  string                `json:"createdAt"`
	StartedAt  string                `json:"startedAt,omitempty"`
	FinishedAt string                `json:"finishedAt,omitempty"`
	Error      string                `json:"error,omitempty"`
	Metrics    *BuildResourceMetrics `json:"metrics,omitempty"`
}

type hostCPUCounters struct {
	total uint64
	idle  uint64
}

func (s *Server) monitorAuthorized(r *http.Request) bool {
	if s.hasValidSession(r) {
		return true
	}
	key := r.Header.Get("X-API-Key")
	return key != "" && len(key) == len(s.config.Password) &&
		subtle.ConstantTimeCompare([]byte(key), []byte(s.config.Password)) == 1
}

func (s *Server) handleHostMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Metode HTTP tidak diizinkan.", http.StatusMethodNotAllowed)
		return
	}
	if s.config.Mode != "private" {
		http.Error(w, "Pemantauan server hanya tersedia dalam mode privat.", http.StatusForbidden)
		return
	}
	if !s.monitorAuthorized(r) {
		http.Error(w, "Kunci API atau sesi tidak valid.", http.StatusUnauthorized)
		return
	}
	s.monitorMu.RLock()
	metrics, available := s.hostMetrics, s.hostMetricsOK
	s.monitorMu.RUnlock()
	if !available {
		http.Error(w, "Metrik server belum tersedia. Periksa konfigurasi mount /host-metrics.", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

func (s *Server) handleBuildMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Metode HTTP tidak diizinkan.", http.StatusMethodNotAllowed)
		return
	}
	buildID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/monitor/builds/"), "/")
	if !validBuildID(buildID) || strings.Contains(buildID, "/") {
		http.NotFound(w, r)
		return
	}
	if s.config.Mode != "public" && !s.monitorAuthorized(r) {
		http.Error(w, "Kunci API atau sesi tidak valid.", http.StatusUnauthorized)
		return
	}
	var response BuildMonitorResponse
	var mode string
	err := s.db.QueryRow(`SELECT id,status,variant,created_at,COALESCE(started_at,''),
		COALESCE(finished_at,''),COALESCE(error,''),access_mode FROM builds WHERE id=?`, buildID).
		Scan(&response.BuildID, &response.Status, &response.Variant, &response.CreatedAt,
			&response.StartedAt, &response.FinishedAt, &response.Error, &mode)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Gagal membaca status build.", http.StatusInternalServerError)
		return
	}
	if s.config.Mode == "public" {
		if mode != "public" {
			http.NotFound(w, r)
			return
		}
		response.Error = ""
	} else {
		if mode != "private" {
			http.NotFound(w, r)
			return
		}
		metrics, ok := s.currentBuildResourceMetrics(buildID)
		if ok {
			response.Metrics = &metrics
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) runHostMetricsSampler() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var previous hostCPUCounters
	var initialized bool
	lastError := ""
	for {
		metrics, current, err := readHostResourceMetrics(s.hostMetricsDir, previous, initialized, time.Now())
		if err != nil {
			s.monitorMu.Lock()
			s.hostMetricsOK = false
			s.monitorMu.Unlock()
			if err.Error() != lastError {
				log.Printf("host resource monitor unavailable: %v", err)
				lastError = err.Error()
			}
		} else {
			s.monitorMu.Lock()
			s.hostMetrics = metrics
			s.hostMetricsOK = true
			s.monitorMu.Unlock()
			previous, initialized, lastError = current, true, ""
		}
		<-ticker.C
	}
}

func readHostResourceMetrics(procDir string, previous hostCPUCounters, initialized bool, now time.Time) (HostResourceMetrics, hostCPUCounters, error) {
	stat, err := os.Open(procDir + "/stat")
	if err != nil {
		return HostResourceMetrics{}, hostCPUCounters{}, err
	}
	defer stat.Close()
	scanner := bufio.NewScanner(stat)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return HostResourceMetrics{}, hostCPUCounters{}, err
		}
		return HostResourceMetrics{}, hostCPUCounters{}, errors.New("host CPU counters are empty")
	}
	counters, err := parseHostCPUCounters(scanner.Text())
	if err != nil {
		return HostResourceMetrics{}, hostCPUCounters{}, err
	}
	meminfo, err := os.Open(procDir + "/meminfo")
	if err != nil {
		return HostResourceMetrics{}, hostCPUCounters{}, err
	}
	defer meminfo.Close()
	values := make(map[string]uint64)
	memScanner := bufio.NewScanner(meminfo)
	for memScanner.Scan() {
		fields := strings.Fields(memScanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil {
			return HostResourceMetrics{}, hostCPUCounters{}, fmt.Errorf("invalid host memory value for %s", fields[0])
		}
		if len(fields) > 2 && fields[2] == "kB" {
			value *= 1024
		}
		values[strings.TrimSuffix(fields[0], ":")] = value
	}
	if err := memScanner.Err(); err != nil {
		return HostResourceMetrics{}, hostCPUCounters{}, err
	}
	total, hasTotal := values["MemTotal"]
	available, hasAvailable := values["MemAvailable"]
	if !hasTotal || !hasAvailable || total == 0 || available > total {
		return HostResourceMetrics{}, hostCPUCounters{}, errors.New("host memory counters are incomplete")
	}
	metrics := HostResourceMetrics{
		MemoryTotalBytes: total, MemoryAvailableBytes: available,
		MemoryUsedBytes: total - available, SampledAt: now.UTC().Format(time.RFC3339Nano),
	}
	metrics.MemoryPercent = float64(metrics.MemoryUsedBytes) * 100 / float64(total)
	if initialized && counters.total > previous.total {
		elapsed := counters.total - previous.total
		idle := counters.idle - previous.idle
		if idle <= elapsed {
			metrics.CPUPercent = float64(elapsed-idle) * 100 / float64(elapsed)
		}
	}
	return metrics, counters, nil
}

func parseHostCPUCounters(line string) (hostCPUCounters, error) {
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return hostCPUCounters{}, errors.New("invalid host CPU counters")
	}
	var counters hostCPUCounters
	for index, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return hostCPUCounters{}, errors.New("invalid host CPU counter value")
		}
		counters.total += value
		if index == 3 || index == 4 {
			counters.idle += value
		}
	}
	return counters, nil
}

func parseDockerStats(output string) (float64, uint64, uint64, error) {
	fields := strings.Split(strings.TrimSpace(output), "|")
	if len(fields) != 2 {
		return 0, 0, 0, errors.New("invalid docker stats response")
	}
	cpuText := strings.TrimSpace(strings.TrimSuffix(fields[0], "%"))
	cpu, err := strconv.ParseFloat(cpuText, 64)
	if err != nil || cpu < 0 {
		return 0, 0, 0, errors.New("invalid docker CPU percentage")
	}
	memory := strings.Split(fields[1], "/")
	if len(memory) != 2 {
		return 0, 0, 0, errors.New("invalid docker memory usage")
	}
	used, err := parseDockerByteValue(memory[0])
	if err != nil {
		return 0, 0, 0, err
	}
	limit, err := parseDockerByteValue(memory[1])
	if err != nil || limit == 0 {
		return 0, 0, 0, errors.New("invalid docker memory limit")
	}
	return cpu, used, limit, nil
}

func parseDockerByteValue(value string) (uint64, error) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) != 1 {
		return 0, errors.New("invalid docker byte value")
	}
	text := fields[0]
	units := []struct {
		suffix string
		scale  float64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1},
	}
	scale := float64(1)
	for _, unit := range units {
		if strings.HasSuffix(text, unit.suffix) {
			text = strings.TrimSuffix(text, unit.suffix)
			scale = unit.scale
			break
		}
	}
	amount, err := strconv.ParseFloat(text, 64)
	if err != nil || amount < 0 {
		return 0, errors.New("invalid docker byte amount")
	}
	return uint64(amount * scale), nil
}

func (s *Server) monitorBuildContainer(ctx context.Context, buildID, containerName string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	hasSample := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sampleCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		output, err := exec.CommandContext(sampleCtx, "docker", "stats", "--no-stream",
			"--format", "{{.CPUPerc}}|{{.MemUsage}}", containerName).Output()
		cancel()
		if err != nil {
			if hasSample && ctx.Err() == nil {
				log.Printf("build resource monitor stopped for %s: %v", buildID, err)
				return
			}
			continue
		}
		cpu, used, limit, err := parseDockerStats(string(output))
		if err != nil {
			log.Printf("invalid build resource stats for %s: %v", buildID, err)
			return
		}
		now := time.Now().UTC()
		s.recordBuildResourceSample(buildID, BuildResourceMetrics{
			CPUPercent: cpu, MemoryUsedBytes: used, MemoryLimitBytes: limit,
			MemoryPercent: float64(used) * 100 / float64(limit),
			SampledAt:     now.Format(time.RFC3339Nano),
		})
		hasSample = true
	}
}

func (s *Server) recordBuildResourceSample(buildID string, sample BuildResourceMetrics) {
	now := time.Now()
	s.monitorMu.Lock()
	if s.buildMetrics == nil {
		s.buildMetrics = make(map[string]BuildResourceMetrics)
	}
	for id, metrics := range s.buildMetrics {
		sampledAt, err := time.Parse(time.RFC3339Nano, metrics.SampledAt)
		if err != nil || now.Sub(sampledAt) > time.Hour {
			delete(s.buildMetrics, id)
		}
	}
	metrics := s.buildMetrics[buildID]
	if sample.CPUPercent > metrics.PeakCPUPercent {
		metrics.PeakCPUPercent = sample.CPUPercent
	}
	if sample.MemoryUsedBytes > metrics.PeakMemoryUsedBytes {
		metrics.PeakMemoryUsedBytes = sample.MemoryUsedBytes
	}
	metrics.CPUPercent = sample.CPUPercent
	metrics.MemoryUsedBytes = sample.MemoryUsedBytes
	metrics.MemoryLimitBytes = sample.MemoryLimitBytes
	metrics.MemoryPercent = sample.MemoryPercent
	metrics.SampledAt = sample.SampledAt
	s.buildMetrics[buildID] = metrics
	s.monitorMu.Unlock()
}

func (s *Server) currentBuildResourceMetrics(buildID string) (BuildResourceMetrics, bool) {
	s.monitorMu.Lock()
	defer s.monitorMu.Unlock()
	metrics, ok := s.buildMetrics[buildID]
	if !ok {
		return BuildResourceMetrics{}, false
	}
	sampledAt, err := time.Parse(time.RFC3339Nano, metrics.SampledAt)
	if err != nil || time.Since(sampledAt) > time.Hour {
		delete(s.buildMetrics, buildID)
		return BuildResourceMetrics{}, false
	}
	return metrics, true
}
