/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
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
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
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
	Calls  int64            // 0 or 1 for event rules; a delta count for metric rules
	Tokens map[string]int64 // token_type -> n (n > 0)

	// dedupeKey identifies the source data point for a metric-sourced
	// increment (design §3.3's "interval-fingerprint idea", the same one
	// metricStreams.remember uses): the metric name plus intervalKey's
	// canonicalized (start, end, point) triple. Empty for a log-sourced
	// increment, which dedupes on the log record itself (see
	// UsageDeriver.fingerprint) instead.
	dedupeKey string
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

// codexUsageEventName and codexUsageEventKind identify the codex.* events
// this rule derives from: SSE frames of a model response, one of which is a
// completed response carrying the turn's token usage (design §5).
//
// This rule does not gate on instrumentation scope. A local capture (see
// loadCodexUsageFixture) shows codex's real log-export scope name is
// "codex_otel.log_only" --
// notably *not* the empty string opentelemetry-appender-tracing's
// OpenTelemetryTracingBridge::new/::builder documents as its default
// ("the default scope uses an empty scope name for the appender logger"),
// which is what reading codex-rs/otel/src/provider.rs's
// logger_export_layer in isolation would suggest. Regardless of which of
// the two is accurate for a given codex/appender release, the rule is
// already filtered to SCION_HARNESS=codex at construction, and
// "codex.sse_event" is a namespaced event name, so a scope check adds no
// real safety and only risks dropping codex's usage silently if the scope
// name changes again (design §5's codex row states no scope requirement,
// unlike Claude's, which does name one).
const (
	codexUsageEventName = "codex.sse_event"
	codexUsageEventKind = "response.completed"
)

// codexUsageRule implements the codex row of design §5: one completed
// response is one call with tokens, or one call with no tokens and
// Status=error for a failed request whose source reports it (design §3.2).
// Verified against codex-rs/otel/src/events/session_telemetry.rs and
// core/src/client.rs at tag rust-v0.158.0 (commit
// 54e1bd264b4122fe9471ee7d54c4d021a76bb8ff of github.com/openai/codex,
// which is what @openai/codex resolves to on npm as of this writing --
// harnesses/codex/Dockerfile does not pin a version). See
// loadCodexUsageFixture in usage_codex_test.go for the fixture's capture
// provenance.
//
// Three distinct emitters share event.name=codex.sse_event and
// event.kind=response.completed, and this rule must tell them apart
// (getting this wrong either drops usage entirely or double-counts calls):
//
//   - sse_event(), called from log_sse_event for *every* SSE frame
//     (client.rs's ApiTelemetry::on_sse_poll calls this per frame,
//     session_telemetry.rs:1028-1046). For a plain "response.completed"
//     frame with a JSON body but no usage attached yet, this is the arm
//     taken, emitting event.kind=response.completed, duration_ms, and no
//     token fields. Matching this as a second, zero-token successful call
//     would double-count: it always carries duration_ms.
//   - sse_event_completed(), called once per response with the parsed
//     TokenUsage (client.rs's Ok(ResponseEvent::Completed{..}) arm,
//     session_telemetry.rs:1102-1117): the real, token-bearing event this
//     rule derives calls and tokens from. Never carries duration_ms.
//   - see_event_completed_failed(), called once per response on the
//     stream's Err(..) arm after provider.map_api_error (client.rs's
//     Err(err) arm calling session_telemetry.see_event_completed_failed(
//     &mapped), session_telemetry.rs:1086-1100): a failed request (a
//     transport or API error, not a parse failure), carrying error.message
//     and no token fields, and -- like sse_event_completed -- no
//     duration_ms.
//
// So duration_ms presence is the frame-vs-completion discriminator, and
// error.message presence (only ever set by the failed-request arm)
// distinguishes a failed response from a successful, token-bearing one,
// mirroring the Claude rule's api_request/api_error split.
//
// Token mapping (design §5, §3.2), for the success case only: input =
// input_token_count − cached_token_count (Codex reports input_token_count
// inclusive of cache hits, unlike Claude's exclusive counts); output =
// output_token_count; cache_read = cached_token_count; reasoning =
// reasoning_token_count, informational only. cache_write_token_count exists
// in the source but has no mapping in design §5's codex row, so this rule
// neither reads it nor emits it as a token_type; tool_token_count (the turn
// total) is likewise not part of the canonical contract and is ignored.
type codexUsageRule struct{}

func (codexUsageRule) Harness() string { return "codex" }

func (codexUsageRule) MatchLog(_, eventName string, record *logspb.LogRecord) (usageIncrement, bool, error) {
	if record == nil || eventName != codexUsageEventName {
		return usageIncrement{}, false, nil
	}
	if logAttrString(record.Attributes, "event.kind") != codexUsageEventKind {
		return usageIncrement{}, false, nil
	}
	// The per-SSE-frame marker record (sse_event()): never a completion,
	// never has tokens. Excluding it here, rather than requiring token
	// fields to be present below, keeps a malformed *and* a legitimately
	// zero-token completion both matching and countable as one call.
	if logAttrPresent(record.Attributes, "duration_ms") {
		return usageIncrement{}, false, nil
	}

	// see_event_completed_failed: a failed request. Mirrors the
	// Claude rule's api_error arm -- Calls=1, Status=error, no tokens, and
	// not malformed (a parse-able error is not a malformed event).
	if logAttrPresent(record.Attributes, "error.message") {
		return usageIncrement{
			Model:  logAttrString(record.Attributes, "model"),
			Status: telemetrycontract.StatusError,
			Calls:  1,
		}, true, nil
	}

	input, inputErr := logAttrInt(record.Attributes, "input_token_count")
	output, outputErr := logAttrInt(record.Attributes, "output_token_count")
	cached, cachedErr := logAttrInt(record.Attributes, "cached_token_count")
	reasoning, reasoningErr := logAttrInt(record.Attributes, "reasoning_token_count")

	var malformed error
	for _, err := range []error{inputErr, outputErr, cachedErr, reasoningErr} {
		if err != nil && malformed == nil {
			malformed = fmt.Errorf("codex sse_event response.completed: %w", err)
		}
	}
	if malformed == nil && cached > input {
		malformed = fmt.Errorf("codex sse_event response.completed: cached_token_count %d exceeds input_token_count %d", cached, input)
	}

	increment := usageIncrement{
		Model:  logAttrString(record.Attributes, "model"),
		Status: telemetrycontract.StatusSuccess,
		Calls:  1,
	}
	if malformed == nil {
		tokens := make(map[string]int64, 3)
		if remaining := input - cached; remaining > 0 {
			tokens[telemetrycontract.TokenTypeInput] = remaining
		}
		if output > 0 {
			tokens[telemetrycontract.TokenTypeOutput] = output
		}
		if cached > 0 {
			tokens[telemetrycontract.TokenTypeCacheRead] = cached
		}
		if reasoning > 0 {
			tokens[telemetrycontract.TokenTypeReasoning] = reasoning
		}
		increment.Tokens = tokens
	}
	return increment, true, malformed
}

// copilotCallsMetric is the histogram whose delta observation count stands
// in for calls (design §5 copilot row): each observation is one model call.
// A real capture confirms this is exactly one observation per model call
// (its count matches the number of chat spans, and
// gen_ai.invoke_agent.inference_calls' delta sum independently agrees).
const copilotCallsMetric = "gen_ai.client.inference.operation.input_tokens"

// copilotTokenCounters maps Copilot's per-type token usage counters to
// canonical token_type values (design §3.2). This is the post-CLI-1.0.45
// GenAI-semconv shape confirmed by `copilot help monitoring` on CLI 1.0.88
// and 1.0.89, and by a real capture on 1.0.89 (design §5 copilot row): the
// pre-1.0.45 gen_ai.client.token.usage histogram with a gen_ai.token.type
// attribute does not exist in any of them and is not supported here.
//
// reasoning.output_tokens maps to the informational "reasoning" token_type
// (§3.2: "never added to totals"): output_tokens is already the total,
// inclusive of reasoning, by the same naming convention the capture proves
// for cache_read.input_tokens/cache_write.input_tokens (both confirmed
// subsets of the raw input_tokens counter -- see
// copilotUsageRule.canonicalizeTokenGroups). input_tokens itself is NOT
// exclusive of cached tokens -- see canonicalizeTokenGroups for the
// subtraction this requires.
var copilotTokenCounters = map[string]string{
	"gen_ai.client.inference.usage.input_tokens":             telemetrycontract.TokenTypeInput,
	"gen_ai.client.inference.usage.output_tokens":            telemetrycontract.TokenTypeOutput,
	"gen_ai.client.inference.usage.cache_read.input_tokens":  telemetrycontract.TokenTypeCacheRead,
	"gen_ai.client.inference.usage.cache_write.input_tokens": telemetrycontract.TokenTypeCacheWrite,
	"gen_ai.client.inference.usage.reasoning.output_tokens":  telemetrycontract.TokenTypeReasoning,
}

// copilotUsageRule implements the copilot row of design §5 (revised): the
// only metric-sourced rule in this project (design §3.3), and the only rule
// that implements metricBatchDeriver (usage_copilot_metrics.go). It never
// matches a log record (Copilot's usage signal is metrics, not native log
// events).
//
// Unlike claudeUsageRule/codexUsageRule (stateless value types shared
// through usageRuleFactories), copilotUsageRule carries per-process state --
// converter, for cumulative-to-delta conversion, and the clamp diagnostics
// below -- so it must be constructed fresh per UsageDeriver via
// newCopilotUsageRule, never reused as a shared zero value.
type copilotUsageRule struct {
	converter     *cumulativeToDeltaConverter
	clampedInput  atomic.Int64
	clampWarnOnce sync.Once
}

// newCopilotUsageRule constructs a copilotUsageRule with a fresh converter
// state, anchored to the current time: only a stream whose Copilot-reported
// start_time is at or after this moment is trusted on first sighting (design
// decision, restart baseline; see cumulativeToDeltaConverter).
func newCopilotUsageRule() *copilotUsageRule {
	return &copilotUsageRule{
		converter: newCumulativeToDeltaConverter(uint64(time.Now().UnixNano()), copilotCumulativeStateCapacity),
	}
}

func (*copilotUsageRule) Harness() string { return "copilot" }

func (*copilotUsageRule) MatchLog(string, string, *logspb.LogRecord) (usageIncrement, bool, error) {
	return usageIncrement{}, false, nil
}

// copilotPointModel resolves the model for one Copilot metric point. A real
// capture confirms gen_ai.request.model/gen_ai.response.model are always on
// the point itself, never only on the resource; the resource fallback below
// is kept as a defensive no-op (design §3.2 names both attributes without
// specifying placement), not because any observed payload has needed it.
func copilotPointModel(pointAttrs, resourceAttrs []*commonpb.KeyValue) string {
	for _, attrs := range [][]*commonpb.KeyValue{pointAttrs, resourceAttrs} {
		if model := logAttrString(attrs, "gen_ai.request.model"); model != "" {
			return model
		}
		if model := logAttrString(attrs, "gen_ai.response.model"); model != "" {
			return model
		}
	}
	return ""
}

// usageRuleFactories lists a constructor for every rule this build knows
// about. rulesForHarness calls each factory fresh and filters to the active
// harness, so an unrelated harness (or none) gets an empty, no-op deriver.
// A factory, rather than a shared prototype value, is required because
// copilotUsageRule carries per-process state (usage_copilot_metrics.go's
// converter): a package-level copilotUsageRule{} shared across every
// UsageDeriver in the process (or across tests in the same binary) would
// leak cumulative-to-delta state between otherwise-independent derivers.
var usageRuleFactories = []func() usageRule{
	func() usageRule { return claudeUsageRule{} },
	func() usageRule { return codexUsageRule{} },
	func() usageRule { return newCopilotUsageRule() },
}

func rulesForHarness(harness string) []usageRule {
	if harness == "" {
		return nil
	}
	var out []usageRule
	for _, factory := range usageRuleFactories {
		rule := factory()
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
// "Diagnostics"). ClampedInput, StaleBackwards and BaselinedAfterCap are
// populated only by a rule that tracks them (usageRuleDiagnostics; today
// only copilotUsageRule, for its cumulative-to-delta conversion) and stay 0
// for every other harness.
type UsageDiagnostics struct {
	Derived, Duplicate, Malformed int64
	ClampedInput                  int64
	StaleBackwards                int64
	BaselinedAfterCap             int64
}

// usageRuleDiagnostics is implemented by a rule that tracks additional,
// rule-specific data-quality counters beyond the deriver's own
// derived/duplicate/malformed counts. Checked as an optional capability, the
// same way metricBatchDeriver is.
type usageRuleDiagnostics interface {
	clampedInputCount() int64
	staleBackwardsCount() int64
	baselinedAfterCapCount() int64
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

// ProcessResourceMetrics derives usage from native metrics matched by a
// metric-sourced usage rule (design §3.3, "Consume semantics for
// metric-sourced rules"; Copilot only in this project, via metricBatchDeriver)
// and reports which metric names were matched, keyed by name alone (matching
// is name-based, not scope- or shape-qualified). The caller uses the
// returned set to apply the design's consume semantics itself: on GCP, strip
// matched metrics from the request before metricStreams.add; on generic
// OTLP, leave the request untouched. This method never mutates
// resourceMetrics.
//
// Called from Pipeline.handleMetrics on the raw, pre-policy input (like
// ProcessResourceLogs), so point attributes the deriver reads (for example
// the model) are never distorted by the policy's redactor — there is no
// metric-level event filter to route around here (processMetrics has none),
// but reading raw keeps both signal types' derivation on the same footing.
//
// Returns nil (a no-op) whenever no active rule implements metricBatchDeriver
// -- today that means every harness except copilot never even builds a
// resourceAttrs/scope/metric walk over the batch, since claude and codex
// have no metric-sourced rule at all.
func (d *UsageDeriver) ProcessResourceMetrics(ctx context.Context, resourceMetrics []*metricpb.ResourceMetrics) map[string]bool {
	if d == nil || len(d.rules) == 0 {
		return nil
	}
	var batchRules []metricBatchDeriver
	for _, rule := range d.rules {
		if br, ok := rule.(metricBatchDeriver); ok {
			batchRules = append(batchRules, br)
		}
	}
	if len(batchRules) == 0 {
		return nil
	}

	matched := make(map[string]bool)
	recorded := false
	for _, rm := range resourceMetrics {
		if rm == nil {
			continue
		}
		for _, br := range batchRules {
			if d.observeMetricBatch(ctx, br, rm, matched) {
				recorded = true
			}
		}
	}
	if !recorded {
		return matched
	}
	// See ProcessResourceLogs: forces the derived points to reach
	// metricStreams now rather than waiting for the periodic reader.
	flushCtx, cancel := context.WithTimeout(ctx, usageDeriverFlushTimeout)
	defer cancel()
	if err := d.providers.MeterProvider.ForceFlush(flushCtx); err != nil {
		log.Debug("Usage deriver flush failed: %v", err)
	}
	return matched
}

// observeMetricBatch is ProcessResourceMetrics' path for a rule that
// implements metricBatchDeriver: called once per ResourceMetrics, since the
// correlation copilotUsageRule needs (input minus its cache siblings)
// requires seeing every sibling counter in the same export together. rule's
// own DeriveBatch already returns fully-qualified dedupeKeys (its stream/
// correlation keys already fold in the resource attributes and scope), so
// this passes an empty scope to metricFingerprint rather than double-keying
// on scope.
func (d *UsageDeriver) observeMetricBatch(ctx context.Context, rule metricBatchDeriver, rm *metricpb.ResourceMetrics, matched map[string]bool) bool {
	increments, names, err := rule.DeriveBatch(rm)
	for name := range names {
		matched[name] = true
	}
	if err != nil {
		d.malformed.Add(1)
		d.malformedWarnOnce.Do(func() {
			slog.Warn("usage deriver observed a malformed native usage metric; the affected data points were dropped")
		})
	}
	recorded := false
	for _, increment := range increments {
		if increment.Calls == 0 && len(increment.Tokens) == 0 {
			continue
		}
		fingerprint := d.metricFingerprint("", increment.dedupeKey)
		if d.seen.SeenBefore(fingerprint) {
			d.duplicate.Add(1)
			continue
		}
		d.record(ctx, increment)
		d.derived.Add(1)
		recorded = true
	}
	return recorded
}

// metricFingerprint implements the dedupe key for a metric-sourced increment
// (design §3.3: "the same interval-fingerprint idea as metricStreams.remember"):
// resource identity and scope (as fingerprint does for logs) plus the data
// point's own dedupeKey (metric name and its canonicalized interval).
func (d *UsageDeriver) metricFingerprint(scopeName, dedupeKey string) string {
	h := sha256.New()
	h.Write([]byte(d.resourceIdentity))
	h.Write([]byte{0})
	h.Write([]byte(scopeName))
	h.Write([]byte{0})
	h.Write([]byte(dedupeKey))
	return hex.EncodeToString(h.Sum(nil))
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
	diag := UsageDiagnostics{
		Derived:   d.derived.Load(),
		Duplicate: d.duplicate.Load(),
		Malformed: d.malformed.Load(),
	}
	for _, rule := range d.rules {
		if rd, ok := rule.(usageRuleDiagnostics); ok {
			diag.ClampedInput += rd.clampedInputCount()
			diag.StaleBackwards += rd.staleBackwardsCount()
			diag.BaselinedAfterCap += rd.baselinedAfterCapCount()
		}
	}
	return diag
}

// HasMetricRule reports whether the deriver has at least one rule that
// derives from metrics (metricBatchDeriver) -- today, that means the active
// harness is copilot. Pipeline.handleMetrics gates its extra validateMetrics
// pass and the whole ProcessResourceMetrics walk on this, so claude/codex
// (and every harness with no usage rule at all) pay neither cost.
func (d *UsageDeriver) HasMetricRule() bool {
	if d == nil {
		return false
	}
	for _, rule := range d.rules {
		if _, ok := rule.(metricBatchDeriver); ok {
			return true
		}
	}
	return false
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

// logAttrPresent reports whether key is present in attrs at all, regardless
// of its value's type. Unlike logAttrString, this correctly detects a
// present non-string-typed attribute (for example an intValue or
// doubleValue), which logAttrString cannot distinguish from "absent" since
// GetStringValue() returns "" for either.
func logAttrPresent(attrs []*commonpb.KeyValue, key string) bool {
	for _, kv := range attrs {
		if kv != nil && kv.Key == key {
			return true
		}
	}
	return false
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

// metricPointInt64 reads a NumberDataPoint's value as a non-negative int64,
// the same tolerance logAttrInt applies to log attributes: an integral
// double is accepted, a negative or non-integral value is malformed. Unlike
// logAttrInt, absence has no meaning here (a data point always carries a
// value), so there is no zero-value "not present" case to special-case.
func metricPointInt64(point *metricpb.NumberDataPoint) (int64, error) {
	switch v := point.GetValue().(type) {
	case *metricpb.NumberDataPoint_AsInt:
		if v.AsInt < 0 {
			return 0, errors.New("value is negative")
		}
		return v.AsInt, nil
	case *metricpb.NumberDataPoint_AsDouble:
		if v.AsDouble < 0 {
			return 0, errors.New("value is negative")
		}
		if math.Trunc(v.AsDouble) != v.AsDouble {
			return 0, errors.New("value is not an integer")
		}
		return int64(v.AsDouble), nil
	default:
		return 0, errors.New("value is not numeric")
	}
}

// stripMatchedUsageMetrics returns a copy of resourceMetrics with every
// metric whose name is in matched removed (design §3.3 "Consume semantics
// for metric-sourced rules", GCP only). resourceMetrics is assumed to
// already be a policy-owned clone (Pipeline.handleMetrics calls this on
// decision.Data), so mutating its ScopeMetrics/Metrics slices in place is
// safe. A ScopeMetrics or ResourceMetrics left with nothing else in it is
// dropped, consistent with how an already-empty batch is handled elsewhere
// in this pipeline.
func stripMatchedUsageMetrics(resourceMetrics []*metricpb.ResourceMetrics, matched map[string]bool) []*metricpb.ResourceMetrics {
	if len(matched) == 0 {
		return resourceMetrics
	}
	result := make([]*metricpb.ResourceMetrics, 0, len(resourceMetrics))
	for _, rm := range resourceMetrics {
		if rm == nil {
			continue
		}
		keptScopes := rm.ScopeMetrics[:0]
		for _, sm := range rm.ScopeMetrics {
			if sm == nil {
				continue
			}
			keptMetrics := sm.Metrics[:0]
			for _, m := range sm.Metrics {
				if m == nil || matched[m.Name] {
					continue
				}
				keptMetrics = append(keptMetrics, m)
			}
			if len(keptMetrics) == 0 {
				continue
			}
			sm.Metrics = keptMetrics
			keptScopes = append(keptScopes, sm)
		}
		if len(keptScopes) == 0 {
			continue
		}
		rm.ScopeMetrics = keptScopes
		result = append(result, rm)
	}
	return result
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
