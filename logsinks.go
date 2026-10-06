package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// LogSink is the abstraction used by the synthetic log load generator to ship a formatted
// record somewhere. Implementations must be safe for concurrent Send calls.
type LogSink interface {
	Send(ctx context.Context, record, level, contentType string) error
	Close() error
	Name() string
}

// severityFromLevel maps a formatter level string to an OTel Severity.
func severityFromLevel(level string) otellog.Severity {
	switch strings.ToUpper(level) {
	case "TRACE":
		return otellog.SeverityTrace
	case "DEBUG":
		return otellog.SeverityDebug
	case "INFO", "NOTICE":
		return otellog.SeverityInfo
	case "WARN", "WARNING":
		return otellog.SeverityWarn
	case "ERROR", "ERR":
		return otellog.SeverityError
	case "CRIT", "CRITICAL", "ALERT", "EMERG", "FATAL":
		return otellog.SeverityFatal
	default:
		return otellog.SeverityInfo
	}
}

// ---------- OTLPSink ----------

// OTLPSink emits each synthetic record through the existing OTel LoggerProvider so it
// takes the same path as the app's own logs (OTLP -> collector).
//
// NOTE (design caveat, mirrored in the UI): what arrives on the wire is OTLP-encoded log
// records. The record body is the formatted string, so the *body* preserves the chosen
// format (JSON/syslog/logfmt/...), but the *wire* is OTLP. For format-faithful wire
// testing (e.g. verifying a downstream's syslog parser) use a DirectSink instead.
type OTLPSink struct {
	logger otellog.Logger
}

// NewOTLPSink builds an OTLPSink over the given provider. If provider is nil, Send is a
// no-op error so the generator can surface the misconfiguration.
func NewOTLPSink(provider *sdklog.LoggerProvider) *OTLPSink {
	if provider == nil {
		return &OTLPSink{}
	}
	return &OTLPSink{logger: provider.Logger("logloadgen")}
}

func (s *OTLPSink) Name() string { return "otlp" }

func (s *OTLPSink) Send(ctx context.Context, record, level, _ string) error {
	if s.logger == nil {
		return errors.New("otlp sink: no logger provider configured")
	}
	var r otellog.Record
	now := time.Now()
	r.SetTimestamp(now)
	r.SetObservedTimestamp(now)
	r.SetSeverity(severityFromLevel(level))
	r.SetSeverityText(strings.ToUpper(level))
	r.SetBody(otellog.StringValue(record))
	s.logger.Emit(ctx, r)
	return nil
}

func (s *OTLPSink) Close() error { return nil }

// ---------- DirectSink ----------

// DirectSink writes the raw formatted bytes to the operator-supplied target. Choose this
// when the downstream needs the record on the wire in its declared format.
type DirectSink struct {
	scheme string // "http" | "tcp" | "udp"
	target string

	// HTTP
	httpClient  *http.Client
	httpURL     string
	httpHeaders map[string]string // extra headers (e.g. Authorization: Bearer …). HTTP only.

	// TCP
	tcpAddr string
	tcpMu   sync.Mutex
	tcpConn net.Conn
	tcpDial time.Duration

	// UDP
	udpAddr *net.UDPAddr
	udpConn *net.UDPConn
}

// NewDirectSink constructs a DirectSink for the given scheme and target. Target format:
//   - http/https: full URL (validated by url.Parse)
//   - tcp/udp:    host:port (validated by net.ResolveTCPAddr/ResolveUDPAddr)
//
// headers is only meaningful for the http scheme; ignored for tcp/udp.
func NewDirectSink(scheme, target string, headers map[string]string) (*DirectSink, error) {
	scheme = strings.ToLower(scheme)
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, errors.New("direct sink: empty target")
	}
	s := &DirectSink{scheme: scheme, target: target}
	switch scheme {
	case "http":
		u, err := url.Parse(target)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("direct sink: invalid http url %q", target)
		}
		s.httpURL = target
		s.httpHeaders = headers
		s.httpClient = &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     30 * time.Second,
				DisableCompression:  true,
			},
		}
	case "tcp":
		if _, err := net.ResolveTCPAddr("tcp", target); err != nil {
			return nil, fmt.Errorf("direct sink: invalid tcp target %q: %w", target, err)
		}
		s.tcpAddr = target
		s.tcpDial = 3 * time.Second
	case "udp":
		addr, err := net.ResolveUDPAddr("udp", target)
		if err != nil {
			return nil, fmt.Errorf("direct sink: invalid udp target %q: %w", target, err)
		}
		s.udpAddr = addr
		conn, err := net.DialUDP("udp", nil, addr)
		if err != nil {
			return nil, fmt.Errorf("direct sink: udp dial: %w", err)
		}
		s.udpConn = conn
	default:
		return nil, fmt.Errorf("direct sink: unsupported scheme %q", scheme)
	}
	return s, nil
}

func (s *DirectSink) Name() string { return s.scheme }

func (s *DirectSink) Send(ctx context.Context, record, _ string, contentType string) error {
	switch s.scheme {
	case "http":
		return s.sendHTTP(ctx, record, contentType)
	case "tcp":
		return s.sendTCP(record)
	case "udp":
		return s.sendUDP(record)
	}
	return fmt.Errorf("direct sink: unknown scheme %q", s.scheme)
}

func (s *DirectSink) sendHTTP(ctx context.Context, record, contentType string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.httpURL, bytes.NewBufferString(record))
	if err != nil {
		return err
	}
	if contentType == "" {
		contentType = "text/plain"
	}
	req.Header.Set("Content-Type", contentType)
	// Operator-supplied headers (Authorization, X-Api-Key, custom tenant hints, etc.).
	// Content-Type above wins if the operator also set one — swap the two lines to invert.
	for k, v := range s.httpHeaders {
		req.Header.Set(k, v)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("direct sink: http status %d", resp.StatusCode)
	}
	return nil
}

func (s *DirectSink) sendTCP(record string) error {
	s.tcpMu.Lock()
	defer s.tcpMu.Unlock()
	if s.tcpConn == nil {
		c, err := net.DialTimeout("tcp", s.tcpAddr, s.tcpDial)
		if err != nil {
			return err
		}
		s.tcpConn = c
	}
	line := record
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if _, err := s.tcpConn.Write([]byte(line)); err != nil {
		// Drop the broken connection so the next Send redials.
		_ = s.tcpConn.Close()
		s.tcpConn = nil
		return err
	}
	return nil
}

func (s *DirectSink) sendUDP(record string) error {
	if s.udpConn == nil {
		return errors.New("direct sink: udp connection closed")
	}
	_, err := s.udpConn.Write([]byte(record))
	return err
}

func (s *DirectSink) Close() error {
	var firstErr error
	s.tcpMu.Lock()
	if s.tcpConn != nil {
		if err := s.tcpConn.Close(); err != nil {
			firstErr = err
		}
		s.tcpConn = nil
	}
	s.tcpMu.Unlock()
	if s.udpConn != nil {
		if err := s.udpConn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		s.udpConn = nil
	}
	if s.httpClient != nil {
		s.httpClient.CloseIdleConnections()
	}
	return firstErr
}

// SinkMeta is the shape returned by the /meta endpoint so the UI can render the sink
// dropdown and know whether to show the "target" field.
type SinkMeta struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	NeedsTarget bool   `json:"needsTarget"`
}

// SinkMetadata is the ordered display list for the UI.
func SinkMetadata() []SinkMeta {
	return []SinkMeta{
		{Key: "otlp", Label: "OTLP (pipeline)", NeedsTarget: false},
		{Key: "http", Label: "HTTP direct (format-faithful)", NeedsTarget: true},
		{Key: "tcp", Label: "TCP direct (format-faithful)", NeedsTarget: true},
		{Key: "udp", Label: "UDP direct (format-faithful)", NeedsTarget: true},
	}
}
