/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	mexporter "github.com/GoogleCloudPlatform/opentelemetry-operations-go/exporter/metric"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const claudeUsageFixturePath = "testdata/usage/claude-2.1.280.pb.json"

// loadClaudeUsageFixture loads the captured, secret-scrubbed Claude Code
// 2.1.280 payload (api_request success, api_error failure, plus unrelated
// events) used to pin the Claude usage rule.
func loadClaudeUsageFixture(t *testing.T) []*logspb.ResourceLogs {
	t.Helper()
	data, err := os.ReadFile(claudeUsageFixturePath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	var req collogspb.ExportLogsServiceRequest
	if err := protojson.Unmarshal(data, &req); err != nil {
		t.Fatalf("unmarshaling fixture: %v", err)
	}
	if len(req.ResourceLogs) == 0 {
		t.Fatal("fixture has no resource logs")
	}
	return req.ResourceLogs
}

// mustEventName is normalizedLogEventName for tests that exercise
// claudeUsageRule.MatchLog directly: MatchLog takes the already-computed
// event name rather than deriving it itself, so tests calling it directly
// compute it the same way observe() does.
func mustEventName(t *testing.T, record *logspb.LogRecord, scope string) string {
	t.Helper()
	name, err := normalizedLogEventName(record, scope)
	if err != nil {
		t.Fatalf("normalizedLogEventName: %v", err)
	}
	return name
}

// fixtureRecordsByEvent returns every log record in the fixture whose
// event.name attribute equals name.
func fixtureRecordsByEvent(t *testing.T, name string) []*logspb.LogRecord {
	t.Helper()
	var out []*logspb.LogRecord
	for _, rl := range loadClaudeUsageFixture(t) {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				if logAttrString(record.Attributes, "event.name") == name {
					out = append(out, record)
				}
			}
		}
	}
	return out
}

func TestClaudeUsageRuleMatchesFixtureAPIRequest(t *testing.T) {
	records := fixtureRecordsByEvent(t, "api_request")
	if len(records) != 1 {
		t.Fatalf("fixture api_request records = %d, want 1", len(records))
	}
	increment, matched, err := claudeUsageRule{}.MatchLog(claudeUsageScope, mustEventName(t, records[0], claudeUsageScope), records[0])
	if err != nil {
		t.Fatalf("MatchLog error: %v", err)
	}
	if !matched {
		t.Fatal("api_request did not match claudeUsageRule")
	}
	if increment.Calls != 1 || increment.Status != "success" || increment.Model != "claude-sonnet-5" {
		t.Fatalf("increment = %+v", increment)
	}
	want := map[string]int64{"input": 2, "output": 41, "cache_write": 26607}
	if len(increment.Tokens) != len(want) {
		t.Fatalf("tokens = %+v, want %+v", increment.Tokens, want)
	}
	for k, v := range want {
		if increment.Tokens[k] != v {
			t.Errorf("tokens[%q] = %d, want %d", k, increment.Tokens[k], v)
		}
	}
	// cache_read_tokens was 0 in the capture: a zero-valued type must not
	// appear (design §3.3 "Tokens map[string]int64 // token_type -> n (>=0)").
	if _, ok := increment.Tokens["cache_read"]; ok {
		t.Error("zero-valued cache_read token_type must be omitted")
	}
}

func TestClaudeUsageRuleMatchesFixtureAPIError(t *testing.T) {
	records := fixtureRecordsByEvent(t, "api_error")
	if len(records) != 1 {
		t.Fatalf("fixture api_error records = %d, want 1", len(records))
	}
	increment, matched, err := claudeUsageRule{}.MatchLog(claudeUsageScope, mustEventName(t, records[0], claudeUsageScope), records[0])
	if err != nil {
		t.Fatalf("MatchLog error: %v", err)
	}
	if !matched {
		t.Fatal("api_error did not match claudeUsageRule")
	}
	if increment.Calls != 1 || increment.Status != "error" || len(increment.Tokens) != 0 {
		t.Fatalf("increment = %+v", increment)
	}
}

func TestClaudeUsageRuleIgnoresUnrelatedEvents(t *testing.T) {
	rule := claudeUsageRule{}
	for _, name := range []string{"hook_execution_start", "user_prompt", "assistant_response"} {
		records := fixtureRecordsByEvent(t, name)
		if len(records) == 0 {
			t.Fatalf("fixture missing %s records", name)
		}
		if _, matched, err := rule.MatchLog(claudeUsageScope, mustEventName(t, records[0], claudeUsageScope), records[0]); matched || err != nil {
			t.Errorf("event %s: matched=%v err=%v, want matched=false err=nil", name, matched, err)
		}
	}
	// Wrong scope never matches, even for a real api_request record.
	apiRequest := fixtureRecordsByEvent(t, "api_request")[0]
	if _, matched, _ := rule.MatchLog("some.other.scope", mustEventName(t, apiRequest, claudeUsageScope), apiRequest); matched {
		t.Error("claudeUsageRule matched outside its native scope")
	}
}

func TestClaudeUsageRuleMalformedTokenField(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
		{Key: "model", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "claude-sonnet-5"}}},
		{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "not-a-number"}}},
	}}
	increment, matched, err := claudeUsageRule{}.MatchLog(claudeUsageScope, mustEventName(t, record, claudeUsageScope), record)
	if !matched {
		t.Fatal("expected the malformed api_request to still match (so it counts as usage_malformed, not silently ignored)")
	}
	if err == nil {
		t.Fatal("expected a malformed-field error")
	}
	// The call itself is still counted, with no tokens at all (not a
	// partial total) for this event.
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusSuccess {
		t.Fatalf("increment = %+v, want Calls=1 Status=success even when malformed", increment)
	}
	if len(increment.Tokens) != 0 {
		t.Fatalf("increment.Tokens = %+v, want none when any token field is malformed", increment.Tokens)
	}
}

func TestClaudeUsageRuleNegativeTokenField(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
		{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: -1}}},
	}}
	increment, matched, err := (claudeUsageRule{}).MatchLog(claudeUsageScope, mustEventName(t, record, claudeUsageScope), record)
	if !matched || err == nil {
		t.Fatalf("matched=%v err=%v, want matched=true err!=nil", matched, err)
	}
	if increment.Calls != 1 {
		t.Fatalf("increment.Calls = %d, want 1 even when malformed", increment.Calls)
	}
}

// TestClaudeUsageRuleTokenFieldTypeTolerance pins: the fixture itself
// mixes value types across attributes on the same event, and an unpinned
// CLI (harnesses/claude installs @latest) can change encodings across
// releases. A string-encoded non-negative integer and an integral double
// must both be accepted, not treated as malformed.
func TestClaudeUsageRuleTokenFieldTypeTolerance(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
		{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "5"}}},
		{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 7}}},
	}}
	increment, matched, err := (claudeUsageRule{}).MatchLog(claudeUsageScope, mustEventName(t, record, claudeUsageScope), record)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil", matched, err)
	}
	if increment.Tokens["input"] != 5 || increment.Tokens["output"] != 7 {
		t.Fatalf("increment.Tokens = %+v, want input=5 output=7", increment.Tokens)
	}
}

// TestClaudeUsageRuleFractionalDoubleTokenFieldIsMalformed complements the
// tolerance test: a non-integral double is still malformed, not silently
// truncated.
func TestClaudeUsageRuleFractionalDoubleTokenFieldIsMalformed(t *testing.T) {
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
		{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 1.5}}},
	}}
	if _, matched, err := (claudeUsageRule{}).MatchLog(claudeUsageScope, mustEventName(t, record, claudeUsageScope), record); !matched || err == nil {
		t.Fatalf("matched=%v err=%v, want matched=true err!=nil for a fractional double", matched, err)
	}
}

// TestClaudeUsageRuleMatchesEventNameField pins: a native SDK may
// carry the event name in LogRecord's own EventName field instead of the
// event.name attribute. The rule must recognize either.
func TestClaudeUsageRuleMatchesEventNameField(t *testing.T) {
	record := &logspb.LogRecord{
		EventName: "api_error",
	}
	increment, matched, err := (claudeUsageRule{}).MatchLog(claudeUsageScope, mustEventName(t, record, claudeUsageScope), record)
	if !matched || err != nil {
		t.Fatalf("matched=%v err=%v, want matched=true err=nil for a native EventName field", matched, err)
	}
	if increment.Calls != 1 || increment.Status != telemetrycontract.StatusError {
		t.Fatalf("increment = %+v, want Calls=1 Status=error", increment)
	}
}

func TestBoundedLRUDedupeCapacityAndTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newBoundedLRU(2, time.Minute)
	l.now = func() time.Time { return now }

	if l.SeenBefore("a") {
		t.Fatal("first sighting of a must not be a duplicate")
	}
	if !l.SeenBefore("a") {
		t.Fatal("second sighting of a within TTL must be a duplicate")
	}
	// Exceed capacity: "a" is evicted once "b" and "c" both arrive.
	l.SeenBefore("b")
	l.SeenBefore("c")
	if l.SeenBefore("a") {
		t.Fatal("a should have been evicted by capacity and count as new again")
	}
	// TTL expiry: advance past the window and confirm "b" is forgotten too.
	now = now.Add(2 * time.Minute)
	if l.SeenBefore("b") {
		t.Fatal("b should have expired via TTL")
	}
}

// TestBoundedLRUCapacityEvictionZeroesSlot pins that capacity eviction in
// SeenBefore zeroes the evicted lruEntry before reslicing, not just drops it
// from the visible window: l.order[1:] shares the same backing array as
// before the reslice, so until a later append forces a reallocation, the
// evicted entry's string would otherwise stay reachable (and alive to the
// GC) through that array. Captures the slice header before the eviction
// (same backing array, same capacity) and inspects index 0 through that
// captured header afterward, since the live l.order no longer exposes it
// once evicted.
func TestBoundedLRUCapacityEvictionZeroesSlot(t *testing.T) {
	l := newBoundedLRU(2, time.Minute)
	// Pre-size with headroom so the append below (which grows l.order to
	// len 3, one past capacity) reuses this backing array instead of
	// triggering Go's own growth reallocation -- otherwise the eviction's
	// zeroing would land on a freshly allocated array that beforeEviction
	// never pointed at, and the test would pass or fail by accident of the
	// growth heuristic rather than by testing the fix.
	l.order = make([]lruEntry, 0, 8)
	l.SeenBefore("a")
	l.SeenBefore("b")
	beforeEviction := l.order // same backing array as after the next call
	l.SeenBefore("c")         // capacity 2 exceeded: "a" (index 0) is evicted
	if len(beforeEviction) < 1 {
		t.Fatal("test setup: expected at least one entry in the pre-eviction slice")
	}
	if got := beforeEviction[0]; got != (lruEntry{}) {
		t.Fatalf("evicted slot = %+v, want zero value (the entry must not stay reachable through the shared backing array)", got)
	}
}

// TestBoundedLRUTTLEvictionZeroesSlots is the same pin as
// TestBoundedLRUCapacityEvictionZeroesSlot, for evictExpired's TTL path: the
// evicted prefix must be zeroed before l.order = l.order[cut:].
func TestBoundedLRUTTLEvictionZeroesSlots(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newBoundedLRU(8, time.Minute)
	l.now = func() time.Time { return now }
	// Pre-size with headroom (see TestBoundedLRUCapacityEvictionZeroesSlot):
	// evictExpired only reslices, so its own zeroing can't reallocate, but
	// pre-sizing keeps this test's setup insertions from doing so either,
	// so beforeEviction is guaranteed to alias the same backing array.
	l.order = make([]lruEntry, 0, 8)
	l.SeenBefore("a")
	l.SeenBefore("b")
	beforeEviction := l.order // same backing array as after expiry runs
	now = now.Add(2 * time.Minute)
	l.SeenBefore("c") // evictExpired runs first and expires "a" and "b"
	if len(beforeEviction) < 2 {
		t.Fatal("test setup: expected at least two entries in the pre-eviction slice")
	}
	for i, got := range beforeEviction[:2] {
		if got != (lruEntry{}) {
			t.Fatalf("evicted slot %d = %+v, want zero value (the entry must not stay reachable through the shared backing array)", i, got)
		}
	}
}

// TestBoundedLRUSeenBeforeConcurrent is a regression test: the receiver
// runs each OTLP export request on its own goroutine (Pipeline.handleLogs is
// not serialized), so SeenBefore must be safe under concurrent callers. Run
// with -race; before the mutex fix this both raced and could panic with
// "concurrent map read and map write".
func TestBoundedLRUSeenBeforeConcurrent(t *testing.T) {
	const goroutines = 16
	const perGoroutine = 200
	// Capacity comfortably above the total number of distinct keys this test
	// inserts, so capacity-driven eviction of "shared" cannot itself cause a
	// second "not seen before" and confound the atomicity assertion below.
	l := newBoundedLRU(goroutines*perGoroutine+8, time.Minute)
	var wg sync.WaitGroup
	var seenBefore atomic.Int64
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				// A shared key, replayed from every goroutine, exercises the
				// check-and-insert race directly.
				if l.SeenBefore("shared") {
					seenBefore.Add(1)
				}
				// Distinct keys exercise concurrent eviction bookkeeping.
				l.SeenBefore(fmt.Sprintf("g%d-%d", g, i))
			}
		}(g)
	}
	wg.Wait()
	// Exactly one caller, across every goroutine, can be the first to see
	// "shared"; every other observation (goroutines*perGoroutine - 1) must
	// report it was seen before. Atomicity of check-and-insert is what
	// guarantees this count, not just the absence of a crash.
	if want := int64(goroutines*perGoroutine - 1); seenBefore.Load() != want {
		t.Fatalf("seenBefore count = %d, want %d (check-and-insert must be atomic)", seenBefore.Load(), want)
	}
}

// TestUsageDeriverObserveConcurrent exercises the same race at the
// UsageDeriver level, through ProcessResourceLogs and observe, which is what
// the receiver actually calls concurrently.
func TestUsageDeriverObserveConcurrent(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	const goroutines = 16
	const perGoroutine = 50
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				distinct := &logspb.LogRecord{
					TimeUnixNano: uint64(g*perGoroutine + i + 1),
					Attributes: []*commonpb.KeyValue{
						{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_error"}}},
					},
				}
				d.observe(context.Background(), claudeUsageScope, distinct)
				shared := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
					{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_error"}}},
				}}
				d.observe(context.Background(), claudeUsageScope, shared)
			}
		}(g)
	}
	wg.Wait()
	diag := d.Diagnostics()
	wantTotal := int64(goroutines * perGoroutine * 2)
	if diag.Derived+diag.Duplicate != wantTotal {
		t.Fatalf("derived(%d)+duplicate(%d) = %d, want %d", diag.Derived, diag.Duplicate, diag.Derived+diag.Duplicate, wantTotal)
	}
}

// bareUsageDeriver builds a UsageDeriver with no MeterProvider, exercising
// observe()/dedupe/diagnostics without any network dependency. record()
// guards every use of d.calls/d.tokens against nil, so this is safe.
func bareUsageDeriver(rules ...usageRule) *UsageDeriver {
	return &UsageDeriver{
		rules:            rules,
		seen:             newBoundedLRU(usageDedupeCapacity, usageDedupeTTL),
		resourceIdentity: "test-resource",
	}
}

func TestUsageDeriverObserveDedupesReplayedRequest(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	record := fixtureRecordsByEvent(t, "api_request")[0]

	if !d.observe(context.Background(), claudeUsageScope, record) {
		t.Fatal("first observation should record")
	}
	if d.observe(context.Background(), claudeUsageScope, record) {
		t.Fatal("replayed observation should be deduped, not recorded again")
	}
	diag := d.Diagnostics()
	if diag.Derived != 1 || diag.Duplicate != 1 || diag.Malformed != 0 {
		t.Fatalf("diagnostics = %+v", diag)
	}
}

// TestUsageDeriverObserveCountsMalformed pins that a call is a completed
// response, so it is still counted even when one of its token fields could
// not be parsed. Only the tokens for that event are dropped.
func TestUsageDeriverObserveCountsMalformed(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	record := &logspb.LogRecord{Attributes: []*commonpb.KeyValue{
		{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
		{Key: "input_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "bogus"}}},
	}}
	if !d.observe(context.Background(), claudeUsageScope, record) {
		t.Fatal("the call must still be recorded even though a token field is malformed")
	}
	diag := d.Diagnostics()
	if diag.Malformed != 1 || diag.Derived != 1 {
		t.Fatalf("diagnostics = %+v, want Malformed=1 Derived=1", diag)
	}
	// A second malformed event must still be counted (only the Warn log is
	// deduplicated, not the diagnostic counter or the derivation itself).
	record2 := &logspb.LogRecord{
		TimeUnixNano: 1,
		Attributes: []*commonpb.KeyValue{
			{Key: "event.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "api_request"}}},
			{Key: "output_tokens", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "also-bogus"}}},
		},
	}
	if !d.observe(context.Background(), claudeUsageScope, record2) {
		t.Fatal("a second, distinct malformed event must also be recorded")
	}
	diag = d.Diagnostics()
	if diag.Malformed != 2 || diag.Derived != 2 {
		t.Fatalf("diagnostics after second malformed event = %+v, want Malformed=2 Derived=2", diag)
	}
}

func TestUsageDeriverObserveIgnoresRecordWithNoEventName(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	if d.observe(context.Background(), claudeUsageScope, &logspb.LogRecord{}) {
		t.Fatal("a record with no event.name must never be recorded")
	}
}

func TestNewUsageDeriverIsNoOpWithoutNativeSource(t *testing.T) {
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "hooks") // not "native"
	d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 0 {
		t.Fatal("deriver must be a no-op unless SCION_USAGE_SOURCE=native")
	}
	// A no-op deriver must tolerate every call.
	d.ProcessResourceLogs(context.Background(), loadClaudeUsageFixture(t))
	if diag := d.Diagnostics(); diag != (UsageDiagnostics{}) {
		t.Fatalf("no-op deriver diagnostics = %+v", diag)
	}
}

func TestNewUsageDeriverIsNoOpForUnknownHarness(t *testing.T) {
	t.Setenv("SCION_HARNESS", "some-future-harness")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	d, err := NewUsageDeriver(context.Background(), &Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.rules) != 0 {
		t.Fatal("a harness with no rule must yield a no-op deriver")
	}
}

// TestPipelineDerivesClaudeUsageEndToEnd is the §7.5 pipeline end-to-end
// test: it posts the captured Claude fixture over OTLP/HTTP through the real
// receiver, force-flushes, and asserts the exported GCP ResourceMetrics
// contain gen_ai.api.calls and scion.usage.tokens points with exactly the
// canonical label set (AC-1.1, AC-1.1b). The policy's Filter.Include is set
// to exclude every native Claude event, proving derivation runs before the
// filter (AC-1.4), and the fixture is replayed to prove a repeat doesn't
// double count (AC-1.4).
func TestPipelineDerivesClaudeUsageEndToEnd(t *testing.T) {
	// The exported gen_ai.api.calls/scion.usage.tokens resource is built from
	// every env var authoritativeIdentity() (policy.go) and buildResource()
	// (providers.go) read — SCION_AGENT_ID, SCION_AGENT_SLUG, SCION_PROJECT_ID
	// (via projectkeys.ProjectIDFromEnv), SCION_HARNESS, SCION_MODEL,
	// SCION_BROKER_ID, SCION_BROKER_NAME, and SCION_GCP_PROJECT_ID
	// (config.EnvProjectID) — and scion_metric_resource_id (part of the
	// golden comparison below) is a digest of that whole resource. Every one
	// of those must be pinned, not just the ones this test's own assertions
	// name, or an ambient value for any of them (this container sets several,
	// e.g. SCION_BROKER_NAME/SCION_BROKER_ID/SCION_MODEL for its own agent
	// identity) leaks into the digest and makes the golden comparison
	// non-hermetic — it can pass here and fail in CI or under env -i for a
	// reason with no visible connection to this test's inputs.
	t.Setenv("SCION_AGENT_ID", "agent-usage-1")
	t.Setenv("SCION_AGENT_SLUG", "usage-agent-slug")
	t.Setenv("SCION_PROJECT_ID", "project-usage-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_MODEL", "")
	t.Setenv("SCION_BROKER_ID", "")
	t.Setenv("SCION_BROKER_NAME", "")
	t.Setenv("SCION_GCP_PROJECT_ID", "")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	capture := &monitoringCapture{}
	server := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(server, capture)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	sdkExporter, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sdkExporter.Shutdown(context.Background()) }()

	cfg := &Config{
		Enabled: true, CloudProvider: "gcp", GRPCPort: availableTCPPort(t), HTTPPort: 0,
		// No real event name is included, so every raw Claude log record
		// (including api_request/api_error) is dropped by the filter. The
		// derived usage metrics must still appear.
		Filter: FilterConfig{Include: []string{"nonexistent_event"}},
	}
	p := NewWithConfig(cfg)
	p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: sdkExporter}}
	// A controllable, fixed clock, rather than real sleeps or time.Now, drives
	// the collector's own admission bookkeeping (metric_streams.go):
	// snapshotGCP requires at least 2ms between a hook-style point's
	// collector epoch and its observed end (so the pinned Monitoring SDK
	// never has to rewrite a near-zero interval), and at least 5s between two
	// exports of the same GCP identity (metricPossibleEnds' sampling-interval
	// floor). This test exercises the second guard, which a short real sleep
	// cannot satisfy without slowing every test run.
	//
	// The base is a fixed date, not time.Now(): the golden fixture below
	// is checked in, so its own comparison (this package, this test)
	// must reproduce byte-identical Interval timestamps on every run, not
	// only the run that captured it. It is pinned near "today" rather than
	// an arbitrary date because pkg/hub's golden test (which loads the same
	// file) evaluates it against the dashboard's real, wall-clock query
	// window — see that test's own comment for the resulting staleness
	// caveat.
	now := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	p.metricNow = func() time.Time { return now }

	receiver := NewReceiver(cfg, nil, WithLogHandler(p.handleLogs), WithMetricHandler(p.handleMetrics))
	if err := receiver.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Stop(context.Background()) }()

	deriver, err := NewUsageDeriver(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(deriver.rules) == 0 {
		t.Fatal("expected the claude usage rule to be active")
	}
	p.usageDeriver.Store(deriver)
	defer func() { _ = deriver.Shutdown(context.Background()) }()

	postResourceLogs := func(resourceLogs []*logspb.ResourceLogs) {
		t.Helper()
		body, err := proto.Marshal(&collogspb.ExportLogsServiceRequest{ResourceLogs: resourceLogs})
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		receiver.handleHTTPLogs(rec, otlpHTTPRequest("/v1/logs", bytes.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("post status = %d: %s", rec.Code, rec.Body.String())
		}
	}
	postFixture := func() {
		t.Helper()
		postResourceLogs(loadClaudeUsageFixture(t))
	}

	postFixture()
	now = now.Add(10 * time.Millisecond)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("first metric flush not confirmed")
	}

	diag := p.UsageDiagnostics()
	if diag.Derived != 2 || diag.Duplicate != 0 || diag.Malformed != 0 {
		t.Fatalf("diagnostics after first post = %+v", diag)
	}

	series := allCapturedSeries(capture)
	assertUsageSeries(t, series)

	firstSuccessCalls := latestSeries(t, series, "workload.googleapis.com/gen_ai.api.calls", "success")

	// Replay: a retried request must not double count (AC-1.4). Assert
	// this explicitly — no new series, and the already-exported values are
	// unchanged — rather than relying on the replay simply exporting nothing
	// to make the assertion below trivially pass.
	postFixture()
	now = now.Add(10 * time.Millisecond)
	p.flushMetricBuffer(context.Background(), true)
	diag = p.UsageDiagnostics()
	if diag.Derived != 2 || diag.Duplicate != 2 {
		t.Fatalf("diagnostics after replay = %+v, want Derived=2 Duplicate=2", diag)
	}
	afterReplay := allCapturedSeries(capture)
	if len(afterReplay) != len(series) {
		t.Fatalf("replay exported %d series (total captured), want %d unchanged (a duplicate request must export nothing new)", len(afterReplay), len(series))
	}
	assertUsageSeries(t, afterReplay)
	if got := latestSeries(t, afterReplay, "workload.googleapis.com/gen_ai.api.calls", "success"); got.Points[0].Value.GetInt64Value() != firstSuccessCalls.Points[0].Value.GetInt64Value() {
		t.Fatalf("gen_ai.api.calls/success value changed after a deduped replay: %d -> %d", firstSuccessCalls.Points[0].Value.GetInt64Value(), got.Points[0].Value.GetInt64Value())
	}

	// A genuinely new event (distinct request_id, so not deduped) that
	// maps to the same stream must be cumulative, not reset — same
	// collector-epoch StartTime as the first flush, EndTime strictly later,
	// value accumulated rather than replaced (design §7.5).
	secondRequest := loadClaudeUsageFixture(t)
	mutateRequestID(t, secondRequest, "api_request", "synthetic-request-2")
	// The same GCP identity cannot be exported twice within 5s (the
	// sampling-interval floor above); advance past it.
	now = now.Add(5*time.Second + 100*time.Millisecond)
	postResourceLogs(secondRequest)
	now = now.Add(10 * time.Millisecond)
	if !p.flushMetricBuffer(context.Background(), true) {
		t.Fatal("third metric flush not confirmed")
	}
	diag = p.UsageDiagnostics()
	if diag.Derived != 3 {
		t.Fatalf("diagnostics after a genuinely new request = %+v, want Derived=3", diag)
	}
	secondSuccessCalls := latestSeries(t, allCapturedSeries(capture), "workload.googleapis.com/gen_ai.api.calls", "success")
	if !secondSuccessCalls.Points[0].Interval.StartTime.AsTime().Equal(firstSuccessCalls.Points[0].Interval.StartTime.AsTime()) {
		t.Fatalf("gen_ai.api.calls/success StartTime moved: %v -> %v, want the collector epoch stable across flushes",
			firstSuccessCalls.Points[0].Interval.StartTime.AsTime(), secondSuccessCalls.Points[0].Interval.StartTime.AsTime())
	}
	if !secondSuccessCalls.Points[0].Interval.EndTime.AsTime().After(firstSuccessCalls.Points[0].Interval.EndTime.AsTime()) {
		t.Fatalf("gen_ai.api.calls/success EndTime did not advance: %v -> %v",
			firstSuccessCalls.Points[0].Interval.EndTime.AsTime(), secondSuccessCalls.Points[0].Interval.EndTime.AsTime())
	}
	if want := firstSuccessCalls.Points[0].Value.GetInt64Value() + 1; secondSuccessCalls.Points[0].Value.GetInt64Value() != want {
		t.Fatalf("gen_ai.api.calls/success value = %d, want %d (cumulative, not reset)", secondSuccessCalls.Points[0].Value.GetInt64Value(), want)
	}

	// Pin emitter -> dashboard against a golden file of exactly what was
	// captured above (not hand-built), covering two flushes so the
	// epoch-stable/cumulative shape (StartTime fixed, EndTime advancing,
	// value accumulating — just asserted above) is part of what the fixture
	// pins. pkg/hub's golden dashboard test loads this same file by relative
	// path, without importing this package.
	newInThirdFlush := allCapturedSeries(capture)[len(series):]
	checkOrUpdateUsageGolden(t, [][]*monitoringpb.TimeSeries{series, newInThirdFlush})
}

// TestPipelineStartConstructsUsageDeriverFromEnv exercises the
// deriver's construction inside Pipeline.Start itself — including the
// SCION_USAGE_SOURCE/SCION_HARNESS env gating design §3.3/D4/D10 require —
// rather than only through a deriver built by hand and assigned directly, as
// TestPipelineDerivesClaudeUsageEndToEnd above does (that test needs a fake
// GCP exporter and a controllable clock neither of which Start's own
// construction path accepts as an override).
func TestPipelineStartConstructsUsageDeriverFromEnv(t *testing.T) {
	t.Run("native_and_known_harness_is_active", func(t *testing.T) {
		t.Setenv("SCION_HARNESS", "claude")
		t.Setenv("SCION_USAGE_SOURCE", "native")
		cfg := &Config{Enabled: true, GRPCPort: availableTCPPort(t)}
		p := NewWithConfig(cfg)
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Stop shuts the deriver down before closing intake/the receiver
		// (R-2), so its final flush lands in the still-open pipeline — no
		// bounded-context workaround needed here.
		defer func() { _ = p.Stop(context.Background()) }()
		deriver := p.usageDeriver.Load()
		if deriver == nil || len(deriver.rules) == 0 {
			t.Fatal("Pipeline.Start must construct an active claude usage deriver when SCION_USAGE_SOURCE=native and SCION_HARNESS=claude")
		}
	})
	t.Run("not_native_is_a_noop", func(t *testing.T) {
		t.Setenv("SCION_HARNESS", "claude")
		t.Setenv("SCION_USAGE_SOURCE", "hooks") // not "native" (D4/D10)
		cfg := &Config{Enabled: true, GRPCPort: availableTCPPort(t)}
		p := NewWithConfig(cfg)
		if err := p.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		// Stop shuts the deriver down before closing intake/the receiver
		// (R-2), so its final flush lands in the still-open pipeline — no
		// bounded-context workaround needed here.
		defer func() { _ = p.Stop(context.Background()) }()
		deriver := p.usageDeriver.Load()
		if deriver == nil {
			t.Fatal("Start must still store a (no-op) deriver, never a nil pointer")
		}
		if len(deriver.rules) != 0 {
			t.Fatal("Pipeline.Start must not activate usage derivation unless SCION_USAGE_SOURCE=native")
		}
	})
}

// TestPipelineStopFlushesFinalUsageIncrementQuickly is the R-2 regression
// test: with the usage deriver active, Stop must shut it down before
// closing intake/the receiver, so its final ForceFlush lands in the
// still-open pipeline and reaches the exporter — and it must do so without
// waiting out the deriver's own export timeout against an already-closed
// loopback (measured at 10s before this fix; hook-mode Stop is
// sub-millisecond).
func TestPipelineStopFlushesFinalUsageIncrementQuickly(t *testing.T) {
	t.Setenv("SCION_AGENT_ID", "agent-stop-1")
	t.Setenv("SCION_PROJECT_ID", "project-stop-1")
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	capture := &monitoringCapture{}
	server := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(server, capture)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	sdkExporter, err := mexporter.New(mexporter.WithProjectID("test-project"), mexporter.WithMonitoringClientOptions(option.WithGRPCConn(conn)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sdkExporter.Shutdown(context.Background()) }()

	cfg := &Config{Enabled: true, CloudProvider: "gcp", GRPCPort: availableTCPPort(t), HTTPPort: 0}
	p := NewWithConfig(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Cloud isn't configured (no ProjectID), so Start did not create a real
	// exporter; point it at the capture server, as the other Pipeline.Start
	// test in this file does.
	p.exporter = &CloudExporter{gcpExporter: &GCPExporter{metricExporter: sdkExporter}}

	deriver := p.usageDeriver.Load()
	if deriver == nil || len(deriver.rules) == 0 {
		t.Fatal("expected an active claude usage deriver")
	}

	// Record directly, rather than posting a log request through the
	// receiver: ProcessResourceLogs does its own
	// ForceFlush after every matched request, which would let this
	// increment reach the exporter regardless of whether Stop's own final
	// flush works at all. Recording without flushing isolates the property
	// this test exists to pin: the increment must still arrive because
	// Stop's shutdown does the flush, not because something upstream
	// already did.
	deriver.record(context.Background(), usageIncrement{
		Model:  "claude-sonnet-5",
		Status: telemetrycontract.StatusSuccess,
		Calls:  1,
	})

	stopStart := time.Now()
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	if elapsed := time.Since(stopStart); elapsed > time.Second {
		t.Fatalf("Stop took %v, want well under 1s (R-2: the deriver's final flush must not wait out its own export timeout against an already-closed loopback)", elapsed)
	}

	series := allCapturedSeries(capture)
	var sawCalls bool
	for _, ts := range series {
		if ts.GetMetric().GetType() == "workload.googleapis.com/gen_ai.api.calls" {
			sawCalls = true
		}
	}
	if !sawCalls {
		t.Fatalf("Stop's final flush did not reach the exporter with the derived gen_ai.api.calls point: captured %v", describeSeries(series))
	}
}

// mutateRequestID rewrites the request_id attribute (and bumps
// time_unix_nano, so the fingerprint changes even if request_id were
// ignored) on every record of the given event name, so a repost of an
// otherwise-identical fixture is a genuinely new event rather than a
// duplicate (design §3.3 dedupe key).
func mutateRequestID(t *testing.T, resourceLogs []*logspb.ResourceLogs, eventName, newRequestID string) {
	t.Helper()
	found := false
	for _, rl := range resourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				if logAttrString(record.Attributes, "event.name") != eventName {
					continue
				}
				found = true
				record.TimeUnixNano++
				replaced := false
				for _, kv := range record.Attributes {
					if kv.Key == "request_id" {
						kv.Value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: newRequestID}}
						replaced = true
					}
				}
				if !replaced {
					record.Attributes = append(record.Attributes, &commonpb.KeyValue{
						Key:   "request_id",
						Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: newRequestID}},
					})
				}
			}
		}
	}
	if !found {
		t.Fatalf("mutateRequestID: no %q record found in fixture", eventName)
	}
}

// latestSeries returns the most recently captured TimeSeries matching
// metricType and a "status" label (used for gen_ai.api.calls), failing the
// test if none matched. Cumulative GCP series are re-exported on every
// flush that touches them, so allCapturedSeries can contain several matches
// for the same identity across a test; callers that want the current value
// need the last one.
func latestSeries(t *testing.T, series []*monitoringpb.TimeSeries, metricType, status string) *monitoringpb.TimeSeries {
	t.Helper()
	var latest *monitoringpb.TimeSeries
	for _, ts := range series {
		if ts.Metric.Type == metricType && ts.Metric.Labels["status"] == status {
			latest = ts
		}
	}
	if latest == nil {
		t.Fatalf("no captured series for type=%q status=%q", metricType, status)
	}
	return latest
}

func allCapturedSeries(capture *monitoringCapture) []*monitoringpb.TimeSeries {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	var out []*monitoringpb.TimeSeries
	for _, req := range capture.series {
		out = append(out, req.TimeSeries...)
	}
	return out
}

// assertUsageSeries pins AC-1.1, AC-1.1b and AC-1.2: exactly the expected
// points, with exactly the canonical label set and nothing else (no leaked
// Claude attribute such as request_id, prompt.id or event.timestamp).
func assertUsageSeries(t *testing.T, series []*monitoringpb.TimeSeries) {
	t.Helper()
	var calls, tokens []*monitoringpb.TimeSeries
	for _, ts := range series {
		switch ts.Metric.Type {
		case "workload.googleapis.com/gen_ai.api.calls":
			calls = append(calls, ts)
		case "workload.googleapis.com/scion.usage.tokens":
			tokens = append(tokens, ts)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("gen_ai.api.calls series = %d, want 2 (success, error): %+v", len(calls), describeSeries(calls))
	}
	byStatus := map[string]int64{}
	for _, ts := range calls {
		assertLabelKeys(t, ts.Metric.Labels, "agent_id", "project_id", "harness", "model", "status",
			"scion_metric_resource_id", "scion_metric_scope_id", "scion_metric_point_id",
			"scion_agent_id", "scion_project_id", "scion_agent_slug",
			"service_name", "service_instance_id")
		if ts.Metric.Labels["harness"] != "claude" || ts.Metric.Labels["model"] != "claude-sonnet-5" {
			t.Errorf("unexpected calls series labels: %+v", ts.Metric.Labels)
		}
		if ts.Metric.Labels["scion_agent_id"] != "agent-usage-1" || ts.Metric.Labels["scion_project_id"] != "project-usage-1" || ts.Metric.Labels["scion_agent_slug"] != "usage-agent-slug" {
			t.Errorf("unexpected canonical identity labels: %+v", ts.Metric.Labels)
		}
		byStatus[ts.Metric.Labels["status"]] = ts.Points[0].Value.GetInt64Value()
	}
	if byStatus["success"] != 1 || byStatus["error"] != 1 {
		t.Fatalf("calls by status = %+v, want success=1 error=1", byStatus)
	}

	wantTokens := map[string]int64{"input": 2, "output": 41, "cache_write": 26607}
	if len(tokens) != len(wantTokens) {
		t.Fatalf("scion.usage.tokens series = %d, want %d: %+v", len(tokens), len(wantTokens), describeSeries(tokens))
	}
	byType := map[string]int64{}
	for _, ts := range tokens {
		assertLabelKeys(t, ts.Metric.Labels, "harness", "model", "token_type",
			"scion_metric_resource_id", "scion_metric_scope_id", "scion_metric_point_id",
			"scion_agent_id", "scion_project_id", "scion_agent_slug",
			"service_name", "service_instance_id")
		byType[ts.Metric.Labels["token_type"]] = ts.Points[0].Value.GetInt64Value()
	}
	for tokenType, want := range wantTokens {
		if byType[tokenType] != want {
			t.Errorf("tokens[%q] = %d, want %d", tokenType, byType[tokenType], want)
		}
	}
}

func assertLabelKeys(t *testing.T, labels map[string]string, want ...string) {
	t.Helper()
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	for k := range labels {
		if !wantSet[k] {
			t.Errorf("unexpected label leaked into export: %q (value %q)", k, labels[k])
		}
	}
	for _, k := range want {
		if _, ok := labels[k]; !ok {
			t.Errorf("missing expected label %q in %+v", k, labels)
		}
	}
}

// TestTruncateUTF8DoesNotSplitRune pins that truncating a model name to the
// 128-byte label limit must never cut a multi-byte rune in half, which would
// produce an invalid UTF-8 label value.
func TestTruncateUTF8DoesNotSplitRune(t *testing.T) {
	// Each "é" is 2 bytes; 64 of them is exactly 128 bytes, so appending one
	// more forces a cut that would otherwise land mid-rune.
	s := strings.Repeat("é", 65)
	got := truncateUTF8(s, 128)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateUTF8(%d runes, 128) = %q, not valid UTF-8", 65, got)
	}
	if len(got) > 128 {
		t.Fatalf("truncateUTF8 result is %d bytes, want <= 128", len(got))
	}
	if len(got) != 128 {
		t.Fatalf("truncateUTF8 result is %d bytes, want exactly 128 (64 whole runes)", len(got))
	}
}

func describeSeries(series []*monitoringpb.TimeSeries) []string {
	var out []string
	for _, ts := range series {
		out = append(out, ts.Metric.Type)
	}
	sort.Strings(out)
	return out
}
