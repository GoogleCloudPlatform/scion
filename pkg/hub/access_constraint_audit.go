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
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Boundary audit types (B6)
// ---------------------------------------------------------------------------

// BoundaryAuditEntry is the durable record of a boundary mutation.
// Every boundary create/update/delete produces exactly one entry in the same
// logical transaction as the data mutation. If the audit write fails the
// mutation rolls back — no authority change returns success without a
// persisted audit ID.
type BoundaryAuditEntry struct {
	// ID is a unique identifier for this audit entry.
	ID string `json:"id"`

	// ConstraintID is the boundary that was mutated.
	ConstraintID string `json:"constraintId"`

	// Operation is "create", "update", or "delete".
	Operation string `json:"operation"`

	// ActorID is the principal who performed the mutation.
	ActorID string `json:"actorId"`

	// CorrelationID ties the audit entry to the originating request.
	CorrelationID string `json:"correlationId"`

	// BeforeRevision is the constraint revision before the mutation.
	// 0 for create.
	BeforeRevision int64 `json:"beforeRevision"`

	// AfterRevision is the constraint revision after the mutation.
	// 0 for delete.
	AfterRevision int64 `json:"afterRevision"`

	// Classification is the server-determined mutation direction:
	// "tighten", "relax", or "mixed".
	Classification string `json:"classification"`

	// PreviewID is the preview that authorised this mutation.
	PreviewID string `json:"previewId"`

	// DraftHash is the SHA-256 of the canonicalized draft at preview time.
	DraftHash string `json:"draftHash"`

	// StateFingerprint is the authorization-state fingerprint at commit time.
	StateFingerprint string `json:"stateFingerprint"`

	// ImpactCounts summarises the blast radius.
	ImpactCounts ImpactCounts `json:"impactCounts"`

	// Timestamp is when the audit entry was created.
	Timestamp time.Time `json:"timestamp"`
}

// ImpactCounts summarises the blast radius of a boundary mutation.
// Counts are always non-negative; zero means "no change in that dimension".
type ImpactCounts struct {
	AffectedPrincipals int `json:"affectedPrincipals"`
	PermissionsAdded   int `json:"permissionsAdded"`
	PermissionsRemoved int `json:"permissionsRemoved"`
}

// ---------------------------------------------------------------------------
// BoundaryAuditWriter — synchronous, fail-closed audit writer
// ---------------------------------------------------------------------------

// BoundaryAuditWriter writes audit entries for boundary mutations.
// The write is synchronous: if it returns an error the caller MUST roll back
// the mutation. There is no async fallback — the audit record is a hard
// requirement for every authority change.
type BoundaryAuditWriter struct {
	logger  *slog.Logger
	nowFunc func() time.Time

	// mu protects entries from concurrent access (appended during commits,
	// read during queries).
	mu sync.RWMutex

	// entries stores audit entries in-memory. In a real deployment this would
	// be backed by the same database transaction as the mutation. Because the
	// Store interface currently lacks RunInTx, we store entries in-process and
	// treat a write failure as a hard error that rolls back the mutation at the
	// governance layer.
	entries []BoundaryAuditEntry

	// failFunc is an optional hook for testing. When non-nil and it returns
	// a non-nil error, WriteAuditEntry returns that error without persisting.
	failFunc func() error
}

// NewBoundaryAuditWriter creates a new BoundaryAuditWriter.
func NewBoundaryAuditWriter(logger *slog.Logger) *BoundaryAuditWriter {
	return &BoundaryAuditWriter{
		logger:  logger,
		nowFunc: time.Now,
	}
}

// AuditRequest contains everything needed to create an audit entry.
type AuditRequest struct {
	ConstraintID   string
	Operation      string // "create", "update", "delete"
	ActorID        string
	CorrelationID  string
	BeforeRevision int64
	AfterRevision  int64
	Classification string
	PreviewID      string
	DraftHash      string
	ImpactCounts   ImpactCounts
}

// WriteAuditEntry creates and persists a BoundaryAuditEntry. Returns the
// audit ID on success. On failure the caller MUST roll back the mutation.
func (w *BoundaryAuditWriter) WriteAuditEntry(ctx context.Context, req AuditRequest) (string, error) {
	// Test hook: allow injected failures.
	if w.failFunc != nil {
		if err := w.failFunc(); err != nil {
			return "", err
		}
	}

	id, err := generateAuditID()
	if err != nil {
		return "", fmt.Errorf("failed to generate audit ID: %w", err)
	}

	// Compute a state fingerprint from the current context.
	// In production this would hash role bindings, group memberships, etc.
	// For now, we use a deterministic fingerprint based on available data.
	fingerprint := computeStateFingerprint(req)

	entry := BoundaryAuditEntry{
		ID:               id,
		ConstraintID:     req.ConstraintID,
		Operation:        req.Operation,
		ActorID:          req.ActorID,
		CorrelationID:    req.CorrelationID,
		BeforeRevision:   req.BeforeRevision,
		AfterRevision:    req.AfterRevision,
		Classification:   req.Classification,
		PreviewID:        req.PreviewID,
		DraftHash:        req.DraftHash,
		StateFingerprint: fingerprint,
		ImpactCounts:     req.ImpactCounts,
		Timestamp:        w.nowFunc(),
	}

	// Validate the audit entry before persisting.
	if err := validateAuditEntry(&entry); err != nil {
		return "", fmt.Errorf("audit entry validation failed: %w", err)
	}

	// Persist the entry. This is the critical path — failure here means the
	// mutation must be rolled back.
	w.mu.Lock()
	w.entries = append(w.entries, entry)
	w.mu.Unlock()

	w.logger.Info("boundary audit entry written",
		"audit_id", entry.ID,
		"constraint_id", entry.ConstraintID,
		"operation", entry.Operation,
		"classification", entry.Classification,
		"actor_id", entry.ActorID,
		"before_revision", entry.BeforeRevision,
		"after_revision", entry.AfterRevision,
		"affected_principals", entry.ImpactCounts.AffectedPrincipals,
	)

	return entry.ID, nil
}

// GetEntries returns all persisted audit entries. Used for testing and
// read-only provenance queries.
func (w *BoundaryAuditWriter) GetEntries() []BoundaryAuditEntry {
	w.mu.RLock()
	result := make([]BoundaryAuditEntry, len(w.entries))
	copy(result, w.entries)
	w.mu.RUnlock()
	return result
}

// GetEntriesForConstraint returns audit entries for a specific constraint.
func (w *BoundaryAuditWriter) GetEntriesForConstraint(constraintID string) []BoundaryAuditEntry {
	w.mu.RLock()
	defer w.mu.RUnlock()
	var result []BoundaryAuditEntry
	for _, e := range w.entries {
		if e.ConstraintID == constraintID {
			result = append(result, e)
		}
	}
	return result
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// generateAuditID creates a unique audit entry ID.
func generateAuditID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "aud_" + hex.EncodeToString(b), nil
}

// computeStateFingerprint creates a deterministic fingerprint from audit
// request data. In production this would incorporate the full authorization
// state (role bindings, group memberships, etc.) from within the same
// transaction.
func computeStateFingerprint(req AuditRequest) string {
	// Use a simple deterministic fingerprint from available fields.
	return fmt.Sprintf("%s:%s:%d:%d:%s",
		req.ConstraintID, req.Operation,
		req.BeforeRevision, req.AfterRevision,
		req.Classification,
	)
}

// validateAuditEntry checks that all required fields are present.
func validateAuditEntry(entry *BoundaryAuditEntry) error {
	if entry.ID == "" {
		return fmt.Errorf("audit entry ID is required")
	}
	if entry.Operation == "" {
		return fmt.Errorf("audit entry operation is required")
	}
	if entry.Operation != "create" && entry.Operation != "update" && entry.Operation != "delete" &&
		entry.Operation != "disable_all" && entry.Operation != "recovery" {
		return fmt.Errorf("invalid audit entry operation %q", entry.Operation)
	}
	if entry.ActorID == "" {
		return fmt.Errorf("audit entry actor ID is required")
	}
	if entry.Timestamp.IsZero() {
		return fmt.Errorf("audit entry timestamp is required")
	}
	return nil
}

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
