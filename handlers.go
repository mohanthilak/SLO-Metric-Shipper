package main

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
	ctx := r.Context()

	// Get random status code based on configured distribution
	statusCode := h.config.GetRandomStatusCode(h.endpointName)

	// Synthetic work spans so the trace tree has shape — cache lookup then db query.
	h.syntheticChildSpan(ctx, "cache.lookup", 2, 8, map[string]string{
		"cache.system": "redis",
		"cache.key":    h.endpointName + ":lookup",
	})
	h.syntheticChildSpan(ctx, "db.query", 5, 40, map[string]string{
		"db.system":    "postgresql",
		"db.statement": "SELECT * FROM " + h.endpointName + " LIMIT 100",
	})

	// Annotate the parent server span with endpoint/status info and mark errors.
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String("api.endpoint", h.endpointName),
		attribute.Int("http.response.status_code", statusCode),
	)
	if statusCode >= 500 {
		span.SetStatus(codes.Error, "upstream error")
	}

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

// syntheticChildSpan starts a child span with a randomized duration to simulate work.
func (h *APIHandler) syntheticChildSpan(ctx context.Context, name string, minMs, maxMs int, attrs map[string]string) {
	if tracer == nil {
		return
	}
	_, span := tracer.Start(ctx, name)
	defer span.End()

	for k, v := range attrs {
		span.SetAttributes(attribute.String(k, v))
	}

	durMs := minMs + rand.Intn(maxMs-minMs+1)
	time.Sleep(time.Duration(durMs) * time.Millisecond)
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
