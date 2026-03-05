package main

import (
	"encoding/json"
	"net/http"
)

// APIHandler handles requests to simulated API endpoints
type APIHandler struct {
	config       *Config
	endpointName string
}

// NewAPIHandler creates a new API handler for a specific endpoint
func NewAPIHandler(config *Config, endpointName string) *APIHandler {
	return &APIHandler{
		config:       config,
		endpointName: endpointName,
	}
}

// ServeHTTP handles the HTTP request
func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Get random status code based on configured distribution
	statusCode := h.config.GetRandomStatusCode(h.endpointName)

	// Record metric
	RecordRequest(h.endpointName, statusCode, r.Method)

	// Set content type
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	// Generate appropriate response based on status code
	var response interface{}

	switch statusCode {
	case 200:
		response = map[string]interface{}{
			"status": "success",
			"data": map[string]interface{}{
				"endpoint": h.endpointName,
				"message":  "Request processed successfully",
			},
		}
	case 400:
		response = map[string]interface{}{
			"error":   "bad request",
			"message": "Invalid parameters",
		}
	case 404:
		response = map[string]interface{}{
			"error":   "not found",
			"message": "Resource not found",
		}
	case 429:
		response = map[string]interface{}{
			"error":   "too many requests",
			"message": "Rate limit exceeded",
		}
	case 500:
		response = map[string]interface{}{
			"error":   "internal server error",
			"message": "Something went wrong",
		}
	case 503:
		response = map[string]interface{}{
			"error":   "service unavailable",
			"message": "Service temporarily unavailable",
		}
	default:
		response = map[string]interface{}{
			"error":   "unknown error",
			"message": "An unexpected error occurred",
		}
	}

	json.NewEncoder(w).Encode(response)
}

// SetupAPIHandlers registers all API endpoint handlers
func SetupAPIHandlers(mux *http.ServeMux, config *Config) {
	endpoints := map[string]string{
		"/api/users":    "users",
		"/api/products": "products",
		"/api/orders":   "orders",
		"/api/checkout": "checkout",
		"/api/health":   "health",
	}

	for path, name := range endpoints {
		mux.Handle(path, NewAPIHandler(config, name))
	}
}
