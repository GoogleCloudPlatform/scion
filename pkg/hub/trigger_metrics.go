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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Closed-set metric label values for durable queue, trigger ingress, trigger
// fires, and the firing-row reaper (Stage 1 §6.5). Every recorded label value
// is normalized onto one of these constants so metric cardinality stays
// bounded (no IDs, subjects, or free-text errors).

const (
	// Durable queue item outcome labels.
	QueueOutcomeEnqueued        = "enqueued"
	QueueOutcomeClaimed         = "claimed"
	QueueOutcomeCompleted       = "completed"
	QueueOutcomeRetried         = "retried"
	QueueOutcomePermanent       = "permanent"
	QueueOutcomeDroppedAge      = "dropped_age"
	QueueOutcomeDroppedOverflow = "dropped_overflow"
	QueueOutcomeDroppedPoison   = "dropped_poison"
	QueueOutcomeDroppedOther    = "dropped_other"

	// Trigger reaper run outcome labels.
	TriggerReaperOutcomeOK    = "ok"
	TriggerReaperOutcomeError = "error"

	// Fallback label for any unrecognized closed-set value.
	triggerMetricLabelOther = "other"
)

func triggerQueueOutcomes() []string {
	return []string{
		QueueOutcomeEnqueued,
		QueueOutcomeClaimed,
		QueueOutcomeCompleted,
		QueueOutcomeRetried,
		QueueOutcomePermanent,
		QueueOutcomeDroppedAge,
		QueueOutcomeDroppedOverflow,
		QueueOutcomeDroppedPoison,
		QueueOutcomeDroppedOther,
	}
}

func triggerIngressResults() []string {
	return []string{
		trigger.IngressResultAccepted,
		trigger.IngressResultDuplicate,
		trigger.IngressResultRejectedSignature,
		trigger.IngressResultRejectedRateLimit,
		trigger.IngressResultRejectedSize,
		trigger.IngressResultRejectedUnknownEndpoint,
		trigger.IngressResultRejectedSecretUnavailable,
		trigger.IngressResultRejectedMediaType,
		trigger.IngressResultRejectedBadJSON,
		trigger.IngressResultRejectedBadHeader,
		trigger.IngressResultRejectedQueueFull,
		trigger.IngressResultFailedStore,
		triggerMetricLabelOther,
	}
}

func triggerFireStatuses() []string {
	return []string{
		store.TriggerFireStatusFired,
		store.TriggerFireStatusSkipped,
		store.TriggerFireStatusFailed,
		store.TriggerFireStatusRefused,
		triggerMetricLabelOther,
	}
}

func triggerFireSkipReasons() []string {
	return []string{
		trigger.FireSkipReasonRuleBudgetExhausted,
		trigger.FireSkipReasonRuleEvaluationFailed,
		trigger.FireSkipReasonRuleUncertain,
		trigger.FireSkipReasonRuleNoLongerCompiles,
		trigger.FireSkipReasonSubjectBusy,
		trigger.FireSkipReasonOther,
	}
}

func triggerReaperOutcomes() []string {
	return []string{
		TriggerReaperOutcomeOK,
		TriggerReaperOutcomeError,
	}
}

func normalizeQueueKindLabel(kind string) string {
	switch kind {
	case trigger.EventQueueKind:
		return kind
	default:
		return triggerMetricLabelOther
	}
}

func normalizeQueueDropOutcome(reason string) string {
	switch reason {
	case durablequeue.DropReasonAge:
		return QueueOutcomeDroppedAge
	case durablequeue.DropReasonOverflow:
		return QueueOutcomeDroppedOverflow
	case durablequeue.DropReasonPoison:
		return QueueOutcomeDroppedPoison
	default:
		return QueueOutcomeDroppedOther
	}
}

func normalizeTriggerSourceKindLabel(sourceKind string) string {
	switch sourceKind {
	case store.TriggerSourceKindWebhook, store.TriggerSourceKindGitHub:
		return sourceKind
	default:
		return triggerMetricLabelOther
	}
}

func normalizeTriggerIngressResultLabel(result string) string {
	switch result {
	case trigger.IngressResultAccepted,
		trigger.IngressResultDuplicate,
		trigger.IngressResultRejectedSignature,
		trigger.IngressResultRejectedRateLimit,
		trigger.IngressResultRejectedSize,
		trigger.IngressResultRejectedUnknownEndpoint,
		trigger.IngressResultRejectedSecretUnavailable,
		trigger.IngressResultRejectedMediaType,
		trigger.IngressResultRejectedBadJSON,
		trigger.IngressResultRejectedBadHeader,
		trigger.IngressResultRejectedQueueFull,
		trigger.IngressResultFailedStore:
		return result
	default:
		return triggerMetricLabelOther
	}
}

func normalizeTriggerActionLabel(actionType string) string {
	switch actionType {
	case store.TriggerActionMessage, store.TriggerActionDispatchAgent, store.TriggerActionExec:
		return actionType
	default:
		return triggerMetricLabelOther
	}
}

func normalizeTriggerFireStatusLabel(status string) string {
	switch status {
	case store.TriggerFireStatusFired,
		store.TriggerFireStatusSkipped,
		store.TriggerFireStatusFailed,
		store.TriggerFireStatusRefused:
		return status
	default:
		return triggerMetricLabelOther
	}
}

func normalizeTriggerFireSkipReasonLabel(status, skipReason string) string {
	if status != store.TriggerFireStatusSkipped {
		return ""
	}
	switch skipReason {
	case trigger.FireSkipReasonRuleBudgetExhausted,
		trigger.FireSkipReasonRuleEvaluationFailed,
		trigger.FireSkipReasonRuleUncertain,
		trigger.FireSkipReasonRuleNoLongerCompiles,
		trigger.FireSkipReasonSubjectBusy,
		trigger.FireSkipReasonOther:
		return skipReason
	default:
		return trigger.FireSkipReasonOther
	}
}

// TriggerMetricsRecorder records durable queue, trigger ingress, trigger fire,
// and firing-row reaper metrics. Implementations must be safe for concurrent
// use.
type TriggerMetricsRecorder interface {
	durablequeue.QueueMetrics
	durablequeue.Metrics
	trigger.IngressMetrics
	trigger.DispatcherMetrics
	RecordReaperRun(reaped int, at time.Time, err error)
}

type queueKindSnapshotState struct {
	items            map[string]int64
	pending          int64
	leased           int64
	oldestAgeSeconds float64
}

// TriggerSnapshotMetrics is a dependency-free, in-process recorder for durable
// queue, trigger ingress, trigger fire, and firing-row reaper metrics, exposed
// on GET /metrics when hub.triggers is enabled. Server.New wires one instance
// as the default recorder, and NewOTelTriggerMetrics dual-writes into the same
// instance when Cloud Monitoring export is configured.
type TriggerSnapshotMetrics struct {
	since time.Time
	mu    sync.Mutex

	queues          map[string]*queueKindSnapshotState
	ingress         map[string]map[string]int64
	firesByStatus   map[string]int64
	firesByAction   map[string]map[string]int64
	skippedByReason map[string]int64
	reaperReaped    int64
	reaperRuns      map[string]int64
	reaperLastRunAt *time.Time
}

var _ TriggerMetricsRecorder = (*TriggerSnapshotMetrics)(nil)

// NewTriggerSnapshotMetrics creates an empty in-process trigger metrics
// recorder, recording its construction time as Since.
func NewTriggerSnapshotMetrics() *TriggerSnapshotMetrics {
	return &TriggerSnapshotMetrics{
		since:           time.Now().UTC(),
		queues:          make(map[string]*queueKindSnapshotState),
		ingress:         make(map[string]map[string]int64),
		firesByStatus:   make(map[string]int64),
		firesByAction:   make(map[string]map[string]int64),
		skippedByReason: make(map[string]int64),
		reaperRuns:      make(map[string]int64),
	}
}

func (m *TriggerSnapshotMetrics) queueStateLocked(kind string) *queueKindSnapshotState {
	k := normalizeQueueKindLabel(kind)
	st, ok := m.queues[k]
	if !ok {
		st = &queueKindSnapshotState{items: make(map[string]int64)}
		m.queues[k] = st
	}
	return st
}

// Enqueued implements durablequeue.QueueMetrics.
func (m *TriggerSnapshotMetrics) Enqueued(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueStateLocked(kind).items[QueueOutcomeEnqueued]++
}

// Claimed implements durablequeue.Metrics.
func (m *TriggerSnapshotMetrics) Claimed(kind string, n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueStateLocked(kind).items[QueueOutcomeClaimed] += int64(n)
}

// Done implements durablequeue.Metrics.
func (m *TriggerSnapshotMetrics) Done(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueStateLocked(kind).items[QueueOutcomeCompleted]++
}

// Retried implements durablequeue.Metrics.
func (m *TriggerSnapshotMetrics) Retried(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueStateLocked(kind).items[QueueOutcomeRetried]++
}

// Permanent implements durablequeue.Metrics.
func (m *TriggerSnapshotMetrics) Permanent(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueStateLocked(kind).items[QueueOutcomePermanent]++
}

// Dropped implements durablequeue.Metrics.
func (m *TriggerSnapshotMetrics) Dropped(kind, reason string, n int) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueStateLocked(kind).items[normalizeQueueDropOutcome(reason)] += int64(n)
}

// ObserveQueue implements durablequeue.Metrics.
func (m *TriggerSnapshotMetrics) ObserveQueue(kind string, pending, leased int64, oldestAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.queueStateLocked(kind)
	st.pending = pending
	st.leased = leased
	if oldestAge > 0 {
		st.oldestAgeSeconds = oldestAge.Seconds()
	} else {
		st.oldestAgeSeconds = 0
	}
}

// RecordIngressEvent implements trigger.IngressMetrics.
func (m *TriggerSnapshotMetrics) RecordIngressEvent(sourceKind, result string) {
	sk := normalizeTriggerSourceKindLabel(sourceKind)
	res := normalizeTriggerIngressResultLabel(result)
	m.mu.Lock()
	defer m.mu.Unlock()
	byResult, ok := m.ingress[sk]
	if !ok {
		byResult = make(map[string]int64)
		m.ingress[sk] = byResult
	}
	byResult[res]++
}

// RecordTriggerFire implements trigger.DispatcherMetrics.
func (m *TriggerSnapshotMetrics) RecordTriggerFire(actionType, status, skipReason string) {
	act := normalizeTriggerActionLabel(actionType)
	st := normalizeTriggerFireStatusLabel(status)
	sr := normalizeTriggerFireSkipReasonLabel(st, skipReason)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.firesByStatus[st]++
	byStatus, ok := m.firesByAction[act]
	if !ok {
		byStatus = make(map[string]int64)
		m.firesByAction[act] = byStatus
	}
	byStatus[st]++
	if st == store.TriggerFireStatusSkipped {
		m.skippedByReason[sr]++
	}
}

// RecordReaperRun implements TriggerMetricsRecorder.
func (m *TriggerSnapshotMetrics) RecordReaperRun(reaped int, at time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if reaped > 0 {
		m.reaperReaped += int64(reaped)
	}
	if err != nil {
		m.reaperRuns[TriggerReaperOutcomeError]++
	} else {
		m.reaperRuns[TriggerReaperOutcomeOK]++
	}
	if !at.IsZero() {
		utc := at.UTC()
		m.reaperLastRunAt = &utc
	}
}

// TriggerQueueMetricsSnapshot is one durable queue kind's counters and gauges.
type TriggerQueueMetricsSnapshot struct {
	ItemsTotal       map[string]int64 `json:"itemsTotal"`
	Pending          int64            `json:"pending"`
	Leased           int64            `json:"leased"`
	OldestAgeSeconds float64          `json:"oldestAgeSeconds"`
}

// TriggerFiresMetricsSnapshot is the in-process snapshot of trigger fires.
type TriggerFiresMetricsSnapshot struct {
	ByStatus        map[string]int64            `json:"byStatus"`
	ByAction        map[string]map[string]int64 `json:"byAction"`
	SkippedByReason map[string]int64            `json:"skippedByReason"`
}

// TriggerReaperMetricsSnapshot is the in-process snapshot of the firing-row reaper.
type TriggerReaperMetricsSnapshot struct {
	ReapedTotal int64            `json:"reapedTotal"`
	RunsTotal   map[string]int64 `json:"runsTotal"`
	LastRunAt   *string          `json:"lastRunAt,omitempty"`
}

// TriggerMetricsSnapshot is the GET /metrics JSON section for triggers and the
// durable queue (served as "triggers" when hub.triggers is enabled).
type TriggerMetricsSnapshot struct {
	Since   string                                 `json:"since"`
	Queues  map[string]TriggerQueueMetricsSnapshot `json:"queues"`
	Ingress map[string]map[string]int64            `json:"ingress"`
	Fires   TriggerFiresMetricsSnapshot            `json:"fires"`
	Reaper  TriggerReaperMetricsSnapshot           `json:"reaper"`
}

// GetSnapshot returns a point-in-time snapshot with deterministic closed-set keys.
func (m *TriggerSnapshotMetrics) GetSnapshot() *TriggerMetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snap := &TriggerMetricsSnapshot{
		Since:   m.since.UTC().Format(time.RFC3339),
		Queues:  make(map[string]TriggerQueueMetricsSnapshot),
		Ingress: make(map[string]map[string]int64),
		Fires: TriggerFiresMetricsSnapshot{
			ByStatus:        make(map[string]int64, len(triggerFireStatuses())),
			ByAction:        make(map[string]map[string]int64),
			SkippedByReason: make(map[string]int64, len(triggerFireSkipReasons())),
		},
		Reaper: TriggerReaperMetricsSnapshot{
			ReapedTotal: m.reaperReaped,
			RunsTotal:   make(map[string]int64, len(triggerReaperOutcomes())),
		},
	}

	// Always include trigger.event so the JSON shape is deterministic even before
	// the first item is enqueued.
	queueKinds := []string{trigger.EventQueueKind}
	for k := range m.queues {
		if k != trigger.EventQueueKind {
			queueKinds = append(queueKinds, k)
		}
	}
	for _, k := range queueKinds {
		items := make(map[string]int64, len(triggerQueueOutcomes()))
		var pending, leased int64
		var oldest float64
		if st := m.queues[k]; st != nil {
			for _, o := range triggerQueueOutcomes() {
				items[o] = st.items[o]
			}
			pending = st.pending
			leased = st.leased
			oldest = st.oldestAgeSeconds
		} else {
			for _, o := range triggerQueueOutcomes() {
				items[o] = 0
			}
		}
		snap.Queues[k] = TriggerQueueMetricsSnapshot{
			ItemsTotal:       items,
			Pending:          pending,
			Leased:           leased,
			OldestAgeSeconds: oldest,
		}
	}

	sourceKinds := []string{store.TriggerSourceKindWebhook}
	for sk := range m.ingress {
		if sk != store.TriggerSourceKindWebhook {
			sourceKinds = append(sourceKinds, sk)
		}
	}
	for _, sk := range sourceKinds {
		byRes := make(map[string]int64, len(triggerIngressResults()))
		srcMap := m.ingress[sk]
		for _, res := range triggerIngressResults() {
			byRes[res] = srcMap[res]
		}
		snap.Ingress[sk] = byRes
	}

	for _, st := range triggerFireStatuses() {
		snap.Fires.ByStatus[st] = m.firesByStatus[st]
	}
	for act, byStatus := range m.firesByAction {
		cp := make(map[string]int64, len(triggerFireStatuses()))
		for _, st := range triggerFireStatuses() {
			cp[st] = byStatus[st]
		}
		snap.Fires.ByAction[act] = cp
	}
	for _, r := range triggerFireSkipReasons() {
		snap.Fires.SkippedByReason[r] = m.skippedByReason[r]
	}

	for _, o := range triggerReaperOutcomes() {
		snap.Reaper.RunsTotal[o] = m.reaperRuns[o]
	}
	if m.reaperLastRunAt != nil {
		ts := m.reaperLastRunAt.UTC().Format(time.RFC3339)
		snap.Reaper.LastRunAt = &ts
	}
	return snap
}

// serverTriggerMetrics delegates durablequeue, ingress, dispatcher, and reaper
// observations to whichever TriggerMetricsRecorder is currently installed on
// Server (defaulting to s.triggerMetricsSnapshot, upgraded to OTelTriggerMetrics
// by SetTriggerMetrics). Because StoreQueue and trigger.Ingress are created in
// Server.New before SetTriggerMetrics is called, delegating dynamically keeps
// every producer and drainer attached to the active recorder.
type serverTriggerMetrics struct {
	srv *Server
}

var _ TriggerMetricsRecorder = serverTriggerMetrics{}

func (m serverTriggerMetrics) recorder() TriggerMetricsRecorder {
	if m.srv == nil {
		return nil
	}
	if rec := m.srv.triggerMetrics.Load(); rec != nil && *rec != nil {
		return *rec
	}
	if m.srv.triggerMetricsSnapshot == nil {
		return nil
	}
	return m.srv.triggerMetricsSnapshot
}

func (m serverTriggerMetrics) Enqueued(kind string) {
	if rec := m.recorder(); rec != nil {
		rec.Enqueued(kind)
	}
}

func (m serverTriggerMetrics) Claimed(kind string, n int) {
	if rec := m.recorder(); rec != nil {
		rec.Claimed(kind, n)
	}
}

func (m serverTriggerMetrics) Done(kind string) {
	if rec := m.recorder(); rec != nil {
		rec.Done(kind)
	}
}

func (m serverTriggerMetrics) Retried(kind string) {
	if rec := m.recorder(); rec != nil {
		rec.Retried(kind)
	}
}

func (m serverTriggerMetrics) Permanent(kind string) {
	if rec := m.recorder(); rec != nil {
		rec.Permanent(kind)
	}
}

func (m serverTriggerMetrics) Dropped(kind, reason string, n int) {
	if rec := m.recorder(); rec != nil {
		rec.Dropped(kind, reason, n)
	}
}

func (m serverTriggerMetrics) ObserveQueue(kind string, pending, leased int64, oldestAge time.Duration) {
	if rec := m.recorder(); rec != nil {
		rec.ObserveQueue(kind, pending, leased, oldestAge)
	}
}

func (m serverTriggerMetrics) RecordIngressEvent(sourceKind, result string) {
	if rec := m.recorder(); rec != nil {
		rec.RecordIngressEvent(sourceKind, result)
	}
}

func (m serverTriggerMetrics) RecordTriggerFire(actionType, status, skipReason string) {
	if rec := m.recorder(); rec != nil {
		rec.RecordTriggerFire(actionType, status, skipReason)
	}
}

func (m serverTriggerMetrics) RecordReaperRun(reaped int, at time.Time, err error) {
	if rec := m.recorder(); rec != nil {
		rec.RecordReaperRun(reaped, at, err)
	}
}

// TriggerSnapshotMetrics returns the in-process trigger metrics snapshot
// instance wired into s so cmd/server_foreground.go can pass it to
// NewOTelTriggerMetrics for dual-writing.
func (s *Server) TriggerSnapshotMetrics() *TriggerSnapshotMetrics {
	if s == nil {
		return nil
	}
	return s.triggerMetricsSnapshot
}

// SetTriggerMetrics installs rec as the server's trigger and durable queue
// metrics recorder. Passing nil falls back to the in-process snapshot recorder.
func (s *Server) SetTriggerMetrics(rec TriggerMetricsRecorder) {
	if s == nil {
		return
	}
	if rec == nil {
		s.triggerMetrics.Store(nil)
		return
	}
	s.triggerMetrics.Store(&rec)
}
