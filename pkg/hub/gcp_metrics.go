// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"sync"
	"sync/atomic"
	"time"
)

// GCPTokenMetricsRecorder is the interface for recording GCP token metrics.
type GCPTokenMetricsRecorder interface {
	RecordAccessTokenRequest(success bool, latency time.Duration)
	RecordIDTokenRequest(success bool, latency time.Duration)
	RecordRateLimitRejection()
	GetSnapshot() *GCPTokenMetricsSnapshot
}

// GCPTokenMetrics tracks metrics for GCP token operations.
type GCPTokenMetrics struct {
	// Access token counters
	accessTokenRequests  atomic.Int64
	accessTokenSuccesses atomic.Int64
	accessTokenFailures  atomic.Int64

	// Identity token counters
	idTokenRequests  atomic.Int64
	idTokenSuccesses atomic.Int64
	idTokenFailures  atomic.Int64

	// Rate limit rejections
	rateLimitRejections atomic.Int64

	// IAM API latency tracking
	mu               sync.RWMutex
	iamLatencies     []time.Duration
	maxLatencySample int
}

// GCPTokenMetricsSnapshot is a point-in-time snapshot of GCP token metrics.
type GCPTokenMetricsSnapshot struct {
	Timestamp            time.Time `json:"timestamp"`
	AccessTokenRequests  int64     `json:"accessTokenRequests"`
	AccessTokenSuccesses int64     `json:"accessTokenSuccesses"`
	AccessTokenFailures  int64     `json:"accessTokenFailures"`
	IDTokenRequests      int64     `json:"idTokenRequests"`
	IDTokenSuccesses     int64     `json:"idTokenSuccesses"`
	IDTokenFailures      int64     `json:"idTokenFailures"`
	RateLimitRejections  int64     `json:"rateLimitRejections"`
	IAMLatencyP50Ms      float64   `json:"iamLatencyP50Ms,omitempty"`
	IAMLatencyP95Ms      float64   `json:"iamLatencyP95Ms,omitempty"`
	IAMLatencyP99Ms      float64   `json:"iamLatencyP99Ms,omitempty"`
}

// NewGCPTokenMetrics creates a new GCP token metrics tracker.
func NewGCPTokenMetrics() *GCPTokenMetrics {
	return &GCPTokenMetrics{
		iamLatencies:     make([]time.Duration, 0, 1000),
		maxLatencySample: 1000,
	}
}

// RecordAccessTokenRequest records an access token request.
func (m *GCPTokenMetrics) RecordAccessTokenRequest(success bool, latency time.Duration) {
	m.accessTokenRequests.Add(1)
	if success {
		m.accessTokenSuccesses.Add(1)
	} else {
		m.accessTokenFailures.Add(1)
	}
	m.recordLatency(latency)
}

// RecordIDTokenRequest records an identity token request.
func (m *GCPTokenMetrics) RecordIDTokenRequest(success bool, latency time.Duration) {
	m.idTokenRequests.Add(1)
	if success {
		m.idTokenSuccesses.Add(1)
	} else {
		m.idTokenFailures.Add(1)
	}
	m.recordLatency(latency)
}

// RecordRateLimitRejection records a rate limit rejection.
func (m *GCPTokenMetrics) RecordRateLimitRejection() {
	m.rateLimitRejections.Add(1)
}

func (m *GCPTokenMetrics) recordLatency(d time.Duration) {
	if d <= 0 {
		return
	}
	m.mu.Lock()
	if len(m.iamLatencies) < m.maxLatencySample {
		m.iamLatencies = append(m.iamLatencies, d)
	} else {
		copy(m.iamLatencies, m.iamLatencies[1:])
		m.iamLatencies[len(m.iamLatencies)-1] = d
	}
	m.mu.Unlock()
}

// GetSnapshot returns a point-in-time snapshot of GCP token metrics.
func (m *GCPTokenMetrics) GetSnapshot() *GCPTokenMetricsSnapshot {
	snap := &GCPTokenMetricsSnapshot{
		Timestamp:            time.Now(),
		AccessTokenRequests:  m.accessTokenRequests.Load(),
		AccessTokenSuccesses: m.accessTokenSuccesses.Load(),
		AccessTokenFailures:  m.accessTokenFailures.Load(),
		IDTokenRequests:      m.idTokenRequests.Load(),
		IDTokenSuccesses:     m.idTokenSuccesses.Load(),
		IDTokenFailures:      m.idTokenFailures.Load(),
		RateLimitRejections:  m.rateLimitRejections.Load(),
	}
	m.mu.RLock()
	if len(m.iamLatencies) > 0 {
		sorted := make([]time.Duration, len(m.iamLatencies))
		copy(sorted, m.iamLatencies)
		sortDurations(sorted)
		snap.IAMLatencyP50Ms = float64(percentile(sorted, 0.50)) / float64(time.Millisecond)
		snap.IAMLatencyP95Ms = float64(percentile(sorted, 0.95)) / float64(time.Millisecond)
		snap.IAMLatencyP99Ms = float64(percentile(sorted, 0.99)) / float64(time.Millisecond)
	}
	m.mu.RUnlock()
	return snap
}
