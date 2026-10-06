package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	traceapi "go.opentelemetry.io/otel/trace"
)

// TraceLoadGenerator emits synthetic trace trees at a configurable rate/depth/fanout.
// Each simulated service gets its own TracerProvider so Tempo's span-metrics and
// service-graph processors see a realistic multi-service topology.
type TraceLoadGenerator struct {
	mu   sync.RWMutex
	job  *TraceLoadJob
	last *TraceLoadStats

	savedConfig *TraceLoadRequest
	configPath  string
}

// TraceLoadRequest is the JSON body of /admin/api/traceloadgen/start.
type TraceLoadRequest struct {
	// Trace throughput. Each trace produces (fanout^depth - 1)/(fanout - 1) spans
	// so total span rate = TracesPerSec × trace_span_count. Cap accordingly.
	TracesPerSec int `json:"tracesPerSec"`

	// Tree topology.
	Depth  int `json:"depth"`  // 1 = flat, N = spans nested N deep
	Fanout int `json:"fanout"` // children per non-leaf span

	// Duration shape: fast (1-50ms), medium (10-500ms), slow (100-5000ms)
	DurationProfile string `json:"durationProfile"`

	// Fraction of spans marked STATUS_CODE_ERROR. 0.0-1.0.
	ErrorRate float64 `json:"errorRate"`

	// Number of distinct simulated services. Each gets a TracerProvider with its
	// own service.name resource, so Tempo's span-metrics/service-graph pipelines
	// see traces coming from N different services.
	ServiceCount int `json:"serviceCount"`

	// Adds `user.id` and `request.id` high-cardinality attributes to every span
	// when enabled — the trace analog of metric cardinality explosion.
	HighCardAttrs        bool `json:"highCardAttrs"`
	HighCardAttrPoolSize int  `json:"highCardAttrPoolSize"` // pool size for user.id churn

	Workers     int `json:"workers"`
	DurationSec int `json:"durationSec"`
}

// TraceLoadStats mirrors the shape of the other generators.
type TraceLoadStats struct {
	IsRunning            bool          `json:"isRunning"`
	TracesPerSec         int           `json:"tracesPerSec"`
	Depth                int           `json:"depth"`
	Fanout               int           `json:"fanout"`
	DurationProfile      string        `json:"durationProfile"`
	ErrorRate            float64       `json:"errorRate"`
	ServiceCount         int           `json:"serviceCount"`
	HighCardAttrs        bool          `json:"highCardAttrs"`
	HighCardAttrPoolSize int           `json:"highCardAttrPoolSize"`
	Workers              int           `json:"workers"`
	DurationSec          int           `json:"durationSec"`
	ElapsedSeconds       float64       `json:"elapsedSeconds"`
	TracesEmitted        int64         `json:"tracesEmitted"`
	SpansEmitted         int64         `json:"spansEmitted"`
	Errors               int64         `json:"errors"`
	AchievedTps          float64       `json:"achievedTps"`
	SpansPerTrace        int           `json:"spansPerTrace"` // derived: geometric sum
	Series               []SeriesPoint `json:"series"`
}

// TraceLoadJob owns the runtime state.
type TraceLoadJob struct {
	req       TraceLoadRequest
	StartTime time.Time

	// One provider per simulated service, plus shared batch exporter.
	svcProviders []*sdktrace.TracerProvider
	svcTracers   []traceapi.Tracer

	// High-card attribute pool.
	userIDPool []string

	tracesEmitted atomic.Int64
	spansEmitted  atomic.Int64
	errors        atomic.Int64

	cancel context.CancelFunc
	done   chan struct{}
	series *seriesBuffer
}

// -----------------------------------------------------------------------------
// Constructor + persistence
// -----------------------------------------------------------------------------

func NewTraceLoadGenerator(configPath string) *TraceLoadGenerator {
	g := &TraceLoadGenerator{configPath: configPath}
	if configPath != "" {
		if err := g.loadSavedConfig(); err != nil {
			log.Printf("traceloadgen: failed to load saved config: %v", err)
		}
	}
	return g
}

func (g *TraceLoadGenerator) loadSavedConfig() error {
	data, err := os.ReadFile(g.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var r TraceLoadRequest
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	g.mu.Lock()
	g.savedConfig = &r
	g.mu.Unlock()
	return nil
}

func (g *TraceLoadGenerator) writeConfigLocked(req TraceLoadRequest) {
	if g.configPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(g.configPath), 0o755); err != nil {
		return
	}
	b, _ := json.MarshalIndent(req, "", "  ")
	_ = os.WriteFile(g.configPath, b, 0o644)
}

// -----------------------------------------------------------------------------
// Metadata + validation
// -----------------------------------------------------------------------------

var AllDurationProfiles = []string{"fast", "medium", "slow"}

const (
	minTracesPerSec         = 1
	maxTracesPerSec         = 5_000
	minTraceDepth           = 1
	maxTraceDepth           = 10
	minTraceFanout          = 1
	maxTraceFanout          = 20
	minServiceCount         = 1
	maxServiceCount         = 20
	defaultHighCardPoolSize = 10_000
	defaultTraceWorkers     = 8
)

func validateTraceLoadRequest(r *TraceLoadRequest) error {
	if r.TracesPerSec < minTracesPerSec || r.TracesPerSec > maxTracesPerSec {
		return fmt.Errorf("tracesPerSec must be in [%d,%d]", minTracesPerSec, maxTracesPerSec)
	}
	if r.Depth < minTraceDepth || r.Depth > maxTraceDepth {
		return fmt.Errorf("depth must be in [%d,%d]", minTraceDepth, maxTraceDepth)
	}
	if r.Fanout < minTraceFanout || r.Fanout > maxTraceFanout {
		return fmt.Errorf("fanout must be in [%d,%d]", minTraceFanout, maxTraceFanout)
	}
	if r.ServiceCount < minServiceCount || r.ServiceCount > maxServiceCount {
		return fmt.Errorf("serviceCount must be in [%d,%d]", minServiceCount, maxServiceCount)
	}
	if r.ErrorRate < 0 || r.ErrorRate > 1 {
		return errors.New("errorRate must be in [0,1]")
	}
	if strings.TrimSpace(r.DurationProfile) == "" {
		r.DurationProfile = "medium"
	}
	if !containsStr(AllDurationProfiles, r.DurationProfile) {
		return fmt.Errorf("durationProfile must be one of %v", AllDurationProfiles)
	}
	if r.HighCardAttrPoolSize <= 0 {
		r.HighCardAttrPoolSize = defaultHighCardPoolSize
	}
	if r.Workers < 0 || r.Workers > 256 {
		return fmt.Errorf("workers must be in [0,256]")
	}
	if r.DurationSec < 0 {
		return errors.New("durationSec must be >= 0")
	}
	return nil
}

func containsStr(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// spansPerTrace returns the geometric-sum span count for a full-fanout tree.
// depth=1 → 1 span. depth=3 fanout=3 → 1 + 3 + 9 = 13 spans.
func spansPerTrace(depth, fanout int) int {
	if depth <= 1 {
		return 1
	}
	if fanout == 1 {
		return depth // degenerates to a linear chain
	}
	// (fanout^depth - 1) / (fanout - 1)
	total := 1
	pow := 1
	for i := 1; i < depth; i++ {
		pow *= fanout
		total += pow
	}
	return total
}

// -----------------------------------------------------------------------------
// Start / Stop / Stats
// -----------------------------------------------------------------------------

func (g *TraceLoadGenerator) Start(req TraceLoadRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.job != nil {
		return errors.New("a synthetic trace job is already running")
	}
	g.last = nil

	if err := validateTraceLoadRequest(&req); err != nil {
		return err
	}

	if req.Workers == 0 {
		req.Workers = defaultTraceWorkers
		if runtime.NumCPU()*2 > req.Workers {
			req.Workers = runtime.NumCPU() * 2
		}
		if req.Workers > 32 {
			req.Workers = 32
		}
	}

	parent := context.Background()
	var cancel context.CancelFunc
	if req.DurationSec > 0 {
		parent, cancel = context.WithTimeout(parent, time.Duration(req.DurationSec)*time.Second)
	} else {
		parent, cancel = context.WithCancel(parent)
	}

	job := &TraceLoadJob{
		req:       req,
		StartTime: time.Now(),
		cancel:    cancel,
		done:      make(chan struct{}),
		series:    newSeriesBuffer(),
	}

	// Build one TracerProvider per simulated service so each service gets its
	// own resource.service.name — Tempo's span-metrics/service-graph pipelines
	// key on this, so per-service metrics differentiate.
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:4317"
	}
	ctxInit := context.Background()
	for i := 0; i < req.ServiceCount; i++ {
		exporter, err := otlptrace.New(ctxInit, otlptracegrpc.NewClient(
			otlptracegrpc.WithEndpoint(endpoint),
			otlptracegrpc.WithInsecure(),
		))
		if err != nil {
			cancel()
			return fmt.Errorf("build trace exporter for svc %d: %w", i, err)
		}
		res, err := resource.New(ctxInit,
			resource.WithAttributes(
				semconv.ServiceName(fmt.Sprintf("gen-svc-%02d", i)),
				semconv.ServiceVersion("1.0.0"),
				attribute.String("gen.source", "traceloadgen"),
			),
		)
		if err != nil {
			cancel()
			return fmt.Errorf("build resource for svc %d: %w", i, err)
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithBatcher(exporter),
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
		)
		job.svcProviders = append(job.svcProviders, tp)
		job.svcTracers = append(job.svcTracers, tp.Tracer(fmt.Sprintf("gen-svc-%02d/tracer", i)))
	}

	// Pre-generate user.id pool for high-card attrs.
	if req.HighCardAttrs {
		job.userIDPool = make([]string, req.HighCardAttrPoolSize)
		for i := range job.userIDPool {
			job.userIDPool[i] = fmt.Sprintf("u-%08d", i)
		}
	}

	g.job = job
	g.savedConfig = &req
	g.writeConfigLocked(req)

	go g.runJob(parent, job)
	return nil
}

func (g *TraceLoadGenerator) Stop() error {
	g.mu.Lock()
	job := g.job
	g.mu.Unlock()
	if job == nil {
		return errors.New("no synthetic trace job running")
	}
	job.cancel()
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

func (g *TraceLoadGenerator) Stats() TraceLoadStats {
	g.mu.RLock()
	job := g.job
	last := g.last
	saved := g.savedConfig
	g.mu.RUnlock()

	if job == nil {
		if last != nil {
			return *last
		}
		if saved != nil {
			return TraceLoadStats{
				IsRunning:            false,
				TracesPerSec:         saved.TracesPerSec,
				Depth:                saved.Depth,
				Fanout:               saved.Fanout,
				DurationProfile:      saved.DurationProfile,
				ErrorRate:            saved.ErrorRate,
				ServiceCount:         saved.ServiceCount,
				HighCardAttrs:        saved.HighCardAttrs,
				HighCardAttrPoolSize: saved.HighCardAttrPoolSize,
				Workers:              saved.Workers,
				DurationSec:          saved.DurationSec,
				SpansPerTrace:        spansPerTrace(saved.Depth, saved.Fanout),
			}
		}
		return TraceLoadStats{IsRunning: false}
	}

	achieved := 0.0
	series := job.series.snapshot()
	if n := len(series); n > 0 {
		achieved = series[n-1].AchievedRps
	}
	return TraceLoadStats{
		IsRunning:            true,
		TracesPerSec:         job.req.TracesPerSec,
		Depth:                job.req.Depth,
		Fanout:               job.req.Fanout,
		DurationProfile:      job.req.DurationProfile,
		ErrorRate:            job.req.ErrorRate,
		ServiceCount:         job.req.ServiceCount,
		HighCardAttrs:        job.req.HighCardAttrs,
		HighCardAttrPoolSize: job.req.HighCardAttrPoolSize,
		Workers:              job.req.Workers,
		DurationSec:          job.req.DurationSec,
		ElapsedSeconds:       time.Since(job.StartTime).Seconds(),
		TracesEmitted:        job.tracesEmitted.Load(),
		SpansEmitted:         job.spansEmitted.Load(),
		Errors:               job.errors.Load(),
		AchievedTps:          achieved,
		SpansPerTrace:        spansPerTrace(job.req.Depth, job.req.Fanout),
		Series:               series,
	}
}

// -----------------------------------------------------------------------------
// Run loop
// -----------------------------------------------------------------------------

func (g *TraceLoadGenerator) runJob(ctx context.Context, job *TraceLoadJob) {
	defer func() {
		close(job.done)

		// Flush all per-service providers so in-flight batches make it out.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, tp := range job.svcProviders {
			_ = tp.Shutdown(shutdownCtx)
		}

		final := job.series.snapshot()
		frozen := TraceLoadStats{
			IsRunning:            false,
			TracesPerSec:         job.req.TracesPerSec,
			Depth:                job.req.Depth,
			Fanout:               job.req.Fanout,
			DurationProfile:      job.req.DurationProfile,
			ErrorRate:            job.req.ErrorRate,
			ServiceCount:         job.req.ServiceCount,
			HighCardAttrs:        job.req.HighCardAttrs,
			HighCardAttrPoolSize: job.req.HighCardAttrPoolSize,
			Workers:              job.req.Workers,
			DurationSec:          job.req.DurationSec,
			ElapsedSeconds:       time.Since(job.StartTime).Seconds(),
			TracesEmitted:        job.tracesEmitted.Load(),
			SpansEmitted:         job.spansEmitted.Load(),
			Errors:               job.errors.Load(),
			SpansPerTrace:        spansPerTrace(job.req.Depth, job.req.Fanout),
			Series:               final,
		}
		if n := len(final); n > 0 {
			frozen.AchievedTps = final[n-1].AchievedRps
		}

		g.mu.Lock()
		if g.job == job {
			g.job = nil
		}
		g.last = &frozen
		g.mu.Unlock()
	}()

	// Batch pacer. At high rates we tick every 10ms and emit rate/100 per tick.
	// At low rates that would round to 0 → clamp perTick to 1 AND stretch the
	// interval so we still hit the exact target instead of over-emitting.
	tickInterval := 10 * time.Millisecond
	perTick := job.req.TracesPerSec / int(time.Second/tickInterval)
	if perTick < 1 {
		perTick = 1
		tickInterval = time.Second / time.Duration(job.req.TracesPerSec)
	}

	tokens := make(chan struct{}, job.req.Workers*4)
	var wg sync.WaitGroup
	for i := 0; i < job.req.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-tokens:
					if !ok {
						return
					}
					g.emitTrace(ctx, job)
				}
			}
		}()
	}

	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var lastTraces, lastErrs int64
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				traces := job.tracesEmitted.Load()
				errs := job.errors.Load()
				job.series.append(SeriesPoint{
					T:             t.UnixMilli(),
					AchievedRps:   float64(traces - lastTraces),
					TargetRps:     job.req.TracesPerSec,
					ThroughputMBs: 0,
					Errors:        errs - lastErrs,
				})
				lastTraces, lastErrs = traces, errs
			}
		}
	}()

	pacer := time.NewTicker(tickInterval)
	defer pacer.Stop()
pacerLoop:
	for {
		select {
		case <-ctx.Done():
			break pacerLoop
		case <-pacer.C:
			for i := 0; i < perTick; i++ {
				select {
				case tokens <- struct{}{}:
				default:
				}
			}
		}
	}

	close(tokens)
	wg.Wait()
	<-sampleDone
}

// emitTrace builds one synthetic trace tree per the configured knobs. Each trace
// starts at a random service; child spans alternate services so span-graphs get
// interesting cross-service edges.
func (g *TraceLoadGenerator) emitTrace(ctx context.Context, job *TraceLoadJob) {
	svcIdx := rand.Intn(len(job.svcTracers))
	tracer := job.svcTracers[svcIdx]

	// Build the root span; recursively spawn children up to configured depth.
	spanCtx, root := tracer.Start(ctx, spanNameFor(0),
		traceapi.WithSpanKind(traceapi.SpanKindServer),
	)
	g.decorateSpan(root, job, 0)
	g.spawnChildren(spanCtx, job, 1, svcIdx)

	g.finishSpan(root, job)
	job.tracesEmitted.Add(1)
}

// spawnChildren recursively builds the trace tree.
func (g *TraceLoadGenerator) spawnChildren(ctx context.Context, job *TraceLoadJob, level, parentSvc int) {
	if level >= job.req.Depth {
		return
	}
	// Alternate to a different service every other level so span-graph edges form.
	svcIdx := parentSvc
	if level%2 == 1 && len(job.svcTracers) > 1 {
		svcIdx = (parentSvc + 1 + rand.Intn(len(job.svcTracers)-1)) % len(job.svcTracers)
	}
	tracer := job.svcTracers[svcIdx]
	kind := traceapi.SpanKindInternal
	if level == 1 {
		kind = traceapi.SpanKindClient
	}
	for i := 0; i < job.req.Fanout; i++ {
		spanCtx, span := tracer.Start(ctx, spanNameFor(level),
			traceapi.WithSpanKind(kind),
		)
		g.decorateSpan(span, job, level)
		g.spawnChildren(spanCtx, job, level+1, svcIdx)
		g.finishSpan(span, job)
	}
}

func (g *TraceLoadGenerator) decorateSpan(span traceapi.Span, job *TraceLoadJob, level int) {
	span.SetAttributes(
		attribute.Int("gen.tree.level", level),
		attribute.String("gen.duration_profile", job.req.DurationProfile),
	)
	if job.req.HighCardAttrs {
		span.SetAttributes(
			attribute.String("user.id", job.userIDPool[rand.Intn(len(job.userIDPool))]),
			attribute.String("request.id", fmt.Sprintf("req-%016x", rand.Uint64())),
		)
	}
}

func (g *TraceLoadGenerator) finishSpan(span traceapi.Span, job *TraceLoadJob) {
	// Sleep for configured duration profile.
	dur := durationSample(job.req.DurationProfile)
	if dur > 0 {
		time.Sleep(dur)
	}
	// Chance of error status.
	if rand.Float64() < job.req.ErrorRate {
		span.SetStatus(codes.Error, "synthetic error")
	}
	span.End()
	job.spansEmitted.Add(1)
}

// spanNameFor picks a synthetic name for a span at a given tree level. Kept
// small (bounded set) so Tempo's span-metrics doesn't explode on span_name.
func spanNameFor(level int) string {
	pool := []string{"HTTP GET", "HTTP POST", "db.query", "cache.lookup", "grpc.call", "internal.compute", "queue.send"}
	return pool[(level*3+rand.Intn(len(pool)))%len(pool)]
}

// durationSample returns a sleep duration matching the configured profile.
// Note we sleep synchronously — that's not ideal for pure throughput, but it
// keeps span durations realistic which is the whole point.
func durationSample(profile string) time.Duration {
	switch profile {
	case "fast":
		return time.Duration(1+rand.Intn(50)) * time.Millisecond
	case "slow":
		// Exponentially distributed heavy tail.
		ms := int(-math.Log(1-rand.Float64()) * 1000)
		if ms < 100 {
			ms = 100
		}
		if ms > 5000 {
			ms = 5000
		}
		return time.Duration(ms) * time.Millisecond
	default: // medium
		return time.Duration(10+rand.Intn(500)) * time.Millisecond
	}
}
