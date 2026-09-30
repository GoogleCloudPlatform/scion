/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

// This file implements design §3.3's batch-correlation contract, applied to
// Copilot's metric-sourced usage rule (design §5 copilot row): a real
// capture showed two things a simple per-metric mapping cannot handle.
//
//  1. Every Copilot metric point is AGGREGATION_TEMPORALITY_CUMULATIVE,
//     always -- `copilot help monitoring` documents no temporality knob, and
//     setting OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta (which
//     harnesses/copilot/provision.py still sets, in case a future Copilot
//     release honors it) makes no observed difference. Treating a cumulative
//     point as malformed would mean Copilot usage is silently always zero in
//     production; treating the raw cumulative value as if it were already a
//     delta would double- (or worse-) count on every periodic re-export of a
//     long session's running total. cumulativeToDeltaConverter below
//     recovers the real per-export delta.
//  2. gen_ai.client.inference.usage.input_tokens is not exclusive of cached
//     tokens: per-call trace evidence shows input − cache_read − cache_write
//     equals a small constant (the call's genuinely new prompt tokens) on
//     every call. Since design §3.2's canonical "input" is non-cached, this
//     needs cross-metric correlation within one export batch -- something a
//     single metric on its own cannot express. canonicalizeTokenGroups below
//     does this correlation, matched by stream identity minus the
//     token-type dimension (the metric name), per design §3.3's
//     batch-correlation contract.

// copilotCumulativeStateCapacity bounds cumulativeToDeltaConverter's
// per-stream state (a named cap, per the design decision). Eviction is
// least-recently-updated (an entry moves to the back of the order on every
// touch, so an idle stream is evicted before an active one), and a returning
// evicted stream is always baselined rather than trusted -- see
// cumulativeToDeltaConverter's doc comment for how maxEvictedStart makes
// that precise instead of latching permanently for every future stream.
const copilotCumulativeStateCapacity = 4096

// cumulativeEntry is what cumulativeToDeltaConverter remembers about one
// metric stream between exports.
type cumulativeEntry struct {
	key       string
	last      int64
	startTime uint64
}

// cumulativeToDeltaConverter turns a cumulative running total into a
// per-export delta. A "stream" is identified by resource identity + scope +
// metric name + point attributes + the point's own start_time_unix_nano:
// folding start_time into the identity (rather than treating it purely as a
// tie-break) gets two requirements for free —
//
//   - two concurrent Copilot processes that happen to share every other
//     attribute (for example two subagents on the same model) get different
//     start times from their own independent OTel SDK instances, so their
//     counters never collide;
//   - a process restart, which resets Copilot's own in-process counters to
//     zero under a new start_time, is automatically a new stream rather
//     than a backwards jump in an existing one.
//
// startedAt bounds how far back a genuine "first sighting" can be trusted:
// a stream whose start_time predates the converter's own construction may
// have already had part of its history counted by a prior sciontool
// process, so its first sighting here is baselined (delta 0) instead of
// re-emitted in full.
//
// A value that goes backwards without the stream identity changing (which it
// cannot, for start_time itself, since that is part of the identity) is
// stale, not a reset: an older export delivered after a newer one, for
// example on a retry or reordering. A stale point never produces a delta and
// never regresses the remembered high-water mark -- the next, larger export
// still diffs correctly against it. Only a genuinely new start_time is ever
// treated as this stream starting over.
type cumulativeToDeltaConverter struct {
	mu        sync.Mutex
	startedAt uint64
	capacity  int
	state     map[string]*list.Element // -> *cumulativeEntry
	order     *list.List               // front = least recently updated

	// maxEvictedStart is the largest start_time ever evicted from state.
	// A first sighting is trusted only if its start_time is both at or after
	// startedAt AND strictly greater than maxEvictedStart: the latter is
	// what lets a genuinely new stream (a fresh Copilot process, started
	// after the last eviction) stay trusted even once the map has filled up
	// at least once, while a stream that is itself returning after eviction
	// -- whose start_time can never exceed the value recorded for it when
	// it was evicted -- is still always baselined. Known, accepted
	// imprecision: a still-running, long-lived process's OWN new stream (for
	// example a new model it starts using mid-session) is also baselined,
	// losing its first interval, if that stream's start_time happens not to
	// exceed maxEvictedStart -- the check cannot distinguish "belongs to an
	// evicted process" from "belongs to a still-live one" by start_time
	// alone. This only becomes reachable after copilotCumulativeStateCapacity
	// distinct streams have been seen, is conservative (undercounts, never
	// over-counts), and is counted in baselinedAfterCap.
	maxEvictedStart uint64

	staleBackwards    atomic.Int64
	staleWarnOnce     sync.Once
	baselinedAfterCap atomic.Int64
}

func newCumulativeToDeltaConverter(startedAtUnixNano uint64, capacity int) *cumulativeToDeltaConverter {
	return &cumulativeToDeltaConverter{
		startedAt: startedAtUnixNano,
		capacity:  capacity,
		state:     make(map[string]*list.Element),
		order:     list.New(),
	}
}

// withLock runs fn once while holding the converter's lock for fn's entire
// duration. DeriveBatch calls every convertLocked for one ResourceMetrics
// inside a single withLock, so the whole batch is atomic with respect to
// every other batch touching the same converter -- see DeriveBatch's doc
// comment for why that atomicity is required. This is production code's
// only locking entry point; converter-level unit tests that exercise one
// conversion in isolation use their own convertForTest test helper instead
// (usage_copilot_test.go), which wraps withLock plus convertLocked the same
// way DeriveBatch does.
func (c *cumulativeToDeltaConverter) withLock(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn()
}

// convertLocked converts one data point's cumulative value to a delta. The
// caller must already hold c.mu (DeriveBatch, via withLock). A DELTA-
// temporal point already is the delta sciontool wants: it is returned
// unchanged without ever touching or growing the state map, which is what
// keeps this converter a no-op for any point that isn't cumulative.
func (c *cumulativeToDeltaConverter) convertLocked(streamKey string, startTimeUnixNano uint64, current int64, temporality metricpb.AggregationTemporality) int64 {
	if temporality != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		return current
	}

	if el, ok := c.state[streamKey]; ok {
		c.order.MoveToBack(el)
		entry := el.Value.(*cumulativeEntry)
		delta := current - entry.last
		if delta < 0 {
			// Stale: an older export delivered after a newer one, for the
			// same stream identity (a genuine reset instead arrives under a
			// new start_time, which is a different key entirely -- see the
			// type doc comment). Never emit a negative delta. entry.last is
			// deliberately left unwritten here (not set to current): delta<0
			// means current < entry.last already, so the remembered
			// high-water mark is already the larger of the two, and a later,
			// larger export still diffs correctly against it.
			c.staleBackwards.Add(1)
			c.staleWarnOnce.Do(func() {
				slog.Warn("usage deriver saw a copilot metric go backwards for the same stream identity; treated as a stale or reordered export, not a reset")
			})
			return 0
		}
		entry.last = current
		return delta
	}

	beforeStart := startTimeUnixNano < c.startedAt
	afterCap := !beforeStart && startTimeUnixNano <= c.maxEvictedStart
	trust := !beforeStart && !afterCap
	c.insert(streamKey, current, startTimeUnixNano)
	if trust {
		return current
	}
	if afterCap {
		c.baselinedAfterCap.Add(1)
	}
	return 0
}

// insert records streamKey's current value at the back of the eviction
// order (most recently updated) and evicts the front (least recently
// updated) entry if the map is now over capacity, folding its start_time
// into maxEvictedStart -- see the type doc comment for why.
func (c *cumulativeToDeltaConverter) insert(streamKey string, current int64, startTimeUnixNano uint64) {
	el := c.order.PushBack(&cumulativeEntry{key: streamKey, last: current, startTime: startTimeUnixNano})
	c.state[streamKey] = el
	if c.order.Len() <= c.capacity {
		return
	}
	oldest := c.order.Front()
	c.order.Remove(oldest)
	evicted := oldest.Value.(*cumulativeEntry)
	delete(c.state, evicted.key)
	if evicted.startTime > c.maxEvictedStart {
		c.maxEvictedStart = evicted.startTime
	}
}

// copilotStreamKey builds a stable identity for one metric stream: resource
// attributes, scope, metric name, point attributes and start time. This is
// used for the cumulative-to-delta converter's per-metric state (start_time
// in the key is what separates two concurrent Copilot processes that
// otherwise share every attribute).
func copilotStreamKey(resourceAttrs []*commonpb.KeyValue, scopeName, metricName string, pointAttrs []*commonpb.KeyValue, startTimeUnixNano uint64) (string, error) {
	r, err := canonicalAttrs(resourceAttrs)
	if err != nil {
		return "", fmt.Errorf("resource attributes: %w", err)
	}
	p, err := canonicalAttrs(pointAttrs)
	if err != nil {
		return "", fmt.Errorf("point attributes: %w", err)
	}
	var startBuf [8]byte
	binary.BigEndian.PutUint64(startBuf[:], startTimeUnixNano)
	return strings.Join([]string{r, scopeName, metricName, p, string(startBuf[:])}, "\x00"), nil
}

// copilotCorrelationKey builds the coarser key design §3.3's
// batch-correlation contract calls for: "stream identity minus the
// token-type dimension, where the token type is the metric name" -- used to
// group input/cache_read/cache_write's post-conversion deltas back together
// as one call's tokens.
//
// Unlike copilotStreamKey, this deliberately excludes start_time. A real
// capture shows Copilot's sibling token counters for the very same call do
// NOT share one start_time_unix_nano: each counter is an independently
// created OTel instrument, and their start times differ by a few thousand
// nanoseconds from each other even though they describe the same call.
// Keying correlation on start_time as well would put every sibling in its
// own group of one and the cache subtraction would never fire.
//
// This is only safe because correlation is scoped to a single
// *metricpb.ResourceMetrics as delivered in ONE OTLP export request, never
// merged across requests -- and the capture shows the resource itself
// carries only service.name/service.version, so two concurrent Copilot
// processes are otherwise indistinguishable by resource attributes alone.
// Verified nothing upstream of DeriveBatch batches or merges separate
// requests before this rule sees them: Receiver.handleHTTPMetrics reads
// exactly one HTTP request body, unmarshals it into one
// ExportMetricsServiceRequest and invokes the metric handler once with that
// request's own ResourceMetrics -- no buffering or merging across separate
// HTTP requests; metricsServiceServer.Export does the same for the gRPC
// path, once per RPC; Pipeline.handleMetrics passes that exact slice
// straight into UsageDeriver.ProcessResourceMetrics, which calls DeriveBatch
// once per ResourceMetrics in it. Two concurrent processes therefore always
// arrive as two separate calls to this function, never interleaved into one
// -- see TestCopilotUsageRuleDeriveBatchTwoExportRequestsNeverCorrelate.
func copilotCorrelationKey(resourceAttrs []*commonpb.KeyValue, scopeName string, pointAttrs []*commonpb.KeyValue) (string, error) {
	r, err := canonicalAttrs(resourceAttrs)
	if err != nil {
		return "", fmt.Errorf("resource attributes: %w", err)
	}
	p, err := canonicalAttrs(pointAttrs)
	if err != nil {
		return "", fmt.Errorf("point attributes: %w", err)
	}
	return strings.Join([]string{r, scopeName, p}, "\x00"), nil
}

// metricBatchDeriver is implemented by a usageRule that must correlate
// several sibling metrics within one export batch to produce a canonical
// increment (design §3.3's batch-correlation contract). Only
// copilotUsageRule needs this today.
type metricBatchDeriver interface {
	// DeriveBatch inspects one ResourceMetrics (one export batch's worth of
	// metrics sharing a resource) and returns every increment it can derive,
	// plus the set of metric names it recognized (independent of whether an
	// increment was actually produced for them -- a cumulative-but-baselined
	// or all-zero-delta point is still "matched" for the caller's GCP
	// consume-semantics purposes). A non-nil error means at least one data
	// point was malformed and its contribution was dropped; other,
	// well-formed points still produce increments.
	DeriveBatch(rm *metricpb.ResourceMetrics) (increments []usageIncrement, matchedNames map[string]bool, err error)
}

// copilotTokenGroup accumulates one call's worth of post-conversion token
// deltas, keyed by canonical token_type, plus enough context to emit the
// resulting increment.
type copilotTokenGroup struct {
	deltas       map[string]int64
	model        string
	timeUnixNano uint64
}

// DeriveBatch implements metricBatchDeriver for copilotUsageRule (design §5
// copilot row and its batch-correlation contract). See the file doc comment
// for why a single metric at a time cannot express this rule's mapping.
//
// Every conversion in this batch runs inside one r.converter.withLock call,
// so the whole batch is atomic with respect to any other concurrent
// DeriveBatch call on the same rule (the receiver's HTTP and gRPC handlers
// run concurrently, so two exports for the same stream -- for example a
// client retry that overlaps its own still-in-flight original request --
// can and do arrive as overlapping DeriveBatch calls). A per-point lock is
// not enough: two batches for the same stream could then interleave at the
// per-metric level, so one batch's input delta gets correlated with the
// other batch's cache_read delta instead of its own, silently over-reporting
// canonical input. Correlation itself (canonicalizeTokenGroups) runs after
// the lock is released: it only touches this call's own local groups map,
// never shared converter state, so holding the lock for it would just
// needlessly extend the critical section.
func (r *copilotUsageRule) DeriveBatch(rm *metricpb.ResourceMetrics) ([]usageIncrement, map[string]bool, error) {
	if rm == nil {
		return nil, nil, nil
	}
	resourceAttrs := rm.GetResource().GetAttributes()
	matched := make(map[string]bool)
	groups := make(map[string]*copilotTokenGroup)
	var groupOrder []string
	var increments []usageIncrement
	var malformed error
	markMalformed := func(err error) {
		if malformed == nil {
			malformed = err
		}
	}

	r.converter.withLock(func() {
		for _, sm := range rm.ScopeMetrics {
			if sm == nil {
				continue
			}
			scopeName := sm.GetScope().GetName()
			for _, m := range sm.Metrics {
				switch {
				case m == nil:
					continue
				case m.Name == copilotCallsMetric:
					matched[m.Name] = true
					increments = append(increments, r.deriveCallsIncrementsLocked(scopeName, resourceAttrs, m, &malformed)...)
				default:
					if tokenType, ok := copilotTokenCounters[m.Name]; ok {
						matched[m.Name] = true
						r.deriveTokenDeltasLocked(scopeName, resourceAttrs, m, tokenType, groups, &groupOrder, markMalformed)
					}
				}
			}
		}
	})

	increments = append(increments, r.canonicalizeTokenGroups(groups, groupOrder)...)
	return increments, matched, malformed
}

// deriveCallsIncrementsLocked converts the calls histogram's per-point delta
// observation count into Calls-only increments (design §5: calls come from
// this histogram's count, converted through the same cumulative-to-delta
// path as the token counters). Requires r.converter's lock to already be
// held (see DeriveBatch's doc comment); only DeriveBatch calls this.
func (r *copilotUsageRule) deriveCallsIncrementsLocked(scopeName string, resourceAttrs []*commonpb.KeyValue, m *metricpb.Metric, malformed *error) []usageIncrement {
	hist := m.GetHistogram()
	if hist == nil {
		if *malformed == nil {
			*malformed = fmt.Errorf("copilot %s: not a histogram", m.Name)
		}
		return nil
	}
	var increments []usageIncrement
	for _, point := range hist.DataPoints {
		if point == nil {
			continue
		}
		if point.Count > math.MaxInt64 {
			if *malformed == nil {
				*malformed = fmt.Errorf("copilot %s: count %d overflows int64", m.Name, point.Count)
			}
			continue
		}
		streamKey, err := copilotStreamKey(resourceAttrs, scopeName, m.Name, point.Attributes, point.StartTimeUnixNano)
		if err != nil {
			if *malformed == nil {
				*malformed = fmt.Errorf("copilot %s: %w", m.Name, err)
			}
			continue
		}
		delta := r.converter.convertLocked(streamKey, point.StartTimeUnixNano, int64(point.Count), hist.AggregationTemporality)
		if delta <= 0 {
			continue
		}
		increments = append(increments, usageIncrement{
			Model:     copilotPointModel(point.Attributes, resourceAttrs),
			Status:    telemetrycontract.StatusSuccess,
			Calls:     delta,
			dedupeKey: "copilot_calls\x00" + streamKey + "\x00" + strconv.FormatUint(point.TimeUnixNano, 10),
		})
	}
	return increments
}

// deriveTokenDeltasLocked converts one token counter's per-point delta and
// accumulates it into the correlation group for its call (identified by the
// point's stream identity minus the metric-name/token-type dimension).
// Grouping, rather than emitting immediately, is what lets
// canonicalizeTokenGroups later compute input − cache_read − cache_write
// once every sibling in the batch has been seen. Requires r.converter's
// lock to already be held (see DeriveBatch's doc comment); only DeriveBatch
// calls this.
func (r *copilotUsageRule) deriveTokenDeltasLocked(scopeName string, resourceAttrs []*commonpb.KeyValue, m *metricpb.Metric, tokenType string, groups map[string]*copilotTokenGroup, groupOrder *[]string, markMalformed func(error)) {
	sum := m.GetSum()
	if sum == nil {
		markMalformed(fmt.Errorf("copilot %s: not a sum", m.Name))
		return
	}
	for _, point := range sum.DataPoints {
		if point == nil {
			continue
		}
		n, err := metricPointInt64(point)
		if err != nil {
			markMalformed(fmt.Errorf("copilot %s: %w", m.Name, err))
			continue
		}
		streamKey, err := copilotStreamKey(resourceAttrs, scopeName, m.Name, point.Attributes, point.StartTimeUnixNano)
		if err != nil {
			markMalformed(fmt.Errorf("copilot %s: %w", m.Name, err))
			continue
		}
		delta := r.converter.convertLocked(streamKey, point.StartTimeUnixNano, n, sum.AggregationTemporality)
		if delta == 0 {
			continue
		}
		corrKey, err := copilotCorrelationKey(resourceAttrs, scopeName, point.Attributes)
		if err != nil {
			markMalformed(fmt.Errorf("copilot %s: %w", m.Name, err))
			continue
		}
		g, ok := groups[corrKey]
		if !ok {
			g = &copilotTokenGroup{
				deltas:       make(map[string]int64, 5),
				model:        copilotPointModel(point.Attributes, resourceAttrs),
				timeUnixNano: point.TimeUnixNano,
			}
			groups[corrKey] = g
			*groupOrder = append(*groupOrder, corrKey)
		}
		g.deltas[tokenType] += delta
	}
}

// canonicalizeTokenGroups turns each correlation group's raw per-metric
// deltas into one increment per call, applying design §3.2's non-cached
// "input" definition: canonical input = input − cache_read − cache_write. A
// cache metric absent from the batch counts as 0 (Copilot may not export a
// counter until its first observation). A negative result is clamped to 0
// -- never emitted, never panics -- and logged and counted once.
func (r *copilotUsageRule) canonicalizeTokenGroups(groups map[string]*copilotTokenGroup, groupOrder []string) []usageIncrement {
	var increments []usageIncrement
	for _, corrKey := range groupOrder {
		g := groups[corrKey]
		cacheRead := g.deltas[telemetrycontract.TokenTypeCacheRead]
		cacheWrite := g.deltas[telemetrycontract.TokenTypeCacheWrite]
		rawInput, hadInput := g.deltas[telemetrycontract.TokenTypeInput]
		canonicalInput := rawInput - cacheRead - cacheWrite
		if canonicalInput < 0 {
			canonicalInput = 0
			r.clampedInput.Add(1)
			r.clampWarnOnce.Do(func() {
				slog.Warn("copilot usage input_tokens is less than its cache_read+cache_write components after cumulative-to-delta conversion; clamped to 0")
			})
		}

		tokens := make(map[string]int64, 5)
		if hadInput && canonicalInput > 0 {
			tokens[telemetrycontract.TokenTypeInput] = canonicalInput
		}
		if cacheRead > 0 {
			tokens[telemetrycontract.TokenTypeCacheRead] = cacheRead
		}
		if cacheWrite > 0 {
			tokens[telemetrycontract.TokenTypeCacheWrite] = cacheWrite
		}
		if v := g.deltas[telemetrycontract.TokenTypeOutput]; v > 0 {
			tokens[telemetrycontract.TokenTypeOutput] = v
		}
		if v := g.deltas[telemetrycontract.TokenTypeReasoning]; v > 0 {
			tokens[telemetrycontract.TokenTypeReasoning] = v
		}
		if len(tokens) == 0 {
			continue
		}
		increments = append(increments, usageIncrement{
			Model:     g.model,
			Status:    telemetrycontract.StatusSuccess,
			Tokens:    tokens,
			dedupeKey: "copilot_tokens\x00" + corrKey + "\x00" + strconv.FormatUint(g.timeUnixNano, 10),
		})
	}
	return increments
}

// clampedInputCount reports how many times canonicalizeTokenGroups has
// clamped a negative canonical input to 0.
func (r *copilotUsageRule) clampedInputCount() int64 { return r.clampedInput.Load() }

// staleBackwardsCount reports how many times the converter saw a cumulative
// value go backwards for the same stream identity (a stale or reordered
// export, never emitted as a delta).
func (r *copilotUsageRule) staleBackwardsCount() int64 { return r.converter.staleBackwards.Load() }

// baselinedAfterCapCount reports how many times a first sighting that would
// otherwise have been trusted was instead baselined because its start_time
// did not exceed maxEvictedStart (see cumulativeToDeltaConverter).
func (r *copilotUsageRule) baselinedAfterCapCount() int64 {
	return r.converter.baselinedAfterCap.Load()
}
