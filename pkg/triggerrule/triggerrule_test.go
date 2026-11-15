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
	"log/slog"
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
