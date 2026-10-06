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
	metricapi "go.opentelemetry.io/otel/metric"
)

// MetricLoadGenerator emits synthetic OTel metrics of every instrument kind, with
// operator control over rate, cardinality, and cardinality strategy. Designed to
// reproduce the two things that break Prometheus-family TSDBs in production:
//   1. High sustained sample rates (rate knob)
//   2. High series churn (cardinality + strategy knobs)
type MetricLoadGenerator struct {
	mu sync.RWMutex

	job  *MetricLoadJob
	last *MetricLoadStats

	// Persisted last-used request. Same pattern as LogLoadGenerator.
	savedConfig *MetricLoadRequest
	configPath  string
}

// MetricLoadRequest is the JSON body of /admin/api/metricloadgen/start.
type MetricLoadRequest struct {
	// Per-point emission rate across all selected instrument kinds. For observable
	// instruments this doesn't apply (they emit at meter's collection tick), but
	// their cardinality still counts against the pool.
	RatePerSec int `json:"ratePerSec"`

	// Which of the 7 OTel instrument kinds to include. Empty = all kinds.
	// Valid entries: "counter", "updowncounter", "histogram", "gauge",
	//                "observable_counter", "observable_updowncounter", "observable_gauge"
	Kinds []string `json:"kinds"`

	// Target number of unique series across the job — the "explosion" knob.
	// Setting this to 1M will visibly hurt Mimir; that's the point.
	Cardinality int `json:"cardinality"`

	// How the label pool is exercised:
	//   "random"    — each point picks a random slot; all slots warm over time
	//   "monotonic" — cycles 0..Cardinality-1; even sample distribution per series
	//   "churning"  — actively rotates ChurnFraction of the pool every ChurnIntervalSec.
	//                 Creates brand-new series continuously — the pattern that most
	//                 badly hurts TSDBs because it defeats block compaction.
	CardStrategy string `json:"cardStrategy"`

	// Churning strategy tuning. Ignored for random/monotonic.
	ChurnIntervalSec int     `json:"churnIntervalSec"` // rotate every N seconds
	ChurnFraction    float64 `json:"churnFraction"`    // fraction of pool to rotate (0.0-1.0)

	// Histogram value distribution: "uniform" | "bimodal" | "exponential"
	HistogramDist string `json:"histogramDist"`

	Workers     int `json:"workers"`
	DurationSec int `json:"durationSec"`
}

// MetricLoadStats mirrors the log-loadgen stats shape so the UI code can be nearly
// identical. Everything except IsRunning is populated whether running or frozen.
type MetricLoadStats struct {
	IsRunning        bool          `json:"isRunning"`
	RatePerSec       int           `json:"ratePerSec"`
	Kinds            []string      `json:"kinds"`
	Cardinality      int           `json:"cardinality"`
	CardStrategy     string        `json:"cardStrategy"`
	ChurnIntervalSec int           `json:"churnIntervalSec"`
	ChurnFraction    float64       `json:"churnFraction"`
	HistogramDist    string        `json:"histogramDist"`
	Workers          int           `json:"workers"`
	DurationSec      int           `json:"durationSec"`
	ElapsedSeconds   float64       `json:"elapsedSeconds"`
	PointsEmitted    int64         `json:"pointsEmitted"`
	UniqueSeries     int64         `json:"uniqueSeries"` // high-watermark of distinct labels seen
	Errors           int64         `json:"errors"`
	AchievedPps      float64       `json:"achievedPps"`
	Series           []SeriesPoint `json:"series"`
}

// MetricLoadJob owns the runtime state for one active job.
type MetricLoadJob struct {
	// Immutable config for the run
	req       MetricLoadRequest
	StartTime time.Time

	// Registered instruments. Nil for kinds the operator did not select.
	counter        metricapi.Int64Counter
	updown         metricapi.Int64UpDownCounter
	histogram      metricapi.Float64Histogram
	gauge          metricapi.Float64Gauge
	obsCounter     metricapi.Float64ObservableCounter
	obsUpDown      metricapi.Int64ObservableUpDownCounter
	obsGauge       metricapi.Float64ObservableGauge
	obsCallbackReg metricapi.Registration // for unregistering on stop

	// Label pool — the source of cardinality. Each entry is a unique value used
	// as the `series_id` attribute on the emitted point.
	poolMu     sync.RWMutex
	labelPool  []string
	poolCursor atomic.Int64 // monotonic strategy pointer
	nextNewID  atomic.Int64 // for churning: guaranteed-unique next id

	// Hot-path atomic counters
	pointsEmitted atomic.Int64
	uniqueSeries  atomic.Int64
	errors        atomic.Int64

	cancel context.CancelFunc
	done   chan struct{}
	series *seriesBuffer
}

// -----------------------------------------------------------------------------
// Constructor + persistence
// -----------------------------------------------------------------------------

// NewMetricLoadGenerator constructs the generator. configPath — same pattern as
// LogLoadGenerator — persists the last-used request so the UI can hydrate after
// restart. Pass "" to disable persistence (tests).
func NewMetricLoadGenerator(configPath string) *MetricLoadGenerator {
	g := &MetricLoadGenerator{configPath: configPath}
	if configPath != "" {
		if err := g.loadSavedConfig(); err != nil {
			log.Printf("metricloadgen: failed to load saved config: %v", err)
		}
	}
	return g
}

func (g *MetricLoadGenerator) loadSavedConfig() error {
	data, err := os.ReadFile(g.configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var r MetricLoadRequest
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("parse %s: %w", g.configPath, err)
	}
	g.mu.Lock()
	g.savedConfig = &r
	g.mu.Unlock()
	log.Printf("metricloadgen: restored last-used config from %s", g.configPath)
	return nil
}

func (g *MetricLoadGenerator) writeConfigLocked(req MetricLoadRequest) {
	if g.configPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(g.configPath), 0o755); err != nil {
		log.Printf("metricloadgen: mkdir %s: %v", filepath.Dir(g.configPath), err)
		return
	}
	b, _ := json.MarshalIndent(req, "", "  ")
	if err := os.WriteFile(g.configPath, b, 0o644); err != nil {
		log.Printf("metricloadgen: write %s: %v", g.configPath, err)
	}
}

// -----------------------------------------------------------------------------
// Metadata + validation
// -----------------------------------------------------------------------------

// AllMetricKinds is the canonical order returned by /meta and used when Kinds is empty.
var AllMetricKinds = []string{
	"counter",
	"updowncounter",
	"histogram",
	"gauge",
	"observable_counter",
	"observable_updowncounter",
	"observable_gauge",
}

// AllCardStrategies — same, for the UI dropdown.
var AllCardStrategies = []string{"random", "monotonic", "churning"}

// AllHistogramDists — same.
var AllHistogramDists = []string{"uniform", "bimodal", "exponential"}

const (
	minMetricRate         = 1
	maxMetricRate         = 500_000
	minCardinality        = 1
	maxCardinality        = 10_000_000 // Yes, 10M. This is a playground for pain.
	defaultChurnInterval  = 30
	defaultChurnFraction  = 0.10
	defaultHistogramDist  = "uniform"
	defaultCardStrategy   = "random"
	defaultMetricWorkers  = 8
	metricSeriesCap       = 300
)

func validateMetricLoadRequest(r *MetricLoadRequest) error {
	if r.RatePerSec < minMetricRate || r.RatePerSec > maxMetricRate {
		return fmt.Errorf("ratePerSec must be in [%d,%d]", minMetricRate, maxMetricRate)
	}
	if r.Cardinality < minCardinality || r.Cardinality > maxCardinality {
		return fmt.Errorf("cardinality must be in [%d,%d]", minCardinality, maxCardinality)
	}
	// Fill defaults
	if strings.TrimSpace(r.CardStrategy) == "" {
		r.CardStrategy = defaultCardStrategy
	}
	valid := false
	for _, s := range AllCardStrategies {
		if s == r.CardStrategy {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("cardStrategy must be one of %v", AllCardStrategies)
	}
	if r.CardStrategy == "churning" {
		if r.ChurnIntervalSec <= 0 {
			r.ChurnIntervalSec = defaultChurnInterval
		}
		if r.ChurnFraction <= 0 || r.ChurnFraction > 1 {
			r.ChurnFraction = defaultChurnFraction
		}
	}
	if strings.TrimSpace(r.HistogramDist) == "" {
		r.HistogramDist = defaultHistogramDist
	}
	valid = false
	for _, d := range AllHistogramDists {
		if d == r.HistogramDist {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("histogramDist must be one of %v", AllHistogramDists)
	}
	if len(r.Kinds) == 0 {
		r.Kinds = append([]string(nil), AllMetricKinds...)
	} else {
		for _, k := range r.Kinds {
			if !contains(AllMetricKinds, k) {
				return fmt.Errorf("unknown metric kind %q; must be one of %v", k, AllMetricKinds)
			}
		}
	}
	if r.Workers < 0 || r.Workers > 256 {
		return fmt.Errorf("workers must be in [0,256]")
	}
	if r.DurationSec < 0 {
		return errors.New("durationSec must be >= 0")
	}
	return nil
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// -----------------------------------------------------------------------------
// Start / Stop / Stats
// -----------------------------------------------------------------------------

func (g *MetricLoadGenerator) Start(req MetricLoadRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.job != nil {
		return errors.New("a synthetic metric job is already running")
	}
	g.last = nil

	if err := validateMetricLoadRequest(&req); err != nil {
		return err
	}
	if meter == nil {
		return errors.New("OTel meter not initialised — is InitMetrics running?")
	}

	if req.Workers == 0 {
		req.Workers = defaultMetricWorkers
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

	job := &MetricLoadJob{
		req:       req,
		StartTime: time.Now(),
		cancel:    cancel,
		done:      make(chan struct{}),
		series:    newSeriesBuffer(),
	}

	// Pre-generate the label pool. Values are "s0000000000", "s0000000001", …
	// Padded so lexical sort matches numeric — helps Grafana table displays.
	job.labelPool = make([]string, req.Cardinality)
	for i := 0; i < req.Cardinality; i++ {
		job.labelPool[i] = fmt.Sprintf("s%010d", i)
	}
	job.nextNewID.Store(int64(req.Cardinality))
	job.uniqueSeries.Store(int64(req.Cardinality))

	// Register the requested instruments on the shared meter.
	if err := g.registerInstruments(job); err != nil {
		cancel()
		return fmt.Errorf("register instruments: %w", err)
	}

	g.job = job
	g.savedConfig = &req
	g.writeConfigLocked(req)

	go g.runJob(parent, job)
	return nil
}

func (g *MetricLoadGenerator) Stop() error {
	g.mu.Lock()
	job := g.job
	g.mu.Unlock()
	if job == nil {
		return errors.New("no synthetic metric job running")
	}
	job.cancel()
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
	}
	return nil
}

func (g *MetricLoadGenerator) Stats() MetricLoadStats {
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
			return MetricLoadStats{
				IsRunning:        false,
				RatePerSec:       saved.RatePerSec,
				Kinds:            saved.Kinds,
				Cardinality:      saved.Cardinality,
				CardStrategy:     saved.CardStrategy,
				ChurnIntervalSec: saved.ChurnIntervalSec,
				ChurnFraction:    saved.ChurnFraction,
				HistogramDist:    saved.HistogramDist,
				Workers:          saved.Workers,
				DurationSec:      saved.DurationSec,
			}
		}
		return MetricLoadStats{IsRunning: false}
	}

	achieved := 0.0
	series := job.series.snapshot()
	if n := len(series); n > 0 {
		achieved = series[n-1].AchievedRps
	}
	return MetricLoadStats{
		IsRunning:        true,
		RatePerSec:       job.req.RatePerSec,
		Kinds:            job.req.Kinds,
		Cardinality:      job.req.Cardinality,
		CardStrategy:     job.req.CardStrategy,
		ChurnIntervalSec: job.req.ChurnIntervalSec,
		ChurnFraction:    job.req.ChurnFraction,
		HistogramDist:    job.req.HistogramDist,
		Workers:          job.req.Workers,
		DurationSec:      job.req.DurationSec,
		ElapsedSeconds:   time.Since(job.StartTime).Seconds(),
		PointsEmitted:    job.pointsEmitted.Load(),
		UniqueSeries:     job.uniqueSeries.Load(),
		Errors:           job.errors.Load(),
		AchievedPps:      achieved,
		Series:           series,
	}
}

// -----------------------------------------------------------------------------
// Instrument registration
// -----------------------------------------------------------------------------

func (g *MetricLoadGenerator) registerInstruments(job *MetricLoadJob) error {
	kinds := make(map[string]bool, len(job.req.Kinds))
	for _, k := range job.req.Kinds {
		kinds[k] = true
	}

	var err error
	if kinds["counter"] {
		if job.counter, err = meter.Int64Counter("mt_gen_counter",
			metricapi.WithDescription("Synthetic Int64Counter for load-gen playground"),
			metricapi.WithUnit("{op}"),
		); err != nil {
			return fmt.Errorf("counter: %w", err)
		}
	}
	if kinds["updowncounter"] {
		if job.updown, err = meter.Int64UpDownCounter("mt_gen_updowncounter",
			metricapi.WithDescription("Synthetic Int64UpDownCounter"),
		); err != nil {
			return fmt.Errorf("updown: %w", err)
		}
	}
	if kinds["histogram"] {
		if job.histogram, err = meter.Float64Histogram("mt_gen_histogram",
			metricapi.WithDescription("Synthetic Float64Histogram"),
			metricapi.WithUnit("s"),
		); err != nil {
			return fmt.Errorf("histogram: %w", err)
		}
	}
	if kinds["gauge"] {
		if job.gauge, err = meter.Float64Gauge("mt_gen_gauge",
			metricapi.WithDescription("Synthetic Float64Gauge"),
		); err != nil {
			return fmt.Errorf("gauge: %w", err)
		}
	}

	// Async instruments share a single callback so we only walk the pool once.
	var obs []metricapi.Observable
	if kinds["observable_counter"] {
		if job.obsCounter, err = meter.Float64ObservableCounter("mt_gen_obs_counter",
			metricapi.WithDescription("Synthetic Float64ObservableCounter"),
		); err != nil {
			return fmt.Errorf("obs_counter: %w", err)
		}
		obs = append(obs, job.obsCounter)
	}
	if kinds["observable_updowncounter"] {
		if job.obsUpDown, err = meter.Int64ObservableUpDownCounter("mt_gen_obs_updowncounter",
			metricapi.WithDescription("Synthetic Int64ObservableUpDownCounter"),
		); err != nil {
			return fmt.Errorf("obs_updown: %w", err)
		}
		obs = append(obs, job.obsUpDown)
	}
	if kinds["observable_gauge"] {
		if job.obsGauge, err = meter.Float64ObservableGauge("mt_gen_obs_gauge",
			metricapi.WithDescription("Synthetic Float64ObservableGauge"),
		); err != nil {
			return fmt.Errorf("obs_gauge: %w", err)
		}
		obs = append(obs, job.obsGauge)
	}

	if len(obs) > 0 {
		// Observable callback: reads a bounded snapshot of the current label pool
		// and observes one point per pool slot per registered instrument. Reading
		// the whole pool would flood a large-cardinality job, so we cap the walk.
		const maxObserveSlots = 5000
		obsFn := func(_ context.Context, o metricapi.Observer) error {
			job.poolMu.RLock()
			pool := job.labelPool
			job.poolMu.RUnlock()
			walk := len(pool)
			if walk > maxObserveSlots {
				walk = maxObserveSlots
			}
			for i := 0; i < walk; i++ {
				attrs := metricapi.WithAttributes(attribute.String("series_id", pool[i]))
				if job.obsCounter != nil {
					// Monotonic; use a value derived from iteration + wall time.
					o.ObserveFloat64(job.obsCounter, float64(i)+float64(time.Now().Unix()%600), attrs)
				}
				if job.obsUpDown != nil {
					o.ObserveInt64(job.obsUpDown, int64(rand.Intn(1000)-500), attrs)
				}
				if job.obsGauge != nil {
					o.ObserveFloat64(job.obsGauge, 50+25*math.Sin(float64(i)/10), attrs)
				}
			}
			return nil
		}
		if job.obsCallbackReg, err = meter.RegisterCallback(obsFn, obs...); err != nil {
			return fmt.Errorf("register obs callback: %w", err)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Run loop
// -----------------------------------------------------------------------------

func (g *MetricLoadGenerator) runJob(ctx context.Context, job *MetricLoadJob) {
	defer func() {
		close(job.done)

		// Unregister observable callback so obs series go stale in Mimir instead
		// of continuing to emit forever.
		if job.obsCallbackReg != nil {
			_ = job.obsCallbackReg.Unregister()
		}

		// Freeze the final snapshot for the UI.
		final := job.series.snapshot()
		frozen := MetricLoadStats{
			IsRunning:        false,
			RatePerSec:       job.req.RatePerSec,
			Kinds:            job.req.Kinds,
			Cardinality:      job.req.Cardinality,
			CardStrategy:     job.req.CardStrategy,
			ChurnIntervalSec: job.req.ChurnIntervalSec,
			ChurnFraction:    job.req.ChurnFraction,
			HistogramDist:    job.req.HistogramDist,
			Workers:          job.req.Workers,
			DurationSec:      job.req.DurationSec,
			ElapsedSeconds:   time.Since(job.StartTime).Seconds(),
			PointsEmitted:    job.pointsEmitted.Load(),
			UniqueSeries:     job.uniqueSeries.Load(),
			Errors:           job.errors.Load(),
			Series:           final,
		}
		if n := len(final); n > 0 {
			frozen.AchievedPps = final[n-1].AchievedRps
		}

		g.mu.Lock()
		if g.job == job {
			g.job = nil
		}
		g.last = &frozen
		g.mu.Unlock()
	}()

	// Sync-instrument emission pacer — 10ms batch pacing (same trick as logloadgen).
	// Only sync instruments count against ratePerSec; observable instruments emit
	// via their callback on the meter provider's own schedule.
	// Batch pacer (see traceloadgen for the low-rate rationale).
	tickInterval := 10 * time.Millisecond
	perTick := job.req.RatePerSec / int(time.Second/tickInterval)
	if perTick < 1 {
		perTick = 1
		tickInterval = time.Second / time.Duration(job.req.RatePerSec)
	}

	// Which sync kinds are active? Precompute so the hot path skips work.
	haveCounter := job.counter != nil
	haveUpdown := job.updown != nil
	haveHisto := job.histogram != nil
	haveGauge := job.gauge != nil
	syncKindsN := 0
	for _, b := range []bool{haveCounter, haveUpdown, haveHisto, haveGauge} {
		if b {
			syncKindsN++
		}
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
					if syncKindsN == 0 {
						// All chosen kinds are observable — pacer still runs but
						// there's nothing for workers to do.
						continue
					}
					g.emitOne(ctx, job, haveCounter, haveUpdown, haveHisto, haveGauge)
				}
			}
		}()
	}

	// Sampler: 1-second series buffer for the UI, same shape as logloadgen.
	sampleDone := make(chan struct{})
	go func() {
		defer close(sampleDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var lastPoints, lastErrs int64
		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				points := job.pointsEmitted.Load()
				errs := job.errors.Load()
				job.series.append(SeriesPoint{
					T:             t.UnixMilli(),
					AchievedRps:   float64(points - lastPoints),
					TargetRps:     job.req.RatePerSec,
					ThroughputMBs: 0, // metrics don't have byte-size — leave 0 for the chart
					Errors:        errs - lastErrs,
				})
				lastPoints, lastErrs = points, errs
			}
		}
	}()

	// Churning driver — periodically rotates the label pool.
	churnDone := make(chan struct{})
	if job.req.CardStrategy == "churning" {
		go g.runChurner(ctx, job, churnDone)
	} else {
		close(churnDone)
	}

	// Pacer — dispenses `perTick` tokens every tickInterval.
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
	<-churnDone
}

// emitOne dispatches one metric point across the active sync kinds. Each token
// corresponds to ONE point; if multiple kinds are enabled, that point counts as
// one per selected kind (so ratePerSec=1000 with 4 kinds = 4000 total emissions).
// This matches operator intuition: "1000 rps" means 1000 sample-events on each
// selected kind, not 1000 divided across them.
func (g *MetricLoadGenerator) emitOne(ctx context.Context, job *MetricLoadJob, haveCounter, haveUpdown, haveHisto, haveGauge bool) {
	seriesID := g.pickSeriesID(job)
	attrs := metricapi.WithAttributes(attribute.String("series_id", seriesID))

	if haveCounter {
		job.counter.Add(ctx, 1, attrs)
		job.pointsEmitted.Add(1)
	}
	if haveUpdown {
		delta := int64(rand.Intn(11) - 5) // random walk step
		job.updown.Add(ctx, delta, attrs)
		job.pointsEmitted.Add(1)
	}
	if haveHisto {
		job.histogram.Record(ctx, g.histogramSample(job.req.HistogramDist), attrs)
		job.pointsEmitted.Add(1)
	}
	if haveGauge {
		job.gauge.Record(ctx, rand.Float64()*100, attrs)
		job.pointsEmitted.Add(1)
	}
}

// pickSeriesID applies the configured cardinality strategy to choose which slot
// of the pool to attribute this point to.
func (g *MetricLoadGenerator) pickSeriesID(job *MetricLoadJob) string {
	job.poolMu.RLock()
	pool := job.labelPool
	job.poolMu.RUnlock()
	if len(pool) == 0 {
		return "s0000000000"
	}
	switch job.req.CardStrategy {
	case "monotonic":
		idx := job.poolCursor.Add(1) - 1
		return pool[int(idx)%len(pool)]
	case "churning":
		// Churning still emits from the current pool — the churner goroutine
		// rotates the pool in the background.
		return pool[rand.Intn(len(pool))]
	default: // random
		return pool[rand.Intn(len(pool))]
	}
}

// histogramSample returns one sample for the configured distribution.
func (g *MetricLoadGenerator) histogramSample(dist string) float64 {
	switch dist {
	case "bimodal":
		// Two clusters — one fast (~50ms), one slow (~2s). Reproduces the
		// "hot vs. cold path" pattern that reveals bugs in p99 alerting.
		if rand.Float64() < 0.85 {
			return 0.02 + rand.Float64()*0.06
		}
		return 1.5 + rand.Float64()*1.0
	case "exponential":
		// Exponential w/ mean 100ms — long-tailed like real request latency.
		return -math.Log(1-rand.Float64()) * 0.1
	default: // uniform
		return rand.Float64() * 2.0
	}
}

// runChurner rotates ChurnFraction of the label pool every ChurnIntervalSec,
// replacing rotated slots with brand-new never-before-seen IDs. This is the
// pattern that most badly hurts a Prometheus-family TSDB.
func (g *MetricLoadGenerator) runChurner(ctx context.Context, job *MetricLoadJob, done chan struct{}) {
	defer close(done)
	interval := time.Duration(job.req.ChurnIntervalSec) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			job.poolMu.Lock()
			n := len(job.labelPool)
			rotate := int(float64(n) * job.req.ChurnFraction)
			if rotate < 1 {
				rotate = 1
			}
			for i := 0; i < rotate; i++ {
				slot := rand.Intn(n)
				newID := job.nextNewID.Add(1)
				job.labelPool[slot] = fmt.Sprintf("s%010d", newID)
				job.uniqueSeries.Add(1)
			}
			job.poolMu.Unlock()
		}
	}
}
