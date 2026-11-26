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

package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// ErrPayloadTooLarge marks an HTTP 413 Payload Too Large rejection from the
// Hub usage ingest endpoint. HubUsageSink bisects multi-event batches on this
// error and only dead-letters a single event that exceeds the limit on its own.
var ErrPayloadTooLarge = errors.New("hub usage ingest: payload too large")

// ErrUsageIngestRevoked marks an HTTP 404 Not Found from the Hub usage ingest
// endpoint, indicating the capability was revoked mid-run (e.g. experiment
// turned off or Hub rolled back). The drainer stops the Hub sink rather than
// retrying forever.
var ErrUsageIngestRevoked = errors.New("hub usage ingest: capability revoked (404)")

// UsageIngestPostFunc sends a usage ingest request (batch or empty heartbeat)
// to the Hub's POST /api/v1/agents/{id}/usage/events endpoint.
type UsageIngestPostFunc func(ctx context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error)

// HubUsageSinkConfig configures draining agent.usage batches and meter-health
// heartbeats to the Scion Hub.
type HubUsageSinkConfig struct {
	// Post delivers one UsageIngestRequest to the Hub. Required.
	Post UsageIngestPostFunc
	// RunID is the Hub-minted run identifier for heartbeats and fallback event
	// attribution when an outbox event omits run_id.
	RunID string
	// HubID is the Hub identifier returned by the capabilities probe (optional).
	HubID string
	// MaxBatchEvents caps events per batch (defaults to UsageIngestMaxBatchEvents).
	MaxBatchEvents int
	// MaxBatchBytes caps HTTP request body size (defaults to UsageIngestMaxBatchBytes).
	MaxBatchBytes int
	// DrainInterval sets the periodic drain cadence (defaults to DefaultUsageDrainIntervalSec).
	DrainInterval time.Duration
	// HeartbeatInterval sets the periodic empty-heartbeat cadence (defaults to DefaultUsageHeartbeatIntervalSec).
	HeartbeatInterval time.Duration
	// SupportedSignals returns the usage signal categories this producer can
	// report for the current run (e.g. ["calls", "input", "output", ...]).
	SupportedSignals func() []string
	// SuppressDirectOTel records whether the Hub capability probe instructed
	// this container to suppress direct OTel export of gen_ai.api.calls and
	// scion.usage.tokens (Tokenomics P2.2 D3).
	SuppressDirectOTel bool
}

func (c HubUsageSinkConfig) maxBatchEvents() int {
	if c.MaxBatchEvents > 0 {
		return c.MaxBatchEvents
	}
	return telemetrycontract.UsageIngestMaxBatchEvents
}

func (c HubUsageSinkConfig) maxBatchBytes() int {
	if c.MaxBatchBytes > 0 {
		return c.MaxBatchBytes
	}
	return telemetrycontract.UsageIngestMaxBatchBytes
}

func (c HubUsageSinkConfig) drainInterval() time.Duration {
	if c.DrainInterval > 0 {
		return c.DrainInterval
	}
	return time.Duration(telemetrycontract.DefaultUsageDrainIntervalSec) * time.Second
}

func (c HubUsageSinkConfig) heartbeatInterval() time.Duration {
	if c.HeartbeatInterval > 0 {
		return c.HeartbeatInterval
	}
	return time.Duration(telemetrycontract.DefaultUsageHeartbeatIntervalSec) * time.Second
}

// HubUsageSink drains finalized usage batches from an AnalyticsOutbox to the
// Hub and sends periodic meter-health heartbeats.
type HubUsageSink struct {
	cfg          HubUsageSinkConfig
	outbox       *AnalyticsOutbox
	lastActivity atomic.Int64 // UnixNano of most recent observed source activity
	now          func() time.Time
}

// NewHubUsageSink creates a Hub usage sink for outbox. Returns nil if outbox
// or cfg.Post is nil. When cfg.RunID is set, it ensures the outbox's producer
// epoch is rotated if the run changed since the last process.
func NewHubUsageSink(outbox *AnalyticsOutbox, cfg HubUsageSinkConfig) *HubUsageSink {
	if outbox == nil || cfg.Post == nil {
		return nil
	}
	if cfg.RunID != "" {
		if err := outbox.EnsureRunEpoch(cfg.RunID); err != nil {
			log.Debug("Hub usage sink: ensure run epoch failed: %v", err)
		}
	}
	return &HubUsageSink{
		cfg:    cfg,
		outbox: outbox,
		now:    time.Now,
	}
}

// NoteActivity records observed source/harness activity at t, advancing the
// watermark monotonically.
func (s *HubUsageSink) NoteActivity(t time.Time) {
	if s == nil || t.IsZero() {
		return
	}
	ns := t.UTC().UnixNano()
	for {
		cur := s.lastActivity.Load()
		if ns <= cur {
			return
		}
		if s.lastActivity.CompareAndSwap(cur, ns) {
			return
		}
	}
}

func (s *HubUsageSink) lastActivityRFC3339() string {
	ns := s.lastActivity.Load()
	if ns <= 0 {
		return ""
	}
	return telemetrycontract.FormatTS(time.Unix(0, ns).UTC())
}

func (s *HubUsageSink) supportedSignals() []string {
	if s.cfg.SupportedSignals == nil {
		return nil
	}
	sigs := s.cfg.SupportedSignals()
	if len(sigs) == 0 {
		return nil
	}
	return append([]string(nil), sigs...)
}

func (s *HubUsageSink) ledgerMeta() (epoch string, seq int64) {
	if s.outbox == nil {
		return "", 0
	}
	if ledger, err := s.outbox.LedgerSnapshot(); err == nil {
		return ledger.Epoch, ledger.LastSeq
	}
	return "", 0
}

// EffectiveMaxBatchBytes returns the payload byte budget for DrainBatch after
// subtracting the outer UsageIngestRequest JSON envelope overhead so that the
// serialized HTTP request body stays within MaxBatchBytes.
func (s *HubUsageSink) EffectiveMaxBatchBytes() int {
	limit := s.cfg.maxBatchBytes()
	epoch, seq := s.ledgerMeta()
	probe := telemetrycontract.UsageIngestRequest{
		Events:               []telemetrycontract.UsageEvent{},
		RunID:                s.cfg.RunID,
		ProducerEpoch:        epoch,
		ProducerSeq:          seq,
		LastSourceActivityAt: s.lastActivityRFC3339(),
		SupportedSignals:     s.supportedSignals(),
		FinalState:           telemetrycontract.MeterFinalStateIncomplete,
	}
	overhead := 256
	if b, err := json.Marshal(probe); err == nil {
		overhead = len(b) + 64
	}
	if limit <= overhead {
		return 1
	}
	return limit - overhead
}

// SendBatch implements AnalyticsBatchSendFunc for the Hub usage ingest endpoint.
//
// Events are grouped by contiguous (run_id, producer_epoch) so WAL replays from
// an earlier run preserve their original run_id and epoch (using cfg.RunID only
// as a fallback when an event omits run_id). Re-encoded request bodies that
// exceed maxBatchBytes or receive HTTP 413 (ErrPayloadTooLarge) are bisected so
// good events are never dead-lettered; only a single event that exceeds the
// limit on its own is dead-lettered.
func (s *HubUsageSink) SendBatch(ctx context.Context, lines [][]byte) (BatchSendOutcome, error) {
	if s == nil || s.cfg.Post == nil {
		return BatchSendOutcome{}, errors.New("hub usage sink not configured")
	}
	events := make([]telemetrycontract.UsageEvent, 0, len(lines))
	origIndex := make([]int, 0, len(lines))
	var rejected []int

	for i, line := range lines {
		var ev telemetrycontract.UsageEvent
		if err := telemetrycontract.Unmarshal(line, &ev); err != nil {
			log.Warn("Hub usage sink: dead-lettering undecodable outbox line: %v", err)
			rejected = append(rejected, i)
			continue
		}
		if ts, err := telemetrycontract.ParseTS(ev.ObservedTS); err == nil {
			s.NoteActivity(ts)
		} else if ts, err := telemetrycontract.ParseTS(ev.EventTS); err == nil {
			s.NoteActivity(ts)
		} else {
			s.NoteActivity(s.now())
		}
		events = append(events, ev)
		origIndex = append(origIndex, i)
	}

	if len(events) == 0 {
		return BatchSendOutcome{RejectedIndices: rejected}, nil
	}

	ledgerEpoch, ledgerSeq := s.ledgerMeta()

	// Partition into contiguous groups sharing (run_id, producer_epoch) so each
	// Hub ingest request has a single consistent meter_health (run, epoch) key
	// and preserves per-event run_id across restarts.
	start := 0
	for start < len(events) {
		groupRunID := events[start].RunID
		groupEpoch := events[start].ProducerEpoch
		end := start + 1
		for end < len(events) && events[end].RunID == groupRunID && events[end].ProducerEpoch == groupEpoch {
			end++
		}
		if err := s.sendGroup(ctx, events[start:end], origIndex[start:end], ledgerEpoch, ledgerSeq, &rejected); err != nil {
			return BatchSendOutcome{}, err
		}
		start = end
	}

	return BatchSendOutcome{RejectedIndices: rejected}, nil
}

func (s *HubUsageSink) sendGroup(
	ctx context.Context,
	events []telemetrycontract.UsageEvent,
	origIndices []int,
	ledgerEpoch string,
	ledgerSeq int64,
	rejected *[]int,
) error {
	if len(events) == 0 {
		return nil
	}

	runID := events[0].RunID
	if runID == "" {
		runID = s.cfg.RunID
	}
	epoch := events[0].ProducerEpoch
	if epoch == "" {
		epoch = ledgerEpoch
	}
	var maxSeq int64
	for _, ev := range events {
		if ev.ProducerSeq > maxSeq {
			maxSeq = ev.ProducerSeq
		}
	}
	seq := maxSeq
	if epoch != "" && epoch == ledgerEpoch && ledgerSeq > seq {
		seq = ledgerSeq
	}

	// Only mark DirectOTelSuppressed=true when suppression is active AND the
	// batch belongs to the current run (or no RunID was pinned). Residual WAL
	// events replayed from an earlier run (runID != s.cfg.RunID) may have been
	// exported directly by the container before suppression took effect, so
	// leaving DirectOTelSuppressed=false prevents Hub re-emission from double
	// counting them while still persisting them to the ledger and rollups.
	directSuppressed := s.cfg.SuppressDirectOTel && (s.cfg.RunID == "" || runID == s.cfg.RunID)

	req := telemetrycontract.UsageIngestRequest{
		Events:               events,
		RunID:                runID,
		ProducerEpoch:        epoch,
		ProducerSeq:          seq,
		LastSourceActivityAt: s.lastActivityRFC3339(),
		SupportedSignals:     s.supportedSignals(),
		DirectOTelSuppressed: directSuppressed,
	}

	// Proactive re-encoded size check (O2): encoding/json HTML-escaping or
	// field normalization can expand the wire body past maxBatchBytes. Bisect
	// multi-event batches before posting so good events are never rejected with
	// a batch-level 413.
	if len(events) > 1 {
		if encoded, err := json.Marshal(req); err == nil && len(encoded) > s.cfg.maxBatchBytes() {
			mid := len(events) / 2
			if err := s.sendGroup(ctx, events[:mid], origIndices[:mid], ledgerEpoch, ledgerSeq, rejected); err != nil {
				return err
			}
			return s.sendGroup(ctx, events[mid:], origIndices[mid:], ledgerEpoch, ledgerSeq, rejected)
		}
	}

	resp, err := s.cfg.Post(ctx, req)
	if err != nil {
		if errors.Is(err, ErrPayloadTooLarge) {
			if len(events) > 1 {
				mid := len(events) / 2
				if err := s.sendGroup(ctx, events[:mid], origIndices[:mid], ledgerEpoch, ledgerSeq, rejected); err != nil {
					return err
				}
				return s.sendGroup(ctx, events[mid:], origIndices[mid:], ledgerEpoch, ledgerSeq, rejected)
			}
			// Single event exceeded the Hub's payload limit on its own: dead-letter only this event.
			log.Warn("Hub usage sink: single event %s exceeded payload limit (413); dead-lettering", events[0].EventID)
			*rejected = append(*rejected, origIndices[0])
			return nil
		}
		return err
	}

	if resp != nil {
		for _, rej := range resp.Rejected {
			if rej.Index >= 0 && rej.Index < len(origIndices) {
				*rejected = append(*rejected, origIndices[rej.Index])
				log.Warn("Hub usage sink: event %s rejected (%s: %s); dead-lettering", rej.EventID, rej.Code, rej.Reason)
			}
		}
	}
	return nil
}

// SendHeartbeat sends an empty UsageIngestRequest carrying meter-health fields
// (producer_epoch, producer_seq, last_source_activity_at, supported_signals,
// and optional final_state).
func (s *HubUsageSink) SendHeartbeat(ctx context.Context, finalState string) error {
	if s == nil || s.cfg.Post == nil {
		return nil
	}
	if s.cfg.RunID != "" {
		_ = s.outbox.EnsureRunEpoch(s.cfg.RunID)
	}
	epoch, seq := s.ledgerMeta()
	if epoch == "" {
		return nil
	}
	req := telemetrycontract.UsageIngestRequest{
		Events:               []telemetrycontract.UsageEvent{},
		RunID:                s.cfg.RunID,
		ProducerEpoch:        epoch,
		ProducerSeq:          seq,
		LastSourceActivityAt: s.lastActivityRFC3339(),
		SupportedSignals:     s.supportedSignals(),
		FinalState:           finalState,
	}
	_, err := s.cfg.Post(ctx, req)
	return err
}
