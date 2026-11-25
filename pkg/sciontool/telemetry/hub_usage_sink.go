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

// UsageIngestPostFunc sends a usage ingest request (batch or empty heartbeat)
// to the Hub's POST /api/v1/agents/{id}/usage/events endpoint.
type UsageIngestPostFunc func(ctx context.Context, req telemetrycontract.UsageIngestRequest) (*telemetrycontract.UsageIngestResponse, error)

// HubUsageSinkConfig configures draining agent.usage batches and meter-health
// heartbeats to the Scion Hub.
type HubUsageSinkConfig struct {
	// Post delivers one UsageIngestRequest to the Hub. Required.
	Post UsageIngestPostFunc
	// RunID is the Hub-minted run identifier for heartbeats and fallback event
	// attribution.
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
// or cfg.Post is nil.
func NewHubUsageSink(outbox *AnalyticsOutbox, cfg HubUsageSinkConfig) *HubUsageSink {
	if outbox == nil || cfg.Post == nil {
		return nil
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
func (s *HubUsageSink) SendBatch(ctx context.Context, lines [][]byte) (BatchSendOutcome, error) {
	if s == nil || s.cfg.Post == nil {
		return BatchSendOutcome{}, errors.New("hub usage sink not configured")
	}
	events := make([]telemetrycontract.UsageEvent, 0, len(lines))
	origIndex := make([]int, 0, len(lines))
	var rejected []int
	var maxBatchSeq int64
	var batchEpoch string

	for i, line := range lines {
		var ev telemetrycontract.UsageEvent
		if err := telemetrycontract.Unmarshal(line, &ev); err != nil {
			log.Warn("Hub usage sink: dead-lettering undecodable outbox line: %v", err)
			rejected = append(rejected, i)
			continue
		}
		if batchEpoch == "" && ev.ProducerEpoch != "" {
			batchEpoch = ev.ProducerEpoch
		}
		if ev.ProducerSeq > maxBatchSeq {
			maxBatchSeq = ev.ProducerSeq
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

	epoch, seq := s.ledgerMeta()
	if epoch == "" {
		epoch = batchEpoch
	}
	if maxBatchSeq > seq {
		seq = maxBatchSeq
	}

	req := telemetrycontract.UsageIngestRequest{
		Events:               events,
		RunID:                s.cfg.RunID,
		ProducerEpoch:        epoch,
		ProducerSeq:          seq,
		LastSourceActivityAt: s.lastActivityRFC3339(),
		SupportedSignals:     s.supportedSignals(),
	}

	resp, err := s.cfg.Post(ctx, req)
	if err != nil {
		return BatchSendOutcome{}, err
	}
	if resp != nil {
		for _, rej := range resp.Rejected {
			if rej.Index >= 0 && rej.Index < len(origIndex) {
				rejected = append(rejected, origIndex[rej.Index])
				log.Warn("Hub usage sink: event %s rejected (%s: %s); dead-lettering", rej.EventID, rej.Code, rej.Reason)
			}
		}
	}
	return BatchSendOutcome{RejectedIndices: rejected}, nil
}

// SendHeartbeat sends an empty UsageIngestRequest carrying meter-health fields
// (producer_epoch, producer_seq, last_source_activity_at, supported_signals,
// and optional final_state).
func (s *HubUsageSink) SendHeartbeat(ctx context.Context, finalState string) error {
	if s == nil || s.cfg.Post == nil {
		return nil
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
