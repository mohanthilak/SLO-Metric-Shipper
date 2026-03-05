package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// LoadGenerator manages traffic generation for endpoints
type LoadGenerator struct {
	mu              sync.RWMutex
	activeJobs      map[string]*LoadJob
	httpClient      *http.Client
	serverURL       string
}

// LoadJob represents an active load generation job
type LoadJob struct {
	Endpoint        string
	RequestsPerSec  int
	TotalRequests   int
	StartTime       time.Time
	cancel          context.CancelFunc
	requestsSent    atomic.Int64
	successCount    atomic.Int64
	clientErrorCount atomic.Int64
	serverErrorCount atomic.Int64
	mu              sync.RWMutex
}

// LoadJobRequest represents a request to start load generation
type LoadJobRequest struct {
	Endpoint       string `json:"endpoint"`
	RequestsPerSec int    `json:"requestsPerSec"`
	TotalRequests  int    `json:"totalRequests"`
}

// LoadJobStats represents statistics for a load generation job
type LoadJobStats struct {
	Endpoint         string  `json:"endpoint"`
	RequestsSent     int64   `json:"requestsSent"`
	SuccessCount     int64   `json:"successCount"`
	ClientErrorCount int64   `json:"clientErrorCount"`
	ServerErrorCount int64   `json:"serverErrorCount"`
	IsRunning        bool    `json:"isRunning"`
	ElapsedSeconds   float64 `json:"elapsedSeconds"`
	RequestsPerSec   int     `json:"requestsPerSec"`
	TotalRequests    int     `json:"totalRequests"`
}

// NewLoadGenerator creates a new load generator
func NewLoadGenerator(serverURL string) *LoadGenerator {
	return &LoadGenerator{
		activeJobs: make(map[string]*LoadJob),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		serverURL: serverURL,
	}
}

// StartLoadJob starts a new load generation job for an endpoint
func (lg *LoadGenerator) StartLoadJob(endpoint string, requestsPerSec, totalRequests int) error {
	lg.mu.Lock()
	defer lg.mu.Unlock()

	// Check if job already running for this endpoint
	if job, exists := lg.activeJobs[endpoint]; exists && job != nil {
		return fmt.Errorf("load generation already running for endpoint %s", endpoint)
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())

	// Create new job
	job := &LoadJob{
		Endpoint:       endpoint,
		RequestsPerSec: requestsPerSec,
		TotalRequests:  totalRequests,
		StartTime:      time.Now(),
		cancel:         cancel,
	}

	lg.activeJobs[endpoint] = job

	// Start load generation in background
	go lg.runLoadJob(ctx, job)

	return nil
}

// StopLoadJob stops a running load generation job
func (lg *LoadGenerator) StopLoadJob(endpoint string) error {
	lg.mu.Lock()
	defer lg.mu.Unlock()

	job, exists := lg.activeJobs[endpoint]
	if !exists || job == nil {
		return fmt.Errorf("no active load generation for endpoint %s", endpoint)
	}

	// Cancel the job
	job.cancel()

	return nil
}

// GetJobStats returns statistics for a load generation job
func (lg *LoadGenerator) GetJobStats(endpoint string) (*LoadJobStats, error) {
	lg.mu.RLock()
	defer lg.mu.RUnlock()

	job, exists := lg.activeJobs[endpoint]
	if !exists || job == nil {
		return &LoadJobStats{
			Endpoint:  endpoint,
			IsRunning: false,
		}, nil
	}

	return &LoadJobStats{
		Endpoint:         endpoint,
		RequestsSent:     job.requestsSent.Load(),
		SuccessCount:     job.successCount.Load(),
		ClientErrorCount: job.clientErrorCount.Load(),
		ServerErrorCount: job.serverErrorCount.Load(),
		IsRunning:        true,
		ElapsedSeconds:   time.Since(job.StartTime).Seconds(),
		RequestsPerSec:   job.RequestsPerSec,
		TotalRequests:    job.TotalRequests,
	}, nil
}

// GetAllJobStats returns statistics for all endpoints
func (lg *LoadGenerator) GetAllJobStats() map[string]*LoadJobStats {
	lg.mu.RLock()
	defer lg.mu.RUnlock()

	stats := make(map[string]*LoadJobStats)
	endpoints := []string{"users", "products", "orders", "checkout", "health"}

	for _, endpoint := range endpoints {
		job, exists := lg.activeJobs[endpoint]
		if !exists || job == nil {
			stats[endpoint] = &LoadJobStats{
				Endpoint:  endpoint,
				IsRunning: false,
			}
		} else {
			stats[endpoint] = &LoadJobStats{
				Endpoint:         endpoint,
				RequestsSent:     job.requestsSent.Load(),
				SuccessCount:     job.successCount.Load(),
				ClientErrorCount: job.clientErrorCount.Load(),
				ServerErrorCount: job.serverErrorCount.Load(),
				IsRunning:        true,
				ElapsedSeconds:   time.Since(job.StartTime).Seconds(),
				RequestsPerSec:   job.RequestsPerSec,
				TotalRequests:    job.TotalRequests,
			}
		}
	}

	return stats
}

// runLoadJob executes the load generation job
func (lg *LoadGenerator) runLoadJob(ctx context.Context, job *LoadJob) {
	defer func() {
		lg.mu.Lock()
		delete(lg.activeJobs, job.Endpoint)
		lg.mu.Unlock()
	}()

	// Calculate interval between requests
	interval := time.Second / time.Duration(job.RequestsPerSec)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	url := fmt.Sprintf("%s/api/%s", lg.serverURL, job.Endpoint)

	for {
		select {
		case <-ctx.Done():
			// Job cancelled
			return
		case <-ticker.C:
			// Send request
			go lg.sendRequest(url, job)

			// Check if we've reached total requests
			if job.TotalRequests > 0 && job.requestsSent.Load() >= int64(job.TotalRequests) {
				return
			}
		}
	}
}

// sendRequest sends a single HTTP request
func (lg *LoadGenerator) sendRequest(url string, job *LoadJob) {
	job.requestsSent.Add(1)

	resp, err := lg.httpClient.Get(url)
	if err != nil {
		// Network error - count as server error
		job.serverErrorCount.Add(1)
		return
	}
	defer resp.Body.Close()

	// Discard response body
	io.Copy(io.Discard, resp.Body)

	// Update counters based on status code
	statusCode := resp.StatusCode
	if statusCode >= 200 && statusCode < 300 {
		job.successCount.Add(1)
	} else if statusCode >= 400 && statusCode < 500 {
		job.clientErrorCount.Add(1)
	} else if statusCode >= 500 {
		job.serverErrorCount.Add(1)
	}
}
