package main

import (
	"context"
	"fmt"
	stdlog "log"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

var loggerProvider *sdklog.LoggerProvider

// InitLogs wires slog through an OTel LoggerProvider with an OTLP gRPC exporter,
// so structured log records are shipped to the collector alongside metrics/traces.
func InitLogs(ctx context.Context) error {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}

	exporter, err := otlploggrpc.New(ctx,
		otlploggrpc.WithEndpoint(endpoint),
		otlploggrpc.WithInsecure(),
	)
	if err != nil {
		return fmt.Errorf("failed to create OTLP log exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("slo-metric-generator"),
			semconv.ServiceVersion("1.0.0"),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create log resource: %w", err)
	}

	loggerProvider = sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
	)

	slog.SetDefault(slog.New(otelslog.NewHandler("slo-metric-generator",
		otelslog.WithLoggerProvider(loggerProvider),
	)))
	// slog.SetDefault also redirects the stdlib log package into the slog handler,
	// which would swallow every log.Printf (startup banner included) into OTLP and
	// leave `docker logs` silent. Keep stdlib log on stderr; slog stays on OTel.
	stdlog.SetOutput(os.Stderr)
	stdlog.SetFlags(stdlog.LstdFlags)

	stdlog.Printf("Logs initialized with OTLP endpoint: %s", endpoint)
	return nil
}

// ShutdownLogs flushes buffered log records and shuts the provider down.
func ShutdownLogs(ctx context.Context) error {
	if loggerProvider == nil {
		return nil
	}
	stdlog.Println("Flushing logs...")
	return loggerProvider.Shutdown(ctx)
}
