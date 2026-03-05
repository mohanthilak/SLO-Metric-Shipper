package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	metricapi "go.opentelemetry.io/otel/metric"
)

var (
	meter            metricapi.Meter
	requestCounter   metricapi.Int64Counter
	meterProvider    *metric.MeterProvider
)

// InitMetrics initializes the OpenTelemetry metrics SDK with OTLP exporter
func InitMetrics(ctx context.Context) error {
	// Get OTLP endpoint from environment variable
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}

	// Create OTLP exporter
	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(), // Use insecure for local development
	)
	if err != nil {
		return fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	// Create resource with service information
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("slo-metric-generator"),
			semconv.ServiceVersion("1.0.0"),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create resource: %w", err)
	}

	// Create meter provider with periodic reader
	meterProvider = metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter,
			metric.WithInterval(10*time.Second),
		)),
	)

	// Set global meter provider
	otel.SetMeterProvider(meterProvider)

	// Create meter
	meter = otel.Meter("slo-metric-generator")

	// Create http_requests_total counter
	requestCounter, err = meter.Int64Counter(
		"http_requests_total",
		metricapi.WithDescription("Total number of HTTP requests"),
		metricapi.WithUnit("{request}"),
	)
	if err != nil {
		return fmt.Errorf("failed to create counter: %w", err)
	}

	log.Printf("Metrics initialized with OTLP endpoint: %s", endpoint)
	return nil
}

// RecordRequest records an HTTP request metric
func RecordRequest(endpoint string, statusCode int, method string) {
	if requestCounter == nil {
		log.Println("Warning: requestCounter not initialized")
		return
	}

	requestCounter.Add(context.Background(), 1,
		metricapi.WithAttributes(
			attribute.String("endpoint", endpoint),
			attribute.Int("status_code", statusCode),
			attribute.String("method", method),
		),
	)
}

// ShutdownMetrics gracefully shuts down the metrics provider and flushes metrics
func ShutdownMetrics(ctx context.Context) error {
	if meterProvider == nil {
		return nil
	}

	log.Println("Flushing metrics...")
	return meterProvider.Shutdown(ctx)
}
