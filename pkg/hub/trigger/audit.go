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

package trigger

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Audit event type names and rate-limit constants (§8.1).
const (
	AuditEventReceived        = "trigger.event.received"
	AuditEventSignatureFailed = "trigger.webhook.signature_failed"

	FailureAuditInterval   = time.Minute
	maxFailureAuditEntries = 10000
)

// ReceiptAuditRecord carries content-free metadata for an accepted (queued or
// duplicate) webhook event (§8.1). It never carries the request body, secret
// material, or header values other than the validated X-Scion-Event name.
type ReceiptAuditRecord struct {
	ProjectID    string
	EndpointID   string
	EventID      string
	WebhookEvent string
	PayloadBytes int
	Result       string // "queued" | "duplicate"
	RemoteIP     string
}

// SignatureFailureAuditRecord carries content-free metadata for a webhook
// signature verification failure (§8.1).
type SignatureFailureAuditRecord struct {
	ProjectID  string
	EndpointID string
	RemoteIP   string
}

// AuditSink records webhook receipt and signature-failure audit events.
type AuditSink interface {
	RecordReceipt(ctx context.Context, rec ReceiptAuditRecord)
	RecordSignatureFailure(ctx context.Context, rec SignatureFailureAuditRecord)
}

type failureAuditWindow struct {
	lastEmitted time.Time
	suppressed  int
}

// SlogAuditSink emits content-free structured audit records to a *slog.Logger
// (defaulting to logging.Subsystem("hub.audit")) and rate-limits signature
// failure records to at most one per endpoint per minute (§8.1).
type SlogAuditSink struct {
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration

	mu       sync.Mutex
	failures *boundedTable[string, failureAuditWindow]
}

// SlogAuditConfig configures a SlogAuditSink.
type SlogAuditConfig struct {
	Logger          *slog.Logger
	FailureInterval time.Duration
	MaxEntries      int
	Now             func() time.Time
	OnEvictScan     func(steps int)
}

// NewSlogAuditSink constructs a SlogAuditSink with Stage 1 defaults.
func NewSlogAuditSink(cfg SlogAuditConfig) *SlogAuditSink {
	l := cfg.Logger
	if l == nil {
		l = logging.Subsystem("hub.audit")
	}
	interval := cfg.FailureInterval
	if interval <= 0 {
		interval = FailureAuditInterval
	}
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = maxFailureAuditEntries
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &SlogAuditSink{
		log:      l,
		now:      nowFn,
		interval: interval,
		failures: newBoundedTable[string, failureAuditWindow](maxEntries, cfg.OnEvictScan, nil),
	}
}

// RecordReceipt emits one trigger.event.received structured audit record per
// accepted webhook event (§8.1).
func (s *SlogAuditSink) RecordReceipt(ctx context.Context, rec ReceiptAuditRecord) {
	if s == nil {
		return
	}
	attrs := []slog.Attr{
		slog.String("event_type", AuditEventReceived),
		slog.String("project_id", rec.ProjectID),
		slog.String("endpoint_id", rec.EndpointID),
		slog.String("event_id", rec.EventID),
		slog.String("webhook_event", rec.WebhookEvent),
		slog.Int("payload_bytes", rec.PayloadBytes),
		slog.String("result", rec.Result),
		slog.String("remote_ip", rec.RemoteIP),
	}
	if reqID := logging.RequestIDFromContext(ctx); reqID != "" {
		attrs = append(attrs, slog.String(logging.AttrRequestID, reqID))
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, AuditEventReceived, attrs...)
}

// RecordSignatureFailure emits at most one signature-failure audit record per
// endpoint per minute (§8.1) so an unauthenticated flood cannot fill the audit
// log.
func (s *SlogAuditSink) RecordSignatureFailure(ctx context.Context, rec SignatureFailureAuditRecord) {
	if s == nil || rec.EndpointID == "" {
		return
	}
	now := s.now()

	s.mu.Lock()
	win, exists := s.failures.get(rec.EndpointID)
	if exists && now.Sub(win.lastEmitted) < s.interval {
		win.suppressed++
		s.failures.setInPlace(rec.EndpointID, win)
		s.failures.recordScan(0)
		s.mu.Unlock()
		return
	}
	suppressed := win.suppressed
	s.failures.putEvictOldest(rec.EndpointID, failureAuditWindow{
		lastEmitted: now,
		suppressed:  0,
	})
	s.mu.Unlock()

	attrs := []slog.Attr{
		slog.String("event_type", AuditEventSignatureFailed),
		slog.String("project_id", rec.ProjectID),
		slog.String("endpoint_id", rec.EndpointID),
		slog.String("remote_ip", rec.RemoteIP),
	}
	if suppressed > 0 {
		attrs = append(attrs, slog.Int("suppressed_count", suppressed))
	}
	if reqID := logging.RequestIDFromContext(ctx); reqID != "" {
		attrs = append(attrs, slog.String(logging.AttrRequestID, reqID))
	}
	s.log.LogAttrs(ctx, slog.LevelWarn, AuditEventSignatureFailed, attrs...)
}

// FailureWindowLen returns the number of tracked endpoint failure windows (for tests).
func (s *SlogAuditSink) FailureWindowLen() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures.len()
}
