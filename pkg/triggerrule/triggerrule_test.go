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

package triggerrule_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

func sampleFireEvent(payload string) hubfire.FireEvent {
	t0 := time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC)
	return hubfire.FireEvent{
		ID:         "evt-123",
		ProjectID:  "proj-abc",
		SourceKind: "webhook",
		SourceID:   "ep-456",
		Type:       "webhook.deploy.finished",
		Subject:    "webhook:ep-456/default",
		OccurredAt: t0,
		ReceivedAt: t0.Add(2 * time.Second),
		Attributes: map[string]string{
			"delivery_id": "deliv-789",
			"verified":    "scion-v1",
		},
		Payload: []byte(payload),
	}
}

func TestTriggerEnvironmentAndActivationFields(t *testing.T) {
	rules, err := triggerrule.NewDefault()
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	ctx := context.Background()

	ev := sampleFireEvent(`{"status":"ok","service":"billing","retries":3,"tags":["canary","us-east1"]}`)
	tr := hubfire.FireTriggerRef{ID: "trig-1", Name: "deploy-finished"}

	act, err := triggerrule.NewActivation(ev, tr)
	if err != nil {
		t.Fatalf("NewActivation: %v", err)
	}

	// Verify every §7.1 variable is reachable and typed as documented.
	expr := strings.Join([]string{
		`event.id == "evt-123"`,
		`event.type == "webhook.deploy.finished"`,
		`event.source.kind == "webhook"`,
		`event.source.id == "ep-456"`,
		`event.subject == "webhook:ep-456/default"`,
		`event.received_at > event.occurred_at`,
		`has(event.attributes.verified) && event.attributes.verified == "scion-v1"`,
		`!has(event.attributes.nonexistent)`,
		`has(event.payload) && has(event.payload.status) && event.payload.status == "ok"`,
		`!has(event.payload.missing_field)`,
		`event.payload.retries == 3 && event.payload.retries + 1 == 4`,
		`event.payload.tags.exists(x, x.startsWith("us-"))`,
		`trigger.id == "trig-1"`,
		`trigger.name == "deploy-finished"`,
		`trigger.project_id == "proj-abc"`,
	}, " && ")

	rule := triggerrule.Rule{Dialect: "cel", Expr: expr}
	if err := rules.CompileMatch(ctx, rule); err != nil {
		t.Fatalf("CompileMatch: %v", err)
	}

	scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr.ID, Revision: 1}
	matched, err := rules.Match(ctx, scope, rule, act)
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if !matched {
		t.Fatalf("Match = false, want true")
	}

	// Reuse the once-parsed event activation for a second trigger without re-parsing JSON.
	tr2 := hubfire.FireTriggerRef{ID: "trig-2", Name: "other-trigger"}
	act2 := act.WithTrigger(tr2)
	matched2, err := rules.Match(ctx, celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr2.ID, Revision: 1}, triggerrule.Rule{
		Expr: `trigger.id == "trig-2" && trigger.name == "other-trigger" && event.payload.service == "billing"`,
	}, act2)
	if err != nil || !matched2 {
		t.Fatalf("Match with WithTrigger = (%v, %v), want (true, nil)", matched2, err)
	}

	// Empty payload omits event.payload so has(event.payload) is false.
	emptyEv := sampleFireEvent("")
	emptyAct, err := triggerrule.NewActivation(emptyEv, tr)
	if err != nil {
		t.Fatalf("NewActivation empty payload: %v", err)
	}
	hasPayload, err := rules.Match(ctx, scope, triggerrule.Rule{Expr: `has(event.payload)`}, emptyAct)
	if err != nil {
		t.Fatalf("Match has(event.payload): %v", err)
	}
	if hasPayload {
		t.Fatalf("has(event.payload) = true on empty payload, want false")
	}
}

func TestNonJSONPayloadOmitsEventPayload(t *testing.T) {
	rules, err := triggerrule.NewDefault()
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	ctx := context.Background()
	tr := hubfire.FireTriggerRef{ID: "trig-nonjson", Name: "nonjson"}
	scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr.ID, Revision: 1}

	ev := sampleFireEvent("plain-text-or-form-encoded-body")
	act, err := triggerrule.NewActivation(ev, tr)
	if err != nil {
		t.Fatalf("NewActivation on non-JSON payload must succeed and omit event.payload, got: %v", err)
	}

	hasPayload, err := rules.Match(ctx, scope, triggerrule.Rule{Expr: `has(event.payload)`}, act)
	if err != nil {
		t.Fatalf("Match has(event.payload): %v", err)
	}
	if hasPayload {
		t.Fatalf("has(event.payload) = true on non-JSON payload, want false")
	}

	// Non-payload trigger rules still evaluate normally on the same event.
	matched, err := rules.Match(ctx, scope, triggerrule.Rule{
		Expr: `event.type == "webhook.deploy.finished" && event.attributes.verified == "scion-v1"`,
	}, act)
	if err != nil || !matched {
		t.Fatalf("non-payload Match on non-JSON event = (%v, %v), want (true, nil)", matched, err)
	}

	// Oversized payload (> 64 KiB) is still rejected by NewActivation.
	oversized := sampleFireEvent(strings.Repeat("x", triggerrule.MaxPayloadBytes+1))
	if _, err := triggerrule.NewActivation(oversized, tr); err == nil {
		t.Fatalf("expected NewActivation error on payload exceeding MaxPayloadBytes")
	}
}

func TestAuthoringCompileValidationAndDialects(t *testing.T) {
	rules, err := triggerrule.NewDefault()
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	ctx := context.Background()

	// Empty rule with default/cel dialect is valid (unconditional match / no subject override).
	if err := rules.CompileMatch(ctx, triggerrule.Rule{}); err != nil {
		t.Fatalf("empty match rule failed: %v", err)
	}
	if err := rules.CompileSubject(ctx, triggerrule.Rule{}); err != nil {
		t.Fatalf("empty subject rule failed: %v", err)
	}

	// Unsupported dialect returns ruleengine.UnsupportedDialectError (400 unsupported dialect).
	err = rules.CompileMatch(ctx, triggerrule.Rule{Dialect: "system_one", Expr: "is urgent"})
	var unsupported *ruleengine.UnsupportedDialectError
	if !errors.As(err, &unsupported) {
		t.Fatalf("expected *UnsupportedDialectError, got %T: %v", err, err)
	}

	// match_rule > 4096 bytes rejected with CategorySize; subject_rule > 1024 bytes rejected with CategorySize.
	err = rules.CompileMatch(ctx, triggerrule.Rule{Expr: strings.Repeat("a", triggerrule.MaxMatchRuleBytes+1)})
	var cerr *celengine.CompileError
	if !errors.As(err, &cerr) || cerr.Category != celengine.CategorySize {
		t.Fatalf("expected match CompileError(size), got %T: %v", err, err)
	}

	err = rules.CompileSubject(ctx, triggerrule.Rule{Expr: `"` + strings.Repeat("a", triggerrule.MaxSubjectRuleBytes) + `"`})
	if !errors.As(err, &cerr) || cerr.Category != celengine.CategorySize {
		t.Fatalf("expected subject CompileError(size), got %T: %v", err, err)
	}

	// Compile errors carry 1-based line and column.
	err = rules.CompileMatch(ctx, triggerrule.Rule{Expr: "event.type == \"ok\" &&\n  unknown_root == 1"})
	if !errors.As(err, &cerr) || cerr.Category != celengine.CategoryUnknownVariable || cerr.Line != 2 || cerr.Column != 3 {
		t.Fatalf("unexpected CompileError: %+v (err=%v)", cerr, err)
	}

	// Static result type check: match must be bool, subject must be string.
	err = rules.CompileMatch(ctx, triggerrule.Rule{Expr: `"not-a-bool"`})
	if !errors.As(err, &cerr) || cerr.Category != celengine.CategoryResultType {
		t.Fatalf("expected CategoryResultType for string match rule, got %v", err)
	}
	err = rules.CompileSubject(ctx, triggerrule.Rule{Expr: `event.type == "webhook.received"`})
	if !errors.As(err, &cerr) || cerr.Category != celengine.CategoryResultType {
		t.Fatalf("expected CategoryResultType for bool subject rule, got %v", err)
	}
}

func TestDeriveSubjectBoundsAndFallback(t *testing.T) {
	rules, err := triggerrule.NewDefault()
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	ctx := context.Background()
	tr := hubfire.FireTriggerRef{ID: "trig-subj", Name: "subj-test"}
	ev := sampleFireEvent(`{"service":"payments","huge":"` + strings.Repeat("s", 300) + `","ctrl":"bad\u0007bell","num":99}`)
	act, err := triggerrule.NewActivation(ev, tr)
	if err != nil {
		t.Fatalf("NewActivation: %v", err)
	}
	scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr.ID, Revision: 1}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	// 1. Empty rule -> fallback subject, no warning.
	got := rules.DeriveSubject(ctx, scope, triggerrule.Rule{}, act, ev.Subject, logger)
	if got != ev.Subject || logBuf.Len() != 0 {
		t.Fatalf("empty rule DeriveSubject = %q (log=%q), want %q with no warning", got, logBuf.String(), ev.Subject)
	}

	// 2. Valid printable <= 256-byte string overrides event subject.
	got = rules.DeriveSubject(ctx, scope, triggerrule.Rule{Expr: `"svc/" + event.payload.service`}, act, ev.Subject, logger)
	if got != "svc/payments" {
		t.Fatalf("DeriveSubject = %q, want \"svc/payments\"", got)
	}

	// 3. Output > 256 bytes falls back to event.Subject and logs a warning (without leaking value).
	logBuf.Reset()
	got = rules.DeriveSubject(ctx, scope, triggerrule.Rule{Expr: `event.payload.huge`}, act, ev.Subject, logger)
	if got != ev.Subject {
		t.Fatalf("oversize DeriveSubject = %q, want fallback %q", got, ev.Subject)
	}
	if logBuf.Len() == 0 || strings.Contains(logBuf.String(), strings.Repeat("s", 20)) {
		t.Fatalf("expected content-free warning log for oversize subject, got %q", logBuf.String())
	}

	// 4. Non-printable control character falls back to event.Subject and logs warning.
	logBuf.Reset()
	got = rules.DeriveSubject(ctx, scope, triggerrule.Rule{Expr: `event.payload.ctrl`}, act, ev.Subject, logger)
	if got != ev.Subject || logBuf.Len() == 0 {
		t.Fatalf("non-printable DeriveSubject = %q (log=%q), want fallback + warning", got, logBuf.String())
	}

	// 5. Eval error (non-string dyn value) falls back to event.Subject and logs warning.
	logBuf.Reset()
	got = rules.DeriveSubject(ctx, scope, triggerrule.Rule{Expr: `event.payload.num`}, act, ev.Subject, logger)
	if got != ev.Subject || logBuf.Len() == 0 {
		t.Fatalf("eval error DeriveSubject = %q (log=%q), want fallback + warning", got, logBuf.String())
	}
}

func TestPerEventBudgetAndSkipReasonMapping(t *testing.T) {
	rules, err := triggerrule.NewDefault()
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	ctx := context.Background()
	ev := sampleFireEvent(`{"items":["a","b","c"],"status":"ok"}`)
	tr1 := hubfire.FireTriggerRef{ID: "trig-bad", Name: "bad"}
	tr2 := hubfire.FireTriggerRef{ID: "trig-good", Name: "good"}
	act, err := triggerrule.NewActivation(ev, tr1)
	if err != nil {
		t.Fatalf("NewActivation: %v", err)
	}

	budget := triggerrule.NewEventBudget()

	// Trigger 1 fails at runtime (missing unguarded key) -> maps to "rule evaluation failed",
	// and does NOT block Trigger 2 on the same event!
	_, err = rules.Match(
		ctx,
		celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr1.ID, Revision: 1},
		triggerrule.Rule{Expr: `event.payload.nonexistent == "x"`},
		act,
		triggerrule.WithBudget(budget),
	)
	reason, ok := triggerrule.SkipReason(err)
	if !ok || reason != triggerrule.SkipReasonEvalFailed {
		t.Fatalf("SkipReason(%v) = (%q, %v), want (%q, true)", err, reason, ok, triggerrule.SkipReasonEvalFailed)
	}

	// Trigger 2 still runs and matches cleanly!
	matched, err := rules.Match(
		ctx,
		celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr2.ID, Revision: 1},
		triggerrule.Rule{Expr: `event.payload.status == "ok"`},
		act.WithTrigger(tr2),
		triggerrule.WithBudget(budget),
	)
	if err != nil || !matched {
		t.Fatalf("Trigger 2 Match = (%v, %v), want (true, nil)", matched, err)
	}

	// Stored rule that no longer compiles -> SkipReasonNoLongerCompiles.
	_, err = rules.Match(
		ctx,
		celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-broken", Revision: 1},
		triggerrule.Rule{Expr: `invalid syntax ===`},
		act,
		triggerrule.WithBudget(budget),
	)
	if reason, ok := triggerrule.SkipReason(err); !ok || reason != triggerrule.SkipReasonNoLongerCompiles {
		t.Fatalf("SkipReason(compile err) = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonNoLongerCompiles)
	}

	// UncertainError from a model dialect -> SkipReasonRuleUncertain.
	if reason, ok := triggerrule.SkipReason(&ruleengine.UncertainError{Reason: "ambiguous"}); !ok || reason != triggerrule.SkipReasonRuleUncertain {
		t.Fatalf("SkipReason(UncertainError) = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonRuleUncertain)
	}

	// Exhausted per-event budget -> SkipReasonBudgetExhausted.
	budget.Charge(triggerrule.EventRuleBudget)
	_, err = rules.Match(
		ctx,
		celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr2.ID, Revision: 1},
		triggerrule.Rule{Expr: `event.payload.status == "ok"`},
		act.WithTrigger(tr2),
		triggerrule.WithBudget(budget),
	)
	if reason, ok := triggerrule.SkipReason(err); !ok || reason != triggerrule.SkipReasonBudgetExhausted {
		t.Fatalf("SkipReason(budget exhausted) = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonBudgetExhausted)
	}
}

func TestCacheInvalidationOnTriggerRevisionBump(t *testing.T) {
	celEng, err := celengine.New(triggerrule.Env(), triggerrule.Stage1Limits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	re := ruleengine.NewEngine()
	if err := re.Register(celDialect); err != nil {
		t.Fatalf("Register: %v", err)
	}
	re.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.DialectCEL)

	rules, err := triggerrule.New(re)
	if err != nil {
		t.Fatalf("triggerrule.New: %v", err)
	}

	ctx := context.Background()
	tr := hubfire.FireTriggerRef{ID: "trig-cache", Name: "cache-test"}
	act, err := triggerrule.NewActivation(sampleFireEvent(`{"v":1}`), tr)
	if err != nil {
		t.Fatalf("NewActivation: %v", err)
	}

	// Revision 1 caches `event.payload.v == 1`.
	sc1 := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr.ID, Revision: 1}
	ok, err := rules.Match(ctx, sc1, triggerrule.Rule{Expr: `event.payload.v == 1`}, act)
	if err != nil || !ok {
		t.Fatalf("Match rev 1: (%v, %v)", ok, err)
	}
	if celEng.CacheStats().Entries != 1 {
		t.Fatalf("Entries = %d, want 1", celEng.CacheStats().Entries)
	}

	// Revision 2 edits expression -> old entry evicted, new entry cached.
	sc2 := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr.ID, Revision: 2}
	ok, err = rules.Match(ctx, sc2, triggerrule.Rule{Expr: `event.payload.v == 2`}, act)
	if err != nil || ok {
		t.Fatalf("Match rev 2: (%v, %v), want (false, nil)", ok, err)
	}
	if celEng.CacheStats().Entries != 1 {
		t.Fatalf("Entries after revision bump = %d, want 1", celEng.CacheStats().Entries)
	}

	// Deleting trigger invalidates its cached program.
	rules.Invalidate("cel", sc2)
	if celEng.CacheStats().Entries != 0 {
		t.Fatalf("Entries after Invalidate = %d, want 0", celEng.CacheStats().Entries)
	}
}

func TestMatchTypesValidationAndPrefilter(t *testing.T) {
	validSets := [][]string{
		{"webhook.received"},
		{"webhook.*"},
		{"webhook.deploy.started", "webhook.deploy.*"},
		{"schedule_1.fired_2"},
	}
	for _, patterns := range validSets {
		if err := triggerrule.ValidateMatchTypes(patterns); err != nil {
			t.Errorf("ValidateMatchTypes(%v) unexpected error: %v", patterns, err)
		}
	}

	tooMany := make([]string, triggerrule.MaxMatchTypes+1)
	for i := range tooMany {
		tooMany[i] = "webhook.received"
	}

	invalidSets := [][]string{
		nil,
		{},
		{""},
		{"*"},
		{"webhook."},
		{".webhook"},
		{"webhook..received"},
		{"Webhook.Received"},
		{"webhook.dep*"},
		{"webhook.*.finished"},
		{strings.Repeat("a", triggerrule.MaxMatchTypeBytes+1)},
		tooMany,
	}
	for _, patterns := range invalidSets {
		if err := triggerrule.ValidateMatchTypes(patterns); err == nil {
			t.Errorf("ValidateMatchTypes(%v) = nil, expected error", patterns)
		}
	}

	// Exact and prefix matching semantics (§7.3).
	if !triggerrule.MatchTypes([]string{"webhook.received"}, "webhook.received") {
		t.Errorf("exact match failed")
	}
	if triggerrule.MatchTypes([]string{"webhook.received"}, "webhook.received.extra") {
		t.Errorf("exact pattern must not prefix-match")
	}
	if !triggerrule.MatchTypes([]string{"webhook.*"}, "webhook.received") {
		t.Errorf("wildcard prefix match failed on single segment")
	}
	if !triggerrule.MatchTypes([]string{"webhook.*"}, "webhook.deploy.finished") {
		t.Errorf("wildcard prefix match failed on multi-segment")
	}
	if triggerrule.MatchTypes([]string{"webhook.*"}, "webhook") {
		t.Errorf("webhook.* must not match bare 'webhook'")
	}
	if triggerrule.MatchTypes([]string{"webhook.*"}, "webhook2.received") {
		t.Errorf("webhook.* must not match 'webhook2.received'")
	}
}

// TestTriggerEnvironmentFunctionAllowList pins the exact function and macro
// surface of triggerrule.Env() so a cel-go upgrade cannot add functions silently (F7).
func TestTriggerEnvironmentFunctionAllowList(t *testing.T) {
	eng, err := celengine.New(triggerrule.Env(), triggerrule.Stage1Limits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	got := eng.Functions()

	want := []string{
		"!_",
		"-_",
		"@in",
		"@not_strictly_false",
		"_!=_",
		"_%_",
		"_&&_",
		"_*_",
		"_+_",
		"_-_",
		"_/_",
		"_<=_",
		"_<_",
		"_==_",
		"_>=_",
		"_>_",
		"_?_:_",
		"_[_]",
		"__not_strictly_false__",
		"_in_",
		"_||_",
		"all",
		"bool",
		"bytes",
		"charAt",
		"contains",
		"double",
		"duration",
		"dyn",
		"endsWith",
		"exists",
		"exists_one",
		"filter",
		"format",
		"getDate",
		"getDayOfMonth",
		"getDayOfWeek",
		"getDayOfYear",
		"getFullYear",
		"getHours",
		"getMilliseconds",
		"getMinutes",
		"getMonth",
		"getSeconds",
		"has",
		"in",
		"indexOf",
		"int",
		"join",
		"lastIndexOf",
		"lowerAscii",
		"map",
		"matches",
		"replace",
		"reverse",
		"size",
		"split",
		"startsWith",
		"string",
		"strings.quote",
		"substring",
		"timestamp",
		"trim",
		"type",
		"uint",
		"upperAscii",
	}
	slices.Sort(want)

	if !slices.Equal(got, want) {
		t.Fatalf("trigger environment function list drifted!\ngot (%d):  %v\nwant (%d): %v", len(got), got, len(want), want)
	}
}

func TestMatchAndSubjectShareCacheScopeWithoutEviction(t *testing.T) {
	celEng, err := celengine.New(triggerrule.Env(), triggerrule.Stage1Limits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	re := ruleengine.NewEngine()
	if err := re.Register(celDialect); err != nil {
		t.Fatalf("Register: %v", err)
	}
	re.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.DialectCEL)
	rules, err := triggerrule.New(re)
	if err != nil {
		t.Fatalf("triggerrule.New: %v", err)
	}

	ctx := context.Background()
	scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-both", Revision: 1}
	matchRule := triggerrule.Rule{Expr: `event.type == "webhook.deploy.finished"`}
	subjRule := triggerrule.Rule{Expr: `"done:" + string(event.source.id)`}

	for i := 0; i < 5; i++ {
		ev := sampleFireEvent(`{"ok":true}`)
		act, err := triggerrule.NewActivation(ev, hubfire.FireTriggerRef{ID: "trig-both", Name: "both"})
		if err != nil {
			t.Fatalf("NewActivation %d: %v", i, err)
		}
		ok, err := rules.Match(ctx, scope, matchRule, act)
		if err != nil || !ok {
			t.Fatalf("iter %d Match = (%v, %v), want (true, nil)", i, ok, err)
		}
		subj, err := rules.Subject(ctx, scope, subjRule, act)
		if err != nil || subj != "done:ep-456" {
			t.Fatalf("iter %d Subject = (%q, %v), want (\"done:ep-456\", nil)", i, subj, err)
		}
	}

	stats := celEng.CacheStats()
	if stats.Entries != 2 || stats.Misses != 2 || stats.Hits != 8 {
		t.Fatalf("expected Match+Subject on shared scope to hit cache (Entries=2, Misses=2, Hits=8), got %+v", stats)
	}

	rules.Invalidate(ruleengine.DialectCEL, scope)
	if got := celEng.CacheStats().Entries; got != 0 {
		t.Fatalf("Entries after Invalidate = %d, want 0", got)
	}
}

func TestMatchTimeoutAndCallerCancellationSkipReason(t *testing.T) {
	limits := triggerrule.Stage1Limits()
	limits.CostLimit = celengine.MaxCostLimitCeiling
	limits.EvalTimeout = 20 * time.Millisecond
	limits.InterruptFrequency = 1

	celEng, err := celengine.New(triggerrule.Env(), limits, celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	re := ruleengine.NewEngine()
	if err := re.Register(celDialect); err != nil {
		t.Fatalf("Register: %v", err)
	}
	re.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.DialectCEL)
	rules, err := triggerrule.New(re)
	if err != nil {
		t.Fatalf("triggerrule.New: %v", err)
	}

	var buf bytes.Buffer
	buf.WriteString(`{"items":[`)
	for i := 0; i < 150; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`"elem"`)
	}
	buf.WriteString(`]}`)

	ev := sampleFireEvent(buf.String())
	tr := hubfire.FireTriggerRef{ID: "trig-timeout", Name: "timeout"}
	act, err := triggerrule.NewActivation(ev, tr)
	if err != nil {
		t.Fatalf("NewActivation: %v", err)
	}
	scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr.ID, Revision: 1}
	slowRule := triggerrule.Rule{
		Expr: `event.payload.items.all(a, event.payload.items.all(b, event.payload.items.all(c, string(a).size() + string(b).size() + string(c).size() >= 0)))`,
	}

	// 1. Pathological expression hits engine timeout -> ReasonTimeout and terminal skip reason.
	_, err = rules.Match(context.Background(), scope, slowRule, act)
	var ferr *ruleengine.FailedError
	if !errors.As(err, &ferr) || ferr.Reason != celengine.ReasonTimeout {
		t.Fatalf("Match pathological expr: got %T (%v), want *FailedError{Reason: %q}", err, err, celengine.ReasonTimeout)
	}
	if reason, ok := triggerrule.SkipReason(err); !ok || reason != triggerrule.SkipReasonEvalFailed {
		t.Fatalf("SkipReason(timeout) = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonEvalFailed)
	}

	// 2. Pre-canceled context is NOT a terminal skip reason (both cached and uncached).
	preCanceled, preCancel := context.WithCancel(context.Background())
	preCancel()
	_, err = rules.Match(preCanceled, scope, slowRule, act)
	if err == nil {
		t.Fatalf("expected error on pre-canceled context")
	}
	if reason, ok := triggerrule.SkipReason(err); ok {
		t.Fatalf("SkipReason(pre-canceled) = (%q, true), want (\"\", false)", reason)
	}

	// 3. Caller context expiring mid-evaluation is NOT a terminal skip reason.
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer shortCancel()
	_, err = rules.Match(shortCtx, scope, slowRule, act)
	ferr = nil
	if !errors.As(err, &ferr) || ferr.Reason != celengine.ReasonCanceled {
		t.Fatalf("Match caller deadline: got %T (%v), want *FailedError{Reason: %q}", err, err, celengine.ReasonCanceled)
	}
	if reason, ok := triggerrule.SkipReason(err); ok {
		t.Fatalf("SkipReason(caller deadline) = (%q, true), want (\"\", false)", reason)
	}

	// 4. Per-event budget remainder expiring mid-eval maps to SkipReasonBudgetExhausted.
	budget := triggerrule.NewEventBudget()
	budget.Charge(triggerrule.EventRuleBudget - 8*time.Millisecond)
	_, err = rules.Match(context.Background(), scope, slowRule, act, triggerrule.WithBudget(budget))
	if !errors.Is(err, ruleengine.ErrBudgetExhausted) {
		t.Fatalf("Match budget remainder expiry: got %T (%v), want ErrBudgetExhausted", err, err)
	}
	if reason, ok := triggerrule.SkipReason(err); !ok || reason != triggerrule.SkipReasonBudgetExhausted {
		t.Fatalf("SkipReason(budget remainder) = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonBudgetExhausted)
	}
}

func TestAmplifyingCallsOn64KiBPayloadBounded(t *testing.T) {
	rules, err := triggerrule.NewDefault()
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	ctx := context.Background()

	// Build a ~62 KiB valid JSON payload (within the 64 KiB ingress bound).
	s60k := strings.Repeat("a", 60*1024)
	payload := `{"s":"` + s60k + `","q":"` + strings.Repeat(`\"`, 1024) + `","p":"[a-z]{0,1000}x"}`
	if len(payload) > triggerrule.MaxPayloadBytes {
		t.Fatalf("test payload length %d exceeds MaxPayloadBytes %d", len(payload), triggerrule.MaxPayloadBytes)
	}

	ev := sampleFireEvent(payload)
	tr := hubfire.FireTriggerRef{ID: "trig-amp", Name: "amplifying-guard"}
	act, err := triggerrule.NewActivation(ev, tr)
	if err != nil {
		t.Fatalf("NewActivation: %v", err)
	}

	nestedListConcat := `[event.payload.s.split("")].map(l, (l + l).size() > 0)[0]`
	literalOverworkRegex := strings.Repeat("a?", 80) + strings.Repeat("a", 80) + "x"

	matchVectors := []struct {
		name string
		expr string
	}{
		{
			name: "split_join_amplification",
			expr: `event.payload.s.split("").join(event.payload.s).size() > 0`,
		},
		{
			name: "replace_amplification",
			expr: `event.payload.s.replace("a", event.payload.s).size() > 0`,
		},
		{
			name: "replace_empty_old_amplification",
			expr: `event.payload.s.replace("", event.payload.s).size() > 0`,
		},
		{
			name: "format_list_amplification",
			expr: `"%s".format([[event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s, event.payload.s]]).size() > 0`,
		},
		{
			name: "strings_quote_amplification",
			expr: `[event.payload.q].map(a, [a + a].map(b, [b + b].map(c, [c + c].map(d, [d + d].map(e, [e + e].map(f, [f + f].map(g, [g + g].map(h, [h + h].map(k, strings.quote(k + k))))))))))[0][0][0][0][0][0][0][0][0].size() > 0`,
		},
		{
			name: "string_concat_amplification",
			expr: `[event.payload.s].map(a, [a + a].map(b, [b + b].map(c, [c + c].map(d, [d + d].map(e, e + e)))))[0][0][0][0][0].size() > 0`,
		},
		{
			name: "list_concat_amplification",
			expr: nestedListConcat,
		},
		{
			name: "matches_literal_overwork",
			expr: `string(event.payload.s).matches("` + literalOverworkRegex + `")`,
		},
		{
			name: "short_circuit_or_cannot_bypass",
			expr: `event.payload.s.split("").join(event.payload.s) == "" || true`,
		},
	}

	maxElapsed := 2 * triggerrule.Stage1Limits().EvalTimeout
	if raceEnabled {
		maxElapsed = 3 * time.Second
	}
	const maxAllocDelta = 64 << 20 // 64 MiB

	for i, tc := range matchVectors {
		t.Run(tc.name, func(t *testing.T) {
			scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr.ID, Revision: int64(i + 1)}

			var mBefore, mAfter runtime.MemStats
			runtime.ReadMemStats(&mBefore)
			start := time.Now()
			matched, evalErr := rules.Match(ctx, scope, triggerrule.Rule{Expr: tc.expr}, act)
			elapsed := time.Since(start)
			runtime.ReadMemStats(&mAfter)

			if matched {
				t.Fatalf("Match(%s) = true, want false on cost_limit error", tc.name)
			}
			var ferr *ruleengine.FailedError
			allowedErr := errors.As(evalErr, &ferr) &&
				(ferr.Reason == celengine.ReasonCostLimit || (raceEnabled && ferr.Reason == celengine.ReasonTimeout))
			if !allowedErr {
				t.Fatalf("Match(%s) err = %T (%v), want *FailedError{Reason: %q}", tc.name, evalErr, evalErr, celengine.ReasonCostLimit)
			}
			if reason, ok := triggerrule.SkipReason(evalErr); !ok || reason != triggerrule.SkipReasonEvalFailed {
				t.Fatalf("SkipReason = (%q, %v), want (%q, true)", reason, ok, triggerrule.SkipReasonEvalFailed)
			}
			if elapsed > maxElapsed {
				t.Errorf("elapsed %s exceeds wall-clock bound (%s)", elapsed, maxElapsed)
			}
			allocDelta := mAfter.TotalAlloc - mBefore.TotalAlloc
			if allocDelta > maxAllocDelta {
				t.Errorf("TotalAlloc delta %d bytes (%.2f MiB) exceeds 64 MiB bound", allocDelta, float64(allocDelta)/(1<<20))
			}
		})
	}

	// Also verify Subject / DeriveSubject falls back safely within time & memory bounds.
	t.Run("subject_amplification_falls_back", func(t *testing.T) {
		scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: tr.ID, Revision: 99}
		var mBefore, mAfter runtime.MemStats
		runtime.ReadMemStats(&mBefore)
		start := time.Now()
		got := rules.DeriveSubject(ctx, scope, triggerrule.Rule{
			Expr: `event.payload.s.split("").join(event.payload.s)`,
		}, act, ev.Subject, nil)
		elapsed := time.Since(start)
		runtime.ReadMemStats(&mAfter)

		if got != ev.Subject {
			t.Fatalf("DeriveSubject = %q, want fallback %q", got, ev.Subject)
		}
		if elapsed > maxElapsed {
			t.Errorf("elapsed %s exceeds wall-clock bound (%s)", elapsed, maxElapsed)
		}
		allocDelta := mAfter.TotalAlloc - mBefore.TotalAlloc
		if allocDelta > maxAllocDelta {
			t.Errorf("TotalAlloc delta %d bytes exceeds 64 MiB bound", allocDelta)
		}
	})
}

func TestPathologicalRuleDoesNotBlockSubsequentTriggerOnSameEvent(t *testing.T) {
	ctx := context.Background()

	newBenignSurvivalBudget := func() *ruleengine.BudgetTracker {
		if raceEnabled {
			return ruleengine.NewBudgetTracker(5 * time.Second)
		}
		return triggerrule.NewEventBudget()
	}
	maxEventWall := triggerrule.EventRuleBudget
	if raceEnabled {
		maxEventWall = 5 * time.Second
	}

	t.Run("pathological_single_call_rules_then_benign_rule_matches_under_1s", func(t *testing.T) {
		rules, err := triggerrule.NewDefault()
		if err != nil {
			t.Fatalf("NewDefault: %v", err)
		}

		s60k := strings.Repeat("a", 60*1024)
		payload := `{"s":"` + s60k + `","p":"[a-z]{0,1000}x","env":"prod"}`
		ev := sampleFireEvent(payload)
		baseAct, err := triggerrule.NewEventActivation(ev)
		if err != nil {
			t.Fatalf("NewEventActivation: %v", err)
		}

		literalOverwork := strings.Repeat("a?", 80) + strings.Repeat("a", 80) + "x"
		badRules := []struct {
			id         string
			expr       string
			wantReason string
		}{
			{id: "trig-bad-dyn-re", expr: `string(event.payload.s).matches(string(event.payload.p))`, wantReason: triggerrule.SkipReasonNoLongerCompiles},
			{id: "trig-bad-lit-re", expr: `string(event.payload.s).matches("` + literalOverwork + `")`, wantReason: triggerrule.SkipReasonEvalFailed},
			{id: "trig-bad-split-join", expr: `event.payload.s.split("").join(event.payload.s).size() > 0`, wantReason: triggerrule.SkipReasonEvalFailed},
		}

		budget := newBenignSurvivalBudget()
		start := time.Now()

		for _, br := range badRules {
			act := baseAct.WithTrigger(hubfire.FireTriggerRef{ID: br.id, Name: br.id})
			scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: br.id, Revision: 1}
			matched, evalErr := rules.Match(ctx, scope, triggerrule.Rule{Expr: br.expr}, act, triggerrule.WithBudget(budget))
			if matched {
				t.Fatalf("%s unexpectedly matched", br.id)
			}
			reason, ok := triggerrule.SkipReason(evalErr)
			if !ok || reason != br.wantReason {
				t.Fatalf("%s SkipReason = (%q, %v), want (%q, true) (err=%v)", br.id, reason, ok, br.wantReason, evalErr)
			}
		}

		benignRef := hubfire.FireTriggerRef{ID: "trig-benign", Name: "benign"}
		benignAct := baseAct.WithTrigger(benignRef)
		benignScope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: benignRef.ID, Revision: 1}
		matched, err := rules.Match(ctx, benignScope, triggerrule.Rule{
			Expr: `event.type == "webhook.deploy.finished" && event.payload.env == "prod"`,
		}, benignAct, triggerrule.WithBudget(budget))
		if err != nil || !matched {
			t.Fatalf("benign trigger Match = (%v, %v), want (true, nil)", matched, err)
		}
		subj := rules.DeriveSubject(ctx, benignScope, triggerrule.Rule{
			Expr: `"deploy:" + string(event.payload.env)`,
		}, benignAct, ev.Subject, nil, triggerrule.WithBudget(budget))
		elapsed := time.Since(start)

		if subj != "deploy:prod" {
			t.Fatalf("benign DeriveSubject = %q, want %q", subj, "deploy:prod")
		}
		if elapsed > maxEventWall {
			t.Errorf("end-to-end event evaluation took %s, want <= %s", elapsed, maxEventWall)
		}
	})

	t.Run("container_eq_comprehension_rule_then_benign_rule_matches_under_1s", func(t *testing.T) {
		rules, err := triggerrule.NewDefault()
		if err != nil {
			t.Fatalf("NewDefault: %v", err)
		}

		var buf bytes.Buffer
		buf.WriteString(`{"env":"prod","s":"`)
		buf.WriteString(strings.Repeat("a", 20_000))
		buf.WriteString(`","l":[`)
		for i := 0; i < 12_000; i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteByte('0')
		}
		buf.WriteString(`]}`)
		if buf.Len() > triggerrule.MaxPayloadBytes {
			t.Fatalf("payload len %d exceeds MaxPayloadBytes %d", buf.Len(), triggerrule.MaxPayloadBytes)
		}

		ev := sampleFireEvent(buf.String())
		baseAct, err := triggerrule.NewEventActivation(ev)
		if err != nil {
			t.Fatalf("NewEventActivation: %v", err)
		}

		terms := make([]string, 25)
		for i := range terms {
			terms[i] = fmt.Sprintf(
				"[event.payload.l,event.payload.l,event.payload.l,event.payload.l,event.payload.l]==[event.payload.l,event.payload.l,event.payload.l,event.payload.l,%d]",
				i,
			)
		}
		eqComprehensionExpr := "event.payload.l.exists(x," + strings.Join(terms, "||") + ")"
		if len(eqComprehensionExpr) > triggerrule.MaxMatchRuleBytes {
			t.Fatalf("eqComprehensionExpr len %d exceeds MaxMatchRuleBytes %d", len(eqComprehensionExpr), triggerrule.MaxMatchRuleBytes)
		}

		budget := newBenignSurvivalBudget()
		start := time.Now()

		badAct := baseAct.WithTrigger(hubfire.FireTriggerRef{ID: "trig-bad-eq-comp", Name: "bad-eq-comp"})
		badScope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-bad-eq-comp", Revision: 1}
		matched, evalErr := rules.Match(ctx, badScope, triggerrule.Rule{Expr: eqComprehensionExpr}, badAct, triggerrule.WithBudget(budget))
		badElapsed := time.Since(start)
		t.Logf("bad eq comprehension elapsed=%s err=%v", badElapsed, evalErr)
		if matched {
			t.Fatalf("trig-bad-eq-comp unexpectedly matched")
		}
		reason, ok := triggerrule.SkipReason(evalErr)
		if !ok || reason != triggerrule.SkipReasonEvalFailed {
			t.Fatalf("trig-bad-eq-comp SkipReason = (%q, %v), want (%q, true) (err=%v)", reason, ok, triggerrule.SkipReasonEvalFailed, evalErr)
		}

		benignRef := hubfire.FireTriggerRef{ID: "trig-benign-after-eq", Name: "benign"}
		benignAct := baseAct.WithTrigger(benignRef)
		benignScope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: benignRef.ID, Revision: 1}
		matched, err = rules.Match(ctx, benignScope, triggerrule.Rule{
			Expr: `event.type == "webhook.deploy.finished" && event.payload.env == "prod"`,
		}, benignAct, triggerrule.WithBudget(budget))
		elapsed := time.Since(start)
		if err != nil || !matched {
			t.Fatalf("benign trigger Match = (%v, %v), want (true, nil) (total elapsed=%s)", matched, err, elapsed)
		}
		if elapsed > maxEventWall {
			t.Errorf("end-to-end event evaluation took %s, want <= %s", elapsed, maxEventWall)
		}
	})

	t.Run("cumulative_timeout_rules_exhaust_1s_budget_and_record_budget_exhausted", func(t *testing.T) {
		limits := triggerrule.Stage1Limits()
		limits.CostLimit = celengine.MaxCostLimitCeiling
		limits.EvalTimeout = 100 * time.Millisecond
		limits.InterruptFrequency = 1

		celEng, err := celengine.New(triggerrule.Env(), limits, celengine.DefaultCacheConfig())
		if err != nil {
			t.Fatalf("celengine.New: %v", err)
		}
		celDialect, err := ruleengine.NewCELDialect(celEng)
		if err != nil {
			t.Fatalf("NewCELDialect: %v", err)
		}
		re := ruleengine.NewEngine()
		if err := re.Register(celDialect); err != nil {
			t.Fatalf("Register: %v", err)
		}
		re.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.DialectCEL)
		rules, err := triggerrule.New(re)
		if err != nil {
			t.Fatalf("triggerrule.New: %v", err)
		}

		var buf bytes.Buffer
		buf.WriteString(`{"env":"prod","items":[`)
		for i := 0; i < 160; i++ {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(`"elem"`)
		}
		buf.WriteString(`]}`)

		ev := sampleFireEvent(buf.String())
		baseAct, err := triggerrule.NewEventActivation(ev)
		if err != nil {
			t.Fatalf("NewEventActivation: %v", err)
		}

		slowRule := triggerrule.Rule{
			Expr: `event.payload.items.all(a, event.payload.items.all(b, event.payload.items.all(c, string(a).size() + string(b).size() + string(c).size() >= 0)))`,
		}
		budget := triggerrule.NewEventBudget()
		start := time.Now()

		for i := 0; i < 12; i++ {
			id := "trig-slow-" + string(rune('a'+i))
			act := baseAct.WithTrigger(hubfire.FireTriggerRef{ID: id, Name: id})
			scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: id, Revision: 1}
			matched, evalErr := rules.Match(ctx, scope, slowRule, act, triggerrule.WithBudget(budget))
			if matched {
				t.Fatalf("%s unexpectedly matched", id)
			}
			reason, ok := triggerrule.SkipReason(evalErr)
			if !ok || (reason != triggerrule.SkipReasonEvalFailed && reason != triggerrule.SkipReasonBudgetExhausted) {
				t.Fatalf("%s SkipReason = (%q, %v), want eval failed or budget exhausted (err=%v)", id, reason, ok, evalErr)
			}
			if reason == triggerrule.SkipReasonBudgetExhausted {
				break
			}
		}

		benignRef := hubfire.FireTriggerRef{ID: "trig-after-exhausted", Name: "benign"}
		benignAct := baseAct.WithTrigger(benignRef)
		benignScope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: benignRef.ID, Revision: 1}
		matched, evalErr := rules.Match(ctx, benignScope, triggerrule.Rule{
			Expr: `event.payload.env == "prod"`,
		}, benignAct, triggerrule.WithBudget(budget))
		elapsed := time.Since(start)

		if matched {
			t.Fatalf("expected benign trigger after exhausted event budget to skip, got matched=true")
		}
		reason, ok := triggerrule.SkipReason(evalErr)
		if !ok || reason != triggerrule.SkipReasonBudgetExhausted {
			t.Fatalf("benign trigger SkipReason = (%q, %v), want (%q, true) (err=%v)", reason, ok, triggerrule.SkipReasonBudgetExhausted, evalErr)
		}
		maxCumulativeWall := 1400 * time.Millisecond
		if raceEnabled {
			maxCumulativeWall = 5 * time.Second
		}
		if elapsed > maxCumulativeWall {
			t.Errorf("cumulative 1s event budget run took %s, want <= %s", elapsed, maxCumulativeWall)
		}
	})
}

type wrappingStubDialect struct {
	evalFn func(ctx context.Context) error
}

func (d *wrappingStubDialect) Name() string { return "wrap_stub" }
func (d *wrappingStubDialect) Declares() ruleengine.Capabilities {
	return ruleengine.Capabilities{
		PerformsIO:  true,
		MaxEvalTime: 20 * time.Millisecond,
		ResultKinds: []ruleengine.Kind{ruleengine.KindFilter},
	}
}
func (d *wrappingStubDialect) Compile(_ context.Context, rule string, kind ruleengine.Kind, _ ruleengine.EnvironmentRef) (ruleengine.Compiled, error) {
	return ruleengine.NewCompiled(d.Name(), kind, rule), nil
}
func (d *wrappingStubDialect) Evaluate(ctx context.Context, _ ruleengine.Compiled, _ ruleengine.Input, _ ruleengine.Budget) (ruleengine.Decision, error) {
	return ruleengine.Decision{}, d.evalFn(ctx)
}

func TestSkipReasonPreservedWhenEngineWrapsDialectErrors(t *testing.T) {
	celEng, err := celengine.New(triggerrule.Env(), triggerrule.Stage1Limits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("celengine.New: %v", err)
	}
	celDialect, err := ruleengine.NewCELDialect(celEng)
	if err != nil {
		t.Fatalf("NewCELDialect: %v", err)
	}
	stub := &wrappingStubDialect{}
	re := ruleengine.NewEngine()
	if err := re.Register(celDialect); err != nil {
		t.Fatalf("Register CEL: %v", err)
	}
	if err := re.Register(stub); err != nil {
		t.Fatalf("Register stub: %v", err)
	}
	re.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.DialectCEL, "wrap_stub")

	rules, err := triggerrule.New(re)
	if err != nil {
		t.Fatalf("triggerrule.New: %v", err)
	}

	ev := sampleFireEvent(`{"env":"prod"}`)
	act, err := triggerrule.NewActivation(ev, hubfire.FireTriggerRef{ID: "trig-wrap", Name: "wrap"})
	if err != nil {
		t.Fatalf("NewActivation: %v", err)
	}
	scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-wrap", Revision: 1}
	rule := triggerrule.Rule{Dialect: "wrap_stub", Expr: "check"}

	// 1. Dialect timeout wrapping an inner UncertainError still maps to SkipReasonEvalFailed.
	stub.evalFn = func(ctx context.Context) error {
		<-ctx.Done()
		return &ruleengine.UncertainError{Reason: "inner_uncertain", Cause: ctx.Err()}
	}
	_, evalErr := rules.Match(context.Background(), scope, rule, act)
	if reason, ok := triggerrule.SkipReason(evalErr); !ok || reason != triggerrule.SkipReasonEvalFailed {
		t.Fatalf("timeout wrapping UncertainError: SkipReason = (%q, %v), want (%q, true) (err=%v)",
			reason, ok, triggerrule.SkipReasonEvalFailed, evalErr)
	}

	// 2. Tracker budget exhausted wrapping an inner canceled FailedError still maps to SkipReasonBudgetExhausted.
	stub.evalFn = func(ctx context.Context) error {
		<-ctx.Done()
		return &ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: ctx.Err()}
	}
	tightTracker := ruleengine.NewBudgetTracker(8 * time.Millisecond)
	_, evalErr = rules.Match(context.Background(), scope, rule, act, triggerrule.WithBudget(tightTracker))
	if reason, ok := triggerrule.SkipReason(evalErr); !ok || reason != triggerrule.SkipReasonBudgetExhausted {
		t.Fatalf("budget exhausted wrapping inner canceled: SkipReason = (%q, %v), want (%q, true) (err=%v)",
			reason, ok, triggerrule.SkipReasonBudgetExhausted, evalErr)
	}

	// 3. Caller cancellation wrapping an inner UncertainError still returns ("", false).
	stub.evalFn = func(ctx context.Context) error {
		<-ctx.Done()
		return &ruleengine.UncertainError{Reason: "inner_uncertain", Cause: ctx.Err()}
	}
	callerCtx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	_, evalErr = rules.Match(callerCtx, scope, rule, act)
	if reason, ok := triggerrule.SkipReason(evalErr); ok || reason != "" {
		t.Fatalf("caller cancel wrapping UncertainError: SkipReason = (%q, %v), want (\"\", false) (err=%v)",
			reason, ok, evalErr)
	}
}

func TestPass6RegexOptionAPreScanAndOrdinaryRules(t *testing.T) {
	rules, err := triggerrule.NewDefault()
	if err != nil {
		t.Fatalf("NewDefault: %v", err)
	}
	ctx := context.Background()
	maxCompileElapsed := 50 * time.Millisecond
	if raceEnabled {
		maxCompileElapsed = 500 * time.Millisecond
	}

	// 1. Authoring CompileMatch / CompileSubject rejects:
	//    (a) Pass-6 4,022-byte folded-range literal (CategorySize, fast, no 2.8 s parse);
	//    (b) payload-sourced pattern (CategoryRegexLiteralRequired);
	//    (c) Pass-6 expression-built map/join pattern (CategoryRegexLiteralRequired).
	t.Run("compile_rejects_pass6_folded_literal_payload_and_expr_patterns_fast", func(t *testing.T) {
		pass6FoldedLiteral := `"a".matches(r"(?i)[` + strings.Repeat(`\x{41}-\x{1e942}`, 250) + `]")`
		var z50, z80 strings.Builder
		z50.WriteByte('[')
		for i := 0; i < 50; i++ {
			if i > 0 {
				z50.WriteByte(',')
			}
			z50.WriteString("0")
		}
		z50.WriteByte(']')
		z80.WriteByte('[')
		for i := 0; i < 80; i++ {
			if i > 0 {
				z80.WriteByte(',')
			}
			z80.WriteString("0")
		}
		z80.WriteByte(']')
		pass6ExprBuilt := `"a".matches("(?i)" + ` + z50.String() + `.map(i, ` + z80.String() + `.map(j, r"[\x{41}-\x{1e942}]").join()).join())`

		cases := []struct {
			name    string
			expr    string
			wantCat string
		}{
			{name: "pass6_folded_range_literal_4k", expr: pass6FoldedLiteral, wantCat: celengine.CategorySize},
			{name: "payload_sourced_receiver_matches", expr: `"a".matches(event.payload.p)`, wantCat: celengine.CategoryRegexLiteralRequired},
			{name: "payload_sourced_global_matches", expr: `matches("a", event.payload.p)`, wantCat: celengine.CategoryRegexLiteralRequired},
			{name: "pass6_expr_built_folded_range", expr: pass6ExprBuilt, wantCat: celengine.CategoryRegexLiteralRequired},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				start := time.Now()
				err := rules.CompileMatch(ctx, triggerrule.Rule{Expr: tc.expr})
				elapsed := time.Since(start)
				if err == nil {
					t.Fatalf("CompileMatch(%s) succeeded, want *CompileError", tc.name)
				}
				var cerr *celengine.CompileError
				if !errors.As(err, &cerr) {
					t.Fatalf("CompileMatch(%s) err = %T (%v), want *CompileError", tc.name, err, err)
				}
				if cerr.Category != tc.wantCat {
					t.Errorf("Category = %q, want %q (err=%v)", cerr.Category, tc.wantCat, cerr)
				}
				if cerr.Line <= 0 || cerr.Column <= 0 {
					t.Errorf("invalid 1-based position %d:%d", cerr.Line, cerr.Column)
				}
				if elapsed > maxCompileElapsed {
					t.Errorf("CompileMatch(%s) took %s, want <= %s", tc.name, elapsed, maxCompileElapsed)
				}
			})
		}

		// Also verify CompileSubject rejects non-literal matches in ternary subject expression.
		subjErr := rules.CompileSubject(ctx, triggerrule.Rule{
			Expr: `"a".matches(event.payload.p) ? "x" : "y"`,
		})
		var subjCerr *celengine.CompileError
		if !errors.As(subjErr, &subjCerr) || subjCerr.Category != celengine.CategoryRegexLiteralRequired {
			t.Fatalf("CompileSubject non-literal matches = %v, want Category %q", subjErr, celengine.CategoryRegexLiteralRequired)
		}
	})

	// 2. If a stored pre-Option-A dynamic rule encounters the exact Pass-6 64 KiB
	// 3,600-range payload at fire time, Match refuses at compile in milliseconds
	// (SkipReasonNoLongerCompiles) and never parses the sender's payload as a regex.
	t.Run("stored_dynamic_rule_against_64k_folded_payload_never_parses_payload", func(t *testing.T) {
		bombedPat := `(?i)[` + strings.Repeat(`\\x{41}-\\x{1e942}`, 3200) + `]`
		ev := sampleFireEvent(`{"p":"` + bombedPat + `"}`)
		act, err := triggerrule.NewActivation(ev, hubfire.FireTriggerRef{ID: "trig-stored-dyn", Name: "stored-dyn"})
		if err != nil {
			t.Fatalf("NewActivation: %v", err)
		}
		scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: "trig-stored-dyn", Revision: 1}

		start := time.Now()
		matched, evalErr := rules.Match(ctx, scope, triggerrule.Rule{Expr: `"a".matches(event.payload.p)`}, act)
		elapsed := time.Since(start)
		if matched {
			t.Fatalf("unexpected match")
		}
		reason, ok := triggerrule.SkipReason(evalErr)
		if !ok || reason != triggerrule.SkipReasonNoLongerCompiles {
			t.Fatalf("SkipReason = (%q, %v), want (%q, true) (err=%v)", reason, ok, triggerrule.SkipReasonNoLongerCompiles, evalErr)
		}
		if elapsed > maxCompileElapsed {
			t.Errorf("Match took %s, want <= %s", elapsed, maxCompileElapsed)
		}
	})

	// 3. Ordinary rules: 14-keyword (?i) alternation over 60 KiB payload field,
	// plus cached brace-containing patterns (SHA-256, UUID, semver, conventional-commit).
	t.Run("ordinary_14_keyword_60k_and_cached_brace_patterns_succeed", func(t *testing.T) {
		ordRulesEngine := rules
		if raceEnabled {
			lim := triggerrule.Stage1Limits()
			lim.EvalTimeout = 500 * time.Millisecond
			celEng, err := celengine.New(triggerrule.Env(), lim, celengine.DefaultCacheConfig())
			if err != nil {
				t.Fatalf("celengine.New: %v", err)
			}
			celDialect, err := ruleengine.NewCELDialect(celEng)
			if err != nil {
				t.Fatalf("NewCELDialect: %v", err)
			}
			re := ruleengine.NewEngine()
			if err := re.Register(celDialect); err != nil {
				t.Fatalf("Register: %v", err)
			}
			re.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.DialectCEL)
			ordRulesEngine, err = triggerrule.New(re)
			if err != nil {
				t.Fatalf("triggerrule.New: %v", err)
			}
		}

		body60k := strings.Repeat("normal log line ok ", 3200) + "CRITICAL alert fired"
		payload := fmt.Sprintf(
			`{"body":%q,"sha":"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855","uuid":"550e8400-e29b-41d4-a716-446655440000","ver":"v2.5.1-rc.2","msg":"fix(hub): guard webhook"}`,
			body60k,
		)
		ev := sampleFireEvent(payload)
		act, err := triggerrule.NewActivation(ev, hubfire.FireTriggerRef{ID: "trig-ord", Name: "ord"})
		if err != nil {
			t.Fatalf("NewActivation: %v", err)
		}

		ordRules := []string{
			`string(event.payload.body).matches("(?i)\\b(deploy|rollback|incident|outage|hotfix|release|canary|promote|degraded|critical|escalate|mitigate|resolved|postmortem)\\b")`,
			`string(event.payload.sha).matches("^[0-9a-fA-F]{64}$")`,
			`string(event.payload.uuid).matches("^(?i)[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")`,
			`string(event.payload.ver).matches("^v?(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?$")`,
			`string(event.payload.msg).matches("^(feat|fix|docs|chore)(\\([a-z0-9_/-]+\\))?!?: .+$")`,
		}

		for i, expr := range ordRules {
			if err := ordRulesEngine.CompileMatch(ctx, triggerrule.Rule{Expr: expr}); err != nil {
				t.Fatalf("CompileMatch[%d]: %v", i, err)
			}
			scope := celengine.CacheScope{Consumer: triggerrule.ConsumerName, ID: fmt.Sprintf("trig-ord-%d", i), Revision: 1}
			for pass := 0; pass < 3; pass++ {
				ok, err := ordRulesEngine.Match(ctx, scope, triggerrule.Rule{Expr: expr}, act)
				if err != nil || !ok {
					t.Fatalf("Match[%d] pass %d = (%v, %v), want (true, nil)", i, pass, ok, err)
				}
			}
		}
	})
}
