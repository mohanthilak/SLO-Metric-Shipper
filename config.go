package main

import (
	"math/rand"
	"sync"
)

// StatusCodeDistribution represents the probability distribution for status codes
type StatusCodeDistribution struct {
	SuccessRate      float64 // 2xx status codes
	ClientErrorRate  float64 // 4xx status codes
	ServerErrorRate  float64 // 5xx status codes
}

// Config holds thread-safe configuration for all endpoints
type Config struct {
	mu            sync.RWMutex
	distributions map[string]StatusCodeDistribution
}

// NewConfig creates a new configuration store with default values (100% success)
func NewConfig() *Config {
	endpoints := []string{"users", "products", "orders", "checkout", "health"}
	distributions := make(map[string]StatusCodeDistribution)

	for _, endpoint := range endpoints {
		distributions[endpoint] = StatusCodeDistribution{
			SuccessRate:     1.0,
			ClientErrorRate: 0.0,
			ServerErrorRate: 0.0,
		}
	}

	return &Config{
		distributions: distributions,
	}
}

// GetStatusCodeDistribution returns the current distribution for an endpoint
func (c *Config) GetStatusCodeDistribution(endpoint string) StatusCodeDistribution {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if dist, ok := c.distributions[endpoint]; ok {
		return dist
	}

	// Default to 100% success if endpoint not found
	return StatusCodeDistribution{
		SuccessRate:     1.0,
		ClientErrorRate: 0.0,
		ServerErrorRate: 0.0,
	}
}

// SetStatusCodeRates updates the distribution for an endpoint
func (c *Config) SetStatusCodeRates(endpoint string, success, clientError, serverError float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.distributions[endpoint] = StatusCodeDistribution{
		SuccessRate:     success,
		ClientErrorRate: clientError,
		ServerErrorRate: serverError,
	}
}

// GetRandomStatusCode returns a status code based on the configured distribution
func (c *Config) GetRandomStatusCode(endpoint string) int {
	dist := c.GetStatusCodeDistribution(endpoint)

	// Generate random number between 0 and 1
	r := rand.Float64()

	// Determine status code category based on probability
	if r < dist.SuccessRate {
		// 2xx - Success
		return 200
	} else if r < dist.SuccessRate+dist.ClientErrorRate {
		// 4xx - Client errors
		// Randomly choose between different 4xx codes
		clientErrors := []int{400, 404, 429}
		return clientErrors[rand.Intn(len(clientErrors))]
	} else {
		// 5xx - Server errors
		// Randomly choose between different 5xx codes
		serverErrors := []int{500, 503}
		return serverErrors[rand.Intn(len(serverErrors))]
	}
}

// GetAllConfigs returns all endpoint configurations
func (c *Config) GetAllConfigs() map[string]StatusCodeDistribution {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Return a copy to prevent external modifications
	result := make(map[string]StatusCodeDistribution)
	for k, v := range c.distributions {
		result[k] = v
	}

	return result
}
