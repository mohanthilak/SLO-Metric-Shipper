package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
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
	config       *Config
	loadGen      *LoadGenerator
}

// NewAdminHandler creates a new admin handler
func NewAdminHandler(config *Config, loadGen *LoadGenerator) *AdminHandler {
	return &AdminHandler{
		config:  config,
		loadGen: loadGen,
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

// SetupAdminHandlers registers all admin-related handlers
func SetupAdminHandlers(mux *http.ServeMux, config *Config, loadGen *LoadGenerator) {
	handler := NewAdminHandler(config, loadGen)

	// Initialize OTel config handler
	otelHandler := NewOtelConfigHandler()

	mux.HandleFunc("/admin", handler.ServeAdminUI)
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
