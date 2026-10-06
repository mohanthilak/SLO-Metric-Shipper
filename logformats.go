package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LogFormatter renders one synthetic log record of approximately targetBytes.
// The returned level is the severity/category (used for stats and sink severity mapping).
// Formatters MUST NOT truncate structural fields to satisfy targetBytes — they pad the
// message field instead, and if the envelope already exceeds targetBytes, they emit as-is.
type LogFormatter interface {
	Format(seq int64, targetBytes int) (record string, level string)
	Name() string
	ContentType() string // "" for line protocols (syslog/plaintext); MIME for HTTP direct sink
}

// FormatMeta is the shape returned by the /meta endpoint so the UI dropdown is server-driven.
type FormatMeta struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// Realistic sample pools — keep runs from looking like identical repeated lines.
var (
	sampleServices = []string{"orders-api", "checkout-svc", "auth-gateway", "cart-worker", "inventory", "billing", "notifier", "search-svc"}
	sampleHosts    = []string{"host-a1", "host-b2", "host-c3", "web-01", "web-02", "worker-07", "edge-11"}
	sampleLevels   = []string{"INFO", "INFO", "INFO", "INFO", "WARN", "WARN", "ERROR", "DEBUG"}
	sampleMessages = []string{
		"request handled",
		"cache miss for key",
		"db query completed",
		"payment authorized",
		"user session created",
		"rate limit engaged",
		"downstream timeout",
		"replay backlog draining",
		"connection reset by peer",
		"queue depth exceeded threshold",
	}
	sampleMethods = []string{"GET", "POST", "PUT", "DELETE"}
	samplePaths   = []string{"/api/users", "/api/products", "/api/orders", "/api/checkout", "/api/health"}
	fillerWords   = []string{"lorem", "ipsum", "dolor", "sit", "amet", "consectetur", "adipiscing", "elit", "sed", "eiusmod", "tempor", "incididunt", "labore", "magna", "aliqua"}

	processID  = os.Getpid()
	hostname   = mustHostname()
	startBoot  = time.Now()
	messageSD  = "[origin ip=\"127.0.0.1\" software=\"slo-metric-generator\"]"
)

func mustHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	return h
}

// pickWithRand returns a pseudo-random element without allocating a new rand.Rand.
func pick[T any](s []T) T { return s[mrand.Intn(len(s))] }

// padTo grows s with word-like filler until it reaches targetBytes. If s already exceeds
// target, s is returned unchanged.
func padTo(s string, targetBytes int) string {
	if len(s) >= targetBytes {
		return s
	}
	need := targetBytes - len(s)
	var b strings.Builder
	b.Grow(len(s) + need)
	b.WriteString(s)
	// Space before filler so we don't glue onto the last word.
	if !strings.HasSuffix(s, " ") {
		b.WriteByte(' ')
		need--
	}
	for need > 0 {
		w := fillerWords[mrand.Intn(len(fillerWords))]
		if need <= len(w) {
			b.WriteString(w[:need])
			need = 0
			break
		}
		b.WriteString(w)
		b.WriteByte(' ')
		need -= len(w) + 1
	}
	return b.String()
}

// newTraceID returns a 32-hex-char id suitable for correlation fields.
func newTraceID() string {
	var buf [16]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// severityToPRI maps a level to the syslog PRI value using facility user (1).
// facility*8 + severity.
func severityToPRI(level string) int {
	facility := 1
	sev := 6 // info
	switch strings.ToUpper(level) {
	case "DEBUG":
		sev = 7
	case "INFO":
		sev = 6
	case "NOTICE":
		sev = 5
	case "WARN", "WARNING":
		sev = 4
	case "ERROR", "ERR":
		sev = 3
	case "CRIT", "CRITICAL":
		sev = 2
	case "ALERT":
		sev = 1
	case "EMERG":
		sev = 0
	}
	return facility*8 + sev
}

// ---------- JSON ----------

type jsonFormatter struct{}

func (jsonFormatter) Name() string        { return "json" }
func (jsonFormatter) ContentType() string { return "application/json" }
func (jsonFormatter) Format(seq int64, targetBytes int) (string, string) {
	level := pick(sampleLevels)
	msg := pick(sampleMessages)
	// Hand-serialised JSON to keep the hot path allocation-light.
	var b strings.Builder
	b.WriteString(`{"timestamp":"`)
	b.WriteString(time.Now().UTC().Format(time.RFC3339Nano))
	b.WriteString(`","level":"`)
	b.WriteString(level)
	b.WriteString(`","service":"`)
	b.WriteString(pick(sampleServices))
	b.WriteString(`","host":"`)
	b.WriteString(pick(sampleHosts))
	b.WriteString(`","trace_id":"`)
	b.WriteString(newTraceID())
	b.WriteString(`","seq":`)
	b.WriteString(strconv.FormatInt(seq, 10))
	b.WriteString(`,"msg":"`)
	// Compute how much padding to fit in msg. Reserve 2 bytes for closing quote+brace.
	envelope := b.Len() + 2
	msgBudget := targetBytes - envelope
	if msgBudget < len(msg) {
		msgBudget = len(msg) // never truncate structural content
	}
	b.WriteString(escapeJSON(padTo(msg, msgBudget)))
	b.WriteString(`"}`)
	return b.String(), level
}

func escapeJSON(s string) string {
	// Cheap escape — filler + samples never contain " or backslashes, but be safe.
	if !strings.ContainsAny(s, `"\`) {
		return s
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// ---------- Syslog RFC3164 (BSD) ----------

type syslog3164Formatter struct{}

func (syslog3164Formatter) Name() string        { return "syslog3164" }
func (syslog3164Formatter) ContentType() string { return "" }
func (syslog3164Formatter) Format(seq int64, targetBytes int) (string, string) {
	level := pick(sampleLevels)
	pri := severityToPRI(level)
	// Mmm d hh:mm:ss (day is space-padded, not zero-padded, per RFC3164).
	now := time.Now()
	ts := now.Format("Jan _2 15:04:05")
	app := pick(sampleServices)
	envelope := fmt.Sprintf("<%d>%s %s %s[%d]: ", pri, ts, hostname, app, processID)
	msg := fmt.Sprintf("seq=%d %s", seq, pick(sampleMessages))
	return padTo(envelope+msg, targetBytes), level
}

// ---------- Syslog RFC5424 (IETF) ----------

type syslog5424Formatter struct{}

func (syslog5424Formatter) Name() string        { return "syslog5424" }
func (syslog5424Formatter) ContentType() string { return "" }
func (syslog5424Formatter) Format(seq int64, targetBytes int) (string, string) {
	level := pick(sampleLevels)
	pri := severityToPRI(level)
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	app := pick(sampleServices)
	msgID := fmt.Sprintf("ID%d", seq%1000)
	// <PRI>1 TIMESTAMP HOSTNAME APP-NAME PROCID MSGID SD MSG
	envelope := fmt.Sprintf("<%d>1 %s %s %s %d %s %s ", pri, ts, hostname, app, processID, msgID, messageSD)
	msg := fmt.Sprintf("seq=%d %s", seq, pick(sampleMessages))
	return padTo(envelope+msg, targetBytes), level
}

// ---------- logfmt ----------

type logfmtFormatter struct{}

func (logfmtFormatter) Name() string        { return "logfmt" }
func (logfmtFormatter) ContentType() string { return "text/plain" }
func (logfmtFormatter) Format(seq int64, targetBytes int) (string, string) {
	level := pick(sampleLevels)
	msg := pick(sampleMessages)
	envelope := fmt.Sprintf(
		`ts=%s level=%s service=%s host=%s seq=%d trace_id=%s msg=`,
		time.Now().UTC().Format(time.RFC3339Nano),
		strings.ToLower(level),
		pick(sampleServices),
		pick(sampleHosts),
		seq,
		newTraceID(),
	)
	// Quote the msg value; pad inside quotes.
	msgBudget := targetBytes - len(envelope) - 2
	if msgBudget < len(msg) {
		msgBudget = len(msg)
	}
	return envelope + `"` + padTo(msg, msgBudget) + `"`, level
}

// ---------- Apache Common Log Format ----------

type clfFormatter struct{}

func (clfFormatter) Name() string        { return "clf" }
func (clfFormatter) ContentType() string { return "text/plain" }
func (clfFormatter) Format(seq int64, targetBytes int) (string, string) {
	// host ident authuser [date] "METHOD path HTTP/1.1" status size
	// Choose a status; treat non-2xx as WARN/ERROR for stats.
	statusCodes := []int{200, 200, 200, 200, 301, 404, 500, 502}
	status := statusCodes[mrand.Intn(len(statusCodes))]
	level := "INFO"
	switch {
	case status >= 500:
		level = "ERROR"
	case status >= 400:
		level = "WARN"
	}
	// [10/Oct/2000:13:55:36 -0700]
	date := time.Now().Format("[02/Jan/2006:15:04:05 -0700]")
	host := "10.0." + strconv.Itoa(mrand.Intn(255)) + "." + strconv.Itoa(mrand.Intn(255))
	method := pick(sampleMethods)
	path := pick(samplePaths)
	size := 100 + mrand.Intn(9000)
	envelope := fmt.Sprintf(`%s - - %s "%s %s HTTP/1.1" %d %d`, host, date, method, path, status, size)
	// CLF has no free-form field. Append a trailing comment-like padding so byte target is met.
	if len(envelope) >= targetBytes {
		return envelope, level
	}
	return envelope + " " + padTo("#seq="+strconv.FormatInt(seq, 10), targetBytes-len(envelope)-1), level
}

// ---------- CEF (Common Event Format) — ArcSight/Splunk security-event standard ----------
//
// Wire shape: CEF:0|Vendor|Product|Version|SignatureID|Name|Severity|k=v k=v ...
// The extension key=value part is where padding lives — the header fields must
// remain structural.

type cefFormatter struct{}

var (
	cefVendors   = []string{"Trend Micro", "Palo Alto", "Cisco", "Fortinet", "Symantec", "CrowdStrike", "Splunk"}
	cefProducts  = []string{"Deep Security", "PAN-OS", "ASA", "FortiGate", "Endpoint Protection", "Falcon", "Enterprise"}
	cefSignames  = []string{"Application Blocked", "Suspicious Login", "Port Scan Detected", "Malware Quarantined", "Firewall Denied", "SSH Brute Force"}
	cefSeverities = []int{2, 4, 5, 6, 7, 8, 9, 10} // CEF severity is 0-10
	cefExtKeys    = []string{"src", "dst", "spt", "dpt", "usrName", "proto", "cs1", "cs2", "duser", "outcome"}
)

func (cefFormatter) Name() string        { return "cef" }
func (cefFormatter) ContentType() string { return "text/plain" }
func (cefFormatter) Format(seq int64, targetBytes int) (string, string) {
	sev := cefSeverities[mrand.Intn(len(cefSeverities))]
	// Map CEF severity → text level for stats.
	level := "INFO"
	switch {
	case sev >= 9:
		level = "CRITICAL"
	case sev >= 7:
		level = "ERROR"
	case sev >= 4:
		level = "WARN"
	}
	// Header: 7 pipe-delimited fields per RFC-ish CEF:0 spec.
	header := fmt.Sprintf("CEF:0|%s|%s|1.0.0|%d|%s|%d|",
		pick(cefVendors), pick(cefProducts),
		1000+mrand.Intn(200), // SignatureID
		pick(cefSignames), sev,
	)
	// Extension section — repeated key=value. Add a base set, then pad with
	// synthetic k=v pairs until we hit targetBytes.
	var b strings.Builder
	b.Grow(targetBytes)
	b.WriteString(header)
	b.WriteString(fmt.Sprintf("src=10.0.0.%d dst=192.168.1.%d spt=%d dpt=%d usrName=u%d proto=TCP seq=%d",
		mrand.Intn(255), mrand.Intn(255),
		1024+mrand.Intn(60000), 80+mrand.Intn(10),
		mrand.Intn(10000), seq))
	// Pad by appending more k=v pairs. Never truncate structural fields.
	for b.Len() < targetBytes {
		k := cefExtKeys[mrand.Intn(len(cefExtKeys))]
		v := fillerWords[mrand.Intn(len(fillerWords))]
		add := fmt.Sprintf(" %s=%s", k, v)
		if b.Len()+len(add) > targetBytes {
			// Not enough room for another full pair — fill with a single k= to
			// meet the byte target while keeping the wire shape parsable.
			remaining := targetBytes - b.Len()
			if remaining > 3 {
				b.WriteString(" x=")
				for i := 0; i < remaining-3; i++ {
					b.WriteByte('_')
				}
			}
			break
		}
		b.WriteString(add)
	}
	return b.String(), level
}

// ---------- File replay — reads lines from an operator-supplied file on loop ----------
//
// The "sample" name refers to a file under configs/logloadgen-samples/{name}.txt.
// The file is loaded once into memory at first use, then emitted line-by-line on
// a rolling cursor. When the cursor hits the end, it wraps.

type fileReplayFormatter struct {
	name  string // sample name (file base name without extension)
	mu    sync.RWMutex
	lines []string
	cur   atomic.Int64
	err   error // sticky error if loading fails
}

func newFileReplayFormatter(sampleName string) *fileReplayFormatter {
	f := &fileReplayFormatter{name: sampleName}
	if err := f.load(); err != nil {
		f.err = err
	}
	return f
}

const logSamplesDir = "configs/logloadgen-samples"

func (f *fileReplayFormatter) load() error {
	// Sanitise: sample names must be plain identifiers to prevent path escape.
	if strings.ContainsAny(f.name, "/.\\") || f.name == "" {
		return fmt.Errorf("invalid sample name %q", f.name)
	}
	path := filepath.Join(logSamplesDir, f.name+".txt")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	// Drop blank trailing lines.
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return fmt.Errorf("sample %q has no non-empty lines", f.name)
	}
	f.mu.Lock()
	f.lines = kept
	f.mu.Unlock()
	return nil
}

func (f *fileReplayFormatter) Name() string        { return "replay:" + f.name }
func (f *fileReplayFormatter) ContentType() string { return "text/plain" }
func (f *fileReplayFormatter) Format(seq int64, targetBytes int) (string, string) {
	if f.err != nil {
		// Emit a diagnostic line so operators see the load failure downstream.
		return "REPLAY_ERROR: " + f.err.Error(), "ERROR"
	}
	f.mu.RLock()
	lines := f.lines
	f.mu.RUnlock()
	idx := int(f.cur.Add(1)-1) % len(lines)
	line := lines[idx]
	// Replay ignores targetBytes — the whole point is to ship the file faithfully.
	// Guess a level from the content for stats colouring.
	return line, guessLevel(line)
}

// guessLevel is a best-effort log-level sniffer for replayed lines.
func guessLevel(line string) string {
	up := strings.ToUpper(line)
	switch {
	case strings.Contains(up, "ERROR") || strings.Contains(up, "ERR "):
		return "ERROR"
	case strings.Contains(up, "WARN"):
		return "WARN"
	case strings.Contains(up, "DEBUG"):
		return "DEBUG"
	default:
		return "INFO"
	}
}

// ---------- plaintext ----------

type plaintextFormatter struct{}

func (plaintextFormatter) Name() string        { return "plaintext" }
func (plaintextFormatter) ContentType() string { return "text/plain" }
func (plaintextFormatter) Format(seq int64, targetBytes int) (string, string) {
	level := pick(sampleLevels)
	envelope := fmt.Sprintf("%s %s %s: seq=%d ",
		time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		level,
		pick(sampleServices),
		seq,
	)
	return padTo(envelope+pick(sampleMessages), targetBytes), level
}

// ---------- Registry ----------

var logFormatters = map[string]LogFormatter{
	"json":       jsonFormatter{},
	"syslog3164": syslog3164Formatter{},
	"syslog5424": syslog5424Formatter{},
	"logfmt":     logfmtFormatter{},
	"clf":        clfFormatter{},
	"cef":        cefFormatter{},
	"plaintext":  plaintextFormatter{},
}

// FormatMetadata is the ordered display list for the UI. Non-replay formats only.
// Replay samples are enumerated separately by ListReplaySamples so the UI can
// build a second dropdown for them.
func FormatMetadata() []FormatMeta {
	return []FormatMeta{
		{Key: "json", Label: "JSON"},
		{Key: "syslog3164", Label: "Syslog RFC3164 (BSD)"},
		{Key: "syslog5424", Label: "Syslog RFC5424 (IETF)"},
		{Key: "logfmt", Label: "logfmt"},
		{Key: "clf", Label: "Apache CLF"},
		{Key: "cef", Label: "CEF (ArcSight/Splunk)"},
		{Key: "plaintext", Label: "Plain text"},
	}
}

// LookupFormatter returns the formatter for key, or nil if unknown.
// Keys of the form "replay:<sampleName>" resolve to a file-replay formatter
// that reads from configs/logloadgen-samples/<sampleName>.txt.
func LookupFormatter(key string) LogFormatter {
	if strings.HasPrefix(key, "replay:") {
		return newFileReplayFormatter(strings.TrimPrefix(key, "replay:"))
	}
	if f, ok := logFormatters[key]; ok {
		return f
	}
	return nil
}

// ListReplaySamples enumerates *.txt files under configs/logloadgen-samples/.
// Returns an empty slice (not an error) if the dir doesn't exist yet.
func ListReplaySamples() []string {
	entries, err := os.ReadDir(logSamplesDir)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".txt") {
			out = append(out, strings.TrimSuffix(name, ".txt"))
		}
	}
	return out
}

// SaveReplaySample writes content to configs/logloadgen-samples/<name>.txt.
// Sample names are sanitised — must match [a-zA-Z0-9_-]+.
func SaveReplaySample(name string, content []byte) error {
	if name == "" || strings.ContainsAny(name, "/.\\") {
		return fmt.Errorf("invalid sample name %q", name)
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return fmt.Errorf("sample name may only contain [a-zA-Z0-9_-]")
		}
	}
	if err := os.MkdirAll(logSamplesDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(logSamplesDir, name+".txt"), content, 0o644)
}

