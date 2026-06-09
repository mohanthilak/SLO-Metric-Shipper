package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	traceapi "go.opentelemetry.io/otel/trace"
)

var (
	tracer         traceapi.Tracer
	tracerProvider *sdktrace.TracerProvider
)

// InitTraces initializes the OpenTelemetry tracing SDK with an OTLP gRPC exporter
// pointed at the same endpoint as metrics.
func InitTraces(ctx context.Context) error {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}

	exporter, err := otlptrace.New(ctx, otlptracegrpc.NewClient(
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(),
	))
	if err != nil {
		return fmt.Errorf("failed to create OTLP trace exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("slo-metric-generator"),
			semconv.ServiceVersion("1.0.0"),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create trace resource: %w", err)
	}

	tracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	tracer = otel.Tracer("slo-metric-generator")

	log.Printf("Traces initialized with OTLP endpoint: %s", endpoint)
	return nil
}

// ShutdownTraces flushes any buffered spans and shuts the provider down.
func ShutdownTraces(ctx context.Context) error {
	if tracerProvider == nil {
		return nil
	}
	log.Println("Flushing traces...")
	return tracerProvider.Shutdown(ctx)
}
