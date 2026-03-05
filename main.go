package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
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

	// Create configuration store
	config := NewConfig()

	// Create load generator
	loadGen := NewLoadGenerator("http://localhost:8080")

	// Create HTTP mux
	mux := http.NewServeMux()

	// Setup API handlers
	SetupAPIHandlers(mux, config)

	// Setup admin handlers
	SetupAdminHandlers(mux, config, loadGen)

	// Create HTTP server
	server := &http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in a goroutine
	go func() {
		log.Printf("Starting SLO Metric Generator on http://localhost:8080")
		log.Printf("Admin UI available at http://localhost:8080/admin")
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
