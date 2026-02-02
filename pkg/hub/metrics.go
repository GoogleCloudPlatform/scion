// Package hub provides the Scion Hub API server.
package hub

import (
	"sync"
	"sync/atomic"
	"time"
)

// HostAuthMetrics tracks metrics for host authentication.
// This provides a simple, dependency-free metrics implementation.
// For production use with Prometheus, implement MetricsRecorder interface.
type HostAuthMetrics struct {
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

	// Connected hosts gauge
	connectedHosts atomic.Int64

	// Latency tracking (simple histogram buckets)
	mu               sync.RWMutex
	authLatencies    []time.Duration
	maxLatencySample int
}

// MetricsRecorder is the interface for recording metrics.
// Implement this interface to integrate with Prometheus or other metrics systems.
type MetricsRecorder interface {
	// RecordAuthAttempt records an authentication attempt.
	RecordAuthAttempt(hostID string, success bool, latency time.Duration)

	// RecordRegistration records a host registration.
	RecordRegistration(hostID string)

	// RecordJoin records a host join attempt.
	RecordJoin(hostID string, success bool)

	// RecordRotation records a secret rotation.
	RecordRotation(hostID string)

	// RecordDispatch records a dispatch attempt to a runtime host.
	RecordDispatch(hostID string, operation string, success bool, latency time.Duration)

	// SetConnectedHosts sets the current number of connected hosts.
	SetConnectedHosts(count int64)

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

	// Connected hosts
	ConnectedHosts int64 `json:"connectedHosts"`

	// Latency percentiles (in milliseconds)
	AuthLatencyP50 float64 `json:"authLatencyP50Ms,omitempty"`
	AuthLatencyP95 float64 `json:"authLatencyP95Ms,omitempty"`
	AuthLatencyP99 float64 `json:"authLatencyP99Ms,omitempty"`
}

// NewHostAuthMetrics creates a new metrics tracker.
func NewHostAuthMetrics() *HostAuthMetrics {
	return &HostAuthMetrics{
		authLatencies:    make([]time.Duration, 0, 1000),
		maxLatencySample: 1000,
	}
}

// RecordAuthAttempt records an authentication attempt.
func (m *HostAuthMetrics) RecordAuthAttempt(hostID string, success bool, latency time.Duration) {
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

// RecordRegistration records a host registration.
func (m *HostAuthMetrics) RecordRegistration(hostID string) {
	m.registrations.Add(1)
}

// RecordJoin records a host join attempt.
func (m *HostAuthMetrics) RecordJoin(hostID string, success bool) {
	m.joins.Add(1)
	if !success {
		m.joinFailures.Add(1)
	}
}

// RecordRotation records a secret rotation.
func (m *HostAuthMetrics) RecordRotation(hostID string) {
	m.rotations.Add(1)
}

// RecordDispatch records a dispatch attempt to a runtime host.
func (m *HostAuthMetrics) RecordDispatch(hostID string, operation string, success bool, latency time.Duration) {
	m.dispatchAttempts.Add(1)
	if !success {
		m.dispatchFailures.Add(1)
	}
}

// SetConnectedHosts sets the current number of connected hosts.
func (m *HostAuthMetrics) SetConnectedHosts(count int64) {
	m.connectedHosts.Store(count)
}

// GetSnapshot returns a snapshot of current metrics.
func (m *HostAuthMetrics) GetSnapshot() *MetricsSnapshot {
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
		ConnectedHosts:   m.connectedHosts.Load(),
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

func (n *NoOpMetrics) RecordAuthAttempt(hostID string, success bool, latency time.Duration) {}
func (n *NoOpMetrics) RecordRegistration(hostID string)                                     {}
func (n *NoOpMetrics) RecordJoin(hostID string, success bool)                               {}
func (n *NoOpMetrics) RecordRotation(hostID string)                                         {}
func (n *NoOpMetrics) RecordDispatch(hostID, operation string, success bool, latency time.Duration) {
}
func (n *NoOpMetrics) SetConnectedHosts(count int64) {}
func (n *NoOpMetrics) GetSnapshot() *MetricsSnapshot {
	return &MetricsSnapshot{Timestamp: time.Now()}
}
