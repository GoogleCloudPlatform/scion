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

package telemetryemit

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Lifecycle metric names (WP1 design 5.3). Counters are named without the
// _total suffix, which a Prometheus exporter appends, so they export as the
// design's lifecycle_*_total names. All are process-local per replica and
// reset on restart; the exercise manifest samples them per replica.
const (
	MetricEventsEnqueued  = "lifecycle_events_enqueued"  // {transition}
	MetricBuildFailures   = "lifecycle_build_failures"   // {reason}
	MetricResyncs         = "lifecycle_resyncs"          // {cause}
	MetricEmitAttempts    = "lifecycle_emit_attempts"    //
	MetricEmitFailures    = "lifecycle_emit_failures"    // {reason}
	MetricEventsDelivered = "lifecycle_events_delivered" //
	MetricOutboxDropped   = "lifecycle_outbox_dropped"   // {reason=age|overflow|poison}
	MetricSinkBlocked     = "lifecycle_sink_blocked"     // gauge {reason}
	MetricOutboxPending   = "lifecycle_outbox_pending"   // gauge
	MetricOutboxOldestAge = "lifecycle_outbox_oldest_age_seconds"
	MetricSeqAllocatedMax = "lifecycle_outbox_seq_allocated_max" // gauge; lag only, not loss
	MetricSeqDeliveredMax = "lifecycle_outbox_seq_delivered_max" // gauge; lag only, not loss
	MetricObserverState   = "lifecycle_observer_state"           // gauge {state}
	MetricDrainerState    = "lifecycle_drainer_state"            // gauge {state}
	MetricSinkKind        = "lifecycle_sink_kind"                // gauge {kind=cloud_logging|stdout|none}
	MetricAmbiguous       = "lifecycle_ambiguous_transitions"    // {kind}
	MetricUnconfirmed     = "lifecycle_unconfirmed_starts"       //
	MetricStaleEcho       = "lifecycle_stale_echo"               // {path, reason}
	// MetricAttributionStale counts a cursor whose run attribution could
	// not be trusted and was normalised to unattributed (WP1 attribution
	// design 7.1, 3.3): reason is one of the AttributionStale* values.
	MetricAttributionStale = "lifecycle_attribution_stale" // {reason}
	attrPath               = "path"
	attrKind               = "kind"
	attrTransition         = "transition"
	attrReason             = "reason"
	attrCause              = "cause"
	attrState              = "state"
)

// Observer states for MetricObserverState (design 3.3).
const (
	ObserverStateEnabled               = "enabled"
	ObserverStateDisabledExperimentOff = "disabled_experiment_off"
	ObserverStateDisabledUnpinnedHubID = "disabled_unpinned_hub_id"
	// ObserverStateDisabledNoSink: no sink is configured, so nothing could
	// drain or trim the outbox; the observer is not installed.
	ObserverStateDisabledNoSink = "disabled_no_sink"
)

var observerStates = []string{ObserverStateEnabled, ObserverStateDisabledExperimentOff, ObserverStateDisabledUnpinnedHubID, ObserverStateDisabledNoSink}

// Drainer states for MetricDrainerState (design 11, r6 N4): running with
// an installed observer, backlog_only while delivering rows left from an
// earlier run, idle_disabled when no drainer runs.
const (
	DrainerStateRunning      = "running"
	DrainerStateBacklogOnly  = "backlog_only"
	DrainerStateIdleDisabled = "idle_disabled"
)

var drainerStates = []string{DrainerStateRunning, DrainerStateBacklogOnly, DrainerStateIdleDisabled}

// Sink kinds for MetricSinkKind (design section 22 item 4, condition b).
const (
	SinkKindCloudLogging = "cloud_logging"
	SinkKindStdout       = "stdout"
	SinkKindNone         = "none"
)

var sinkKinds = []string{SinkKindCloudLogging, SinkKindStdout, SinkKindNone}

// OutboxGauges is one sample of the outbox gauges.
type OutboxGauges struct {
	Pending          int64
	OldestAgeSeconds int64
	SeqAllocatedMax  int64
	SeqDeliveredMax  int64
}

// LifecycleCounters receives the lifecycle observer's and drainer's
// counters and gauges. Implementations must be safe for concurrent use.
// Nothing calls them inside a database transaction: enqueue counts are
// added after commit.
type LifecycleCounters interface {
	Enqueued(transition string)
	BuildFailure(reason string)
	Resync(cause string)
	EmitAttempt()
	EmitFailure(reason string)
	Delivered()
	Dropped(reason string, n int)
	SinkBlocked(reason string, blocked bool)
	Outbox(g OutboxGauges)
	ObserverState(state string)
	DrainerState(state string)
	// SinkKind records which sink this replica delivers to.
	SinkKind(kind string)
	// AmbiguousTransition counts a transition derived as ambiguous and
	// not emitted (design 2.4a P5b, P9).
	AmbiguousTransition(kind string)
	// UnconfirmedStart counts a start rollback with an unknown outcome.
	UnconfirmedStart()
	// StaleEcho counts a broker effective-configuration echo the hub did
	// not record (WP1 slice 3 addendum v12 section 6): path is
	// StaleEchoPathSync or StaleEchoPathLaunchReport, reason the store's
	// stale reason or unsettled_adoption.
	StaleEcho(path, reason string)
	// AttributionStale counts a cursor normalised to unattributed, or a
	// malformed segment discarded (attribution design 7.1 and 3.3). A
	// LEGACY cursor that also carries a CORRUPT field counts as corrupt,
	// not legacy.
	AttributionStale(reason string)
}

// Reasons of MetricAttributionStale.
const (
	AttributionStaleMissing   = "missing_cursor"
	AttributionStaleLegacy    = "legacy"
	AttributionStaleStamp     = "stale_stamp"
	AttributionStaleCorrupt   = "corrupt"
	AttributionStaleDiscarded = "segment_discarded"
)

// Paths of MetricStaleEcho.
const (
	StaleEchoPathSync         = "sync"
	StaleEchoPathLaunchReport = "launch_report"
)

// StaleEchoKey is the MemoryCounters attribute key of MetricStaleEcho.
func StaleEchoKey(path, reason string) string {
	return attrPath + "=" + path + "," + attrReason + "=" + reason
}

// NopCounters discards everything.
type NopCounters struct{}

func (NopCounters) Enqueued(string)          {}
func (NopCounters) BuildFailure(string)      {}
func (NopCounters) Resync(string)            {}
func (NopCounters) EmitAttempt()             {}
func (NopCounters) EmitFailure(string)       {}
func (NopCounters) Delivered()               {}
func (NopCounters) Dropped(string, int)      {}
func (NopCounters) SinkBlocked(string, bool) {}
func (NopCounters) Outbox(OutboxGauges)      {}
func (NopCounters) ObserverState(string)     {}
func (NopCounters) DrainerState(string)      {}

// SinkKind implements LifecycleCounters.
func (NopCounters) SinkKind(string) {}

// AmbiguousTransition implements LifecycleCounters.
func (NopCounters) AmbiguousTransition(string) {}

// UnconfirmedStart implements LifecycleCounters.
func (NopCounters) UnconfirmedStart() {}

// StaleEcho implements LifecycleCounters.
func (NopCounters) StaleEcho(string, string) {}

// AttributionStale implements LifecycleCounters.
func (NopCounters) AttributionStale(string) {}

// MemoryCounters keeps values in memory, for tests. Get reads a value by
// metric name and attribute value ("" for none).
type MemoryCounters struct {
	mu     sync.Mutex
	values map[string]int64
}

// NewMemoryCounters returns empty MemoryCounters.
func NewMemoryCounters() *MemoryCounters { return &MemoryCounters{values: map[string]int64{}} }

func memKey(name, attr string) string {
	if attr == "" {
		return name
	}
	return name + "{" + attr + "}"
}

func (m *MemoryCounters) add(name, attr string, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string]int64{}
	}
	m.values[memKey(name, attr)] += n
}

func (m *MemoryCounters) set(name, attr string, v int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.values == nil {
		m.values = map[string]int64{}
	}
	m.values[memKey(name, attr)] = v
}

// Get returns the value of name{attr}.
func (m *MemoryCounters) Get(name, attr string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values[memKey(name, attr)]
}

// String lists every value, sorted, for test failure messages.
func (m *MemoryCounters) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.values))
	for k := range m.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%d ", k, m.values[k])
	}
	return strings.TrimSpace(b.String())
}

func (m *MemoryCounters) Enqueued(tr string)      { m.add(MetricEventsEnqueued, tr, 1) }
func (m *MemoryCounters) BuildFailure(r string)   { m.add(MetricBuildFailures, r, 1) }
func (m *MemoryCounters) Resync(c string)         { m.add(MetricResyncs, c, 1) }
func (m *MemoryCounters) EmitAttempt()            { m.add(MetricEmitAttempts, "", 1) }
func (m *MemoryCounters) EmitFailure(r string)    { m.add(MetricEmitFailures, r, 1) }
func (m *MemoryCounters) Delivered()              { m.add(MetricEventsDelivered, "", 1) }
func (m *MemoryCounters) Dropped(r string, n int) { m.add(MetricOutboxDropped, r, int64(n)) }
func (m *MemoryCounters) SinkBlocked(r string, b bool) {
	v := int64(0)
	if b {
		v = 1
	}
	m.set(MetricSinkBlocked, r, v)
}
func (m *MemoryCounters) Outbox(g OutboxGauges) {
	m.set(MetricOutboxPending, "", g.Pending)
	m.set(MetricOutboxOldestAge, "", g.OldestAgeSeconds)
	m.set(MetricSeqAllocatedMax, "", g.SeqAllocatedMax)
	m.set(MetricSeqDeliveredMax, "", g.SeqDeliveredMax)
}
func (m *MemoryCounters) DrainerState(state string) {
	for _, s := range drainerStates {
		v := int64(0)
		if s == state {
			v = 1
		}
		m.set(MetricDrainerState, s, v)
	}
}

// SinkKind implements LifecycleCounters.
func (m *MemoryCounters) SinkKind(kind string) {
	for _, k := range sinkKinds {
		v := int64(0)
		if k == kind {
			v = 1
		}
		m.set(MetricSinkKind, k, v)
	}
}

// AmbiguousTransition implements LifecycleCounters.
func (m *MemoryCounters) AmbiguousTransition(kind string) { m.add(MetricAmbiguous, kind, 1) }

// UnconfirmedStart implements LifecycleCounters.
func (m *MemoryCounters) UnconfirmedStart() { m.add(MetricUnconfirmed, "", 1) }

// StaleEcho implements LifecycleCounters; read it with
// Get(MetricStaleEcho, StaleEchoKey(path, reason)).
func (m *MemoryCounters) StaleEcho(path, reason string) {
	m.add(MetricStaleEcho, StaleEchoKey(path, reason), 1)
}

// AttributionStale implements LifecycleCounters.
func (m *MemoryCounters) AttributionStale(reason string) { m.add(MetricAttributionStale, reason, 1) }

func (m *MemoryCounters) ObserverState(state string) {
	for _, s := range observerStates {
		v := int64(0)
		if s == state {
			v = 1
		}
		m.set(MetricObserverState, s, v)
	}
}

// OTelCounters exports the lifecycle metrics through an OpenTelemetry
// MeterProvider (the hub's metrics path), never through scion-analytics,
// so a failing analytics sink cannot hide its own loss.
type OTelCounters struct {
	enqueued, buildFailures, resyncs, emitAttempts, emitFailures, delivered, dropped metric.Int64Counter
	sinkBlocked, pending, oldestAge, seqAllocated, seqDelivered, observerState       metric.Int64Gauge
	drainerState, sinkKind                                                           metric.Int64Gauge
	ambiguous, unconfirmed, staleEcho, attrStale                                     metric.Int64Counter
}

// NewOTelCounters creates the instruments on mp's meter named scope.
func NewOTelCounters(mp metric.MeterProvider, scope string) (*OTelCounters, error) {
	if mp == nil {
		return nil, errors.New("telemetryemit: nil MeterProvider")
	}
	m := mp.Meter(scope)
	var errs []error
	counter := func(name string) metric.Int64Counter {
		c, err := m.Int64Counter(name)
		errs = append(errs, err)
		return c
	}
	gauge := func(name string) metric.Int64Gauge {
		g, err := m.Int64Gauge(name)
		errs = append(errs, err)
		return g
	}
	c := &OTelCounters{
		enqueued:      counter(MetricEventsEnqueued),
		buildFailures: counter(MetricBuildFailures),
		resyncs:       counter(MetricResyncs),
		emitAttempts:  counter(MetricEmitAttempts),
		emitFailures:  counter(MetricEmitFailures),
		delivered:     counter(MetricEventsDelivered),
		dropped:       counter(MetricOutboxDropped),
		sinkBlocked:   gauge(MetricSinkBlocked),
		pending:       gauge(MetricOutboxPending),
		oldestAge:     gauge(MetricOutboxOldestAge),
		seqAllocated:  gauge(MetricSeqAllocatedMax),
		seqDelivered:  gauge(MetricSeqDeliveredMax),
		observerState: gauge(MetricObserverState),
		drainerState:  gauge(MetricDrainerState),
		sinkKind:      gauge(MetricSinkKind),
		ambiguous:     counter(MetricAmbiguous),
		unconfirmed:   counter(MetricUnconfirmed),
		staleEcho:     counter(MetricStaleEcho),
		attrStale:     counter(MetricAttributionStale),
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return c, nil
}

func attr(k, v string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String(k, v))
}

var bg = context.Background()

func (c *OTelCounters) Enqueued(tr string)      { c.enqueued.Add(bg, 1, attr(attrTransition, tr)) }
func (c *OTelCounters) BuildFailure(r string)   { c.buildFailures.Add(bg, 1, attr(attrReason, r)) }
func (c *OTelCounters) Resync(cause string)     { c.resyncs.Add(bg, 1, attr(attrCause, cause)) }
func (c *OTelCounters) EmitAttempt()            { c.emitAttempts.Add(bg, 1) }
func (c *OTelCounters) EmitFailure(r string)    { c.emitFailures.Add(bg, 1, attr(attrReason, r)) }
func (c *OTelCounters) Delivered()              { c.delivered.Add(bg, 1) }
func (c *OTelCounters) Dropped(r string, n int) { c.dropped.Add(bg, int64(n), attr(attrReason, r)) }
func (c *OTelCounters) SinkBlocked(r string, b bool) {
	v := int64(0)
	if b {
		v = 1
	}
	c.sinkBlocked.Record(bg, v, attr(attrReason, r))
}
func (c *OTelCounters) Outbox(g OutboxGauges) {
	c.pending.Record(bg, g.Pending)
	c.oldestAge.Record(bg, g.OldestAgeSeconds)
	c.seqAllocated.Record(bg, g.SeqAllocatedMax)
	c.seqDelivered.Record(bg, g.SeqDeliveredMax)
}
func (c *OTelCounters) ObserverState(state string) {
	for _, s := range observerStates {
		v := int64(0)
		if s == state {
			v = 1
		}
		c.observerState.Record(bg, v, attr(attrState, s))
	}
}

func (c *OTelCounters) DrainerState(state string) {
	for _, s := range drainerStates {
		v := int64(0)
		if s == state {
			v = 1
		}
		c.drainerState.Record(bg, v, attr(attrState, s))
	}
}

// SinkKind implements LifecycleCounters.
func (c *OTelCounters) SinkKind(kind string) {
	for _, k := range sinkKinds {
		v := int64(0)
		if k == kind {
			v = 1
		}
		c.sinkKind.Record(bg, v, attr(attrKind, k))
	}
}

// AmbiguousTransition implements LifecycleCounters.
func (c *OTelCounters) AmbiguousTransition(kind string) {
	c.ambiguous.Add(bg, 1, attr(attrKind, kind))
}

// UnconfirmedStart implements LifecycleCounters.
func (c *OTelCounters) UnconfirmedStart() { c.unconfirmed.Add(bg, 1) }

// StaleEcho implements LifecycleCounters.
func (c *OTelCounters) StaleEcho(path, reason string) {
	c.staleEcho.Add(bg, 1, metric.WithAttributes(attribute.String(attrPath, path), attribute.String(attrReason, reason)))
}

// AttributionStale implements LifecycleCounters.
func (c *OTelCounters) AttributionStale(reason string) {
	c.attrStale.Add(bg, 1, attr(attrReason, reason))
}
