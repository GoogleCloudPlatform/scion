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

// Package hub provides the Scion Hub API server.
package hub

import (
	"sync"
	"sync/atomic"
	"time"
)

// BrokerAuthMetrics tracks metrics for broker authentication.
// This provides a simple, dependency-free metrics implementation.
// For production use with Prometheus, implement MetricsRecorder interface.
type BrokerAuthMetrics struct {
	// Authentication counters
	authAttempts  atomic.Int64
	authSuccesses atomic.Int64
	authFailures  atomic.Int64

	// Registration counters
	registrations atomic.Int64
	joins         atomic.Int64
	joinFailures  atomic.Int64

	// Rotation counters
	rotations atomic.Int64

	// Dispatch counters
	dispatchAttempts atomic.Int64
	dispatchFailures atomic.Int64

	// Connected brokers gauge
	connectedBrokers atomic.Int64

	// Latency tracking (simple histogram buckets)
	mu               sync.RWMutex
	authLatencies    []time.Duration
	maxLatencySample int
}

// MetricsRecorder is the interface for recording metrics.
// Implement this interface to integrate with Prometheus or other metrics systems.
type MetricsRecorder interface {
	// RecordAuthAttempt records an authentication attempt.
	RecordAuthAttempt(brokerID string, success bool, latency time.Duration)

	// RecordRegistration records a broker registration.
	RecordRegistration(brokerID string)

	// RecordJoin records a broker join attempt.
	RecordJoin(brokerID string, success bool)

	// RecordRotation records a secret rotation.
	RecordRotation(brokerID string)

	// RecordDispatch records a dispatch attempt to a runtime broker.
	RecordDispatch(brokerID string, operation string, success bool, latency time.Duration)

	// SetConnectedBrokers sets the current number of connected brokers.
	SetConnectedBrokers(count int64)

	// GetSnapshot returns a snapshot of current metrics.
	GetSnapshot() *MetricsSnapshot
}

// MetricsSnapshot represents a point-in-time snapshot of metrics.
type MetricsSnapshot struct {
	Timestamp time.Time `json:"timestamp"`

	// Authentication
	AuthAttempts  int64 `json:"authAttempts"`
	AuthSuccesses int64 `json:"authSuccesses"`
	AuthFailures  int64 `json:"authFailures"`

	// Registration
	Registrations int64 `json:"registrations"`
	Joins         int64 `json:"joins"`
	JoinFailures  int64 `json:"joinFailures"`

	// Rotation
	Rotations int64 `json:"rotations"`

	// Dispatch
	DispatchAttempts int64 `json:"dispatchAttempts"`
	DispatchFailures int64 `json:"dispatchFailures"`

	// Connected brokers
	ConnectedBrokers int64 `json:"connectedBrokers"`

	// Latency percentiles (in milliseconds)
	AuthLatencyP50 float64 `json:"authLatencyP50Ms,omitempty"`
	AuthLatencyP95 float64 `json:"authLatencyP95Ms,omitempty"`
	AuthLatencyP99 float64 `json:"authLatencyP99Ms,omitempty"`
}

// NewBrokerAuthMetrics creates a new metrics tracker.
func NewBrokerAuthMetrics() *BrokerAuthMetrics {
	return &BrokerAuthMetrics{
		authLatencies:    make([]time.Duration, 0, 1000),
		maxLatencySample: 1000,
	}
}

// RecordAuthAttempt records an authentication attempt.
func (m *BrokerAuthMetrics) RecordAuthAttempt(brokerID string, success bool, latency time.Duration) {
	m.authAttempts.Add(1)
	if success {
		m.authSuccesses.Add(1)
	} else {
		m.authFailures.Add(1)
	}

	// Track latency
	m.mu.Lock()
	if len(m.authLatencies) < m.maxLatencySample {
		m.authLatencies = append(m.authLatencies, latency)
	} else {
		// Rotate oldest entry
		copy(m.authLatencies, m.authLatencies[1:])
		m.authLatencies[len(m.authLatencies)-1] = latency
	}
	m.mu.Unlock()
}

// RecordRegistration records a broker registration.
func (m *BrokerAuthMetrics) RecordRegistration(brokerID string) {
	m.registrations.Add(1)
}

// RecordJoin records a broker join attempt.
func (m *BrokerAuthMetrics) RecordJoin(brokerID string, success bool) {
	m.joins.Add(1)
	if !success {
		m.joinFailures.Add(1)
	}
}

// RecordRotation records a secret rotation.
func (m *BrokerAuthMetrics) RecordRotation(brokerID string) {
	m.rotations.Add(1)
}

// RecordDispatch records a dispatch attempt to a runtime broker.
func (m *BrokerAuthMetrics) RecordDispatch(brokerID string, operation string, success bool, latency time.Duration) {
	m.dispatchAttempts.Add(1)
	if !success {
		m.dispatchFailures.Add(1)
	}
}

// SetConnectedBrokers sets the current number of connected brokers.
func (m *BrokerAuthMetrics) SetConnectedBrokers(count int64) {
	m.connectedBrokers.Store(count)
}

// GetSnapshot returns a snapshot of current metrics.
func (m *BrokerAuthMetrics) GetSnapshot() *MetricsSnapshot {
	snapshot := &MetricsSnapshot{
		Timestamp:        time.Now(),
		AuthAttempts:     m.authAttempts.Load(),
		AuthSuccesses:    m.authSuccesses.Load(),
		AuthFailures:     m.authFailures.Load(),
		Registrations:    m.registrations.Load(),
		Joins:            m.joins.Load(),
		JoinFailures:     m.joinFailures.Load(),
		Rotations:        m.rotations.Load(),
		DispatchAttempts: m.dispatchAttempts.Load(),
		DispatchFailures: m.dispatchFailures.Load(),
		ConnectedBrokers:   m.connectedBrokers.Load(),
	}

	// Calculate latency percentiles
	m.mu.RLock()
	if len(m.authLatencies) > 0 {
		sorted := make([]time.Duration, len(m.authLatencies))
		copy(sorted, m.authLatencies)
		sortDurations(sorted)

		snapshot.AuthLatencyP50 = float64(percentile(sorted, 0.50)) / float64(time.Millisecond)
		snapshot.AuthLatencyP95 = float64(percentile(sorted, 0.95)) / float64(time.Millisecond)
		snapshot.AuthLatencyP99 = float64(percentile(sorted, 0.99)) / float64(time.Millisecond)
	}
	m.mu.RUnlock()

	return snapshot
}

// sortDurations sorts a slice of durations in place.
func sortDurations(d []time.Duration) {
	// Simple insertion sort for small slices
	for i := 1; i < len(d); i++ {
		key := d[i]
		j := i - 1
		for j >= 0 && d[j] > key {
			d[j+1] = d[j]
			j--
		}
		d[j+1] = key
	}
}

// percentile returns the value at the given percentile (0-1).
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// NoOpMetrics is a no-op implementation of MetricsRecorder.
type NoOpMetrics struct{}

func (n *NoOpMetrics) RecordAuthAttempt(brokerID string, success bool, latency time.Duration) {}
func (n *NoOpMetrics) RecordRegistration(brokerID string)                                     {}
func (n *NoOpMetrics) RecordJoin(brokerID string, success bool)                               {}
func (n *NoOpMetrics) RecordRotation(brokerID string)                                         {}
func (n *NoOpMetrics) RecordDispatch(brokerID, operation string, success bool, latency time.Duration) {
}
func (n *NoOpMetrics) SetConnectedBrokers(count int64) {}
func (n *NoOpMetrics) GetSnapshot() *MetricsSnapshot {
	return &MetricsSnapshot{Timestamp: time.Now()}
}
