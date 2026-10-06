# Spec — Synthetic Log Load Generator with Live Charts

**Audience:** a Claude Code session working inside this repo
(`~/Project/playground/SLO-Metric-Shipper`).
**Status:** implementation spec. Build against the existing patterns described below; do not
restructure the app.

---

## 1. Goal

Add a **second load-generation mode** to the admin UI: a *synthetic log generator* that emits
log records directly (not by triggering the `/api/*` endpoints), with operator control over:

- **Rate** — logs per second.
- **Size** — bytes per log record.
- **Format** — JSON, Syslog (RFC3164 & RFC5424), logfmt, Apache Common Log Format, plaintext.

…and a **Locust-style live graph** of the run (logs/sec target-vs-achieved, throughput MB/s,
cumulative totals, failures).

The existing status-code-distribution + request `LoadGenerator` stays exactly as it is. This is
additive.

---

## 2. Why this exists / what it's for

This app is a load source for testing a downstream log-ingest platform. The purpose of the
synthetic generator is to push **high, adjustable log volume in a chosen wire format and record
size** so the downstream ingest can be exercised. Two properties matter and drive the design:

1. **Format must be faithful on the wire** where it's being tested — see the sink caveat in §6.
2. **The generator must be able to outrun the target** and show it (target-vs-achieved on the
   graph), because the interesting question is where the downstream saturates.

---

## 3. What already exists (read before implementing)

| File | Relevant pattern to reuse |
|---|---|
| `loadgen.go` | `LoadGenerator` holds `map[string]*LoadJob`; jobs use `atomic.Int64` counters; `Start/Stop/GetJobStats`; `runLoadJob` paces with `time.NewTicker(time.Second/rps)`; goroutine per send. **Mirror this structure.** |
| `logs.go` | `InitLogs` builds an OTel `LoggerProvider` (OTLP gRPC → `OTEL_EXPORTER_OTLP_ENDPOINT`, default `localhost:4317`) and wires `slog`. Reuse this provider for the OTLP sink. |
| `admin.go` | `SetupAdminHandlers(mux, config, loadGen)` registers `/admin/api/loadgen/{start,stop,stats}`. **Register the new routes here the same way.** |
| `main.go` | Constructs `config`, `loadGen`, calls `SetupAdminHandlers`. **Construct the log generator here and pass it in.** |
| `static/admin.html` | Single page, inline CSS/JS, CDN `<script>` includes (Monaco). Stats polled every 1s via `setInterval`. **Add the new UI section and a CDN chart lib the same way.** |
| `config.go` | Thread-safe store with `sync.RWMutex`. Style reference for any shared state. |

**Conventions to honor:** standard library net/http (no framework), `encoding/json` handlers that
check method and return `{"status","message"}` JSON, `log.Printf` for server logs, atomic
counters for hot-path stats, `sync.RWMutex` for shared maps.

---

## 4. New files & changes (overview)

| File | Action |
|---|---|
| `logloadgen.go` | **New.** `LogLoadGenerator`, job model, pacing, stats + time-series ring buffer. |
| `logformats.go` | **New.** `LogFormatter` interface + JSON/Syslog3164/Syslog5424/logfmt/CLF/plaintext implementations. |
| `logsinks.go` | **New.** `LogSink` interface + `OTLPSink` and `DirectSink` (HTTP/TCP/UDP). |
| `admin.go` | **Edit.** Add handler methods + register `/admin/api/logloadgen/{start,stop,stats,meta}`. |
| `main.go` | **Edit.** Construct `LogLoadGenerator`, pass to `SetupAdminHandlers`. |
| `static/admin.html` | **Edit.** New "Synthetic Log Generation" section with controls + Chart.js live charts. |
| `README.md` | **Edit.** Document the new feature (brief). |

Keep each concern in its own file (formatters, sinks, generator) so the downstream session can
implement and test them independently.

---

## 5. Backend — `logloadgen.go`

### 5.1 Job model

A **single active synthetic-log job at a time** (Locust-like "one test running"). Do not key by
endpoint — there is no endpoint here.

```go
type LogLoadGenerator struct {
    mu     sync.RWMutex
    job    *LogLoadJob            // nil when idle
}

type LogLoadJob struct {
    // config (immutable for the job's life)
    RatePerSec   int             // target logs/sec across all workers
    BytesPerLog  int             // target size of each formatted record
    Format       string          // "json" | "syslog3164" | "syslog5424" | "logfmt" | "clf" | "plaintext"
    Sink         string          // "otlp" | "http" | "tcp" | "udp"
    SinkTarget   string          // endpoint for direct sinks (host:port or URL); ignored for otlp
    Workers      int             // concurrency; default derived (see §5.3)
    DurationSec  int             // 0 = run until stopped
    StartTime    time.Time

    cancel       context.CancelFunc

    // hot-path counters (atomic)
    logsSent     atomic.Int64
    bytesSent    atomic.Int64
    errors       atomic.Int64

    // time-series (see §5.4)
    series       *seriesBuffer
}
```

### 5.2 Public methods (mirror `loadgen.go`)

```go
func NewLogLoadGenerator(otlpSink *OTLPSink) *LogLoadGenerator
func (g *LogLoadGenerator) Start(req LogLoadRequest) error   // errors if a job is already running
func (g *LogLoadGenerator) Stop() error                      // errors if nothing running
func (g *LogLoadGenerator) Stats() LogLoadStats              // current totals + recent series
```

Request / stats shapes (JSON-tagged, for the API in §7):

```go
type LogLoadRequest struct {
    RatePerSec  int    `json:"ratePerSec"`
    BytesPerLog int    `json:"bytesPerLog"`
    Format      string `json:"format"`
    Sink        string `json:"sink"`
    SinkTarget  string `json:"sinkTarget"`
    Workers     int    `json:"workers"`      // 0 => auto
    DurationSec int    `json:"durationSec"`  // 0 => until stopped
}

type LogLoadStats struct {
    IsRunning      bool          `json:"isRunning"`
    RatePerSec     int           `json:"ratePerSec"`
    BytesPerLog    int           `json:"bytesPerLog"`
    Format         string        `json:"format"`
    Sink           string        `json:"sink"`
    ElapsedSeconds float64       `json:"elapsedSeconds"`
    LogsSent       int64         `json:"logsSent"`
    BytesSent      int64         `json:"bytesSent"`
    Errors         int64         `json:"errors"`
    AchievedRps    float64       `json:"achievedRps"`    // last full second
    ThroughputMBs  float64       `json:"throughputMBs"`  // last full second
    Series         []SeriesPoint `json:"series"`         // sliding window for the chart
}
```

### 5.3 Pacing & concurrency

The existing request loadgen fires one goroutine per tick at `time.Second/rps`. That will not hold
up at high logs/sec (tens of thousands). Use a **worker-pool + shared ticker/token** design:

- Compute `Workers` when `0`: `min(runtime.NumCPU()*2, something sane)`, or scale with `RatePerSec`.
- Split the target rate across workers; each worker paces with its own ticker at
  `time.Second / (RatePerSec/Workers)`.
- For very high rates, pace in **batches** (e.g. wake every 10ms, emit `rate*0.01` records) rather
  than one ticker tick per record — a per-record ticker caps out around a few thousand/sec.
- Each worker: format one record (§6 formatter), send via the sink (§6 sink), bump
  `logsSent`/`bytesSent` on success or `errors` on failure.
- Honor `ctx` cancellation (Stop) and `DurationSec` (auto-stop via `context.WithTimeout`).
- On job end, clear `g.job` under the lock (mirror `runLoadJob`'s deferred cleanup).

**Formatter reuse:** pre-render a template/skeleton once per job where possible; only vary the
cheap fields per record (timestamp, sequence, random level) so the hot path is allocation-light.
Records must still hit `BytesPerLog` (§6.3).

### 5.4 Time-series for the graph

Keep a ring buffer of **per-second samples** so the UI can draw a sliding window without the
server retaining unbounded history.

```go
type SeriesPoint struct {
    T             int64   `json:"t"`             // unix millis (bucket end)
    AchievedRps   float64 `json:"achievedRps"`
    TargetRps     int     `json:"targetRps"`
    ThroughputMBs float64 `json:"throughputMBs"`
    Errors        int64   `json:"errors"`        // in this bucket
}
```

- A background 1s sampler computes deltas of the atomic counters since the last tick, appends a
  `SeriesPoint`, and evicts points older than a fixed window (e.g. **300 points = 5 min**).
- `Stats()` returns the whole ring (already bounded). The UI redraws from it.
- Include `TargetRps` in every point so the chart can overlay target vs achieved (this is the
  load-testing signal — divergence = the generator or the downstream is saturating).

---

## 6. Formatters & sinks

### 6.1 `LogFormatter` (`logformats.go`)

```go
type LogFormatter interface {
    // Format returns one log record of approximately targetBytes, plus a
    // severity/level string for stats/coloring. seq is a monotonic counter.
    Format(seq int64, targetBytes int) (record string, level string)
    Name() string
    ContentType() string  // used by the HTTP direct sink; "" for line protocols
}
```

Implement, each producing realistic-looking output:

| Format key | Shape | ContentType |
|---|---|---|
| `json` | `{"timestamp","level","service","host","trace_id","msg", ...}` one JSON object per line | `application/json` |
| `syslog3164` | `<PRI>Mmm dd hh:mm:ss host app[pid]: msg` (BSD syslog) | `""` |
| `syslog5424` | `<PRI>1 ISO8601 host app pid msgid [sd] msg` (IETF syslog) | `""` |
| `logfmt` | `ts=... level=... service=... msg="..."` key=value pairs | `text/plain` |
| `clf` | Apache Common Log Format: `host - - [date] "GET /path HTTP/1.1" 200 1234` | `text/plain` |
| `plaintext` | `2026-.. LEVEL service: message` free text | `text/plain` |

Provide a small pool of realistic messages, services, hosts, and levels so runs look like real
traffic rather than identical lines (helps the downstream's dedup/cardinality behave realistically).

### 6.2 Format registry

A `map[string]LogFormatter` and a `Formats() []FormatMeta` helper feeding the `meta` endpoint
(§7) so the UI dropdown is server-driven, not hard-coded.

### 6.3 Size control

`targetBytes` is the **whole formatted record** length. Approach:

1. Build the record envelope (all fields except the free message).
2. Compute remaining budget = `targetBytes - len(envelope)`.
3. Pad the message field to fill the budget with word-like filler (not a single repeated char —
   the downstream may compress or tokenize, and repeated bytes would be unrealistic).
4. If the envelope already exceeds `targetBytes`, emit as-is and record the actual size (never
   truncate structural fields — a malformed JSON/syslog line would skew the ingest test).

Enforce sane bounds in validation (§8): e.g. `64B ≤ BytesPerLog ≤ 1MB`.

### 6.4 `LogSink` (`logsinks.go`) — **the important design decision**

```go
type LogSink interface {
    Send(ctx context.Context, record string, level string, contentType string) error
    Close() error
    Name() string
}
```

Two implementations:

**`OTLPSink`** — reuse the existing `LoggerProvider` from `logs.go` (or a dedicated one). Emits
each record as an OTel log record (the formatted string becomes the log body; `level` maps to
severity). Goes through the existing collector pipeline.

**`DirectSink`** — sends the **raw formatted bytes** to `SinkTarget`:
- `http` → POST body to a URL with the formatter's `ContentType`.
- `tcp` → write the line (newline-framed) to a TCP socket (syslog-style).
- `udp` → datagram per record (classic syslog UDP).
Use a connection pool / keep-alive; reconnect on error; count failures as `errors`.

> **Design caveat — state this in the UI and README.** Through **`OTLPSink`, the wire format the
> downstream receives is OTLP**, regardless of the chosen "format" — the format only changes the
> shape of the log *body* string inside the OTLP envelope. If the goal is to test how the
> downstream **parses** a given format (JSON vs syslog vs …), you must use a **`DirectSink`** so
> the bytes arrive on the wire in that format. Default the sink to `otlp` for
> pipeline-consistency runs, but make `DirectSink` a first-class choice and label it in the UI as
> "format-faithful". Do not let an operator believe they are testing syslog parsing while sending
> OTLP.

---

## 7. HTTP API (register in `admin.go` `SetupAdminHandlers`)

Follow the existing loadgen route/handler style (method check, JSON decode, `{"status","message"}`
responses).

| Method | Path | Body | Returns |
|---|---|---|---|
| POST | `/admin/api/logloadgen/start` | `LogLoadRequest` | `{"status","message"}`; 409 if already running |
| POST | `/admin/api/logloadgen/stop` | `{}` | `{"status","message"}`; 404 if nothing running |
| GET | `/admin/api/logloadgen/stats` | — | `LogLoadStats` (includes `series`) |
| GET | `/admin/api/logloadgen/meta` | — | `{ "formats":[{key,label}], "sinks":[{key,label,needsTarget}] }` |

Validation on `start` (return 400 with a clear message):

- `RatePerSec` in `[1, 200000]` (pick a ceiling; document it).
- `BytesPerLog` in `[64, 1048576]`.
- `Format` in the registry; `Sink` in `{otlp,http,tcp,udp}`.
- If `Sink != otlp`, `SinkTarget` required and parseable.
- `Workers` in `[0, 256]` (0 = auto).
- `DurationSec` ≥ 0.

---

## 8. Frontend — `static/admin.html`

Add a new section (a `<div class="config-card">` like the OTel editor block) titled
**"Synthetic Log Generation"**, placed after `#endpoints` and before the OTel config card.

### 8.1 Controls

- **Rate** (logs/sec) — number input + optional range slider.
- **Size per log** (bytes) — number input.
- **Format** — `<select>` populated from `/admin/api/logloadgen/meta`.
- **Sink** — `<select>` (`OTLP (pipeline)`, `HTTP direct`, `TCP direct`, `UDP direct`); show a
  **target endpoint** text field only when a direct sink is selected (`needsTarget`).
- **Workers** (optional, default auto) and **Duration** (0 = until stopped).
- **Start** / **Stop** buttons (disable/enable based on `isRunning`).
- A visible note rendering the §6.4 caveat when a non-OTLP-vs-OTLP mismatch matters.

### 8.2 Live charts (Locust-style)

Load **Chart.js via CDN** the same way Monaco is loaded (`<script src="https://cdn.jsdelivr.net/npm/chart.js">`). Add:

1. **Logs/sec over time** — line chart: achieved (solid) vs target (dashed), failures optionally
   on a second axis or a thin red area. This is the primary Locust analogue.
2. **Throughput (MB/s) over time** — line chart.
3. **Totals panel** — cumulative logs sent, bytes sent (human-readable), errors, elapsed.

Drive all three from a **1s poll of `/admin/api/logloadgen/stats`**, reusing the existing
`setInterval` pattern (add to the same refresh cycle or a dedicated one). Redraw the line charts
from `stats.series` (the server already windows it, so the client just maps points). Keep the
charts responsive and capped in height so the page stays usable.

### 8.3 UX details

- Match the existing CSS (`.config-card`, `.btn`, `.btn-primary`, `.status-message`, etc.).
- Use the existing `showStatus(msg, type)` helper for success/error toasts.
- While running: show a live "● running" indicator; disable config inputs; enable Stop.
- On stop or duration-end: freeze the charts (stop polling or show final series).

---

## 9. Wiring (`main.go`)

```go
// after loadGen := NewLoadGenerator(...)
otlpSink := NewOTLPSink(loggerProvider)     // or an internal provider
logLoadGen := NewLogLoadGenerator(otlpSink)
// pass logLoadGen into SetupAdminHandlers (extend its signature)
```

Extend `SetupAdminHandlers(mux, config, loadGen, logLoadGen)` and register the four routes.
Ensure graceful shutdown stops any running job (call `logLoadGen.Stop()` in the shutdown path, and
`Close()` any direct-sink connections).

---

## 10. Acceptance criteria

1. Starting a JSON run at, say, 5,000 logs/sec × 512 B via OTLP shows both charts updating each
   second, achieved-RPS tracking target, and totals climbing; logs arrive at the collector.
2. Switching format to `syslog3164` + `TCP direct` to a listening socket delivers RFC3164-shaped
   lines on the wire (verify with `nc -l` or the downstream).
3. Requesting a rate the machine/target cannot sustain shows **achieved < target** on the chart
   (the divergence is visible, not hidden).
4. `BytesPerLog` is honored within a small tolerance for every format (spot-check line lengths).
5. Stop halts emission within ~1s and freezes/leaves the final series; a new run can start.
6. `DurationSec > 0` auto-stops at the deadline.
7. Existing request-loadgen, status-code config, and OTel config editor are unchanged and still
   work.
8. Invalid requests (bad format, missing target for a direct sink, out-of-range rate/size) return
   400 with a clear message and do not start a job.

---

## 11. Out of scope (note as future work, do not build now)

- Synthetic **metrics/traces** generation (this spec is logs only).
- Per-log **size distributions** (fixed size is enough for v1; leave a hook).
- **Multiple concurrent named jobs** (single active job for v1).
- Percentile latency of the sink (Locust's response-time chart) — can be added later if the direct
  sinks measure per-send latency.
- Persisting run history across restarts.

---

## 12. Implementation order (suggested)

1. `logformats.go` + unit tests (size accuracy, valid JSON/syslog shapes).
2. `logsinks.go` — `OTLPSink` first (reuses existing provider), then `DirectSink`.
3. `logloadgen.go` — job model, worker-pool pacing, atomic counters, 1s sampler + ring buffer.
4. `admin.go` + `main.go` wiring; verify the four endpoints with `curl`.
5. `static/admin.html` — controls, then Chart.js charts, then polish.
6. README note + acceptance pass.

Keep the hot path allocation-light (§5.3) — the generator's own overhead must not be the
bottleneck you accidentally measure.
