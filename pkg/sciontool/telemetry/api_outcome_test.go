/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
)

// observeFixtureOutcomes feeds every record of a captured fixture through
// a bare deriver with rule and returns the APIOutcomes it reported, in
// order.
func observeFixtureOutcomes(t *testing.T, rule usageRule, fixture []*logspb.ResourceLogs) []APIOutcome {
	t.Helper()
	d := bareUsageDeriver(rule)
	var got []APIOutcome
	d.apiOutcome = func(o APIOutcome) { got = append(got, o) }
	for _, rl := range fixture {
		for _, sl := range rl.ScopeLogs {
			for _, record := range sl.LogRecords {
				d.observeRecord(context.Background(), sl.GetScope().GetName(), rl.GetResource().GetAttributes(), record)
			}
		}
	}
	return got
}

func TestAPIOutcome_ClaudeFixture(t *testing.T) {
	got := observeFixtureOutcomes(t, claudeUsageRule{}, loadClaudeUsageFixture(t))
	var failures, successes []APIOutcome
	for _, o := range got {
		if o.OK {
			successes = append(successes, o)
		} else {
			failures = append(failures, o)
		}
	}
	if len(successes) != 1 || len(failures) != 1 {
		t.Fatalf("outcomes = %+v, want one success and one failure", got)
	}
	f := failures[0]
	if f.Status != 403 || !strings.Contains(f.Message, "permission denied") || f.Model != "claude-sonnet-5" || f.At.IsZero() {
		t.Errorf("failure = %+v, want status 403 with the error text", f)
	}
	if s := successes[0]; s.Message != "" {
		t.Errorf("success carries a message: %+v", s)
	}
}

// gemini_cli reports the provider's JSON error body; the outcome carries
// the provider's own message. The capture is a 503 retried to a 200.
func TestAPIOutcome_GeminiFixture(t *testing.T) {
	got := observeFixtureOutcomes(t, geminiCLIUsageRule{}, loadGeminiCLIUsageFixture(t))
	if len(got) != 2 {
		t.Fatalf("outcomes = %+v, want 2", got)
	}
	if got[0].OK || got[0].Status != 503 || got[0].Message != "mock overloaded" {
		t.Errorf("first = %+v, want a 503 failure with the provider message", got[0])
	}
	if !got[1].OK || got[1].Status != 200 || !got[1].At.After(got[0].At) {
		t.Errorf("second = %+v, want a later 200 success", got[1])
	}
}

func TestAPIOutcome_CodexFixture(t *testing.T) {
	got := observeFixtureOutcomes(t, codexUsageRule{}, loadCodex160UsageFixture(t))
	var http500, transport, stream, ok int
	for _, o := range got {
		switch {
		case o.OK:
			ok++
		case o.Status == 500 && o.Message == "http 500":
			http500++
		case o.Status == 0 && o.Message == "error sending request":
			transport++
		case o.Status == 0 && strings.HasPrefix(o.Message, "stream disconnected"):
			stream++
		default:
			t.Errorf("unexpected outcome %+v", o)
		}
	}
	if http500 != 1 || transport != 1 || stream != 1 || ok == 0 {
		t.Errorf("outcomes = %+v: 500=%d transport=%d stream=%d ok=%d", got, http500, transport, stream, ok)
	}
}

// A replayed record is deduped: no second outcome.
func TestAPIOutcome_NotReportedForDuplicate(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	n := 0
	d.apiOutcome = func(APIOutcome) { n++ }
	record := fixtureRecordsByEvent(t, "api_error")[0]
	d.observe(context.Background(), claudeUsageScope, record)
	d.observe(context.Background(), claudeUsageScope, record)
	if n != 1 {
		t.Fatalf("outcomes = %d, want 1", n)
	}
}

func TestAPIOutcomeFromRecord(t *testing.T) {
	str := func(k, v string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
	}
	received := time.Unix(100, 0)
	failure := usageIncrement{Status: "error", Calls: 1, Model: "m"}

	o := apiOutcomeFromRecord(&logspb.LogRecord{ObservedTimeUnixNano: uint64(time.Unix(50, 0).UnixNano()),
		Attributes: []*commonpb.KeyValue{str("http.response.status_code", "400"), str("error.message", strings.Repeat("x", 5000))}}, failure, received)
	if o.Status != 400 || o.OK || !o.At.Equal(time.Unix(50, 0)) || len([]rune(o.Message)) != apiOutcomeMessageMaxRunes {
		t.Errorf("observed-time record = %+v", o)
	}
	o = apiOutcomeFromRecord(&logspb.LogRecord{Attributes: []*commonpb.KeyValue{str("status_code", "bogus"), str("error", `{"error":{}}`)}}, failure, received)
	if o.Status != 0 || !o.At.Equal(received) || o.Message != `{"error":{}}` {
		t.Errorf("record without time or status = %+v", o)
	}
}

// SetAPIOutcomeObserver is safe on a nil pipeline and with a nil observer.
func TestPipelineAPIOutcomeObserver(t *testing.T) {
	var nilPipeline *Pipeline
	nilPipeline.SetAPIOutcomeObserver(func(APIOutcome) {})

	p := &Pipeline{}
	p.forwardAPIOutcome(APIOutcome{})
	var got []APIOutcome
	p.SetAPIOutcomeObserver(func(o APIOutcome) { got = append(got, o) })
	p.forwardAPIOutcome(APIOutcome{Status: 401})
	p.SetAPIOutcomeObserver(nil)
	p.forwardAPIOutcome(APIOutcome{Status: 404})
	if len(got) != 1 || got[0].Status != 401 {
		t.Fatalf("forwarded = %+v, want only the 401", got)
	}
}

// A started pipeline forwards the outcomes of the native records it
// receives to the observer set on it, including one set after Start.
func TestPipelineForwardsAPIOutcomesFromReceivedLogs(t *testing.T) {
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	t.Setenv("SCION_AGENT_ID", "agent-outcome")
	cfg := &Config{Enabled: true, GRPCPort: availableTCPPort(t)}
	p := NewWithConfig(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop(context.Background()) }()

	var mu sync.Mutex
	var got []APIOutcome
	p.SetAPIOutcomeObserver(func(o APIOutcome) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, o)
	})
	if err := p.handleLogs(context.Background(), loadClaudeUsageFixture(t)); err != nil {
		t.Fatalf("handleLogs: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	var failed []APIOutcome
	for _, o := range got {
		if !o.OK {
			failed = append(failed, o)
		}
	}
	if len(got) != 2 || len(failed) != 1 || failed[0].Status != 403 {
		t.Fatalf("outcomes = %+v, want one success and one 403", got)
	}
}
