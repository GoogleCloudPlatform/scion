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
	"log/slog"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// BoundaryMetrics — structured metrics and tracing (B6 §6)
// ---------------------------------------------------------------------------

// BoundaryMetrics tracks operational metrics for the boundary system.
// Uses atomic counters for thread safety. Metrics are exposed via structured
// logging (slog). No sensitive principal data is included.
type BoundaryMetrics struct {
	// Mutation counts by classification.
	TightenCount atomic.Int64
	RelaxCount   atomic.Int64
	MixedCount   atomic.Int64

	// Latency tracking (in milliseconds).
	LastPreviewLatencyMs  atomic.Int64
	LastMutationLatencyMs atomic.Int64

	// Lockout checks.
	LockoutCheckCount atomic.Int64
	LockoutBlockCount atomic.Int64

	// Security review gates.
	SecurityReviewTriggerCount atomic.Int64

	// Recovery operations.
	RecoveryDisableCount atomic.Int64
	RecoveryEnableCount  atomic.Int64

	logger *slog.Logger
}

// NewBoundaryMetrics creates a new BoundaryMetrics instance.
func NewBoundaryMetrics(logger *slog.Logger) *BoundaryMetrics {
	return &BoundaryMetrics{logger: logger}
}

// RecordMutation records a mutation by classification.
func (m *BoundaryMetrics) RecordMutation(classification string, latencyMs int64) {
	switch classification {
	case ClassificationTighten:
		m.TightenCount.Add(1)
	case ClassificationRelax:
		m.RelaxCount.Add(1)
	case ClassificationMixed:
		m.MixedCount.Add(1)
	}
	m.LastMutationLatencyMs.Store(latencyMs)

	m.logger.Info("boundary_mutation_metric",
		"classification", classification,
		"latency_ms", latencyMs,
		"tighten_total", m.TightenCount.Load(),
		"relax_total", m.RelaxCount.Load(),
		"mixed_total", m.MixedCount.Load(),
	)
}

// RecordPreviewLatency records a preview computation latency.
func (m *BoundaryMetrics) RecordPreviewLatency(latencyMs int64, isAsync bool) {
	m.LastPreviewLatencyMs.Store(latencyMs)

	mode := "sync"
	if isAsync {
		mode = "async"
	}
	m.logger.Info("boundary_preview_metric",
		"mode", mode,
		"latency_ms", latencyMs,
	)
}

// RecordLockoutCheck records a lockout check result.
func (m *BoundaryMetrics) RecordLockoutCheck(blocked bool) {
	m.LockoutCheckCount.Add(1)
	if blocked {
		m.LockoutBlockCount.Add(1)
	}
	m.logger.Debug("boundary_lockout_metric",
		"blocked", blocked,
		"check_total", m.LockoutCheckCount.Load(),
		"block_total", m.LockoutBlockCount.Load(),
		"block_rate", safeRate(m.LockoutBlockCount.Load(), m.LockoutCheckCount.Load()),
	)
}

// RecordSecurityReviewTrigger records that a security review gate was triggered.
func (m *BoundaryMetrics) RecordSecurityReviewTrigger() {
	m.SecurityReviewTriggerCount.Add(1)
	m.logger.Info("boundary_security_review_metric",
		"trigger_total", m.SecurityReviewTriggerCount.Load(),
	)
}

// Snapshot returns a point-in-time copy of the metrics.
func (m *BoundaryMetrics) Snapshot() map[string]int64 {
	return map[string]int64{
		"tighten_count":                 m.TightenCount.Load(),
		"relax_count":                   m.RelaxCount.Load(),
		"mixed_count":                   m.MixedCount.Load(),
		"last_preview_latency_ms":       m.LastPreviewLatencyMs.Load(),
		"last_mutation_latency_ms":      m.LastMutationLatencyMs.Load(),
		"lockout_check_count":           m.LockoutCheckCount.Load(),
		"lockout_block_count":           m.LockoutBlockCount.Load(),
		"security_review_trigger_count": m.SecurityReviewTriggerCount.Load(),
		"recovery_disable_count":        m.RecoveryDisableCount.Load(),
		"recovery_enable_count":         m.RecoveryEnableCount.Load(),
	}
}

// safeRate computes numerator/denominator as a float, returning 0 on division by zero.
func safeRate(numerator, denominator int64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}
