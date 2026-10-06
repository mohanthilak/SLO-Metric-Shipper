package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

func main() {
	// Initialize context
	ctx := context.Background()

	// Initialize OpenTelemetry metrics
	if err := InitMetrics(ctx); err != nil {
		log.Fatalf("Failed to initialize metrics: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ShutdownMetrics(shutdownCtx); err != nil {
			log.Printf("Error shutting down metrics: %v", err)
		}
	}()

	// Initialize OpenTelemetry traces
	if err := InitTraces(ctx); err != nil {
		log.Fatalf("Failed to initialize traces: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ShutdownTraces(shutdownCtx); err != nil {
			log.Printf("Error shutting down traces: %v", err)
		}
	}()

	// Initialize OpenTelemetry logs
	if err := InitLogs(ctx); err != nil {
		log.Fatalf("Failed to initialize logs: %v", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ShutdownLogs(shutdownCtx); err != nil {
			log.Printf("Error shutting down logs: %v", err)
		}
	}()

	// Create configuration store
	config := NewConfig()

	// Listen address is HTTP_ADDR (e.g. ":8181") or defaults to :8080.
	listenAddr := os.Getenv("HTTP_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}
	selfBaseURL := "http://localhost" + listenAddr

	// Create load generator
	loadGen := NewLoadGenerator(selfBaseURL)

	// Create synthetic log load generator (reuses the OTel LoggerProvider from InitLogs).
	// Persists last-used config to configs/logloadgen.json — env override for CI/tests.
	logLoadgenConfigPath := os.Getenv("LOGLOADGEN_CONFIG_PATH")
	if logLoadgenConfigPath == "" {
		logLoadgenConfigPath = "configs/logloadgen.json"
	}
	otlpSink := NewOTLPSink(loggerProvider)
	logLoadGen := NewLogLoadGenerator(otlpSink, logLoadgenConfigPath)

	// Synthetic metric load generator — playground for OTel instrument kinds
	// and cardinality explosion. Uses the same meter set up by InitMetrics.
	metricLoadgenConfigPath := os.Getenv("METRICLOADGEN_CONFIG_PATH")
	if metricLoadgenConfigPath == "" {
		metricLoadgenConfigPath = "configs/metricloadgen.json"
	}
	metricLoadGen := NewMetricLoadGenerator(metricLoadgenConfigPath)

	// Synthetic trace load generator — playground for span topology, cardinality,
	// service-graph richness. Builds its own per-service TracerProviders.
	traceLoadgenConfigPath := os.Getenv("TRACELOADGEN_CONFIG_PATH")
	if traceLoadgenConfigPath == "" {
		traceLoadgenConfigPath = "configs/traceloadgen.json"
	}
	traceLoadGen := NewTraceLoadGenerator(traceLoadgenConfigPath)
	defer func() {
		if err := logLoadGen.Stop(); err != nil {
			// Nothing running is fine at shutdown.
			_ = err
		}
	}()

	// Create HTTP mux
	mux := http.NewServeMux()

	// Setup API handlers
	SetupAPIHandlers(mux, config)

	// Setup admin handlers
	SetupAdminHandlers(mux, config, loadGen, logLoadGen, metricLoadGen, traceLoadGen)

	// Wrap the mux so inbound requests produce server-side spans.
	tracedHandler := otelhttp.NewHandler(mux, "http.server",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)

	// Create HTTP server
	server := &http.Server{
		Addr:         listenAddr,
		Handler:      tracedHandler,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in a goroutine
	go func() {
		log.Printf("Starting SLO Metric Generator on %s", selfBaseURL)
		log.Printf("Admin UI available at %s/admin", selfBaseURL)
		log.Printf("API endpoints:")
		log.Printf("  - GET http://localhost:8080/api/users")
		log.Printf("  - GET http://localhost:8080/api/products")
		log.Printf("  - GET http://localhost:8080/api/orders")
		log.Printf("  - GET http://localhost:8080/api/checkout")
		log.Printf("  - GET http://localhost:8080/api/health")

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	// Graceful shutdown
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Server forced to shutdown: %v", err)
	}

	log.Println("Server stopped")
}
