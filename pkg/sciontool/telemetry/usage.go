/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// usageDeriverFlushTimeout bounds how long ProcessResourceLogs waits for the
// loopback ForceFlush to land in metricStreams. It never blocks the caller
// indefinitely.
const usageDeriverFlushTimeout = 5 * time.Second

// usageDedupeCapacity and usageDedupeTTL bound the dedupe LRU (design §3.3).
const (
	usageDedupeCapacity = 16384
	usageDedupeTTL      = 15 * time.Minute
)

// usageIncrement is what a usageRule derives from one matched native event.
type usageIncrement struct {
	Model  string
	Status string           // telemetrycontract.StatusSuccess | StatusError
	Calls  int64            // 0 or 1 for event rules
	Tokens map[string]int64 // token_type -> n (n > 0)
}

// usageRule matches one per-request native event to a usageIncrement. Rules
// are filtered to SCION_HARNESS at construction (design §3.3). MatchLog
// returns matched=false when the record isn't one this rule recognizes, and
// a non-nil error when it recognizes the event but one or more of its token
// fields are malformed (non-numeric or negative); the caller counts that as
// usage_malformed but still records the returned increment (a call is a
// completed response, even when its token fields could not be parsed).
//
// eventName is normalizedLogEventName's result for record, computed once by
// the caller (observe) rather than a second time inside MatchLog: the
// fingerprint needs it too, and it isn't free (it walks record's attributes
// and must also honor a native LogRecord.EventName field, since a native SDK
// may carry the event name there instead of a bare attribute).
type usageRule interface {
	// Harness is the SCION_HARNESS value this rule applies to.
	Harness() string
	// MatchLog inspects one log record from the given instrumentation scope.
	MatchLog(scopeName, eventName string, record *logspb.LogRecord) (increment usageIncrement, matched bool, err error)
}

// claudeUsageScope is Claude Code's native OTel log instrumentation scope.
const claudeUsageScope = "com.anthropic.claude_code.events"

// claudeTokenFields maps Claude's api_request attribute names to canonical
// token_type values (design §5). Claude reports exclusive counts, so no
// subtraction is needed: total input = input + cache_read + cache_write
// holds because "input_tokens" already excludes cached tokens.
var claudeTokenFields = map[string]string{
	"input_tokens":          telemetrycontract.TokenTypeInput,
	"output_tokens":         telemetrycontract.TokenTypeOutput,
	"cache_read_tokens":     telemetrycontract.TokenTypeCacheRead,
	"cache_creation_tokens": telemetrycontract.TokenTypeCacheWrite,
}

// claudeUsageRule implements the Claude row of design §5: one completed
// api_request is one call with tokens; one api_error is one failed call with
// no tokens. Verified against a captured Claude Code 2.1.280 payload
// (testdata/usage/claude-2.1.280.pb.json).
type claudeUsageRule struct{}

func (claudeUsageRule) Harness() string { return "claude" }

func (claudeUsageRule) MatchLog(scopeName, eventName string, record *logspb.LogRecord) (usageIncrement, bool, error) {
	if scopeName != claudeUsageScope || record == nil || eventName == "" {
		return usageIncrement{}, false, nil
	}
	switch eventName {
	case "api_request":
		tokens := make(map[string]int64, len(claudeTokenFields))
		var malformed error
		for attrKey, tokenType := range claudeTokenFields {
			n, err := logAttrInt(record.Attributes, attrKey)
			if err != nil {
				// Keep checking the remaining fields (for a call still worth
				// counting), but drop every token for this event rather than
				// reporting a partial, misleading total.
				if malformed == nil {
					malformed = fmt.Errorf("claude api_request %s: %w", attrKey, err)
				}
				continue
			}
			if n > 0 {
				tokens[tokenType] = n
			}
		}
		increment := usageIncrement{
			Model:  logAttrString(record.Attributes, "model"),
			Status: telemetrycontract.StatusSuccess,
			Calls:  1,
		}
		if malformed == nil {
			increment.Tokens = tokens
		}
		return increment, true, malformed
	case "api_error":
		return usageIncrement{
			Model:  logAttrString(record.Attributes, "model"),
			Status: telemetrycontract.StatusError,
			Calls:  1,
		}, true, nil
	default:
		return usageIncrement{}, false, nil
	}
}

// usageRuleRegistry lists every rule this build knows about. rulesForHarness
// filters it to the active harness, so an unrelated harness (or none) gets
// an empty, no-op deriver.
var usageRuleRegistry = []usageRule{claudeUsageRule{}}

func rulesForHarness(harness string) []usageRule {
	if harness == "" {
		return nil
	}
	var out []usageRule
	for _, rule := range usageRuleRegistry {
		if rule.Harness() == harness {
			out = append(out, rule)
		}
	}
	return out
}

// UsageDeriver turns native per-request log events into the canonical
// gen_ai.api.calls / scion.usage.tokens counters (design §3.3). It is
// enabled only when SCION_USAGE_SOURCE=native (D4) and filtered to
// SCION_HARNESS; with no matching rules it is a cheap no-op that builds no
// MeterProvider.
type UsageDeriver struct {
	rules            []usageRule
	seen             *boundedLRU
	providers        *Providers
	calls            otelmetric.Int64Counter
	tokens           otelmetric.Int64Counter
	resourceIdentity string

	derived, duplicate, malformed atomic.Int64
	malformedWarnOnce             sync.Once
}

// UsageDiagnostics are fixed-cardinality usage-derivation counters, exposed
// alongside the pipeline's other signal diagnostics (design §3.3
// "Diagnostics").
type UsageDiagnostics struct {
	Derived, Duplicate, Malformed int64
}

// NewUsageDeriver constructs the deriver for the current process's
// environment. It never fails on a harness with no rule, or when usage
// derivation is not the active source (D10): both return a nil-safe no-op
// deriver rather than an error.
func NewUsageDeriver(ctx context.Context, config *Config) (*UsageDeriver, error) {
	if os.Getenv("SCION_USAGE_SOURCE") != "native" {
		return &UsageDeriver{}, nil
	}
	rules := rulesForHarness(os.Getenv("SCION_HARNESS"))
	if len(rules) == 0 {
		return &UsageDeriver{}, nil
	}
	providers, err := NewProviders(ctx, config, false)
	if err != nil {
		return nil, fmt.Errorf("creating usage deriver providers: %w", err)
	}
	if providers == nil || providers.MeterProvider == nil {
		return &UsageDeriver{}, nil
	}
	meter := providers.MeterProvider.Meter(usageMetricScope)
	calls, err := meter.Int64Counter(telemetrycontract.MetricAPICalls,
		otelmetric.WithUnit("{call}"),
		otelmetric.WithDescription("Number of LLM API calls, derived from native usage events"),
	)
	if err != nil {
		_ = providers.Shutdown(ctx)
		return nil, fmt.Errorf("creating usage calls counter: %w", err)
	}
	tokens, err := meter.Int64Counter(telemetrycontract.MetricUsageTokens,
		otelmetric.WithUnit("{token}"),
		otelmetric.WithDescription("Tokens attributed to model requests, derived from native usage events"),
	)
	if err != nil {
		_ = providers.Shutdown(ctx)
		return nil, fmt.Errorf("creating usage tokens counter: %w", err)
	}
	return &UsageDeriver{
		rules:            rules,
		seen:             newBoundedLRU(usageDedupeCapacity, usageDedupeTTL),
		providers:        providers,
		calls:            calls,
		tokens:           tokens,
		resourceIdentity: resourceIdentityFingerprint(),
	}, nil
}

// resourceIdentityFingerprint is the resource-identity component of the
// dedupe fingerprint (design §3.3): stable for the process lifetime, so it
// is computed once.
func resourceIdentityFingerprint() string {
	return strings.Join([]string{
		os.Getenv("SCION_AGENT_ID"),
		projectkeys.ProjectIDFromEnv(os.Getenv),
		os.Getenv("SCION_HARNESS"),
	}, "\x00")
}

// ProcessResourceLogs derives usage from raw, pre-filter log records. It
// must run before the event filter drops records (design §3.3: derivation
// must not depend on Filter.Include), and reads only the event name, model,
// status and numeric token attributes — nothing else leaves this function.
// Called from Pipeline.handleLogs before the policy's event filter.
func (d *UsageDeriver) ProcessResourceLogs(ctx context.Context, resourceLogs []*logspb.ResourceLogs) {
	if d == nil || len(d.rules) == 0 {
		return
	}
	recorded := false
	for _, rl := range resourceLogs {
		if rl == nil {
			continue
		}
		for _, sl := range rl.ScopeLogs {
			if sl == nil {
				continue
			}
			scopeName := sl.GetScope().GetName()
			for _, record := range sl.LogRecords {
				if record == nil {
					continue
				}
				if d.observe(ctx, scopeName, record) {
					recorded = true
				}
			}
		}
	}
	if !recorded {
		return
	}
	// Force the loopback export now, through the same admission and
	// diagnostics path a hook counter uses, so the derived point reaches
	// metricStreams before the caller's next flush instead of waiting for
	// the periodic reader interval.
	flushCtx, cancel := context.WithTimeout(ctx, usageDeriverFlushTimeout)
	defer cancel()
	if err := d.providers.MeterProvider.ForceFlush(flushCtx); err != nil {
		log.Debug("Usage deriver flush failed: %v", err)
	}
}

// observe matches one record against every rule and records at most one
// increment. It reports whether anything was recorded.
//
// The event name comes from normalizedLogEventName, not a bare "event.name"
// attribute read: a native SDK may carry the event name in LogRecord's own
// EventName field instead, and this must recognize either.
func (d *UsageDeriver) observe(ctx context.Context, scopeName string, record *logspb.LogRecord) bool {
	eventName, err := normalizedLogEventName(record, scopeName)
	if err != nil || eventName == "" {
		return false
	}
	for _, rule := range d.rules {
		increment, matched, err := rule.MatchLog(scopeName, eventName, record)
		if !matched {
			continue
		}
		if err != nil {
			d.malformed.Add(1)
			// Warn, not Error: this is a data-quality degradation (tokens
			// dropped, the call is still counted), not a failure of
			// sciontool itself — matching pipeline.go's own slog.Warn calls
			// for comparable operator-visible degradations. log.Init installs
			// the package handler as slog.Default (pkg/sciontool/log), so
			// this lands in the same agent.log sink as everything logged
			// through the log package, just tagged [WARN] [slog]. One
			// malformed event tells the operator everything they need;
			// logging every occurrence would be noisy on a chatty, unpinned
			// CLI. Never includes the offending value, only the
			// (fixed-cardinality) event name.
			d.malformedWarnOnce.Do(func() {
				slog.Warn("usage deriver observed a malformed native usage event; its token fields were dropped, the call is still counted", "event", eventName)
			})
		}
		// A rule that recognizes the event but derives nothing usable from it
		// (no call, no tokens) has nothing to record or dedupe.
		if increment.Calls == 0 && len(increment.Tokens) == 0 {
			return false
		}
		fingerprint := d.fingerprint(scopeName, eventName, record)
		if d.seen.SeenBefore(fingerprint) {
			d.duplicate.Add(1)
			return false
		}
		d.record(ctx, increment)
		d.derived.Add(1)
		return true
	}
	return false
}

// fingerprint implements the dedupe key from design §3.3:
// sha256(resource-identity, scope, time_unix_nano, event.name,
// request_id-or-canonical-attrs).
func (d *UsageDeriver) fingerprint(scopeName, eventName string, record *logspb.LogRecord) string {
	h := sha256.New()
	h.Write([]byte(d.resourceIdentity))
	h.Write([]byte{0})
	h.Write([]byte(scopeName))
	h.Write([]byte{0})
	var timeBuf [8]byte
	binary.BigEndian.PutUint64(timeBuf[:], record.GetTimeUnixNano())
	h.Write(timeBuf[:])
	h.Write([]byte{0})
	h.Write([]byte(eventName))
	h.Write([]byte{0})
	if requestID := logAttrString(record.Attributes, "request_id"); requestID != "" {
		h.Write([]byte("request_id\x00" + requestID))
	} else if key, err := canonicalAttrs(record.Attributes); err == nil {
		h.Write([]byte(key))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// record emits the canonical increments. Point labels are exactly the
// producer set from design §3.2: gen_ai.api.calls gets agent_id, project_id,
// harness, model, status (matching the existing hook descriptor);
// scion.usage.tokens gets harness, model, token_type only.
func (d *UsageDeriver) record(ctx context.Context, increment usageIncrement) {
	model := increment.Model
	if model == "" {
		model = os.Getenv("SCION_MODEL")
	}
	if model == "" {
		model = "unknown"
	}
	model = truncateUTF8(model, 128)
	harness := os.Getenv("SCION_HARNESS")

	if increment.Calls != 0 && d.calls != nil {
		attrs := []attribute.KeyValue{}
		if agentID := os.Getenv("SCION_AGENT_ID"); agentID != "" {
			attrs = append(attrs, attribute.String("agent_id", agentID))
		}
		if projectID := projectkeys.ProjectIDFromEnv(os.Getenv); projectID != "" {
			attrs = append(attrs, attribute.String("project_id", projectID))
		}
		attrs = append(attrs,
			attribute.String(telemetrycontract.HarnessLabel, harness),
			attribute.String(telemetrycontract.ModelLabel, model),
			attribute.String(telemetrycontract.StatusLabel, increment.Status),
		)
		d.calls.Add(ctx, increment.Calls, otelmetric.WithAttributes(attrs...))
	}
	if d.tokens == nil {
		return
	}
	for tokenType, n := range increment.Tokens {
		if n <= 0 {
			continue
		}
		d.tokens.Add(ctx, n, otelmetric.WithAttributes(
			attribute.String(telemetrycontract.HarnessLabel, harness),
			attribute.String(telemetrycontract.ModelLabel, model),
			attribute.String(telemetrycontract.TokenTypeLabel, tokenType),
		))
	}
}

// Diagnostics returns the deriver's fixed-cardinality counters. Safe on a
// nil or no-op deriver.
func (d *UsageDeriver) Diagnostics() UsageDiagnostics {
	if d == nil {
		return UsageDiagnostics{}
	}
	return UsageDiagnostics{
		Derived:   d.derived.Load(),
		Duplicate: d.duplicate.Load(),
		Malformed: d.malformed.Load(),
	}
}

// Shutdown releases the deriver's loopback providers, if any were created.
func (d *UsageDeriver) Shutdown(ctx context.Context) error {
	if d == nil || d.providers == nil {
		return nil
	}
	return d.providers.Shutdown(ctx)
}

// truncateUTF8 truncates s to at most maxBytes bytes without splitting a
// multi-byte rune: it walks back from maxBytes to the nearest rune boundary
// rather than cutting mid-rune, which would produce an invalid UTF-8 label
// value.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

func logAttrString(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv != nil && kv.Key == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// logAttrInt reads a token count attribute. Absence is not malformed (it
// returns 0, nil): Claude only reports fields with a nonzero value in some
// payload shapes. A present but non-numeric or negative value is malformed.
//
// The captured fixture itself mixes value types across attributes on the
// same event (some numeric fields arrive as intValue, others as
// stringValue), and an unpinned CLI (harnesses/claude installs @latest) can
// change which encoding a given field uses from one release to the next.
// So besides IntValue, this also accepts a string that parses as a
// non-negative base-10 integer, and an integral DoubleValue — anything else
// (non-numeric string, fractional double) is malformed.
func logAttrInt(attrs []*commonpb.KeyValue, key string) (int64, error) {
	for _, kv := range attrs {
		if kv == nil || kv.Key != key {
			continue
		}
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_IntValue:
			if v.IntValue < 0 {
				return 0, fmt.Errorf("attribute %q is negative", key)
			}
			return v.IntValue, nil
		case *commonpb.AnyValue_DoubleValue:
			if v.DoubleValue < 0 {
				return 0, fmt.Errorf("attribute %q is negative", key)
			}
			if math.Trunc(v.DoubleValue) != v.DoubleValue {
				return 0, fmt.Errorf("attribute %q is not an integer", key)
			}
			return int64(v.DoubleValue), nil
		case *commonpb.AnyValue_StringValue:
			n, err := strconv.ParseInt(v.StringValue, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("attribute %q is not an integer", key)
			}
			if n < 0 {
				return 0, fmt.Errorf("attribute %q is negative", key)
			}
			return n, nil
		default:
			return 0, fmt.Errorf("attribute %q is not an integer", key)
		}
	}
	return 0, nil
}

// boundedLRU is a fixed-capacity, time-windowed "seen before" set used for
// dedupe (design §3.3: "16k entries / 15 min"). It dedupes discrete events
// across the whole process, keyed on a content fingerprint, which is a
// different problem from metricStreams.remember (that dedupes points within
// one metric stream, keyed on interval end).
//
// SeenBefore is called concurrently: the receiver runs each OTLP gRPC/HTTP
// export request on its own goroutine (Pipeline.handleLogs is not
// serialized), so the map and the eviction order slice are guarded by mu.
type boundedLRU struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	now      func() time.Time
	seen     map[string]time.Time
	order    []lruEntry
}

type lruEntry struct {
	key string
	at  time.Time
}

func newBoundedLRU(capacity int, ttl time.Duration) *boundedLRU {
	return &boundedLRU{capacity: capacity, ttl: ttl, now: time.Now, seen: make(map[string]time.Time)}
}

// SeenBefore records fingerprint and reports whether it was already present
// within the TTL window. The check and the insert happen atomically under
// mu, so two concurrent replays of the same request cannot both observe
// "not seen before".
func (l *boundedLRU) SeenBefore(fingerprint string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.evictExpired(now)
	if at, ok := l.seen[fingerprint]; ok && now.Sub(at) < l.ttl {
		return true
	}
	l.seen[fingerprint] = now
	l.order = append(l.order, lruEntry{fingerprint, now})
	for len(l.order) > l.capacity {
		oldest := l.order[0]
		// Zero the evicted slot before reslicing, so its fingerprint string
		// isn't held reachable through the shared backing array (one entry
		// write, versus an O(n) memmove for a slices.Delete-style compaction).
		l.order[0] = lruEntry{}
		l.order = l.order[1:]
		if at, ok := l.seen[oldest.key]; ok && at.Equal(oldest.at) {
			delete(l.seen, oldest.key)
		}
	}
	return false
}

func (l *boundedLRU) evictExpired(now time.Time) {
	cut := 0
	for cut < len(l.order) && now.Sub(l.order[cut].at) >= l.ttl {
		if at, ok := l.seen[l.order[cut].key]; ok && at.Equal(l.order[cut].at) {
			delete(l.seen, l.order[cut].key)
		}
		cut++
	}
	if cut > 0 {
		// Zero the evicted prefix before reslicing, same reasoning as
		// SeenBefore's eviction loop above.
		for i := range cut {
			l.order[i] = lruEntry{}
		}
		l.order = l.order[cut:]
	}
}
