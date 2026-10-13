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

package telemetry

import (
	"context"
	"sync"
	"testing"
)

// Each increment the native deriver records also reaches the session usage
// hook, mapped like a hook model-end event: input, output, cache_read as
// cached, reasoning. cache_write has no aggregator field. A replayed
// (deduplicated) record is not forwarded again, and a failed call counts
// as a call with no tokens.
func TestUsageDeriverForwardsSessionUsage(t *testing.T) {
	d := bareUsageDeriver(claudeUsageRule{})
	var got []SessionUsage
	d.sessionUsage = func(u SessionUsage) { got = append(got, u) }

	request := fixtureRecordsByEvent(t, "api_request")[0]
	d.observe(context.Background(), claudeUsageScope, request)
	d.observe(context.Background(), claudeUsageScope, request)
	d.observe(context.Background(), claudeUsageScope, fixtureRecordsByEvent(t, "api_error")[0])

	want := []SessionUsage{
		{Calls: 1, TokensInput: 2, TokensOutput: 41},
		{Calls: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("forwarded %d increments %+v, want %+v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("increment %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSessionUsageFromIncrementMapsTokenTypes(t *testing.T) {
	got := sessionUsageFromIncrement(usageIncrement{
		Calls: 2,
		Tokens: map[string]int64{
			"input": 100, "output": 20, "cache_read": 300, "cache_write": 50, "reasoning": 7,
		},
	})
	want := SessionUsage{Calls: 2, TokensInput: 100, TokensOutput: 20, TokensCached: 300, TokensReasoning: 7}
	if got != want {
		t.Errorf("sessionUsageFromIncrement = %+v, want %+v", got, want)
	}
}

// The pipeline forwards to whichever sink is current, and does nothing
// without one.
func TestPipelineForwardSessionUsage(t *testing.T) {
	p := &Pipeline{}
	p.forwardSessionUsage(SessionUsage{Calls: 1}) // no sink: no panic

	var got []SessionUsage
	p.SetSessionUsageSink(func(u SessionUsage) { got = append(got, u) })
	p.forwardSessionUsage(SessionUsage{Calls: 1, TokensInput: 5})
	p.SetSessionUsageSink(nil)
	p.forwardSessionUsage(SessionUsage{Calls: 1})

	if len(got) != 1 || got[0] != (SessionUsage{Calls: 1, TokensInput: 5}) {
		t.Errorf("forwarded %+v, want one {Calls:1 TokensInput:5}", got)
	}

	var nilPipeline *Pipeline
	nilPipeline.SetSessionUsageSink(func(SessionUsage) {}) // nil-safe
}

func TestHookUsageFeedsSessionMetrics(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"native", false},
		{"hooks", true},
		{"", true},
	} {
		t.Setenv("SCION_USAGE_SOURCE", tc.source)
		if got := HookUsageFeedsSessionMetrics(); got != tc.want {
			t.Errorf("SCION_USAGE_SOURCE=%q: HookUsageFeedsSessionMetrics() = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// RecordUsage adds to the running counts and survives a State/RestoreState
// round trip, the way a hook process persists and reloads the session.
// Turn and tool counts are untouched.
func TestAggregatorRecordUsageAccumulatesAcrossRestore(t *testing.T) {
	a := NewAggregator()
	a.StartSession("s1")
	a.RecordTurn()
	a.RecordToolEnd("Bash", "")
	a.RecordUsage(SessionUsage{Calls: 1, TokensInput: 10, TokensOutput: 2, TokensCached: 30, TokensReasoning: 1})

	b := NewAggregator()
	b.RestoreState(a.State())
	b.RecordUsage(SessionUsage{Calls: 2, TokensInput: 5, TokensOutput: 4})

	s := b.Finalize(0, 0, 0, 0, "")
	if s.APICallCount != 3 || s.TokensInput != 15 || s.TokensOutput != 6 || s.TokensCached != 30 || s.TokensReasoning != 1 {
		t.Errorf("usage = calls %d in %d out %d cached %d reasoning %d, want 3/15/6/30/1",
			s.APICallCount, s.TokensInput, s.TokensOutput, s.TokensCached, s.TokensReasoning)
	}
	if s.TurnCount != 1 || s.ToolCalls["Bash"].Calls != 1 {
		t.Errorf("turns %d, tools %+v, want 1 turn and 1 Bash call", s.TurnCount, s.ToolCalls)
	}
}

// feedClaudeRequest pushes one native Claude api_request through the
// pipeline's current deriver, the way handleLogs does.
func feedClaudeRequest(p *Pipeline, requestID string) {
	p.usageDeriver.Load().ProcessResourceLogs(context.Background(), claudeAPIRequestLogs(requestID))
}

// The deriver Pipeline.Start builds forwards derived usage to the
// pipeline's session usage sink, including a sink set after Start.
func TestPipelineStartDeriverForwardsToSessionUsageSink(t *testing.T) {
	t.Setenv("SCION_HARNESS", "claude")
	t.Setenv("SCION_USAGE_SOURCE", "native")
	p := NewWithConfig(&Config{Enabled: true, GRPCPort: availableTCPPort(t)})
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Stop(context.Background()) }()

	var mu sync.Mutex
	var got []SessionUsage
	p.SetSessionUsageSink(func(u SessionUsage) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, u)
	})
	feedClaudeRequest(p, "start-req-1")

	mu.Lock()
	defer mu.Unlock()
	want := SessionUsage{Calls: 1, TokensInput: 2, TokensOutput: 3}
	if len(got) != 1 || got[0] != want {
		t.Errorf("sink received %+v, want one %+v", got, want)
	}
}

// The deriver ActivateUsageSource builds (native usage selected only by the
// provisioner's overlay) forwards to the sink too, including a sink set
// before activation.
func TestPipelineActivatedDeriverForwardsToSessionUsageSink(t *testing.T) {
	p := startPipelineWithoutUsageSource(t, "claude")

	var mu sync.Mutex
	var got []SessionUsage
	p.SetSessionUsageSink(func(u SessionUsage) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, u)
	})
	if outcome, err := p.ActivateUsageSource(context.Background(), UsageSourceNative); err != nil || outcome != UsageActivated {
		t.Fatalf("ActivateUsageSource = %q, %v; want %q", outcome, err, UsageActivated)
	}
	feedClaudeRequest(p, "activate-req-1")

	mu.Lock()
	defer mu.Unlock()
	want := SessionUsage{Calls: 1, TokensInput: 2, TokensOutput: 3}
	if len(got) != 1 || got[0] != want {
		t.Errorf("sink received %+v, want one %+v", got, want)
	}
}
