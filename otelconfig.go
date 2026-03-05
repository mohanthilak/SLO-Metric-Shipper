package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath = "configs/otel-collector.yaml"
	defaultContainerName = "otel-collector"
)

// OtelConfigHandler manages OpenTelemetry Collector configuration
type OtelConfigHandler struct {
	dockerClient    *client.Client
	configPath      string
	containerName   string
	dockerAvailable bool
}

// NewOtelConfigHandler creates a new OTel config handler
func NewOtelConfigHandler() *OtelConfigHandler {
	handler := &OtelConfigHandler{
		configPath:    defaultConfigPath,
		containerName: defaultContainerName,
	}

	// Get container name from environment if set
	if envContainer := os.Getenv("OTEL_COLLECTOR_CONTAINER"); envContainer != "" {
		handler.containerName = envContainer
	}

	// Initialize Docker client (may fail if not in Docker environment)
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Printf("Warning: Docker client unavailable: %v. OTel config restart will not work.", err)
		handler.dockerAvailable = false
	} else {
		handler.dockerClient = dockerClient
		handler.dockerAvailable = true
		log.Printf("Docker client initialized. Container management enabled.")
	}

	return handler
}

// GetConfig returns the current OTel collector configuration
func (h *OtelConfigHandler) GetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Read the current config file
	configData, err := os.ReadFile(h.configPath)
	if err != nil {
		log.Printf("Error reading OTel config: %v", err)
		http.Error(w, fmt.Sprintf("Failed to read config: %v", err), http.StatusInternalServerError)
		return
	}

	// Return as JSON with the YAML content as a string
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"config": string(configData),
		"path":   h.configPath,
	})
}

// UpdateConfig updates the OTel collector configuration and restarts the container
func (h *OtelConfigHandler) UpdateConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var req struct {
		Config string `json:"config"`
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read request body: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, fmt.Sprintf("Invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	// Validate YAML syntax
	if err := h.validateYAML(req.Config); err != nil {
		http.Error(w, fmt.Sprintf("Invalid YAML: %v", err), http.StatusBadRequest)
		return
	}

	// Backup current config
	backupPath := h.configPath + ".backup"
	currentConfig, err := os.ReadFile(h.configPath)
	if err == nil {
		os.WriteFile(backupPath, currentConfig, 0644)
	}

	// Write new config
	if err := os.WriteFile(h.configPath, []byte(req.Config), 0644); err != nil {
		log.Printf("Error writing OTel config: %v", err)
		http.Error(w, fmt.Sprintf("Failed to write config: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("OTel collector config updated successfully")

	// Restart the collector container if Docker is available
	var restartError error
	if h.dockerAvailable {
		restartError = h.restartCollector()
		if restartError != nil {
			log.Printf("Warning: Failed to restart collector: %v", restartError)
			// Restore backup
			if currentConfig != nil {
				os.WriteFile(h.configPath, currentConfig, 0644)
			}
			http.Error(w, fmt.Sprintf("Config updated but failed to restart collector: %v", restartError), http.StatusInternalServerError)
			return
		}
		log.Printf("OTel collector restarted successfully")
	}

	// Return success
	w.Header().Set("Content-Type", "application/json")
	response := map[string]interface{}{
		"status":  "success",
		"message": "Configuration updated successfully",
	}
	if !h.dockerAvailable {
		response["warning"] = "Docker unavailable - please restart collector manually"
	} else {
		response["message"] = "Configuration updated and collector restarted"
	}

	json.NewEncoder(w).Encode(response)
}

// validateYAML validates that the given string is valid YAML
func (h *OtelConfigHandler) validateYAML(yamlContent string) error {
	var data interface{}
	if err := yaml.Unmarshal([]byte(yamlContent), &data); err != nil {
		return fmt.Errorf("YAML parsing error: %v", err)
	}

	// Basic structure validation - check for required sections
	if dataMap, ok := data.(map[string]interface{}); ok {
		requiredSections := []string{"receivers", "exporters", "service"}
		for _, section := range requiredSections {
			if _, exists := dataMap[section]; !exists {
				return fmt.Errorf("missing required section: %s", section)
			}
		}
	}

	return nil
}

// restartCollector restarts the OTel collector container using Docker API
func (h *OtelConfigHandler) restartCollector() error {
	if !h.dockerAvailable {
		return fmt.Errorf("Docker client not available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Find the container
	containers, err := h.dockerClient.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list containers: %v", err)
	}

	var containerID string
	for _, c := range containers {
		for _, name := range c.Names {
			// Container names have a leading slash
			if name == "/"+h.containerName || name == h.containerName {
				containerID = c.ID
				break
			}
		}
		if containerID != "" {
			break
		}
	}

	if containerID == "" {
		return fmt.Errorf("container %s not found", h.containerName)
	}

	// Restart the container
	timeout := 10 // seconds
	if err := h.dockerClient.ContainerRestart(ctx, containerID, container.StopOptions{
		Timeout: &timeout,
	}); err != nil {
		return fmt.Errorf("failed to restart container: %v", err)
	}

	// Wait a moment for the container to come back up
	time.Sleep(2 * time.Second)

	return nil
}

// Close closes the Docker client connection
func (h *OtelConfigHandler) Close() {
	if h.dockerClient != nil {
		h.dockerClient.Close()
	}
}
