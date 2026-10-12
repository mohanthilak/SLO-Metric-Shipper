package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
)

// ConfigRequest represents a request to update endpoint configuration
type ConfigRequest struct {
	Endpoint        string  `json:"endpoint"`
	SuccessRate     float64 `json:"successRate"`
	ClientErrorRate float64 `json:"clientErrorRate"`
	ServerErrorRate float64 `json:"serverErrorRate"`
}

// ConfigResponse represents the response with all endpoint configurations
type ConfigResponse struct {
	Endpoints map[string]StatusCodeDistribution `json:"endpoints"`
}

// AdminHandler handles admin UI requests
type AdminHandler struct {
	config        *Config
	loadGen       *LoadGenerator
	logLoadGen    *LogLoadGenerator
	metricLoadGen *MetricLoadGenerator
	traceLoadGen  *TraceLoadGenerator
}

// NewAdminHandler creates a new admin handler
func NewAdminHandler(config *Config, loadGen *LoadGenerator, logLoadGen *LogLoadGenerator, metricLoadGen *MetricLoadGenerator, traceLoadGen *TraceLoadGenerator) *AdminHandler {
	return &AdminHandler{
		config:        config,
		loadGen:       loadGen,
		logLoadGen:    logLoadGen,
		metricLoadGen: metricLoadGen,
		traceLoadGen:  traceLoadGen,
	}
}

// ServeAdminUI serves the admin HTML page
func (h *AdminHandler) ServeAdminUI(w http.ResponseWriter, r *http.Request) {
	// Read the admin HTML file
	htmlContent, err := os.ReadFile("static/admin.html")
	if err != nil {
		http.Error(w, "Failed to load admin UI", http.StatusInternalServerError)
		log.Printf("Error reading admin.html: %v", err)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.Write(htmlContent)
}

// GetConfig returns current configurations for all endpoints
func (h *AdminHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	configs := h.config.GetAllConfigs()
	response := ConfigResponse{Endpoints: configs}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// UpdateConfig updates the configuration for a specific endpoint
func (h *AdminHandler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	// Validate that rates sum to 1.0 (allowing small floating point errors)
	total := req.SuccessRate + req.ClientErrorRate + req.ServerErrorRate
	if math.Abs(total-1.0) > 0.001 {
		http.Error(w, fmt.Sprintf("Rates must sum to 1.0, got %.3f", total), http.StatusBadRequest)
		return
	}

	// Validate rates are non-negative
	if req.SuccessRate < 0 || req.ClientErrorRate < 0 || req.ServerErrorRate < 0 {
		http.Error(w, "Rates must be non-negative", http.StatusBadRequest)
		return
	}

	// Update configuration
	h.config.SetStatusCodeRates(req.Endpoint, req.SuccessRate, req.ClientErrorRate, req.ServerErrorRate)

	log.Printf("Updated config for %s: success=%.2f%%, client_error=%.2f%%, server_error=%.2f%%",
		req.Endpoint, req.SuccessRate*100, req.ClientErrorRate*100, req.ServerErrorRate*100)

	// Return success response
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Configuration updated successfully",
	})
}

// StartLoadGeneration starts load generation for an endpoint
func (h *AdminHandler) StartLoadGeneration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req LoadJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	// Validate input
	if req.RequestsPerSec <= 0 || req.RequestsPerSec > 1000 {
		http.Error(w, "RequestsPerSec must be between 1 and 1000", http.StatusBadRequest)
		return
	}

	if req.TotalRequests < 0 {
		http.Error(w, "TotalRequests must be non-negative", http.StatusBadRequest)
		return
	}

	// Start load generation
	if err := h.loadGen.StartLoadJob(req.Endpoint, req.RequestsPerSec, req.TotalRequests); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	log.Printf("Started load generation for %s: %d req/s, total: %d", req.Endpoint, req.RequestsPerSec, req.TotalRequests)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Load generation started",
	})
}

// StopLoadGeneration stops load generation for an endpoint
func (h *AdminHandler) StopLoadGeneration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}

	// Stop load generation
	if err := h.loadGen.StopLoadJob(req.Endpoint); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	log.Printf("Stopped load generation for %s", req.Endpoint)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Load generation stopped",
	})
}

// GetLoadStats returns load generation statistics for all endpoints
func (h *AdminHandler) GetLoadStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := h.loadGen.GetAllJobStats()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"stats": stats,
	})
}

// StartLogLoadGen kicks off a synthetic log job.
func (h *AdminHandler) StartLogLoadGen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req LogLoadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if err := h.logLoadGen.Start(req); err != nil {
		// Distinguish "already running" (409) from validation/config errors (400).
		if err.Error() == "a synthetic log job is already running" {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("Started synthetic log job: rate=%d bytes=%d format=%s kinds=%v sink=%s target=%s workers=%d dur=%d",
		req.RatePerSec, req.BytesPerLog, req.Format, req.Kinds, req.Sink, req.SinkTarget, req.Workers, req.DurationSec)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Synthetic log generation started",
	})
}

// StopLogLoadGen halts the currently running synthetic log job.
func (h *AdminHandler) StopLogLoadGen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.logLoadGen.Stop(); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("Stopped synthetic log job")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Synthetic log generation stopped",
	})
}

// GetLogLoadStats returns the current LogLoadStats snapshot.
func (h *AdminHandler) GetLogLoadStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	stats := h.logLoadGen.Stats()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// GetLogLoadMeta returns the format and sink registries so the UI dropdowns are
// server-driven. Also lists available replay samples so the UI can render them
// as extra "replay:<name>" options.
func (h *AdminHandler) GetLogLoadMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"formats":       FormatMetadata(),
		"kinds":         KindMetadata(),
		"sinks":         SinkMetadata(),
		"replaySamples": ListReplaySamples(),
	})
}

// UploadReplaySample accepts multipart form upload OR a JSON body {name, content}
// and saves it under configs/logloadgen-samples/. Names sanitised to [a-zA-Z0-9_-].
// Max size 5 MiB — this is a playground, not a data-lake.
func (h *AdminHandler) UploadReplaySample(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	const maxSampleBytes = 5 * 1024 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxSampleBytes+1024)

	var name string
	var content []byte

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(maxSampleBytes); err != nil {
			http.Error(w, fmt.Sprintf("upload too large or malformed: %v", err), http.StatusBadRequest)
			return
		}
		name = strings.TrimSpace(r.FormValue("name"))
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing 'file' form field", http.StatusBadRequest)
			return
		}
		defer file.Close()
		buf := make([]byte, maxSampleBytes)
		n, _ := file.Read(buf)
		content = buf[:n]
	default:
		var body struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
			return
		}
		name = strings.TrimSpace(body.Name)
		content = []byte(body.Content)
	}

	if name == "" {
		http.Error(w, "sample name required", http.StatusBadRequest)
		return
	}
	if len(content) == 0 {
		http.Error(w, "sample content is empty", http.StatusBadRequest)
		return
	}
	if err := SaveReplaySample(name, content); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Printf("Saved replay sample %q (%d bytes)", name, len(content))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": fmt.Sprintf("Saved %d bytes as replay:%s", len(content), name),
		"name":    name,
		"bytes":   len(content),
	})
}

// --- Metric load generator handlers ---

func (h *AdminHandler) StartMetricLoadGen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req MetricLoadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if err := h.metricLoadGen.Start(req); err != nil {
		status := http.StatusBadRequest
		if err.Error() == "a synthetic metric job is already running" {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	log.Printf("Started metric load: rate=%d card=%d strategy=%s dist=%s kinds=%v dur=%d",
		req.RatePerSec, req.Cardinality, req.CardStrategy, req.HistogramDist, req.Kinds, req.DurationSec)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Synthetic metric generation started",
	})
}

func (h *AdminHandler) StopMetricLoadGen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.metricLoadGen.Stop(); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Synthetic metric generation stopped",
	})
}

func (h *AdminHandler) GetMetricLoadStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.metricLoadGen.Stats())
}

func (h *AdminHandler) GetMetricLoadMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	kindsMeta := make([]map[string]string, 0, len(AllMetricKinds))
	labels := map[string]string{
		"counter":                  "Int64Counter (sync, monotonic)",
		"updowncounter":            "Int64UpDownCounter (sync, up/down)",
		"histogram":                "Float64Histogram (sync, distribution)",
		"gauge":                    "Float64Gauge (sync, snapshot)",
		"observable_counter":       "Float64ObservableCounter (async, monotonic)",
		"observable_updowncounter": "Int64ObservableUpDownCounter (async, up/down)",
		"observable_gauge":         "Float64ObservableGauge (async, snapshot)",
	}
	for _, k := range AllMetricKinds {
		kindsMeta = append(kindsMeta, map[string]string{"key": k, "label": labels[k]})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"kinds":            kindsMeta,
		"cardStrategies":   AllCardStrategies,
		"histogramDists":   AllHistogramDists,
	})
}

// --- Trace load generator handlers ---

func (h *AdminHandler) StartTraceLoadGen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req TraceLoadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid request body: %v", err), http.StatusBadRequest)
		return
	}
	if err := h.traceLoadGen.Start(req); err != nil {
		status := http.StatusBadRequest
		if err.Error() == "a synthetic trace job is already running" {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	log.Printf("Started trace load: tps=%d depth=%d fanout=%d svcs=%d hicard=%v dur=%d",
		req.TracesPerSec, req.Depth, req.Fanout, req.ServiceCount, req.HighCardAttrs, req.DurationSec)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Synthetic trace generation started",
	})
}

func (h *AdminHandler) StopTraceLoadGen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.traceLoadGen.Stop(); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Synthetic trace generation stopped",
	})
}

func (h *AdminHandler) GetTraceLoadStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.traceLoadGen.Stats())
}

func (h *AdminHandler) GetTraceLoadMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"durationProfiles": AllDurationProfiles,
	})
}

// SetupAdminHandlers registers all admin-related handlers
func SetupAdminHandlers(mux *http.ServeMux, config *Config, loadGen *LoadGenerator, logLoadGen *LogLoadGenerator, metricLoadGen *MetricLoadGenerator, traceLoadGen *TraceLoadGenerator) {
	handler := NewAdminHandler(config, loadGen, logLoadGen, metricLoadGen, traceLoadGen)

	// Initialize OTel config handler
	otelHandler := NewOtelConfigHandler()

	mux.HandleFunc("/admin", handler.ServeAdminUI)

	// Static assets (Chart.js, future vendored JS/CSS). Serves the whole static/
	// directory over /static/ — admin.html is redundantly reachable at
	// /static/admin.html but nothing there is sensitive.
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.HandleFunc("/admin/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			handler.GetConfig(w, r)
		} else if r.Method == http.MethodPost {
			handler.UpdateConfig(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/admin/api/loadgen/start", handler.StartLoadGeneration)
	mux.HandleFunc("/admin/api/loadgen/stop", handler.StopLoadGeneration)
	mux.HandleFunc("/admin/api/loadgen/stats", handler.GetLoadStats)

	// Synthetic log load generator
	mux.HandleFunc("/admin/api/logloadgen/start", handler.StartLogLoadGen)
	mux.HandleFunc("/admin/api/logloadgen/stop", handler.StopLogLoadGen)
	mux.HandleFunc("/admin/api/logloadgen/stats", handler.GetLogLoadStats)
	mux.HandleFunc("/admin/api/logloadgen/meta", handler.GetLogLoadMeta)
	mux.HandleFunc("/admin/api/logloadgen/samples", handler.UploadReplaySample)

	// Synthetic metric load generator — all 7 OTel instrument kinds + cardinality knob
	mux.HandleFunc("/admin/api/metricloadgen/start", handler.StartMetricLoadGen)
	mux.HandleFunc("/admin/api/metricloadgen/stop", handler.StopMetricLoadGen)
	mux.HandleFunc("/admin/api/metricloadgen/stats", handler.GetMetricLoadStats)
	mux.HandleFunc("/admin/api/metricloadgen/meta", handler.GetMetricLoadMeta)

	// Synthetic trace load generator
	mux.HandleFunc("/admin/api/traceloadgen/start", handler.StartTraceLoadGen)
	mux.HandleFunc("/admin/api/traceloadgen/stop", handler.StopTraceLoadGen)
	mux.HandleFunc("/admin/api/traceloadgen/stats", handler.GetTraceLoadStats)
	mux.HandleFunc("/admin/api/traceloadgen/meta", handler.GetTraceLoadMeta)

	// OTel collector config management endpoints
	mux.HandleFunc("/admin/api/otel-config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			otelHandler.GetConfig(w, r)
		} else if r.Method == http.MethodPost {
			otelHandler.UpdateConfig(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
}
