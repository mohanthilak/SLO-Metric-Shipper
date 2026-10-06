package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LogLoadGenerator drives a single active synthetic-log job (Locust-style: at most one
// test running at a time). It is safe for concurrent Start/Stop/Stats calls.
type LogLoadGenerator struct {
	mu       sync.RWMutex
	job      *LogLoadJob
	last     *LogLoadStats // snapshot of the last completed run, kept for the UI
	otlpSink *OTLPSink     // reused for every "otlp" job

	// Persisted last-used request. On boot we load configPath into savedConfig; on
	// every successful Start we overwrite the file. Stats() projects savedConfig into
	// LogLoadStats when the generator is idle so the UI can hydrate its form even
	// after a container restart. The file may contain auth tokens — it's written 0600
	// and the API always redacts header values.
	savedConfig *LogLoadRequest
	configPath  string
}

// LogLoadRequest is the JSON body of /admin/api/logloadgen/start.
type LogLoadRequest struct {
	RatePerSec  int               `json:"ratePerSec"`
	BytesPerLog int               `json:"bytesPerLog"`
	Format      string            `json:"format"`
	Sink        string            `json:"sink"`
	SinkTarget  string            `json:"sinkTarget"`
	SinkHeaders map[string]string `json:"sinkHeaders,omitempty"` // HTTP direct only; ignored otherwise
	Workers     int               `json:"workers"`               // 0 = auto
	DurationSec int               `json:"durationSec"`           // 0 = until stopped
}

// SeriesPoint is one 1-second sample of the run, kept in a bounded ring so the UI can
// draw a sliding window without the server retaining unbounded history.
type SeriesPoint struct {
	T             int64   `json:"t"` // unix millis at bucket end
	AchievedRps   float64 `json:"achievedRps"`
	TargetRps     int     `json:"targetRps"`
	ThroughputMBs float64 `json:"throughputMBs"`
	Errors        int64   `json:"errors"` // in this bucket
}

// LogLoadStats is the response of /admin/api/logloadgen/stats.
type LogLoadStats struct {
	IsRunning      bool              `json:"isRunning"`
	RatePerSec     int               `json:"ratePerSec"`
	BytesPerLog    int               `json:"bytesPerLog"`
	Format         string            `json:"format"`
	Sink           string            `json:"sink"`
	SinkTarget     string            `json:"sinkTarget,omitempty"`
	// SinkHeaders returns the *keys* of the operator-supplied headers, with values
	// replaced by "***" — they may contain bearer tokens or API keys and must never
	// be echoed back over the read API.
	SinkHeaders    map[string]string `json:"sinkHeaders,omitempty"`
	Workers        int               `json:"workers"`
	DurationSec    int               `json:"durationSec"`
	ElapsedSeconds float64           `json:"elapsedSeconds"`
	LogsSent       int64             `json:"logsSent"`
	BytesSent      int64             `json:"bytesSent"`
	Errors         int64             `json:"errors"`
	AchievedRps    float64           `json:"achievedRps"`
	ThroughputMBs  float64           `json:"throughputMBs"`
	Series         []SeriesPoint     `json:"series"`
}

// redactHeaders returns a copy of h with every value replaced by "***" so the /stats
// endpoint can advertise which header keys are attached without leaking their values.
func redactHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k := range h {
		out[k] = "***"
	}
	return out
}

// LogLoadJob holds the immutable-per-run configuration and the mutable atomic counters.
type LogLoadJob struct {
	RatePerSec  int
	BytesPerLog int
	Format      string
	Sink        string
	SinkTarget  string
	Headers     map[string]string
	Workers     int
	DurationSec int
	StartTime   time.Time

	sink   LogSink
	cancel context.CancelFunc
	done   chan struct{}

	logsSent  atomic.Int64
	bytesSent atomic.Int64
	errors    atomic.Int64
	seqCtr    atomic.Int64

	series *seriesBuffer
}

const seriesCap = 300 // 5 minutes at 1s cadence

// seriesBuffer is a small ring of SeriesPoint samples.
type seriesBuffer struct {
	mu     sync.Mutex
	points []SeriesPoint
	cap    int
}

func newSeriesBuffer() *seriesBuffer { return &seriesBuffer{cap: seriesCap} }

func (b *seriesBuffer) append(p SeriesPoint) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.points = append(b.points, p)
	if len(b.points) > b.cap {
		b.points = b.points[len(b.points)-b.cap:]
	}
}

func (b *seriesBuffer) snapshot() []SeriesPoint {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]SeriesPoint, len(b.points))
	copy(out, b.points)
	return out
}

// NewLogLoadGenerator constructs a generator that will use otlpSink for jobs whose sink
// is "otlp". configPath is the JSON file used to persist the last-used LogLoadRequest;
// pass an empty string to disable persistence.
func NewLogLoadGenerator(otlpSink *OTLPSink, configPath string) *LogLoadGenerator {
	g := &LogLoadGenerator{otlpSink: otlpSink, configPath: configPath}
	if configPath != "" {
		if err := g.loadSavedConfig(); err != nil {
			log.Printf("logloadgen: failed to load saved config from %s: %v", configPath, err)
		}
	}
	return g
}

// loadSavedConfig reads the persisted LogLoadRequest from disk into g.savedConfig. A
// missing file is not an error — first run has no saved config.
func (g *LogLoadGenerator) loadSavedConfig() error {
	data, err := os.ReadFile(g.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var r LogLoadRequest
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("parse %s: %w", g.configPath, err)
	}
	g.mu.Lock()
	g.savedConfig = &r
	g.mu.Unlock()
	log.Printf("logloadgen: restored last-used config from %s (sink=%s, format=%s)", g.configPath, r.Sink, r.Format)
	return nil
}

// writeConfigLocked persists req to disk. Caller must hold g.mu. Errors are logged but
// not returned — a persistence failure should not abort a valid /start.
func (g *LogLoadGenerator) writeConfigLocked(req LogLoadRequest) {
	if g.configPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(g.configPath), 0o755); err != nil {
		log.Printf("logloadgen: mkdir %s: %v", filepath.Dir(g.configPath), err)
		return
	}
	b, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		log.Printf("logloadgen: marshal config: %v", err)
		return
	}
	// 0600 — the file may contain auth tokens in SinkHeaders.
	if err := os.WriteFile(g.configPath, b, 0o600); err != nil {
		log.Printf("logloadgen: write %s: %v", g.configPath, err)
	}
}

// Start validates the request and launches a new job. Returns an error if a job is
// already running or validation fails.
func (g *LogLoadGenerator) Start(req LogLoadRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.job != nil {
		return errors.New("a synthetic log job is already running")
	}
	// A fresh Start clears the previous run's snapshot so the UI shows the new job
	// from t=0 rather than the old series.
	g.last = nil

	// If the operator submitted a header with value "***" (the redacted placeholder
	// we return in /stats), rehydrate it from the last-saved config. This lets a
	// browser reload → Start reuse tokens without re-typing them, without ever
	// exposing them over the read API.
	if req.Sink == "http" && len(req.SinkHeaders) > 0 && g.savedConfig != nil {
		for k, v := range req.SinkHeaders {
			if v != "***" {
				continue
			}
			saved, ok := g.savedConfig.SinkHeaders[k]
			if !ok || saved == "" {
				return fmt.Errorf("header %q has redacted value but no saved value on file; enter the real value", k)
			}
			req.SinkHeaders[k] = saved
		}
	}

	if err := validateLogLoadRequest(&req); err != nil {
		return err
	}

	formatter := LookupFormatter(req.Format)
	if formatter == nil {
		return fmt.Errorf("unknown format %q", req.Format)
	}

	var sink LogSink
	var err error
	switch req.Sink {
	case "otlp":
		if g.otlpSink == nil {
			return errors.New("otlp sink is not configured")
		}
		sink = g.otlpSink
	case "http", "tcp", "udp":
		sink, err = NewDirectSink(req.Sink, req.SinkTarget, req.SinkHeaders)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown sink %q", req.Sink)
	}

	if req.Workers == 0 {
		req.Workers = defaultWorkers(req.RatePerSec)
	}

	parent := context.Background()
	var cancel context.CancelFunc
	if req.DurationSec > 0 {
		parent, cancel = context.WithTimeout(parent, time.Duration(req.DurationSec)*time.Second)
	} else {
		parent, cancel = context.WithCancel(parent)
	}

	job := &LogLoadJob{
		RatePerSec:  req.RatePerSec,
		BytesPerLog: req.BytesPerLog,
		Format:      req.Format,
		Sink:        req.Sink,
		SinkTarget:  req.SinkTarget,
		Headers:     req.SinkHeaders,
		Workers:     req.Workers,
		DurationSec: req.DurationSec,
		StartTime:   time.Now(),
		sink:        sink,
		cancel:      cancel,
		done:        make(chan struct{}),
		series:      newSeriesBuffer(),
	}
	g.job = job

	// Persist the request that just succeeded validation. Saved after g.job is set so
	// the write is under g.mu — no torn state visible to a concurrent /stats reader.
	g.savedConfig = &req
	g.writeConfigLocked(req)

	go g.runJob(parent, job, formatter)
	return nil
}

// Stop cancels the running job. Returns an error if none is running.
func (g *LogLoadGenerator) Stop() error {
	g.mu.Lock()
	job := g.job
	g.mu.Unlock()
	if job == nil {
		return errors.New("no synthetic log job is running")
	}
	job.cancel()
	// Wait briefly for the sampler to close so callers see the freeze.
	select {
	case <-job.done:
	case <-time.After(2 * time.Second):
	}
	return nil
}

// Stats returns a snapshot of the current run, or — if no run is active — the frozen
// snapshot of the most recently completed run (per spec §10.5), or — failing that —
// a projection of the last-persisted config so the UI can hydrate after a restart.
// Cleared on the next Start.
func (g *LogLoadGenerator) Stats() LogLoadStats {
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
			return LogLoadStats{
				IsRunning:   false,
				RatePerSec:  saved.RatePerSec,
				BytesPerLog: saved.BytesPerLog,
				Format:      saved.Format,
				Sink:        saved.Sink,
				SinkTarget:  saved.SinkTarget,
				SinkHeaders: redactHeaders(saved.SinkHeaders),
				Workers:     saved.Workers,
				DurationSec: saved.DurationSec,
			}
		}
		return LogLoadStats{IsRunning: false}
	}
	logs := job.logsSent.Load()
	bytesSent := job.bytesSent.Load()
	errs := job.errors.Load()
	series := job.series.snapshot()

	achievedRps := 0.0
	throughputMBs := 0.0
	if len(series) > 0 {
		last := series[len(series)-1]
		achievedRps = last.AchievedRps
		throughputMBs = last.ThroughputMBs
	}

	return LogLoadStats{
		IsRunning:      true,
		RatePerSec:     job.RatePerSec,
		BytesPerLog:    job.BytesPerLog,
		Format:         job.Format,
		Sink:           job.Sink,
		SinkTarget:     job.SinkTarget,
		SinkHeaders:    redactHeaders(job.Headers),
		Workers:        job.Workers,
		DurationSec:    job.DurationSec,
		ElapsedSeconds: time.Since(job.StartTime).Seconds(),
		LogsSent:       logs,
		BytesSent:      bytesSent,
		Errors:         errs,
		AchievedRps:    achievedRps,
		ThroughputMBs:  throughputMBs,
		Series:         series,
	}
}

// runJob drives the workers, the 1-second sampler, and cleans up on completion.
func (g *LogLoadGenerator) runJob(ctx context.Context, job *LogLoadJob, formatter LogFormatter) {
	defer func() {
		close(job.done)
		// Freeze the final snapshot before clearing the job so /stats keeps showing
		// the completed run until the next Start.
		finalSeries := job.series.snapshot()
		frozen := LogLoadStats{
			IsRunning:      false,
			RatePerSec:     job.RatePerSec,
			BytesPerLog:    job.BytesPerLog,
			Format:         job.Format,
			Sink:           job.Sink,
			SinkTarget:     job.SinkTarget,
			SinkHeaders:    redactHeaders(job.Headers),
			Workers:        job.Workers,
			DurationSec:    job.DurationSec,
			ElapsedSeconds: time.Since(job.StartTime).Seconds(),
			LogsSent:       job.logsSent.Load(),
			BytesSent:      job.bytesSent.Load(),
			Errors:         job.errors.Load(),
			Series:         finalSeries,
		}
		if n := len(finalSeries); n > 0 {
			frozen.AchievedRps = finalSeries[n-1].AchievedRps
			frozen.ThroughputMBs = finalSeries[n-1].ThroughputMBs
		}

		g.mu.Lock()
		if g.job == job {
			g.job = nil
		}
		g.last = &frozen
		g.mu.Unlock()

		// Direct sinks own their connections; OTLP sink is shared and lives longer.
		if job.Sink != "otlp" {
			if err := job.sink.Close(); err != nil {
				// Best-effort; nothing actionable at this layer.
				_ = err
			}
		}
	}()

	// Pacing strategy — see §5.3.
	//
	// We wake every tickInterval and, on each wake, distribute a batch of records
	// evenly across the workers using a buffered "token" channel. This scales past
	// what a per-record ticker can sustain (~few thousand rps).
	tickInterval := 10 * time.Millisecond
	perTick := job.RatePerSec / int(time.Second/tickInterval) // rate * tickInterval seconds
	if perTick < 1 {
		// Low rate: stretch the interval so we don't over-emit (previously the
		// 10ms tick + perTick=1 gave a floor of 100 rps regardless of target).
		perTick = 1
		tickInterval = time.Second / time.Duration(job.RatePerSec)
	}

	tokens := make(chan struct{}, job.Workers*4)

	// Workers pull tokens and emit one record each.
	var wg sync.WaitGroup
	for i := 0; i < job.Workers; i++ {
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
					g.emitOne(ctx, job, formatter)
				}
			}
		}()
	}

	// Sampler: once per second, compute deltas and append a SeriesPoint.
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var lastLogs, lastBytes, lastErrs int64
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				logs := job.logsSent.Load()
				bytesSent := job.bytesSent.Load()
				errs := job.errors.Load()
				job.series.append(SeriesPoint{
					T:             t.UnixMilli(),
					AchievedRps:   float64(logs - lastLogs),
					TargetRps:     job.RatePerSec,
					ThroughputMBs: float64(bytesSent-lastBytes) / (1024 * 1024),
					Errors:        errs - lastErrs,
				})
				lastLogs, lastBytes, lastErrs = logs, bytesSent, errs
			}
		}
	}()

	// Pacer: 10ms cadence dispenses `perTick` tokens per wake.
	pacer := time.NewTicker(tickInterval)
	defer pacer.Stop()

pacerLoop:
	for {
		select {
		case <-ctx.Done():
			break pacerLoop
		case <-pacer.C:
			// Non-blocking dispatch — if the workers are saturated we simply drop
			// tokens on the floor; achieved rps < target rps is the intended signal.
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

func (g *LogLoadGenerator) emitOne(ctx context.Context, job *LogLoadJob, formatter LogFormatter) {
	seq := job.seqCtr.Add(1)
	record, level := formatter.Format(seq, job.BytesPerLog)
	if err := job.sink.Send(ctx, record, level, formatter.ContentType()); err != nil {
		job.errors.Add(1)
		return
	}
	job.logsSent.Add(1)
	job.bytesSent.Add(int64(len(record)))
}

// ---- validation ----

const (
	minRate         = 1
	maxRate         = 200_000
	minBytesPerLog  = 64
	maxBytesPerLog  = 1_048_576
	maxJobWorkers   = 256
)

func validateLogLoadRequest(r *LogLoadRequest) error {
	if r.RatePerSec < minRate || r.RatePerSec > maxRate {
		return fmt.Errorf("ratePerSec must be in [%d,%d]", minRate, maxRate)
	}
	if r.BytesPerLog < minBytesPerLog || r.BytesPerLog > maxBytesPerLog {
		return fmt.Errorf("bytesPerLog must be in [%d,%d]", minBytesPerLog, maxBytesPerLog)
	}
	if LookupFormatter(r.Format) == nil {
		return fmt.Errorf("unknown format %q", r.Format)
	}
	// Sink defaults to "otlp" so pipeline-consistency runs need only rate/bytes/format.
	if strings.TrimSpace(r.Sink) == "" {
		r.Sink = "otlp"
	}
	switch r.Sink {
	case "otlp":
		// no target required
	case "http", "tcp", "udp":
		if strings.TrimSpace(r.SinkTarget) == "" {
			return fmt.Errorf("sink %q requires sinkTarget", r.Sink)
		}
	default:
		return fmt.Errorf("unknown sink %q", r.Sink)
	}
	if r.Workers < 0 || r.Workers > maxJobWorkers {
		return fmt.Errorf("workers must be in [0,%d]", maxJobWorkers)
	}
	if r.DurationSec < 0 {
		return errors.New("durationSec must be >= 0")
	}
	return nil
}

func defaultWorkers(rate int) int {
	base := runtime.NumCPU() * 2
	if base < 4 {
		base = 4
	}
	if base > 32 {
		base = 32
	}
	// Never provision more workers than 1/100 of the rate; there's no point.
	if maxByRate := rate / 100; maxByRate > 0 && maxByRate < base {
		if maxByRate < 2 {
			maxByRate = 2
		}
		return maxByRate
	}
	return base
}
