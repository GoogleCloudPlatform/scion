/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/protobuf/proto"
)

// Expected values in this file are calculated by hand from the fixture
// attributes (quoted in comments), independently of the rule code.

func setAnalyticsEnv(t *testing.T, harness string) {
	t.Helper()
	t.Setenv("SCION_AGENT_ID", "agent-1")
	t.Setenv("SCION_PROJECT_ID", "proj-1")
	t.Setenv("SCION_HARNESS", harness)
	t.Setenv("SCION_LAUNCH_ID", "launch-1")
	t.Setenv("SCION_BROKER_ID", "broker-1")
	t.Setenv("SCION_MODEL", "")
}

func analyticsDeriver(t *testing.T, outboxDir string, rules ...usageRule) (*UsageDeriver, *AnalyticsOutbox) {
	t.Helper()
	o, err := OpenAnalyticsOutbox(outboxDir)
	if err != nil {
		t.Fatal(err)
	}
	d := bareUsageDeriver(rules...)
	d.analytics = NewUsageAnalytics(o, telemetrycontract.UsageSourceNative)
	return d, o
}

// feed observes every record of resourceLogs the way ProcessResourceLogs
// does, without the loopback flush.
func feed(d *UsageDeriver, resourceLogs []*logspb.ResourceLogs) {
	for _, rl := range resourceLogs {
		for _, sl := range rl.GetScopeLogs() {
			for _, r := range sl.GetLogRecords() {
				d.observeRecord(context.Background(), sl.GetScope().GetName(), rl.GetResource().GetAttributes(), r)
			}
		}
	}
}

func stagedEvents(t *testing.T, o *AnalyticsOutbox) []telemetrycontract.UsageEvent {
	t.Helper()
	var out []telemetrycontract.UsageEvent
	for _, l := range stagingLines(t, o) {
		out = append(out, decodeUsage(t, l))
	}
	return out
}

func i64(n int64) *int64 { return &n }

// tokenView is the token part of an event, for table comparison.
type tokenView struct {
	Input, Output, CacheRead, CacheWrite, Reasoning *int64
	Overlaps                                        bool
	Derivation, Status                              string
	Malformed                                       []string
}

func tokensOf(ev telemetrycontract.UsageEvent) tokenView {
	if len(ev.MalformedFields) == 0 {
		ev.MalformedFields = nil // always written as []; compare as "none"
	}
	return tokenView{ev.Input, ev.Output, ev.CacheRead, ev.CacheWrite, ev.Reasoning,
		ev.ReasoningOverlapsOutput != nil && *ev.ReasoningOverlapsOutput, ev.InputDerivation, ev.TokenStatus, ev.MalformedFields}
}

func assertTokens(t *testing.T, label string, got telemetrycontract.UsageEvent, want tokenView) {
	t.Helper()
	if g := tokensOf(got); !reflect.DeepEqual(g, want) {
		t.Errorf("%s tokens\n got %s\nwant %s", label, fmtView(g), fmtView(want))
	}
}

func fmtView(v tokenView) string {
	p := func(n *int64) string {
		if n == nil {
			return "unknown"
		}
		return itoa(*n)
	}
	return "in=" + p(v.Input) + " out=" + p(v.Output) + " cr=" + p(v.CacheRead) + " cw=" + p(v.CacheWrite) + " rs=" + p(v.Reasoning) +
		" overlaps=" + map[bool]string{true: "y", false: "n"}[v.Overlaps] + " deriv=" + v.Derivation + " status=" + v.Status + " malformed=" + strings.Join(v.Malformed, ",")
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// --- Claude ---------------------------------------------------------------

// Captured Claude Code 2.1.280 api_request: input_tokens=2,
// output_tokens=41, cache_read_tokens=0, cache_creation_tokens=26607,
// cost_usd_micros=66931, request_id="scrubbed-request-id" (a placeholder:
// not the Anthropic req_ shape), session.id="scrubbed-session-id".
func TestUsageAnalyticsClaudeFixture(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), claudeUsageRule{})
	feed(d, loadClaudeUsageFixture(t))

	evs := stagedEvents(t, o)
	var req, errEv *telemetrycontract.UsageEvent
	for i := range evs {
		switch evs[i].SourceKind {
		case "claude.api_request":
			req = &evs[i]
		case "claude.api_error":
			errEv = &evs[i]
		}
	}
	if req == nil || errEv == nil {
		t.Fatalf("events = %+v", evs)
	}
	// Exclusive Claude input: reported as-is; cache_read is a reported 0.
	assertTokens(t, "api_request", *req, tokenView{
		Input: i64(2), Output: i64(41), CacheRead: i64(0), CacheWrite: i64(26607),
		Derivation: "reported", Status: "reported",
	})
	if req.ReportedCostMicros == nil || *req.ReportedCostMicros != 66931 || req.ReportedCostCurrency != "USD" || req.ReportedCostSource != "harness" {
		t.Errorf("reported cost = %v %q %q", req.ReportedCostMicros, req.ReportedCostCurrency, req.ReportedCostSource)
	}
	if req.SourceIdentity != telemetrycontract.SourceIdentityComposite || !strings.HasPrefix(req.SourceEventID, "cmp1:") {
		t.Errorf("placeholder request_id must not be identity: %s %s", req.SourceIdentity, req.SourceEventID)
	}
	if req.Model != "claude-sonnet-5" || req.ModelSource != "event" || req.SessionID != "scrubbed-session-id" || req.Harness != "claude" {
		t.Errorf("model/session = %q %q %q %q", req.Model, req.ModelSource, req.SessionID, req.Harness)
	}
	if req.HarnessVersion != "2.1.280" || req.UsageSource != "native" || req.SourceSchemaVersion != "1" {
		t.Errorf("harness_version %q usage_source %q schema %q", req.HarnessVersion, req.UsageSource, req.SourceSchemaVersion)
	}
	if req.AgentID != "agent-1" || req.ProjectID != "proj-1" || req.LaunchID != "launch-1" || req.BrokerID != "broker-1" || req.IncarnationID != "" {
		t.Errorf("identity = %+v (incarnation_id must be omitted)", req.Envelope)
	}
	if req.ProducerNamespace != "sciontool/project/proj-1/agent/agent-1" {
		t.Errorf("namespace = %q", req.ProducerNamespace)
	}
	// event_ts is the record time (2026-09-27T15:24:56.833Z), not now.
	if req.EventTS != "2026-09-27T15:24:56.833Z" || req.SourceTime != "2026-09-27T15:24:56.833Z" {
		t.Errorf("event_ts %q source_time %q", req.EventTS, req.SourceTime)
	}
	// api_error: one failed call, no tokens at all.
	if errEv.Status != "error" || errEv.Calls != 1 {
		t.Errorf("api_error status %q calls %d", errEv.Status, errEv.Calls)
	}
	assertTokens(t, "api_error", *errEv, tokenView{Status: "not_reported"})
	if strings.Contains(stagingLines(t, o)[0], "prompt") {
		t.Error("no prompt-derived field may appear in an event")
	}
}

func claudeRecord(requestID, sequence string, timeNanos uint64, tokens map[string]string) *logspb.LogRecord {
	attrs := []*commonpb.KeyValue{
		kvs("event.name", "api_request"), kvs("session.id", "s-1"), kvs("event.sequence", sequence), kvs("model", "claude-x"),
	}
	if requestID != "" {
		attrs = append(attrs, kvs("request_id", requestID))
	}
	for k, v := range tokens {
		attrs = append(attrs, kvs(k, v))
	}
	return &logspb.LogRecord{TimeUnixNano: timeNanos, Attributes: attrs}
}

func kvs(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func claudeLogs(records ...*logspb.LogRecord) []*logspb.ResourceLogs {
	return []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: claudeUsageScope}, LogRecords: records}}}}
}

var fullClaudeTokens = map[string]string{"input_tokens": "10", "output_tokens": "5", "cache_read_tokens": "100", "cache_creation_tokens": "7"}

// A verified request ID replay is neutral: one event, also after restart
// (fresh deriver and LRU, same outbox) and after the LRU forgot it.
func TestUsageAnalyticsVerifiedRequestIDReplay(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	dir := filepath.Join(t.TempDir(), "ob")
	rec := claudeRecord("req_011CUabcdefghijklmnopq", "3", 1_000, fullClaudeTokens)
	d, o := analyticsDeriver(t, dir, claudeUsageRule{})
	feed(d, claudeLogs(rec, proto.Clone(rec).(*logspb.LogRecord)))
	if d.Diagnostics().Derived != 1 || d.Diagnostics().Duplicate != 1 {
		t.Errorf("counters: %+v", d.Diagnostics())
	}

	restarted, _ := analyticsDeriver(t, dir, claudeUsageRule{})
	feed(restarted, claudeLogs(proto.Clone(rec).(*logspb.LogRecord)))
	evs := stagedEvents(t, o)
	if len(evs) != 1 || evs[0].SourceIdentity != "request_id" || !strings.HasPrefix(evs[0].SourceEventID, "rid1:") {
		t.Fatalf("events = %+v", evs)
	}
	if o.Stats().CertainDuplicates+restartedOutboxDup(restarted) != 2 {
		t.Errorf("certain duplicates not counted: %+v", o.Stats())
	}
	// Restart re-derives the counter (fresh LRU): the divergence between
	// counter and ledger is exactly what the ledger records as a duplicate.
	if restarted.Diagnostics().Derived != 1 {
		t.Errorf("restarted deriver counters: %+v", restarted.Diagnostics())
	}
}

func restartedOutboxDup(d *UsageDeriver) int64 { return d.analytics.Outbox().Stats().CertainDuplicates }

// Two legitimate identical-looking calls (same tokens, same model) stay
// two: they differ in event.sequence and time.
func TestUsageAnalyticsIdenticalLookingCallsStayTwo(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), claudeUsageRule{})
	feed(d, claudeLogs(
		claudeRecord("", "1", 1_000, fullClaudeTokens),
		claudeRecord("", "2", 2_000, fullClaudeTokens),
	))
	evs := stagedEvents(t, o)
	if len(evs) != 2 || evs[0].SourceEventID == evs[1].SourceEventID {
		t.Fatalf("events = %+v", evs)
	}
	for _, ev := range evs {
		if ev.SourceOccurrence != 1 || ev.IdentityAmbiguous {
			t.Errorf("distinct calls must both be certain: %+v", ev)
		}
	}
	if d.Diagnostics().Derived != 2 {
		t.Errorf("counters derived = %d, want 2", d.Diagnostics().Derived)
	}
}

// Placeholder/redacted request IDs are not identity, in the event or in the
// counter fingerprint: two distinct calls with the same placeholder and the
// same timestamp (differing only in event.sequence) are two calls.
func TestUsageAnalyticsPlaceholderRequestIDsDoNotMerge(t *testing.T) {
	for _, placeholder := range []string{"<REDACTED>", "scrubbed-request-id", "req_short", "redacted"} {
		t.Run(placeholder, func(t *testing.T) {
			setAnalyticsEnv(t, "claude")
			d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), claudeUsageRule{})
			feed(d, claudeLogs(
				claudeRecord(placeholder, "1", 5_000, fullClaudeTokens),
				claudeRecord(placeholder, "2", 5_000, fullClaudeTokens),
			))
			if d.Diagnostics().Derived != 2 || d.Diagnostics().Duplicate != 0 {
				t.Errorf("counter fingerprint merged distinct calls: %+v", d.Diagnostics())
			}
			evs := stagedEvents(t, o)
			if len(evs) != 2 || evs[0].SourceIdentity != "composite" || evs[0].SourceEventID == evs[1].SourceEventID {
				t.Errorf("events = %+v", evs)
			}
		})
	}
	// Missing request ID behaves the same.
	setAnalyticsEnv(t, "claude")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob2"), claudeUsageRule{})
	feed(d, claudeLogs(claudeRecord("", "1", 5_000, fullClaudeTokens), claudeRecord("", "2", 5_000, fullClaudeTokens)))
	if got := len(stagedEvents(t, o)); got != 2 || d.Diagnostics().Derived != 2 {
		t.Errorf("missing request_id: %d events, %d derived", got, d.Diagnostics().Derived)
	}
}

// Missing fields are unknown (omitted), never 0; a malformed field drops
// only itself from the event (the counters keep dropping all tokens).
func TestUsageAnalyticsClaudeMissingAndMalformed(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), claudeUsageRule{})
	feed(d, claudeLogs(
		// cache_read_tokens absent.
		claudeRecord("", "1", 1_000, map[string]string{"input_tokens": "10", "output_tokens": "5", "cache_creation_tokens": "7"}),
		// input_tokens non-numeric, output_tokens negative.
		claudeRecord("", "2", 2_000, map[string]string{"input_tokens": "ten", "output_tokens": "-5", "cache_read_tokens": "3", "cache_creation_tokens": "0"}),
	))
	evs := stagedEvents(t, o)
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
	assertTokens(t, "missing cache_read", evs[0], tokenView{Input: i64(10), Output: i64(5), CacheWrite: i64(7), Derivation: "reported", Status: "partial"})
	assertTokens(t, "malformed", evs[1], tokenView{CacheRead: i64(3), CacheWrite: i64(0), Status: "malformed", Malformed: []string{"input_tokens", "output_tokens"}})
	if evs[1].Calls != 1 || d.Diagnostics().Malformed != 1 {
		t.Errorf("malformed event still counts one call: calls %d, malformed %d", evs[1].Calls, d.Diagnostics().Malformed)
	}
}

// --- Codex ----------------------------------------------------------------

// Captured codex 0.160.0: two completed responses, each
// input_token_count=1500 (inclusive), cached_token_count=1000,
// cache_write_token_count=300, output_token_count=80,
// reasoning_token_count=20, at distinct observed times; plus failures.
// Hand calculation: input = 1500 - 1000 - 300 = 200.
func TestUsageAnalyticsCodex160Fixture(t *testing.T) {
	setAnalyticsEnv(t, "codex")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), codexUsageRule{})
	feed(d, loadCodex160UsageFixture(t))
	var completed, failed []telemetrycontract.UsageEvent
	for _, ev := range stagedEvents(t, o) {
		if ev.SourceKind == "codex.response_completed" {
			completed = append(completed, ev)
		} else {
			failed = append(failed, ev)
		}
	}
	if len(completed) != 2 {
		t.Fatalf("completed events = %d, want 2", len(completed))
	}
	for _, ev := range completed {
		assertTokens(t, "codex completed", ev, tokenView{
			Input: i64(200), Output: i64(80), CacheRead: i64(1000), CacheWrite: i64(300), Reasoning: i64(20),
			Overlaps: true, Derivation: "derived", Status: "reported",
		})
		if ev.SourceIdentity != "composite" || ev.SourceOccurrence != 1 || ev.IdentityAmbiguous {
			t.Errorf("distinct-time completions must be certain: %+v", ev)
		}
		if ev.SessionID != "scrubbed-conversation-id" || ev.HarnessVersion != "0.160.0" || ev.Model != "gpt-5.1-codex" {
			t.Errorf("session %q harness_version %q model %q", ev.SessionID, ev.HarnessVersion, ev.Model)
		}
	}
	// Event and counter agree on every well-formed category.
	totalCalls := int64(0)
	for _, ev := range append(completed, failed...) {
		totalCalls += ev.Calls
		if ev.Status == "error" {
			assertTokens(t, ev.SourceKind, ev, tokenView{Status: "not_reported"})
		}
	}
	if totalCalls != d.Diagnostics().Derived {
		t.Errorf("event calls %d != counter-derived increments %d", totalCalls, d.Diagnostics().Derived)
	}
	kinds := map[string]bool{}
	for _, ev := range failed {
		kinds[ev.SourceKind] = true
	}
	if !kinds["codex.response_failed"] || !kinds["codex.api_request_failed"] {
		t.Errorf("failure kinds = %v", kinds)
	}
}

func codexCompleted(observed uint64, attrs map[string]string) *logspb.LogRecord {
	list := []*commonpb.KeyValue{kvs("event.kind", "response.completed"), kvs("conversation.id", "c-1"), kvs("event.timestamp", "2026-10-04T23:46:31.334Z")}
	for k, v := range attrs {
		list = append(list, kvs(k, v))
	}
	return &logspb.LogRecord{ObservedTimeUnixNano: observed, EventName: codexUsageEventName, Attributes: list}
}

func codexLogs(records ...*logspb.LogRecord) []*logspb.ResourceLogs {
	return []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "codex_otel.log_only"}, LogRecords: records}}}}
}

var codexTokens = map[string]string{"input_token_count": "100", "output_token_count": "10", "cached_token_count": "40", "cache_write_token_count": "60", "reasoning_token_count": "4"}

// Without cache_write_token_count, inclusive input cannot be derived: it is
// unknown, never computed as if cache_write were 0.
func TestUsageAnalyticsCodexInputNotDerivedFromUnknownCache(t *testing.T) {
	setAnalyticsEnv(t, "codex")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), codexUsageRule{})
	feed(d, codexLogs(codexCompleted(1, map[string]string{"input_token_count": "100", "output_token_count": "10", "cached_token_count": "40", "reasoning_token_count": "4"})))
	evs := stagedEvents(t, o)
	if len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	assertTokens(t, "no cache_write", evs[0], tokenView{Output: i64(10), CacheRead: i64(40), Reasoning: i64(4), Overlaps: true, Status: "partial"})
}

// Hand calculation for codexTokens: input = 100 - 40 - 60 = 0 (a reported,
// derived zero). Cached + cache_write > input is inconsistent: the three
// related fields are dropped and named, output/reasoning are kept.
func TestUsageAnalyticsCodexInclusiveAndInconsistent(t *testing.T) {
	setAnalyticsEnv(t, "codex")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), codexUsageRule{})
	bad := map[string]string{"input_token_count": "100", "output_token_count": "10", "cached_token_count": "90", "cache_write_token_count": "20", "reasoning_token_count": "4"}
	feed(d, codexLogs(codexCompleted(1, codexTokens), codexCompleted(2, bad)))
	evs := stagedEvents(t, o)
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
	assertTokens(t, "inclusive", evs[0], tokenView{Input: i64(0), Output: i64(10), CacheRead: i64(40), CacheWrite: i64(60), Reasoning: i64(4), Overlaps: true, Derivation: "derived", Status: "reported"})
	assertTokens(t, "inconsistent", evs[1], tokenView{Output: i64(10), Reasoning: i64(4), Overlaps: true, Status: "malformed",
		Malformed: []string{"input_token_count", "cached_token_count", "cache_write_token_count"}})
}

// Same-millisecond distinct calls (different nanosecond observed time or
// different attributes) are two certain calls; a byte-identical repeat is
// one certain plus one ambiguous occurrence (never silently collapsed),
// while the counters (LRU) count it once.
func TestUsageAnalyticsCodexCollisionsAndRepeats(t *testing.T) {
	setAnalyticsEnv(t, "codex")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), codexUsageRule{})
	a := codexCompleted(1_791_157_591_336_000_001, codexTokens)
	b := codexCompleted(1_791_157_591_336_000_002, codexTokens) // same ms, other ns
	other := map[string]string{"input_token_count": "100", "output_token_count": "11", "cached_token_count": "40", "cache_write_token_count": "60", "reasoning_token_count": "4"}
	c := codexCompleted(1_791_157_591_336_000_001, other) // same ns, other tokens
	repeat := proto.Clone(a).(*logspb.LogRecord)
	feed(d, codexLogs(a, b, c, repeat))

	evs := stagedEvents(t, o)
	if len(evs) != 4 {
		t.Fatalf("events = %d, want 4", len(evs))
	}
	certain, ambiguous := 0, 0
	for _, ev := range evs {
		if ev.IdentityAmbiguous {
			ambiguous++
			if ev.SourceOccurrence != 2 || ev.SourceEventID != evs[0].SourceEventID {
				t.Errorf("repeat = %+v", ev)
			}
		} else {
			certain++
		}
	}
	if certain != 3 || ambiguous != 1 {
		t.Errorf("certain %d ambiguous %d, want 3 and 1", certain, ambiguous)
	}
	if d.Diagnostics().Derived != 3 || d.Diagnostics().Duplicate != 1 {
		t.Errorf("counters %+v, want 3 derived 1 duplicate", d.Diagnostics())
	}
}

// Documented overcount risk: for a source with only observed time, a
// re-stamped observed time gives a different composite identity.
func TestUsageAnalyticsObservedTimeChangeSplitsIdentity(t *testing.T) {
	setAnalyticsEnv(t, "codex")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), codexUsageRule{})
	feed(d, codexLogs(codexCompleted(100, codexTokens), codexCompleted(200, codexTokens)))
	evs := stagedEvents(t, o)
	if len(evs) != 2 || evs[0].SourceEventID == evs[1].SourceEventID || evs[1].IdentityAmbiguous {
		t.Errorf("observed-time change must give two identities: %+v", evs)
	}
}

// After a restart (and so after the LRU's TTL/capacity), a replayed
// composite record is re-derived by the fresh counter LRU but reported by
// the persisted index as an ambiguous occurrence -- never as a silent
// second certain call.
func TestUsageAnalyticsReplayAfterRestartIsAmbiguous(t *testing.T) {
	setAnalyticsEnv(t, "codex")
	dir := filepath.Join(t.TempDir(), "ob")
	d, o := analyticsDeriver(t, dir, codexUsageRule{})
	rec := codexCompleted(42, codexTokens)
	feed(d, codexLogs(rec))
	d2, _ := analyticsDeriver(t, dir, codexUsageRule{})
	feed(d2, codexLogs(proto.Clone(rec).(*logspb.LogRecord)))
	evs := stagedEvents(t, o)
	if len(evs) != 2 || evs[1].SourceOccurrence != 2 || !evs[1].IdentityAmbiguous || evs[0].EventID == evs[1].EventID {
		t.Fatalf("replay after restart: %+v", evs)
	}
	if evs[0].ProducerSeq != 1 || evs[1].ProducerSeq != 2 {
		t.Errorf("seq = %d, %d", evs[0].ProducerSeq, evs[1].ProducerSeq)
	}
}

// Timestamp collision across different event kinds of the same source:
// an error frame and a completion at the same instant stay distinct.
func TestUsageAnalyticsTimestampCollisionAcrossKinds(t *testing.T) {
	setAnalyticsEnv(t, "codex")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), codexUsageRule{})
	failure := codexCompleted(7, map[string]string{"error.message": "boom"})
	feed(d, codexLogs(codexCompleted(7, codexTokens), failure))
	evs := stagedEvents(t, o)
	if len(evs) != 2 || evs[0].SourceEventID == evs[1].SourceEventID || evs[1].SourceKind != "codex.response_failed" {
		t.Errorf("events = %+v", evs)
	}
}

// --- gemini-cli -----------------------------------------------------------

// Captured gemini-cli 0.62.0 api_response: input_token_count=1500,
// cached_content_token_count=1200, tool_token_count=10,
// output_token_count=80, thoughts_token_count=20. Hand calculation:
// input = 1500 - 1200 + 10 = 310; output = 80 + 20 = 100.
func TestUsageAnalyticsGeminiFixture(t *testing.T) {
	setAnalyticsEnv(t, "gemini-cli")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), geminiCLIUsageRule{})
	feed(d, loadGeminiCLIUsageFixture(t))
	var resp *telemetrycontract.UsageEvent
	evs := stagedEvents(t, o)
	for i := range evs {
		if evs[i].SourceKind == "gemini-cli.api_response" {
			resp = &evs[i]
		}
	}
	if resp == nil {
		t.Fatalf("events = %+v", evs)
	}
	assertTokens(t, "gemini", *resp, tokenView{Input: i64(310), CacheRead: i64(1200), Output: i64(100), Reasoning: i64(20), Overlaps: true, Derivation: "derived", Status: "reported"})
}

// --- copilot (metric-derived) -------------------------------------------

func TestUsageAnalyticsCopilotIntervals(t *testing.T) {
	setAnalyticsEnv(t, "copilot")
	o, err := OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "ob"))
	if err != nil {
		t.Fatal(err)
	}
	rule := newCopilotUsageRule()
	rule.converter = newCumulativeToDeltaConverter(0, copilotCumulativeStateCapacity) // trust the fixture's streams
	d := bareUsageDeriver(rule)
	d.analytics = NewUsageAnalytics(o, telemetrycontract.UsageSourceNative)
	d.observeMetricBatch(context.Background(), rule, loadCopilotUsageFixture(t), map[string]bool{})

	evs := stagedEvents(t, o)
	if len(evs) == 0 {
		t.Fatal("no copilot events")
	}
	var calls int64
	for _, ev := range evs {
		if ev.SourceIdentity != "metric_interval" || ev.IntervalStart == "" || ev.IntervalEnd == "" || ev.Temporality == "" {
			t.Errorf("metric event lacks interval semantics: %+v", ev)
		}
		if ev.IntervalStart > ev.IntervalEnd {
			t.Errorf("interval start %s after end %s", ev.IntervalStart, ev.IntervalEnd)
		}
		switch ev.SourceKind {
		case "copilot.metrics.calls":
			calls += ev.Calls
			assertTokens(t, "calls event", ev, tokenView{Status: "not_reported"})
		case "copilot.metrics.tokens":
			if ev.Calls != 0 {
				t.Errorf("token event calls = %d", ev.Calls)
			}
		default:
			t.Errorf("unexpected kind %q", ev.SourceKind)
		}
	}
	if calls == 0 {
		t.Error("no calls in copilot events")
	}
}

// --- selection, delivery, identity ---------------------------------------

// No analytics producer means no outbox writes: a deriver without analytics
// (the pipeline without a GCP exporter) writes nothing.
func TestUsageAnalyticsDisabledWritesNothing(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	d := bareUsageDeriver(claudeUsageRule{})
	feed(d, loadClaudeUsageFixture(t))
	if d.Diagnostics().Derived == 0 {
		t.Fatal("counters must still work")
	}

	var a analyticsState
	a.outboxDir = filepath.Join(t.TempDir(), "never")
	a.start(context.Background(), nil)
	if a.disabledReason != AnalyticsDisabledNoGCPExporter || a.native != nil {
		t.Errorf("state = %+v", a)
	}
	if _, err := os.Stat(a.outboxDir); !os.IsNotExist(err) {
		t.Error("outbox must not be created without a GCP exporter")
	}
	for _, cfg := range []*Config{nil, {Enabled: true}, {Enabled: true, CloudEnabled: true, CloudProvider: "otlp", Endpoint: "x"}} {
		if AnalyticsEnabledForHooks(cfg) {
			t.Errorf("AnalyticsEnabledForHooks(%+v) = true", cfg)
		}
	}
}

func TestUsageAnalyticsNoAgentIDDropsEvent(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	t.Setenv("SCION_AGENT_ID", "")
	d, o := analyticsDeriver(t, filepath.Join(t.TempDir(), "ob"), claudeUsageRule{})
	feed(d, loadClaudeUsageFixture(t))
	if len(stagingLines(t, o)) != 0 || d.analytics.DroppedNoIdentity() == 0 {
		t.Errorf("events without agent identity must be dropped and counted")
	}
}

func TestHealthzHubIDResolver(t *testing.T) {
	for name, body := range map[string]string{
		"hub server":      `{"status":"healthy","hub_id":"hub-123","hub_name":"n"}`,
		"composite web":   `{"status":"healthy","web":{},"hub":{"status":"healthy","hub_id":"hub-123"}}`,
		"degraded status": `{"status":"degraded","hub_id":"hub-123"}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected request %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
				}
				if strings.Contains(body, "degraded") {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			id, err := NewHealthzHubIDResolver(srv.URL+"/", srv.Client())(context.Background())
			if err != nil || id != "hub-123" {
				t.Errorf("resolved %q, %v", id, err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"healthy"}`)) }))
	defer srv.Close()
	if _, err := NewHealthzHubIDResolver(srv.URL, srv.Client())(context.Background()); err == nil {
		t.Error("a /healthz without hub_id must not resolve")
	}
	if _, err := NewHealthzHubIDResolver("", nil)(context.Background()); err == nil {
		t.Error("no endpoint must not resolve")
	}
}

// End to end through the pipeline's analytics wiring: native increment ->
// outbox -> drainer (hub ID pinned from the resolver) -> the dedicated
// scion-analytics entry as structured JSON with event_ts as timestamp.
func TestAnalyticsStateEndToEnd(t *testing.T) {
	setAnalyticsEnv(t, "claude")
	var entries []logging.Entry
	gcp := &GCPExporter{analyticsSink: func(e logging.Entry) error { entries = append(entries, e); return nil }}
	a := analyticsState{
		outboxDir: filepath.Join(t.TempDir(), "ob"),
		resolver:  func(context.Context) (string, error) { return "hub-xyz", nil },
	}
	a.start(context.Background(), gcp)
	if a.native == nil || a.disabledReason != "" {
		t.Fatalf("analytics not enabled: %+v", a)
	}
	d := bareUsageDeriver(claudeUsageRule{})
	d.analytics = a.native
	feed(d, claudeLogs(claudeRecord("req_011CUabcdefghijklmnopq", "1", 1_761_091_200_000_000_000, fullClaudeTokens)))
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.stop(stopCtx)

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	raw, ok := entries[0].Payload.(json.RawMessage)
	if !ok {
		t.Fatalf("payload type %T, want json.RawMessage (structured jsonPayload)", entries[0].Payload)
	}
	var ev telemetrycontract.UsageEvent
	if err := telemetrycontract.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.HubID != "hub-xyz" || ev.SchemaVersion != 1 || ev.EventType != "agent.usage" {
		t.Errorf("event = %+v", ev.Envelope)
	}
	if !entries[0].Timestamp.Equal(time.Unix(0, 1_761_091_200_000_000_000)) || entries[0].Severity != logging.Info {
		t.Errorf("entry timestamp %v severity %v", entries[0].Timestamp, entries[0].Severity)
	}
	if telemetrycontract.AnalyticsLogID != "scion-analytics" {
		t.Error("log ID drifted from the frozen name")
	}
}
