/*
Copyright 2026 The Scion Authors.
*/

package telemetry

// health.go produces the telemetry.health analytics event (WP3 design note
// telemetry/design/wp3-design.md, section 3): a snapshot of the agent's
// usage-source selection, what that source can report, and the
// fixed-cardinality delivery diagnostics, so a quiet agent can be told
// apart from one whose usage collection was never enabled.
//
// Health events are sent directly with GCPExporter.SendAnalytics (Cloud
// Logging log ID scion-analytics), NOT through the usage outbox: a snapshot
// is superseded by the next one, the final snapshot is taken after the
// outbox drainer stopped, and a health event must never consume a
// producer_seq. Delivery is best effort: an in-memory queue of frozen
// bytes, retried byte for byte, and held until the hub ID is pinned.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

const (
	healthPeriodicInterval = 10 * time.Minute
	healthRetryInterval    = 30 * time.Second
	healthQueueCap         = 8
	// healthFinalBudget bounds the final health send inside Stop, so it
	// cannot take the exporter shutdown's share of the stop budget (see
	// finalHealthBudget).
	healthFinalBudget = 2 * time.Second
	// healthFinalMinBudget is the floor when the stop budget is (nearly)
	// spent: a final event recording a not-drained stop is most useful
	// exactly then, so it may overrun an expired deadline by this much.
	healthFinalMinBudget = 250 * time.Millisecond
)

// UsageSourceSelection is the usage-source decision as the pipeline knows
// it. It is computed from the process environment (which wins when the key
// is present) and the provisioner's env overlay, reported by init through
// Pipeline.SettleUsageSource.
type UsageSourceSelection struct {
	Settled      bool
	Source       string // effective SCION_USAGE_SOURCE value, may be "" or unrecognized
	Origin       string // telemetrycontract.UsageSourceOrigin*
	NativePolicy string // "enabled", "disabled" or "" (overlay marker)
}

// resolveUsageSelection applies the runtime-env precedence rule of
// Pipeline.ActivateUsageSource: a SCION_USAGE_SOURCE key in the process
// environment wins even when empty; otherwise the overlay's value; else
// unset.
func resolveUsageSelection(lookupEnv func(string) (string, bool), overlay map[string]string, nativePolicy string) UsageSourceSelection {
	sel := UsageSourceSelection{Settled: true, NativePolicy: nativePolicy, Origin: telemetrycontract.UsageSourceOriginUnset}
	if v, ok := lookupEnv("SCION_USAGE_SOURCE"); ok {
		sel.Source, sel.Origin = v, telemetrycontract.UsageSourceOriginRuntimeEnv
	} else if v, ok := overlay["SCION_USAGE_SOURCE"]; ok {
		sel.Source, sel.Origin = v, telemetrycontract.UsageSourceOriginOverlay
	}
	return sel
}

// usageCollection is the usage-collection state derived from a selection
// and what the pipeline actually activated.
type usageCollection struct {
	State, Reason, Source string
	Capabilities          []string
	CapabilityStatus      string
	Granularity           string
	PositiveOnly          bool
}

// deriveUsageCollection decides enabled/disabled with a reason.
// nativeActive reports whether a native deriver with a rule is running;
// harnessHasRule whether SCION_HARNESS has a native rule at all.
func deriveUsageCollection(sel UsageSourceSelection, harness string, nativeActive, harnessHasRule bool) usageCollection {
	if !sel.Settled {
		if nativeActive {
			// Start found SCION_USAGE_SOURCE=native in its own environment
			// before init reported; the deriver is the evidence.
			sel = UsageSourceSelection{Settled: true, Source: telemetrycontract.UsageSourceNative, Origin: telemetrycontract.UsageSourceOriginRuntimeEnv}
		} else {
			return usageCollection{State: telemetrycontract.UsageCollectionUndetermined}
		}
	}
	disabled := func(reason string) usageCollection {
		return usageCollection{State: telemetrycontract.UsageCollectionDisabled, Reason: reason}
	}
	nativeDisabled := func(reason string) usageCollection {
		// The selected source is known and safe to report.
		u := disabled(reason)
		u.Source = telemetrycontract.UsageSourceNative
		return u
	}
	// Exact comparison, as ActivateUsageSource and the hook gate do: a
	// padded value is unrecognized.
	switch sel.Source {
	case "":
		return disabled(telemetrycontract.UsageDisabledSourceUnset)
	case telemetrycontract.UsageSourceNative:
		if nativeActive {
			return enabledUsage(telemetrycontract.UsageSourceNative, harness)
		}
		if sel.Origin == telemetrycontract.UsageSourceOriginOverlay && sel.NativePolicy == "disabled" {
			return nativeDisabled(telemetrycontract.UsageDisabledNativePolicy)
		}
		if !harnessHasRule {
			return nativeDisabled(telemetrycontract.UsageDisabledNoHarnessRule)
		}
		return nativeDisabled(telemetrycontract.UsageDisabledDeriverFailed)
	case telemetrycontract.UsageSourceHooks:
		return enabledUsage(telemetrycontract.UsageSourceHooks, harness)
	default:
		return disabled(telemetrycontract.UsageDisabledSourceUnrecognized)
	}
}

// usageCapabilityTable is the static capability table (design section
// 3.4), from adapter evidence at this revision: what the adapter CAN
// report, not what a given record did.
var usageCapabilityTable = map[string]struct {
	caps        []string
	granularity string
}{
	"native/claude": {[]string{"calls", "input", "output", "cache_read", "cache_write", "reported_cost"}, telemetrycontract.UsageGranularityRequest},
	"native/codex":  {[]string{"calls", "input", "output", "cache_read", "cache_write", "reasoning"}, telemetrycontract.UsageGranularityRequest},
	"native/gemini-cli": {
		[]string{"calls", "input", "output", "cache_read", "reasoning"}, telemetrycontract.UsageGranularityRequest,
	},
	"native/copilot":    {[]string{"calls", "input", "output", "cache_read", "cache_write"}, telemetrycontract.UsageGranularityInterval},
	"hooks/antigravity": {[]string{"calls"}, telemetrycontract.UsageGranularityInvocation},
}

func enabledUsage(source, harness string) usageCollection {
	u := usageCollection{State: telemetrycontract.UsageCollectionEnabled, Source: source}
	if entry, ok := usageCapabilityTable[source+"/"+harness]; ok {
		u.Capabilities = append([]string(nil), entry.caps...)
		u.CapabilityStatus = telemetrycontract.UsageCapabilityStatusStatic
		u.Granularity = entry.granularity
	} else {
		// No per-dialect evidence at this revision: unknown, never guessed.
		u.CapabilityStatus = telemetrycontract.UsageCapabilityStatusUnknown
		u.Granularity = telemetrycontract.UsageGranularityRequest
	}
	if source == telemetrycontract.UsageSourceHooks {
		u.Granularity = telemetrycontract.UsageGranularityInvocation
		// Hook dialects keep only token values > 0 (WP2 section 3).
		u.PositiveOnly = len(u.Capabilities) != 1 || u.Capabilities[0] != "calls"
	}
	return u
}

// healthInputs is everything one health event is built from. Building is a
// pure function of it, so golden tests pin the bytes.
type healthInputs struct {
	Now           time.Time
	PipelineRunID string
	Seq           int64
	Trigger       string

	AgentID, ProjectID, BrokerID, LaunchID, Harness string
	HarnessVersion                                  string

	Usage         usageCollection
	SourceOrigin  string
	DeliveryState string

	UsageDiag *UsageDiagnostics // nil unless the native source is active

	AnalyticsEnabled        bool
	AnalyticsDisabledReason string
	Analytics               telemetrycontract.HealthAnalyticsCounters

	Metrics, Logs DeliverySnapshot
	Queue         QueueDepth

	SendFailures, Superseded, Rejected int64

	FinalResidualRecords int64
}

// healthEventID is the deterministic event ID: h1- plus 32 hex characters
// of sha256 over the length-prefixed namespace, pipeline run ID and seq.
func healthEventID(namespace, runID string, seq int64) string {
	return "h1-" + lengthPrefixedHash(namespace, runID, strconv.FormatInt(seq, 10))[:32]
}

// buildHealthEvent builds the event (without hub_id, which is stamped when
// the event is first sent). It returns an error only when the agent has no
// identity.
func buildHealthEvent(in healthInputs) (*telemetrycontract.HealthEvent, error) {
	if in.AgentID == "" {
		return nil, errNoAgentIdentity
	}
	namespace := usageProducerNamespace(in.ProjectID, in.AgentID)
	env := telemetrycontract.NewEnvelope(telemetrycontract.EventTypeHealth, healthEventID(namespace, in.PipelineRunID, in.Seq), namespace, in.Now, in.Now)
	env.ProjectID, env.AgentID, env.BrokerID, env.LaunchID = in.ProjectID, in.AgentID, in.BrokerID, in.LaunchID
	env.ProducerBuild = version.Short()
	env.HarnessVersion = in.HarnessVersion

	harness := in.Harness
	if harness == "" {
		harness = "unknown"
	}
	origin := in.SourceOrigin
	if origin == "" {
		origin = telemetrycontract.UsageSourceOriginUnset
	}
	ev := &telemetrycontract.HealthEvent{
		Envelope:                env,
		PipelineRunID:           in.PipelineRunID,
		HealthSeq:               in.Seq,
		Trigger:                 in.Trigger,
		Harness:                 harness,
		UsageSource:             in.Usage.Source,
		UsageSourceOrigin:       origin,
		UsageCollection:         in.Usage.State,
		UsageDisabledReason:     in.Usage.Reason,
		UsageCapabilityStatus:   in.Usage.CapabilityStatus,
		UsageCapabilities:       append([]string{}, in.Usage.Capabilities...),
		UsageGranularity:        in.Usage.Granularity,
		UsageTokensPositiveOnly: in.Usage.PositiveOnly,
		UsageRulesVersion:       UsageRuleSchemaVersion,
		DeliveryState:           in.DeliveryState,

		MetricPointsAccepted:    in.Metrics.Accepted,
		MetricPointsRejected:    in.Metrics.Rejected,
		MetricPointsFiltered:    in.Metrics.Filtered,
		MetricPointsDelivered:   in.Metrics.Delivered,
		MetricPointsDropped:     in.Metrics.Dropped,
		MetricPointsUnconfirmed: in.Metrics.Unconfirmed,
		MetricExportAttempts:    in.Metrics.Attempts,
		MetricExportFailures:    in.Metrics.Failed,
		MetricExportRetries:     in.Metrics.Retries,
		LogRecordsAccepted:      in.Logs.Accepted,
		LogRecordsRejected:      in.Logs.Rejected,
		LogRecordsFiltered:      in.Logs.Filtered,
		LogRecordsDelivered:     in.Logs.Delivered,
		LogRecordsDropped:       in.Logs.Dropped,
		LogRecordsUnconfirmed:   in.Logs.Unconfirmed,
		LogExportAttempts:       in.Logs.Attempts,
		LogExportFailures:       in.Logs.Failed,
		LogExportRetries:        in.Logs.Retries,
		QueueBytes:              int64(in.Queue.Bytes),
		QueueRecords:            int64(in.Queue.Records),
		HealthSendFailures:      in.SendFailures,
		HealthSuperseded:        in.Superseded,
		HealthRejected:          in.Rejected,
	}
	if d := in.UsageDiag; d != nil && in.Usage.Source == telemetrycontract.UsageSourceNative {
		ev.SetUsageCounters(telemetrycontract.HealthUsageCounters{
			LogRecordsReceived:     d.LogRecordsReceived,
			LogRecordsRecognized:   d.LogRecordsRecognized,
			LogRecordsMalformed:    d.LogRecordsMalformed,
			MetricBatchesReceived:  d.MetricBatchesReceived,
			MetricBatchesMalformed: d.MetricBatchesMalformed,
			MetricIncrements:       d.MetricIncrements,
			ObservationsIgnored:    d.Ignored,
			ObservationsDuplicate:  d.Duplicate,
			ObservationsDerived:    d.Derived,
			ClampedInput:           d.ClampedInput,
			StaleBackwards:         d.StaleBackwards,
			BaselinedAfterCap:      d.BaselinedAfterCap,
		})
	}
	if in.AnalyticsEnabled {
		ev.AnalyticsState = telemetrycontract.AnalyticsStateEnabled
		ev.SetAnalyticsCounters(in.Analytics)
	} else {
		ev.AnalyticsState = telemetrycontract.AnalyticsStateDisabled
		ev.AnalyticsDisabledReason = in.AnalyticsDisabledReason
	}
	ev.LossIndicated = lossIndicated(in)
	if in.Trigger == telemetrycontract.HealthTriggerFinal {
		residual := in.FinalResidualRecords
		ev.FinalResidualRecords = &residual
		ev.FinalFlushOutcome = telemetrycontract.FinalFlushDrained
		pending := in.AnalyticsEnabled && (in.Analytics.PendingBytes > 0 || in.Analytics.StatsFailed)
		if residual > 0 || in.Queue.Records > 0 || pending {
			ev.FinalFlushOutcome = telemetrycontract.FinalFlushNotDrained
		}
	}
	return ev, nil
}

// lossIndicated reports whether any counter in the snapshot records a
// possible loss (design section 3.2): refused (overflow, oversize) or failed
// analytics appends, torn lines, events without identity, quarantined and
// dead-lettered lines, corrupt intents, invalid (not appended) events, and
// metric/log records rejected,
// dropped or terminal without confirmation. It is evidence to examine, not
// a measured token loss.
func lossIndicated(in healthInputs) bool {
	a := in.Analytics
	if a.StatsFailed {
		// Disk-derived totals are unknown in this snapshot: only measured
		// counters count (stats_failed is a separate unknown-state signal).
		a.OverflowTotal, a.OversizeTotal, a.AppendFailedTotal, a.DeadLettersTotal = 0, 0, 0, 0
	}
	if in.AnalyticsEnabled && (a.OverflowTotal > 0 || a.Overflow > 0 || a.AppendFailed > 0 || a.AppendFailedTotal > 0 ||
		a.OversizeTotal > 0 || a.Torn > 0 || a.DroppedNoIdentity > 0 || a.Quarantined > 0 || a.DeadLetters > 0 || a.DeadLettersTotal > 0 ||
		a.CorruptIntents > 0 || a.Invalid > 0) {
		return true
	}
	for _, s := range []DeliverySnapshot{in.Metrics, in.Logs} {
		if s.Rejected > 0 || s.Dropped > 0 || s.Unconfirmed > 0 {
			return true
		}
	}
	return false
}

// countingSend wraps the analytics send function and counts retries: a
// send of the same bytes as the immediately preceding failed send.
type countingSend struct {
	send AnalyticsSendFunc

	mu         sync.Mutex
	lastFailed [sha256.Size]byte
	hasFailed  bool
	retries    atomic.Int64
}

func newCountingSend(send AnalyticsSendFunc) *countingSend { return &countingSend{send: send} }

// Send implements AnalyticsSendFunc.
func (c *countingSend) Send(ctx context.Context, line []byte) error {
	sum := sha256.Sum256(line)
	c.mu.Lock()
	if c.hasFailed && sum == c.lastFailed {
		c.retries.Add(1)
	}
	c.mu.Unlock()
	err := c.send(ctx, line)
	c.mu.Lock()
	c.hasFailed, c.lastFailed = err != nil, sum
	c.mu.Unlock()
	return err
}

// Retries returns the retry count. Safe on nil.
func (c *countingSend) Retries() int64 {
	if c == nil {
		return 0
	}
	return c.retries.Load()
}

// pendingHealth is a queued health event: the built event until hub_id is
// known, then its frozen bytes.
type pendingHealth struct {
	ev   *telemetrycontract.HealthEvent
	line []byte
}

// healthEmitter owns the health queue and the periodic loop. Its zero value
// is disabled; Pipeline.startHealth enables it.
type healthEmitter struct {
	mu        sync.Mutex
	enabled   bool
	runID     string
	seq       int64
	queue     []*pendingHealth
	started   bool // the start event was enqueued
	selection UsageSourceSelection

	sendFailures, superseded, rejected int64

	send  AnalyticsSendFunc
	hubID func(context.Context) string
	now   func() time.Time

	// Test overrides, applied on every start.
	sendOverride  AnalyticsSendFunc
	hubIDOverride func(context.Context) string

	periodic, retry time.Duration
	cancel          context.CancelFunc
	done            chan struct{}
	kick            chan struct{}

	// finalUsage is the native deriver's last diagnostics, captured in Stop
	// before the deriver is released.
	finalUsage       *UsageDiagnostics
	finalNative      bool
	finalResidual    int64
	analyticsHandles analyticsHandles
}

// analyticsHandles are the analytics objects the health snapshot reads,
// captured once at start so later lifecycle changes cannot race them.
type analyticsHandles struct {
	native         *UsageAnalytics
	drainer        *AnalyticsDrainer
	send           *countingSend
	disabledReason string
}

// outboxDisk is the outbox's on-disk state read for one snapshot. Each
// value is read exactly once, and failed is set if ANY read failed: then
// none of these values is a measurement (design 1.3, analytics_stats_failed).
type outboxDisk struct {
	epoch                               string
	seq, overflowTotal, oversizeTotal   int64
	appendFailedTotal, deadLettersTotal int64
	pending, retained                   int64
	oldest                              time.Time
	failed                              bool
}

// readOutboxDisk reads the persisted ledger, totals and pending figures,
// keeping every error (Stats discards them as zeros).
func readOutboxDisk(o *AnalyticsOutbox) outboxDisk {
	var d outboxDisk
	if o == nil {
		d.failed = true
		return d
	}
	var errs [8]error
	d.epoch, errs[0] = o.readTrimmed("epoch")
	if errs[0] == nil && d.epoch == "" {
		errs[0] = errors.New("outbox epoch is missing") // created when the outbox opens
	}
	d.seq, errs[1] = o.readInt("seq")
	d.overflowTotal, errs[2] = o.readInt("overflow")
	d.oversizeTotal, errs[3] = o.readInt("oversize")
	d.deadLettersTotal, errs[4] = o.readInt("dead_letters")
	if info, err := os.Stat(o.path("append_failed")); err == nil {
		d.appendFailedTotal = info.Size() // WP2: one byte per failure, lock-free
	} else if !errors.Is(err, os.ErrNotExist) {
		errs[5] = err
	}
	d.pending, d.oldest, errs[6] = o.pendingBytes()
	d.retained, errs[7] = o.retainedBytes()
	for _, err := range errs {
		if err != nil {
			d.failed = true
		}
	}
	return d
}

func (h analyticsHandles) counters() telemetrycontract.HealthAnalyticsCounters {
	outbox := h.native.Outbox()
	s := outbox.Stats() // in-memory counters only; disk values come from readOutboxDisk
	d := readOutboxDisk(outbox)
	c := telemetrycontract.HealthAnalyticsCounters{
		Appended:          s.Appended,
		CertainDuplicates: s.CertainDuplicates,
		Overflow:          s.Overflow,
		OverflowTotal:     d.overflowTotal,
		AppendFailed:      s.AppendFailed,
		DroppedNoIdentity: h.native.DroppedNoIdentity(),
		Finalized:         s.Finalized,
		Delivered:         s.Delivered,
		SendFailures:      s.SendFailures,
		SendRetries:       h.send.Retries(),
		Torn:              s.Torn,
		HubIDMismatch:     s.HubIDMismatch,
		Conflicts:         s.Conflicts,
		Quarantined:       s.Quarantined,
		DeadLetters:       s.DeadLetters,
		OversizeTotal:     d.oversizeTotal,
		AppendFailedTotal: d.appendFailedTotal,
		DeadLettersTotal:  d.deadLettersTotal,
		SyncUncertain:     s.SyncUncertain,
		CorruptIntents:    s.CorruptIntents,
		Invalid:           h.native.Invalid(),
		RetainedBytes:     d.retained,
		PendingBytes:      d.pending,
		ProducerEpoch:     d.epoch,
		ProducerSeqLast:   d.seq,
		StatsFailed:       d.failed,
	}
	if !d.failed && d.pending > 0 && !d.oldest.IsZero() {
		age := time.Since(d.oldest).Milliseconds()
		if age < 0 {
			age = 0
		}
		c.OldestPendingFileAgeMs = &age
	}
	if h.drainer != nil {
		c.RetryBackoffMs = h.drainer.Backoff().Milliseconds()
	}
	return c
}

// SettleUsageSource records the usage-source decision once init has loaded
// the provisioner's env overlay (overlay may be nil), and emits the start
// health event if health is enabled. Only SCION_USAGE_SOURCE is read from
// overlay. Safe on nil and before Start.
func (p *Pipeline) SettleUsageSource(overlay map[string]string, nativePolicy string) {
	if p == nil {
		return
	}
	sel := resolveUsageSelection(os.LookupEnv, overlay, nativePolicy)
	h := &p.health
	h.mu.Lock()
	h.selection = sel
	enabled, kick := h.enabled, h.kick
	h.mu.Unlock()
	if enabled {
		p.emitHealth(telemetrycontract.HealthTriggerStart)
		// The loop sends it; init never waits on Cloud Logging.
		select {
		case kick <- struct{}{}:
		default:
		}
	}
}

// startHealth enables health events when the GCP exporter is active (gcp
// may be nil: then only the analytics handles are recorded for local
// diagnostics). Called from Start with p.mu held, after the analytics
// state and usage deriver are set up.
func (p *Pipeline) startHealth(ctx context.Context, gcp *GCPExporter, handles analyticsHandles) {
	h := &p.health
	h.mu.Lock()
	h.analyticsHandles = handles
	if gcp == nil {
		// No GCP exporter: no health events, local diagnostics only.
		h.mu.Unlock()
		return
	}
	h.enabled = true
	h.runID = telemetrycontract.NewEventID()
	h.seq = 0
	h.queue = nil
	h.started = false
	h.sendFailures, h.superseded, h.rejected = 0, 0, 0
	h.finalUsage, h.finalNative, h.finalResidual = nil, false, 0
	h.send = gcp.SendAnalytics
	if h.sendOverride != nil {
		h.send = h.sendOverride
	}
	if outbox := handles.native.Outbox(); outbox != nil {
		h.hubID = func(context.Context) string { return outbox.PinnedHubID() }
	} else {
		// No outbox (analytics disabled): resolve the hub ID the same
		// way the drainer does, cached in memory.
		h.hubID = cachedHubID(NewHealthzHubIDResolver(analyticsHubEndpoint(), nil))
	}
	if h.hubIDOverride != nil {
		h.hubID = h.hubIDOverride
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.periodic == 0 {
		h.periodic = healthPeriodicInterval
	}
	if h.retry == 0 {
		h.retry = healthRetryInterval
	}
	_, runtimeKey := os.LookupEnv("SCION_USAGE_SOURCE")
	if runtimeKey && !h.selection.Settled {
		// The process environment already decides; init's report cannot
		// change it.
		h.selection = resolveUsageSelection(os.LookupEnv, nil, "")
	}
	settled := h.selection.Settled
	loopCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.done = make(chan struct{})
	h.kick = make(chan struct{}, 1)
	periodic, retry, done, kick := h.periodic, h.retry, h.done, h.kick
	h.mu.Unlock()

	if settled {
		p.emitHealth(telemetrycontract.HealthTriggerStart)
	}
	go p.healthLoop(loopCtx, periodic, retry, done, kick)
}

func (p *Pipeline) healthLoop(ctx context.Context, periodic, retry time.Duration, done chan struct{}, kick <-chan struct{}) {
	defer close(done)
	periodicTicker := time.NewTicker(periodic)
	defer periodicTicker.Stop()
	retryTicker := time.NewTicker(retry)
	defer retryTicker.Stop()
	p.flushHealth(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-periodicTicker.C:
			p.emitHealth(telemetrycontract.HealthTriggerPeriodic)
			p.flushHealth(ctx)
		case <-retryTicker.C:
			p.flushHealth(ctx)
		case <-kick:
			p.flushHealth(ctx)
		}
	}
}

// stopHealthLoop ends the periodic loop (Stop, before the analytics stop).
func (p *Pipeline) stopHealthLoop() {
	h := &p.health
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// captureFinalUsage keeps the native deriver's diagnostics for the final
// snapshot; Stop releases the deriver before the final event is built.
func (p *Pipeline) captureFinalUsage(d *UsageDeriver) {
	diag := d.Diagnostics()
	h := &p.health
	h.mu.Lock()
	h.finalUsage = &diag
	h.finalNative = d != nil && len(d.rules) > 0
	h.mu.Unlock()
}

// recordFinalResidual records the metric records still unconfirmed when the
// final metric flush gave up.
func (p *Pipeline) recordFinalResidual(records int) {
	h := &p.health
	h.mu.Lock()
	h.finalResidual = int64(records)
	h.mu.Unlock()
}

// emitFinalHealth enqueues the final snapshot and makes one bounded
// delivery attempt (final first). Called in Stop after the final analytics
// drain and before the exporter shuts down.
func (p *Pipeline) emitFinalHealth(ctx context.Context) {
	h := &p.health
	h.mu.Lock()
	enabled := h.enabled
	h.mu.Unlock()
	if !enabled {
		return
	}
	p.stopHealthLoop()
	p.emitHealth(telemetrycontract.HealthTriggerFinal)
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalHealthBudget(ctx, time.Now()))
	defer cancel()
	h.mu.Lock()
	// Final first: it carries the final-flush outcome.
	if n := len(h.queue); n > 1 {
		last := h.queue[n-1]
		copy(h.queue[1:], h.queue[:n-1])
		h.queue[0] = last
	}
	h.mu.Unlock()
	p.flushHealth(finalCtx)
	h.mu.Lock()
	if left := len(h.queue); left > 0 {
		log.Error("Telemetry health: %d health events not delivered at stop", left)
	}
	h.enabled = false
	h.queue = nil
	h.mu.Unlock()
}

// healthSnapshotInputs gathers the current inputs for trigger.
func (p *Pipeline) healthSnapshotInputs(trigger string) healthInputs {
	h := &p.health
	h.mu.Lock()
	h.seq++
	in := healthInputs{
		Now:                  h.now(),
		PipelineRunID:        h.runID,
		Seq:                  h.seq,
		Trigger:              trigger,
		SendFailures:         h.sendFailures,
		Superseded:           h.superseded,
		Rejected:             h.rejected,
		FinalResidualRecords: h.finalResidual,
	}
	sel := h.selection
	finalUsage, finalNative := h.finalUsage, h.finalNative
	handles := h.analyticsHandles
	h.mu.Unlock()

	in.AgentID = strings.TrimSpace(os.Getenv("SCION_AGENT_ID"))
	in.ProjectID = projectkeys.ProjectIDFromEnv(os.Getenv)
	in.BrokerID = os.Getenv("SCION_BROKER_ID")
	in.LaunchID = os.Getenv("SCION_LAUNCH_ID")
	in.Harness = os.Getenv("SCION_HARNESS")

	var usage UsageDiagnostics
	nativeActive := false
	if finalUsage != nil {
		usage, nativeActive = *finalUsage, finalNative
	} else if d := p.usageDeriver.Load(); d != nil {
		usage, nativeActive = d.Diagnostics(), len(d.rules) > 0
	}
	in.Usage = deriveUsageCollection(sel, in.Harness, nativeActive, len(rulesForHarness(in.Harness)) > 0)
	in.SourceOrigin = sel.Origin
	if !sel.Settled && nativeActive {
		in.SourceOrigin = telemetrycontract.UsageSourceOriginRuntimeEnv
	}
	if nativeActive {
		in.UsageDiag = &usage
		in.HarnessVersion = usage.HarnessVersion
	}
	in.DeliveryState = p.DeliveryState()
	if handles.native != nil {
		in.AnalyticsEnabled = true
		in.Analytics = handles.counters()
	} else {
		in.AnalyticsDisabledReason = handles.disabledReason
	}
	d := p.Diagnostics()
	in.Metrics, in.Logs = d["metrics"], d["logs"]
	in.Queue = p.QueueDepth()
	return in
}

// emitHealth builds one snapshot, logs it locally and queues it.
func (p *Pipeline) emitHealth(trigger string) {
	h := &p.health
	h.mu.Lock()
	if !h.enabled || (trigger == telemetrycontract.HealthTriggerStart && h.started) {
		h.mu.Unlock()
		return
	}
	if trigger == telemetrycontract.HealthTriggerStart {
		h.started = true
	}
	h.mu.Unlock()

	ev, err := buildHealthEvent(p.healthSnapshotInputs(trigger))
	if err != nil {
		log.Info("Telemetry health not emitted: %v", err)
		return
	}
	if body, err := json.Marshal(ev); err == nil {
		log.Info("Telemetry health %s", body)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.enabled {
		return // the final flush disabled the emitter meanwhile
	}
	if len(h.queue) >= healthQueueCap {
		// Drop the oldest queued periodic snapshot; a newer one supersedes
		// it. Start and final are kept.
		for i, q := range h.queue {
			if q.ev.Trigger == telemetrycontract.HealthTriggerPeriodic {
				h.queue = append(h.queue[:i], h.queue[i+1:]...)
				h.superseded++
				log.Info("Telemetry health: superseded an undelivered periodic snapshot")
				break
			}
		}
		if len(h.queue) >= healthQueueCap {
			h.superseded++
			return
		}
	}
	h.queue = append(h.queue, &pendingHealth{ev: ev})
}

// flushHealth sends queued events in order, freezing each event's bytes
// (hub_id stamped) on its first send attempt. It stops at the first
// failure, or when the hub ID is not yet pinned.
func (p *Pipeline) flushHealth(ctx context.Context) {
	h := &p.health
	for {
		h.mu.Lock()
		if len(h.queue) == 0 || ctx.Err() != nil {
			h.mu.Unlock()
			return
		}
		item := h.queue[0]
		if item.line == nil {
			hubID := h.hubID
			h.mu.Unlock()
			hub := ""
			if hubID != nil {
				hub = hubID(ctx)
			}
			if hub == "" {
				return
			}
			h.mu.Lock()
			if len(h.queue) == 0 || h.queue[0] != item {
				h.mu.Unlock()
				continue
			}
			item.ev.HubID = hub
			line, err := telemetrycontract.Marshal(item.ev)
			if err != nil {
				h.queue = h.queue[1:]
				h.mu.Unlock()
				log.Error("Telemetry health event invalid, dropped: %v", err)
				continue
			}
			item.line = line
		}
		send := h.send
		h.mu.Unlock()

		if err := send(ctx, item.line); err != nil {
			h.mu.Lock()
			h.sendFailures++
			if IsPermanentSendError(err) {
				// The destination rejects these bytes for good: drop the
				// event so it cannot block the queue, count it, log once.
				if len(h.queue) > 0 && h.queue[0] == item {
					h.queue = h.queue[1:]
				}
				h.rejected++
				h.mu.Unlock()
				log.Error("Telemetry health event %s permanently rejected, dropped: %v", item.ev.EventID, err)
				continue
			}
			h.mu.Unlock()
			log.Debug("Telemetry health send failed: %v", err)
			return
		}
		h.mu.Lock()
		if len(h.queue) > 0 && h.queue[0] == item {
			h.queue = h.queue[1:]
		}
		h.mu.Unlock()
	}
}

// AnalyticsDiagnostics returns the analytics outbox counters, or ok=false
// when analytics is disabled (with the reason).
func (p *Pipeline) AnalyticsDiagnostics() (counters telemetrycontract.HealthAnalyticsCounters, ok bool, disabledReason string) {
	if p == nil {
		return counters, false, ""
	}
	h := &p.health
	h.mu.Lock()
	handles := h.analyticsHandles
	h.mu.Unlock()
	if handles.native == nil {
		return counters, false, handles.disabledReason
	}
	return handles.counters(), true, ""
}

// metricRejectReasons returns up to limit metric reject reasons with
// counts, most frequent first (local log only; reasons may name metrics).
func (p *Pipeline) metricRejectReasons(limit int) string {
	p.metricStateMu.Lock()
	var reasons map[string]uint64
	if p.metricStreams != nil {
		reasons = make(map[string]uint64, len(p.metricStreams.rejected))
		for k, v := range p.metricStreams.rejected {
			reasons[k] = v
		}
	}
	p.metricStateMu.Unlock()
	type kv struct {
		k string
		v uint64
	}
	list := make([]kv, 0, len(reasons))
	for k, v := range reasons {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].k < list[j].k
	})
	if len(list) > limit {
		list = list[:limit]
	}
	parts := make([]string, len(list))
	for i, e := range list {
		parts[i] = fmt.Sprintf("%q:%d", e.k, e.v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// handles returns the analytics objects the health snapshot reads.
func (a *analyticsState) handles() analyticsHandles {
	return analyticsHandles{native: a.native, drainer: a.drainer, send: a.send, disabledReason: a.disabledReason}
}

// cachedHubID memoizes the first successful resolution. Each attempt is
// bounded by the caller's ctx (Stop's loop cancellation, the final-send
// budget) and by analyticsHubIDTimeout, whichever ends first. Concurrent
// callers do not wait for an attempt in flight; they report "" (not yet
// known) instead.
func cachedHubID(resolve HubIDResolver) func(context.Context) string {
	var mu sync.Mutex
	var id string
	var inFlight bool
	return func(ctx context.Context) string {
		mu.Lock()
		if id != "" || inFlight {
			v := id
			mu.Unlock()
			return v
		}
		inFlight = true
		mu.Unlock()
		rctx, cancel := context.WithTimeout(ctx, analyticsHubIDTimeout)
		v, err := resolve(rctx)
		cancel()
		mu.Lock()
		defer mu.Unlock()
		inFlight = false
		if err == nil && id == "" {
			id = v
		}
		return id
	}
}

// finalHealthBudget is min(healthFinalBudget, a quarter of the time left
// before ctx's deadline), never below healthFinalMinBudget. The final send
// runs detached from ctx's cancellation, bounded by this budget.
func finalHealthBudget(ctx context.Context, now time.Time) time.Duration {
	budget := healthFinalBudget
	if deadline, ok := ctx.Deadline(); ok {
		if share := deadline.Sub(now) / 4; share < budget {
			budget = share
		}
	}
	if budget < healthFinalMinBudget {
		budget = healthFinalMinBudget
	}
	return budget
}
