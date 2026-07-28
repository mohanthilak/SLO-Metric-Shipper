package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	metricapi "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Instrument registry — one of every OTel instrument kind.
var (
	meter         metricapi.Meter
	meterProvider *metric.MeterProvider

	// Synchronous
	requestCounter       metricapi.Int64Counter          // monotonic count
	requestDuration      metricapi.Float64Histogram      // latency distribution
	inflightRequests     metricapi.Int64UpDownCounter    // gauge-like, can go up or down
	responseSizeBytes    metricapi.Int64Histogram        // payload size distribution
	lastRequestTimestamp metricapi.Float64Gauge          // sync snapshot

	// Async (populated via observer callbacks)
	// (kept as package vars so registration/unregistration is possible later)
	uptimeCounter         metricapi.Float64ObservableCounter
	activeSessions        metricapi.Int64ObservableUpDownCounter
	processMemoryBytes    metricapi.Int64ObservableGauge
	processCPUPercent     metricapi.Float64ObservableGauge
	cacheHitRatio         metricapi.Float64ObservableGauge

	// Simulated backing state for async instruments.
	processStart      = time.Now()
	activeSessionsVal atomic.Int64
)

// InitMetrics initializes the OpenTelemetry metrics SDK with an OTLP gRPC exporter
// and registers every OTel instrument kind (Counter, UpDownCounter, Histogram, Gauge,
// ObservableCounter, ObservableUpDownCounter, ObservableGauge).
func InitMetrics(ctx context.Context) error {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}

	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return fmt.Errorf("failed to create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("slo-metric-generator"),
			semconv.ServiceVersion("1.0.0"),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create resource: %w", err)
	}

	meterProvider = metric.NewMeterProvider(
		metric.WithResource(res),
		metric.WithReader(metric.NewPeriodicReader(exporter,
			metric.WithInterval(10*time.Second),
		)),
	)
	otel.SetMeterProvider(meterProvider)
	meter = otel.Meter("slo-metric-generator")

	if err := registerSyncInstruments(); err != nil {
		return err
	}
	if err := registerAsyncInstruments(); err != nil {
		return err
	}

	activeSessionsVal.Store(50)

	log.Printf("Metrics initialized with OTLP endpoint: %s", endpoint)
	return nil
}

func registerSyncInstruments() error {
	var err error

	if requestCounter, err = meter.Int64Counter(
		"mt_http_requests_total",
		metricapi.WithDescription("Total number of HTTP requests"),
		metricapi.WithUnit("{request}"),
	); err != nil {
		return fmt.Errorf("counter: %w", err)
	}

	if requestDuration, err = meter.Float64Histogram(
		"mt_http_request_duration_seconds",
		metricapi.WithDescription("HTTP request duration"),
		metricapi.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("duration histogram: %w", err)
	}

	if inflightRequests, err = meter.Int64UpDownCounter(
		"mt_http_inflight_requests",
		metricapi.WithDescription("HTTP requests currently in flight"),
		metricapi.WithUnit("{request}"),
	); err != nil {
		return fmt.Errorf("inflight updown: %w", err)
	}

	if responseSizeBytes, err = meter.Int64Histogram(
		"mt_http_response_size_bytes",
		metricapi.WithDescription("HTTP response payload size"),
		metricapi.WithUnit("By"),
	); err != nil {
		return fmt.Errorf("response size histogram: %w", err)
	}

	if lastRequestTimestamp, err = meter.Float64Gauge(
		"mt_http_last_request_epoch_seconds",
		metricapi.WithDescription("Wall-clock timestamp of the last HTTP request served"),
		metricapi.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("last-request gauge: %w", err)
	}

	return nil
}

func registerAsyncInstruments() error {
	var err error

	if uptimeCounter, err = meter.Float64ObservableCounter(
		"mt_process_uptime_seconds",
		metricapi.WithDescription("Seconds since process start"),
		metricapi.WithUnit("s"),
	); err != nil {
		return fmt.Errorf("uptime counter: %w", err)
	}

	if activeSessions, err = meter.Int64ObservableUpDownCounter(
		"mt_active_sessions",
		metricapi.WithDescription("Simulated active user sessions (random walk)"),
		metricapi.WithUnit("{session}"),
	); err != nil {
		return fmt.Errorf("active sessions: %w", err)
	}

	if processMemoryBytes, err = meter.Int64ObservableGauge(
		"mt_process_memory_bytes",
		metricapi.WithDescription("Simulated process resident memory"),
		metricapi.WithUnit("By"),
	); err != nil {
		return fmt.Errorf("memory gauge: %w", err)
	}

	if processCPUPercent, err = meter.Float64ObservableGauge(
		"mt_process_cpu_percent",
		metricapi.WithDescription("Simulated process CPU utilisation"),
		metricapi.WithUnit("%"),
	); err != nil {
		return fmt.Errorf("cpu gauge: %w", err)
	}

	if cacheHitRatio, err = meter.Float64ObservableGauge(
		"mt_cache_hit_ratio",
		metricapi.WithDescription("Simulated cache hit ratio"),
		metricapi.WithUnit("1"),
	); err != nil {
		return fmt.Errorf("cache hit ratio: %w", err)
	}

	_, err = meter.RegisterCallback(
		func(_ context.Context, o metricapi.Observer) error {
			o.ObserveFloat64(uptimeCounter, time.Since(processStart).Seconds())

			// Random walk within [0, 500] so the value is not monotonic.
			delta := int64(rand.Intn(11) - 5)
			next := activeSessionsVal.Add(delta)
			switch {
			case next < 0:
				activeSessionsVal.Store(0)
				next = 0
			case next > 500:
				activeSessionsVal.Store(500)
				next = 500
			}
			o.ObserveInt64(activeSessions, next)

			// 256 MiB base ± 32 MiB jitter.
			mem := int64(256<<20) + int64(rand.Intn(64<<20)) - int64(32<<20)
			o.ObserveInt64(processMemoryBytes, mem)

			// Smooth-ish CPU % via sine of wall clock + noise.
			t := float64(time.Now().Unix() % 600)
			cpu := 35 + 25*math.Sin(t/95.0) + (rand.Float64()*10 - 5)
			o.ObserveFloat64(processCPUPercent, math.Max(0, math.Min(100, cpu)))

			o.ObserveFloat64(cacheHitRatio, 0.85+rand.Float64()*0.14)

			return nil
		},
		uptimeCounter,
		activeSessions,
		processMemoryBytes,
		processCPUPercent,
		cacheHitRatio,
	)
	if err != nil {
		return fmt.Errorf("register callback: %w", err)
	}
	return nil
}

// RecordRequest records the per-request synchronous instruments. It is safe to call
// before/after the handler body — the caller is expected to provide the observed
// duration and response size that correspond to the finished request.
func RecordRequest(endpoint string, statusCode int, method string, duration time.Duration, responseBytes int64) {
	if requestCounter == nil {
		log.Println("Warning: metrics not initialized")
		return
	}
	ctx := context.Background()
	attrs := metricapi.WithAttributes(
		attribute.String("endpoint", endpoint),
		attribute.Int("status_code", statusCode),
		attribute.String("method", method),
	)

	requestCounter.Add(ctx, 1, attrs)
	requestDuration.Record(ctx, duration.Seconds(), attrs)
	responseSizeBytes.Record(ctx, responseBytes, attrs)
	lastRequestTimestamp.Record(ctx, float64(time.Now().UnixNano())/1e9, attrs)
}

// TrackInflight increments the in-flight counter and returns a decrement function
// intended to be deferred by the caller.
func TrackInflight(endpoint, method string) func() {
	if inflightRequests == nil {
		return func() {}
	}
	ctx := context.Background()
	attrs := metricapi.WithAttributes(
		attribute.String("endpoint", endpoint),
		attribute.String("method", method),
	)
	inflightRequests.Add(ctx, 1, attrs)
	return func() {
		inflightRequests.Add(context.Background(), -1, attrs)
	}
}

// ShutdownMetrics flushes and shuts down the meter provider.
func ShutdownMetrics(ctx context.Context) error {
	if meterProvider == nil {
		return nil
	}
	log.Println("Flushing metrics...")
	return meterProvider.Shutdown(ctx)
}
