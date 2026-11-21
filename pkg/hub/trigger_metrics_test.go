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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newTestOTelTriggerMetrics(t *testing.T) (*OTelTriggerMetrics, *TriggerSnapshotMetrics, *metric.ManualReader) {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	snap := NewTriggerSnapshotMetrics()
	rec, err := NewOTelTriggerMetrics(mp, snap)
	require.NoError(t, err)
	return rec, snap, reader
}

func collectTriggerMetrics(t *testing.T, reader *metric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := make(map[string]metricdata.Metrics)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func TestOTelTriggerMetrics_QueueIngressFiresAndReaper(t *testing.T) {
	rec, snap, reader := newTestOTelTriggerMetrics(t)

	// Queue counters and gauges.
	rec.Enqueued(trigger.EventQueueKind)
	rec.Claimed(trigger.EventQueueKind, 2)
	rec.Done(trigger.EventQueueKind)
	rec.Retried(trigger.EventQueueKind)
	rec.Permanent(trigger.EventQueueKind)
	rec.Dropped(trigger.EventQueueKind, durablequeue.DropReasonAge, 3)
	rec.Dropped(trigger.EventQueueKind, durablequeue.DropReasonOverflow, 1)
	rec.Dropped(trigger.EventQueueKind, durablequeue.DropReasonPoison, 2)
	rec.ObserveQueue(trigger.EventQueueKind, 7, 2, 45*time.Second)

	// Ingress outcomes.
	rec.RecordIngressEvent(store.TriggerSourceKindWebhook, trigger.IngressResultAccepted)
	rec.RecordIngressEvent(store.TriggerSourceKindWebhook, trigger.IngressResultDuplicate)
	rec.RecordIngressEvent(store.TriggerSourceKindWebhook, trigger.IngressResultRejectedSignature)
	rec.RecordIngressEvent(store.TriggerSourceKindWebhook, trigger.IngressResultRejectedRateLimit)
	rec.RecordIngressEvent(store.TriggerSourceKindWebhook, trigger.IngressResultRejectedSize)
	rec.RecordIngressEvent(store.TriggerSourceKindWebhook, trigger.IngressResultRejectedUnknownEndpoint)
	rec.RecordIngressEvent(store.TriggerSourceKindWebhook, trigger.IngressResultRejectedSecretUnavailable)

	// Fire outcomes.
	rec.RecordTriggerFire(store.TriggerActionMessage, store.TriggerFireStatusFired, "")
	rec.RecordTriggerFire(store.TriggerActionMessage, store.TriggerFireStatusFailed, "ignored-for-non-skip")
	rec.RecordTriggerFire(store.TriggerActionMessage, store.TriggerFireStatusRefused, "")
	rec.RecordTriggerFire(store.TriggerActionMessage, store.TriggerFireStatusSkipped, trigger.FireSkipReasonRuleBudgetExhausted)

	// Reaper runs.
	runAt := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rec.RecordReaperRun(4, runAt, nil)
	rec.RecordReaperRun(0, runAt.Add(time.Hour), errors.New("db timeout"))

	collected := collectTriggerMetrics(t, reader)
	for _, name := range []string{
		MetricDurableQueueItems,
		MetricDurableQueuePending,
		MetricDurableQueueLeased,
		MetricDurableQueueOldestAge,
		MetricTriggerEvents,
		MetricTriggerFires,
		MetricTriggerReaperReaped,
		MetricTriggerReaperRuns,
		MetricTriggerReaperLastRun,
	} {
		_, ok := collected[name]
		assert.True(t, ok, "expected OTel instrument %q to be recorded", name)
	}

	// Verify dual-written snapshot.
	s := snap.GetSnapshot()
	q := s.Queues[trigger.EventQueueKind]
	assert.Equal(t, int64(1), q.ItemsTotal[QueueOutcomeEnqueued])
	assert.Equal(t, int64(2), q.ItemsTotal[QueueOutcomeClaimed])
	assert.Equal(t, int64(1), q.ItemsTotal[QueueOutcomeCompleted])
	assert.Equal(t, int64(1), q.ItemsTotal[QueueOutcomeRetried])
	assert.Equal(t, int64(1), q.ItemsTotal[QueueOutcomePermanent])
	assert.Equal(t, int64(3), q.ItemsTotal[QueueOutcomeDroppedAge])
	assert.Equal(t, int64(1), q.ItemsTotal[QueueOutcomeDroppedOverflow])
	assert.Equal(t, int64(2), q.ItemsTotal[QueueOutcomeDroppedPoison])
	assert.Equal(t, int64(7), q.Pending)
	assert.Equal(t, int64(2), q.Leased)
	assert.InDelta(t, 45.0, q.OldestAgeSeconds, 0.01)

	ing := s.Ingress[store.TriggerSourceKindWebhook]
	assert.Equal(t, int64(1), ing[trigger.IngressResultAccepted])
	assert.Equal(t, int64(1), ing[trigger.IngressResultDuplicate])
	assert.Equal(t, int64(1), ing[trigger.IngressResultRejectedSignature])
	assert.Equal(t, int64(1), ing[trigger.IngressResultRejectedRateLimit])
	assert.Equal(t, int64(1), ing[trigger.IngressResultRejectedSize])
	assert.Equal(t, int64(1), ing[trigger.IngressResultRejectedUnknownEndpoint])
	assert.Equal(t, int64(1), ing[trigger.IngressResultRejectedSecretUnavailable])

	assert.Equal(t, int64(1), s.Fires.ByStatus[store.TriggerFireStatusFired])
	assert.Equal(t, int64(1), s.Fires.ByStatus[store.TriggerFireStatusFailed])
	assert.Equal(t, int64(1), s.Fires.ByStatus[store.TriggerFireStatusRefused])
	assert.Equal(t, int64(1), s.Fires.ByStatus[store.TriggerFireStatusSkipped])
	assert.Equal(t, int64(1), s.Fires.SkippedByReason[trigger.FireSkipReasonRuleBudgetExhausted])

	assert.Equal(t, int64(4), s.Reaper.ReapedTotal)
	assert.Equal(t, int64(1), s.Reaper.RunsTotal[TriggerReaperOutcomeOK])
	assert.Equal(t, int64(1), s.Reaper.RunsTotal[TriggerReaperOutcomeError])
	require.NotNil(t, s.Reaper.LastRunAt)
	assert.Equal(t, runAt.Add(time.Hour).Format(time.RFC3339), *s.Reaper.LastRunAt)
}

// TestTriggerMetrics_BoundedLabelCardinality ensures arbitrary IDs, subjects,
// and free-text error strings normalize onto closed-set "other" buckets.
func TestTriggerMetrics_BoundedLabelCardinality(t *testing.T) {
	rec, snap, _ := newTestOTelTriggerMetrics(t)

	rec.Enqueued("arbitrary-tenant-queue-id-12345")
	rec.Dropped("arbitrary-tenant-queue-id-12345", "free-text-reason", 1)
	rec.RecordIngressEvent("custom-source-uuid-999", "free-text-result")
	rec.RecordTriggerFire("custom-action-xyz", "weird-status", "secret-subject-text")
	rec.RecordTriggerFire(store.TriggerActionMessage, store.TriggerFireStatusSkipped, "arbitrary error with uuid 1234")

	s := snap.GetSnapshot()
	_, hasArbitraryQueue := s.Queues["arbitrary-tenant-queue-id-12345"]
	assert.False(t, hasArbitraryQueue)
	assert.Equal(t, int64(1), s.Queues[triggerMetricLabelOther].ItemsTotal[QueueOutcomeEnqueued])
	assert.Equal(t, int64(1), s.Queues[triggerMetricLabelOther].ItemsTotal[QueueOutcomeDroppedOther])

	_, hasArbitrarySource := s.Ingress["custom-source-uuid-999"]
	assert.False(t, hasArbitrarySource)
	assert.Equal(t, int64(1), s.Ingress[triggerMetricLabelOther][triggerMetricLabelOther])

	_, hasArbitraryAction := s.Fires.ByAction["custom-action-xyz"]
	assert.False(t, hasArbitraryAction)
	assert.Equal(t, int64(1), s.Fires.ByStatus[triggerMetricLabelOther])
	assert.Equal(t, int64(1), s.Fires.SkippedByReason[trigger.FireSkipReasonOther])
}

func TestTriggerAttention_UnitCases(t *testing.T) {
	// Nil section (experiment off) emits no attention items.
	assert.Nil(t, triggerAttention(nil))

	// Unavailable section emits one warning.
	assert.Equal(t, []HealthAttentionItem{{
		Severity: HealthAttentionWarning,
		Kind:     HealthAttentionTrigger,
		Subject:  HealthAttentionSubject{Type: HealthSubjectTriggers},
		Message:  "Trigger data not available",
	}}, triggerAttention(&HealthSummaryTriggers{Unavailable: true}))

	// Failing and hub-paused triggers emit warnings without leaking LastError.
	sec := &HealthSummaryTriggers{
		Failing: []HealthFailingTrigger{
			{ID: "t1", Name: "deploy-hook", ProjectID: "p1", ProjectSlug: "alpha", LastFireStatus: store.TriggerFireStatusFailed, LastError: "raw-secret-err"},
			{ID: "t2", Name: "pr-hook", ProjectID: "p2", LastFireStatus: store.TriggerFireStatusRefused},
		},
		Total:     3,
		Truncated: true,
		Paused: []HealthPausedTrigger{
			{ID: "t3", Name: "old-hook", ProjectID: "p1", ProjectSlug: "alpha", Reason: store.TriggerStatusReasonSourceDeleted},
		},
		PausedByReason: map[string]int{store.TriggerStatusReasonSourceDeleted: 1},
		PausedTotal:    1,
	}
	items := triggerAttention(sec)
	require.Len(t, items, 4)
	assert.Equal(t, "Trigger alpha / deploy-hook failed its last fire", items[0].Message)
	assert.Equal(t, "Trigger pr-hook was refused on its last fire", items[1].Message)
	assert.Equal(t, "1 more trigger failed its last fire", items[2].Message)
	assert.Equal(t, "Trigger alpha / old-hook was paused by the hub: its webhook source was deleted", items[3].Message)
	for _, it := range items {
		assert.NotContains(t, it.Message, "raw-secret-err")
		assert.Equal(t, HealthAttentionWarning, it.Severity)
	}

	// Reason code normalization rejects arbitrary stored text.
	assert.Equal(t, store.TriggerStatusReasonCreatorDeleted, healthTriggerReasonCode(store.TriggerStatusReasonCreatorDeleted))
	assert.Equal(t, store.TriggerStatusReasonTargetDeleted, healthTriggerReasonCode(store.TriggerStatusReasonTargetDeleted))
	assert.Equal(t, store.TriggerStatusReasonSourceDeleted, healthTriggerReasonCode(store.TriggerStatusReasonSourceDeleted))
	assert.Equal(t, healthTriggerReasonOther, healthTriggerReasonCode("arbitrary-free-text"))
}

// TestServerTriggerMetrics_BareServerNilSafe ensures serverTriggerMetrics never
// returns a typed-nil interface on bare &Server{} structs constructed without New.
func TestServerTriggerMetrics_BareServerNilSafe(t *testing.T) {
	m := serverTriggerMetrics{srv: &Server{}}
	assert.Nil(t, m.recorder())
	assert.NotPanics(t, func() {
		m.Enqueued(trigger.EventQueueKind)
		m.Claimed(trigger.EventQueueKind, 1)
		m.Done(trigger.EventQueueKind)
		m.Retried(trigger.EventQueueKind)
		m.Permanent(trigger.EventQueueKind)
		m.Dropped(trigger.EventQueueKind, durablequeue.DropReasonAge, 1)
		m.ObserveQueue(trigger.EventQueueKind, 1, 0, time.Second)
		m.RecordIngressEvent(store.TriggerSourceKindWebhook, trigger.IngressResultAccepted)
		m.RecordTriggerFire(store.TriggerActionMessage, store.TriggerFireStatusFired, "")
		m.RecordReaperRun(1, time.Now(), nil)
	})
}

func TestOTelTriggerMetrics_SkippedFireReasonsCompileBudgetAndAgentNameInUse(t *testing.T) {
	rec, snap, reader := newTestOTelTriggerMetrics(t)

	rec.RecordTriggerFire(
		store.TriggerActionMessage,
		store.TriggerFireStatusSkipped,
		trigger.NormalizeFireSkipReason(trigger.ErrFireRuleCompileBudgetExhausted),
	)
	rec.RecordTriggerFire(
		store.TriggerActionDispatchAgent,
		store.TriggerFireStatusSkipped,
		trigger.NormalizeFireSkipReason("agent name in use"),
	)
	rec.RecordTriggerFire(
		store.TriggerActionGitHubComment,
		store.TriggerFireStatusSkipped,
		trigger.NormalizeFireSkipReason("comment rate limited"),
	)
	rec.RecordTriggerFire(
		store.TriggerActionMessage,
		store.TriggerFireStatusSkipped,
		"unrecognized free-text skip reason",
	)

	// Verify snapshot counts preserve all new reasons and bucket unknown text as "other".
	s := snap.GetSnapshot()
	assert.Equal(t, int64(1), s.Fires.SkippedByReason[trigger.FireSkipReasonRuleCompileBudgetExhausted])
	assert.Equal(t, int64(1), s.Fires.SkippedByReason[trigger.FireSkipReasonAgentNameInUse])
	assert.Equal(t, int64(1), s.Fires.SkippedByReason[trigger.FireSkipReasonCommentRateLimited])
	assert.Equal(t, int64(1), s.Fires.SkippedByReason[trigger.FireSkipReasonOther])

	// Verify OTel counter exports the exact same reason labels.
	collected := collectTriggerMetrics(t, reader)
	firesMetric, ok := collected[MetricTriggerFires]
	require.True(t, ok, "expected %s OTel metric", MetricTriggerFires)
	sum, ok := firesMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)

	gotByReason := make(map[string]int64)
	for _, dp := range sum.DataPoints {
		if val, exists := dp.Attributes.Value("reason"); exists {
			gotByReason[val.AsString()] += dp.Value
		}
	}
	assert.Equal(t, int64(1), gotByReason[trigger.FireSkipReasonRuleCompileBudgetExhausted])
	assert.Equal(t, int64(1), gotByReason[trigger.FireSkipReasonAgentNameInUse])
	assert.Equal(t, int64(1), gotByReason[trigger.FireSkipReasonCommentRateLimited])
	assert.Equal(t, int64(1), gotByReason[trigger.FireSkipReasonOther])
}
