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

package messagepolicy_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

func sampleFacts(body string) messagepolicy.Facts {
	return messagepolicy.Facts{
		Surface: messagepolicy.SurfaceAgentToUser,
		Sender: messagepolicy.SenderFacts{
			Kind:      "agent",
			ID:        "agent-uuid-1",
			Slug:      "dev-worker",
			ProjectID: "proj-uuid-1",
		},
		Recipient: messagepolicy.RecipientFacts{
			Kind: "user",
			ID:   "user-uuid-1",
			Slug: "alice",
		},
		Message: messagepolicy.MessageFacts{
			Type:            "instruction",
			Body:            body,
			HasChoices:      false,
			Urgent:          true,
			AttachmentCount: 1,
		},
		MentionedAgentIDs: []string{"agent-uuid-2"},
		MentionedUserIDs:  []string{"user-uuid-1"},
		Conversation: messagepolicy.ConversationFacts{
			Kind:                "direct",
			AgentParticipantIDs: []string{"agent-uuid-1"},
			UserParticipantIDs:  []string{"user-uuid-1"},
		},
	}
}

func TestEnvAndActivation_V1Contract(t *testing.T) {
	ev, err := messagepolicy.NewDefault()
	require.NoError(t, err)
	ctx := context.Background()

	// Exercise all 10 declared v1 variables from consumer design §5.4.
	rule := `
		surface == "agent_to_user" &&
		sender.kind == "agent" &&
		sender.slug == "dev-worker" &&
		recipient.kind == "user" &&
		recipient.slug == "alice" &&
		message.type == "instruction" &&
		!message.has_choices &&
		message.urgent &&
		message.attachment_count == 1 &&
		message.bytes == 19 &&
		!message.body_truncated &&
		"agent-uuid-2" in mentioned_agent_ids &&
		"user-uuid-1" in mentioned_user_ids &&
		conversation.kind == "direct" &&
		size(conversation.agent_participant_ids) == 1 &&
		size(judgment_scores) == 0
	`
	act := messagepolicy.NewActivation(sampleFacts("Please pick A or B."))
	res := ev.EvaluateViolation(ctx, "pol-1", 1, rule, act)
	assert.Equal(t, messagepolicy.ClassViolation, res.Class)
	assert.Empty(t, res.Reason)

	passRes := ev.EvaluateViolation(ctx, "pol-2", 1, `message.has_choices`, act)
	assert.Equal(t, messagepolicy.ClassPass, passRes.Class)
	assert.Empty(t, passRes.Reason)
}

func TestActivation_BodyTruncationUTF8Safe(t *testing.T) {
	ev, err := messagepolicy.NewDefault()
	require.NoError(t, err)
	ctx := context.Background()

	// Place a 4-byte emoji ("🚀") straddling the 16 KiB boundary (offset 16382..16386)
	// to verify UTF-8 safe truncation without splitting the multi-byte codepoint.
	prefix := strings.Repeat("a", messagepolicy.MaxBodyBytes-2)
	rawBody := prefix + "🚀tail"
	require.Greater(t, len(rawBody), messagepolicy.MaxBodyBytes)

	act := messagepolicy.NewActivation(sampleFacts(rawBody))
	msgMap, ok := act.Map()["message"].(map[string]any)
	require.True(t, ok)
	truncBody, ok := msgMap["body"].(string)
	require.True(t, ok)

	assert.True(t, utf8.ValidString(truncBody), "truncated body must remain valid UTF-8")
	assert.Equal(t, messagepolicy.MaxBodyBytes-2, len(truncBody), "partial 4-byte rune at boundary must be dropped cleanly")
	assert.Equal(t, true, msgMap["body_truncated"])
	assert.Equal(t, int64(len(rawBody)), msgMap["bytes"], "message.bytes must report original pre-truncation byte length")

	res := ev.EvaluateViolation(ctx, "pol-trunc", 1, `message.body_truncated && message.bytes > 16384`, act)
	assert.Equal(t, messagepolicy.ClassViolation, res.Class)
}

// Target 4: CEL error, missing input and budget exhaustion -> class failed,
// mapped strictly by on_failed / on_failed_reasons.
func TestClassification_Target4_ErrorsMissingInputAndBudget(t *testing.T) {
	ev, err := messagepolicy.NewDefault()
	require.NoError(t, err)
	ctx := context.Background()
	act := messagepolicy.NewActivation(sampleFacts("hello"))

	// 1. Reading an unevaluated judgment key MUST produce missing_input -> failed, never false.
	missingRes := ev.EvaluateViolation(ctx, "pol-missing", 1, `judgment_flags.asks_choice`, act)
	assert.Equal(t, messagepolicy.ClassFailed, missingRes.Class)
	assert.Equal(t, celengine.ReasonMissingInput, missingRes.Reason)
	assert.Equal(t, messagepolicy.OutcomeAdmit, messagepolicy.ResolveOutcome(
		missingRes.Class, missingRes.Reason, "", messagepolicy.OutcomeAdmit, nil,
	))
	assert.Equal(t, messagepolicy.OutcomeReject, messagepolicy.ResolveOutcome(
		missingRes.Class, missingRes.Reason, "", messagepolicy.OutcomeAdmit,
		map[string]string{celengine.ReasonMissingInput: messagepolicy.OutcomeReject},
	))

	// 2. Runtime division-by-zero error in CEL -> class failed (reason "runtime").
	divZeroRes := ev.EvaluateViolation(ctx, "pol-div0", 1, `(1 / (message.attachment_count - 1)) > 0`, act)
	assert.Equal(t, messagepolicy.ClassFailed, divZeroRes.Class)
	assert.Equal(t, celengine.ReasonRuntime, divZeroRes.Reason)
	assert.Equal(t, messagepolicy.OutcomeReject, messagepolicy.ResolveOutcome(
		divZeroRes.Class, divZeroRes.Reason, "", messagepolicy.OutcomeReject, nil,
	))

	// 3. Exhausted per-send 250ms BudgetTracker -> class failed with reason "budget_exhausted".
	tracker := messagepolicy.NewSendBudget()
	tracker.Charge(messagepolicy.DefaultSendBudget)
	require.True(t, tracker.Exhausted())

	budgetRes := ev.EvaluateViolation(ctx, "pol-budget", 1, `true`, act, messagepolicy.WithBudget(tracker))
	assert.Equal(t, messagepolicy.ClassFailed, budgetRes.Class)
	assert.Equal(t, messagepolicy.ReasonBudgetExhausted, budgetRes.Reason)
	assert.Equal(t, messagepolicy.OutcomeReject, messagepolicy.ResolveOutcome(
		budgetRes.Class, budgetRes.Reason, "", messagepolicy.OutcomeAdmit,
		map[string]string{messagepolicy.ReasonBudgetExhausted: messagepolicy.OutcomeReject},
	))
}

// Target 4a: cost_limit and timeout from shared engine map to class failed and
// configured action; oversized literal regex or non-literal regex fails authoring
// compile with structured category.
func TestEngineBounds_Target4a_CostLimitTimeoutAndRegexCategories(t *testing.T) {
	ev, err := messagepolicy.NewDefault()
	require.NoError(t, err)
	ctx := context.Background()

	fullBody := strings.Repeat("word ", messagepolicy.MaxBodyBytes/5)
	act := messagepolicy.NewActivation(sampleFacts(fullBody))

	// 1. Expensive comprehension exceeding CostLimit (250k) -> class failed, reason "cost_limit".
	costExpr := `[1,2,3,4,5,6,7,8,9,10].all(a, [1,2,3,4,5,6,7,8,9,10].all(b, (string(message.body) + string(message.body)).size() >= a + b - 20))`
	costRes := ev.EvaluateViolation(ctx, "pol-cost", 1, costExpr, act)
	assert.Equal(t, messagepolicy.ClassFailed, costRes.Class)
	assert.Equal(t, celengine.ReasonCostLimit, costRes.Reason)
	assert.Equal(t, messagepolicy.OutcomeReject, messagepolicy.ResolveOutcome(
		costRes.Class, costRes.Reason, "", messagepolicy.OutcomeAdmit,
		map[string]string{celengine.ReasonCostLimit: messagepolicy.OutcomeReject},
	))

	// 2. Timeout outcome from shared engine -> class failed, reason "timeout".
	heavyPattern := `(?i)\b(option|choice|select|pick|choose|prefer|alternative|decide|vote|confirm|approve|reject|abort|proceed|continue|cancel)\b`
	timeoutRes := ev.EvaluateViolation(ctx, "pol-timeout", 1,
		fmt.Sprintf("message.body.matches(%q)", heavyPattern),
		act,
		messagepolicy.WithMaxEvalTime(1*time.Nanosecond),
	)
	assert.Equal(t, messagepolicy.ClassFailed, timeoutRes.Class)
	assert.Equal(t, celengine.ReasonTimeout, timeoutRes.Reason)

	// 3. Non-literal regex at authoring -> ErrCodeInvalidRule with Category == "regex_literal_required".
	baseSpec := validAuthoringSpec()
	baseSpec.ViolationRule = `message.body.matches(sender.slug)`
	_, err = ev.ValidateAuthoring(ctx, baseSpec)
	var vErr *messagepolicy.ValidationError
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, messagepolicy.ErrCodeInvalidRule, vErr.Code)
	assert.Equal(t, celengine.CategoryRegexLiteralRequired, vErr.Category)
	assert.Positive(t, vErr.Line)

	// 4. Oversized literal regex (> 512 bytes) at authoring -> ErrCodeInvalidRule with Category == "size".
	oversizedSpec := validAuthoringSpec()
	oversizedSpec.ViolationRule = fmt.Sprintf(`message.body.matches("%s")`, strings.Repeat("a", 600))
	_, err = ev.ValidateAuthoring(ctx, oversizedSpec)
	vErr = nil
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, messagepolicy.ErrCodeInvalidRule, vErr.Code)
	assert.Equal(t, celengine.CategorySize, vErr.Category)
}

// Target 4b: Benign full-size 16 KiB case with one literal case-insensitive
// keyword alternation evaluates to a bool without hitting cost_limit.
func TestBenignFullSizeBody_Target4b(t *testing.T) {
	ev, err := messagepolicy.NewDefault()
	require.NoError(t, err)
	ctx := context.Background()

	rule := `recipient.kind != "agent" && !message.has_choices && message.body.matches("(?i)(\\bchoose\\s+(between|one\\s+of)\\b|\\boption\\s+[a-d1-4]\\b|\\breply\\s+with\\s+(1|2|a|b)\\b)")`

	benign16K := strings.Repeat("Status update: build and unit tests completed cleanly on all packages.\n", 240)[:messagepolicy.MaxBodyBytes]
	require.Len(t, benign16K, messagepolicy.MaxBodyBytes)
	passRes := ev.EvaluateExpr(ctx, rule, messagepolicy.NewActivation(sampleFacts(benign16K)))
	assert.Equal(t, messagepolicy.ClassPass, passRes.Class)
	assert.Empty(t, passRes.Reason)

	violating16K := benign16K[:messagepolicy.MaxBodyBytes-40] + "\nPlease choose between Option A and B.  "
	require.Len(t, violating16K, messagepolicy.MaxBodyBytes)
	violRes := ev.EvaluateExpr(ctx, rule, messagepolicy.NewActivation(sampleFacts(violating16K)))
	assert.Equal(t, messagepolicy.ClassViolation, violRes.Class)
	assert.Empty(t, violRes.Reason)
}

// Target 4c: Error handling uses errors.Is / errors.As against wrapped ruleengine errors.
func TestWrappedErrors_Target4c(t *testing.T) {
	// Wrapped ErrBudgetExhausted (exactly as ruleengine.Engine.Evaluate wraps it on tracker timeout).
	wrappedBudget := fmt.Errorf("gate wrapper: %w",
		fmt.Errorf("%w: %w", ruleengine.ErrBudgetExhausted, &ruleengine.FailedError{Reason: celengine.ReasonTimeout}),
	)
	c1 := messagepolicy.ClassifyError(wrappedBudget)
	assert.Equal(t, messagepolicy.ClassFailed, c1.Class)
	assert.Equal(t, messagepolicy.ReasonBudgetExhausted, c1.Reason)

	// errors.Join-wrapped FailedError.
	joinedFailed := errors.Join(
		errors.New("context wrapper"),
		&ruleengine.FailedError{
			Reason: celengine.ReasonCostLimit,
			Cause:  errors.Join(errors.New("inner"), &celengine.EvalError{Reason: celengine.ReasonCostLimit}),
		},
	)
	c2 := messagepolicy.ClassifyError(joinedFailed)
	assert.Equal(t, messagepolicy.ClassFailed, c2.Class)
	assert.Equal(t, celengine.ReasonCostLimit, c2.Reason)

	// Wrapped UnsupportedDialectError.
	wrappedUnsup := fmt.Errorf("eval: %w", &ruleengine.UnsupportedDialectError{
		Dialect:  "cel",
		Consumer: "message_policy",
		Reason:   ruleengine.ReasonDialectNotAllowed,
	})
	c3 := messagepolicy.ClassifyError(wrappedUnsup)
	assert.Equal(t, messagepolicy.ClassFailed, c3.Class)
	assert.Equal(t, ruleengine.ReasonDialectNotAllowed, c3.Reason)
	assert.True(t, c3.AdmissionError)
}

type spyCachedDialect struct {
	*ruleengine.CELDialect
	invalidated []celengine.CacheScope
}

func (s *spyCachedDialect) Invalidate(envName string, scope celengine.CacheScope) {
	s.invalidated = append(s.invalidated, scope)
	s.CELDialect.Invalidate(envName, scope)
}

// Target 5: Compile errors return line, column, category; policy edit bumps
// revision and invalidates cached program with Consumer: "message_policy".
func TestCompileErrorsAndScopedInvalidate_Target5(t *testing.T) {
	celEng, err := messagepolicy.NewCELEngine()
	require.NoError(t, err)
	baseCEL, err := ruleengine.NewCELDialect(celEng)
	require.NoError(t, err)
	spy := &spyCachedDialect{CELDialect: baseCEL}

	re := ruleengine.NewEngine()
	require.NoError(t, re.Register(spy))
	require.NoError(t, re.AllowConsumerDialects(messagepolicy.ConsumerName, ruleengine.Grant(ruleengine.DialectCEL)))

	ev, err := messagepolicy.New(re)
	require.NoError(t, err)
	ctx := context.Background()

	// 1. Compile errors return line, column, and category across syntax, unknown_variable, and result_type.
	errCases := []struct {
		expr         string
		wantCategory string
	}{
		{`message.has_choices &&`, celengine.CategorySyntax},
		{`nonexistent_var == "x"`, celengine.CategoryUnknownVariable},
		{`sender.slug`, celengine.CategoryResultType},
	}
	for _, tc := range errCases {
		spec := validAuthoringSpec()
		spec.ViolationRule = tc.expr
		_, err := ev.ValidateAuthoring(ctx, spec)
		var vErr *messagepolicy.ValidationError
		require.ErrorAs(t, err, &vErr, "expr=%s", tc.expr)
		assert.Equal(t, messagepolicy.ErrCodeInvalidRule, vErr.Code)
		assert.Equal(t, tc.wantCategory, vErr.Category)
		assert.GreaterOrEqual(t, vErr.Line, 1)
		assert.GreaterOrEqual(t, vErr.Column, 0)
	}

	// 2. Cached compile + revision bump + Invalidate with Consumer: "message_policy" evicts the entry.
	act := messagepolicy.NewActivation(sampleFacts("choose option A"))
	assert.Equal(t, 0, celEng.CacheStats().Entries)

	res1 := ev.EvaluateViolation(ctx, "policy-uuid-42", 1, `!message.has_choices && message.urgent`, act)
	assert.Equal(t, messagepolicy.ClassViolation, res1.Class)
	assert.Equal(t, 1, celEng.CacheStats().Entries)

	// Invalidate old revision on delete/edit: must pass Consumer: "message_policy" and drop cache entry.
	ev.Invalidate("policy-uuid-42", 1)
	require.Len(t, spy.invalidated, 1)
	assert.Equal(t, celengine.CacheScope{
		Consumer: messagepolicy.ConsumerName,
		ID:       "policy-uuid-42",
		Revision: 1,
	}, spy.invalidated[0])
	assert.Equal(t, 0, celEng.CacheStats().Entries, "Invalidate must actually evict the scoped cache entry")

	// Even if caller passes a CacheScope with empty Consumer to InvalidateScope, Consumer is populated.
	_ = ev.EvaluateViolation(ctx, "policy-uuid-42", 2, `message.has_choices`, act)
	assert.Equal(t, 1, celEng.CacheStats().Entries)
	ev.InvalidateScope(celengine.CacheScope{ID: "policy-uuid-42", Revision: 2})
	require.Len(t, spy.invalidated, 2)
	assert.Equal(t, messagepolicy.ConsumerName, spy.invalidated[1].Consumer)
	assert.Equal(t, 0, celEng.CacheStats().Entries)
}

func validAuthoringSpec() messagepolicy.AuthoringSpec {
	return messagepolicy.AuthoringSpec{
		Name: "no-prose-choices",
		Scope: messagepolicy.Scope{
			Surfaces: []string{
				messagepolicy.SurfaceAgentToUser,
				messagepolicy.SurfaceAgentToConversation,
			},
			Sender: messagepolicy.SenderSelector{AllAgents: true},
			Recipient: messagepolicy.RecipientSelector{
				Kinds: []string{"user", "conversation"},
			},
		},
		ViolationRule:     `!message.has_choices`,
		OnFailed:          messagepolicy.OutcomeAdmit,
		RejectReason:      "Policy {policy} blocked message to {recipient}",
		RejectRemediation: "Use --choices.",
	}
}

func TestAuthoringValidationAndCoverage(t *testing.T) {
	ev, err := messagepolicy.NewDefault()
	require.NoError(t, err)
	ctx := context.Background()

	// Valid spec defaults Mode to "shadow".
	mode, err := ev.ValidateAuthoring(ctx, validAuthoringSpec())
	require.NoError(t, err)
	assert.Equal(t, messagepolicy.ModeShadow, mode)

	// Coverage metadata exposes M1 enforced surfaces and uncovered agent paths.
	cov := messagepolicy.Coverage()
	assert.Equal(t, []string{
		messagepolicy.SurfaceAgentToUser,
		messagepolicy.SurfaceAgentToConversation,
	}, cov.EnforcedSurfaces)
	assert.Contains(t, cov.UncoveredAgentPaths, messagepolicy.SurfaceAgentDM)
	assert.Contains(t, cov.UncoveredAgentPaths, messagepolicy.SurfaceGroupSet)
	assert.Contains(t, cov.UncoveredAgentPaths, messagepolicy.SurfaceMentionDelivery)

	// Non-empty system_one_rule requires explicit on_uncertain (lead decision).
	s1 := validAuthoringSpec()
	s1.SystemOneRule = `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"questions":[{"id":"asks_choice","type":"binary","instructions":"Does the message ask the recipient to choose?"}]}}`
	_, err = ev.ValidateAuthoring(ctx, s1)
	var vErr *messagepolicy.ValidationError
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, messagepolicy.ErrCodeValidation, vErr.Code)
	assert.Equal(t, "on_uncertain", vErr.Field)

	// Uncovered surfaces refused with surface_not_covered.
	for _, uncovered := range []string{
		messagepolicy.SurfaceAgentDM,
		messagepolicy.SurfaceGroupPost,
		messagepolicy.SurfaceGroupSet,
		messagepolicy.SurfaceMentionFanout,
		messagepolicy.SurfaceMentionDelivery,
		"broadcast",
	} {
		sp := validAuthoringSpec()
		sp.Scope.Surfaces = []string{uncovered}
		_, err := ev.ValidateAuthoring(ctx, sp)
		vErr = nil
		require.ErrorAs(t, err, &vErr, "surface=%s", uncovered)
		assert.Equal(t, messagepolicy.ErrCodeSurfaceNotCovered, vErr.Code)
	}

	// Agent recipient selectors refused with surface_not_covered.
	agentKindSpec := validAuthoringSpec()
	agentKindSpec.Scope.Recipient.Kinds = []string{"agent"}
	_, err = ev.ValidateAuthoring(ctx, agentKindSpec)
	vErr = nil
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, messagepolicy.ErrCodeSurfaceNotCovered, vErr.Code)

	agentIDsSpec := validAuthoringSpec()
	agentIDsSpec.Scope.Recipient.AgentIDs = []string{"agent-1"}
	_, err = ev.ValidateAuthoring(ctx, agentIDsSpec)
	vErr = nil
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, messagepolicy.ErrCodeSurfaceNotCovered, vErr.Code)

	// Recipient kinds unreachable from the listed surfaces refused with validation_error (O3).
	unreachableCases := []struct {
		name     string
		surfaces []string
		kinds    []string
	}{
		{
			name:     "agent_to_user with conversation kind",
			surfaces: []string{messagepolicy.SurfaceAgentToUser},
			kinds:    []string{"conversation"},
		},
		{
			name:     "agent_to_conversation with user kind",
			surfaces: []string{messagepolicy.SurfaceAgentToConversation},
			kinds:    []string{"user"},
		},
	}
	for _, tc := range unreachableCases {
		unreachSpec := validAuthoringSpec()
		unreachSpec.Scope.Surfaces = tc.surfaces
		unreachSpec.Scope.Recipient.Kinds = tc.kinds
		_, err = ev.ValidateAuthoring(ctx, unreachSpec)
		vErr = nil
		require.ErrorAs(t, err, &vErr, tc.name)
		assert.Equal(t, messagepolicy.ErrCodeValidation, vErr.Code, tc.name)
		assert.Equal(t, "scope.recipient.kinds", vErr.Field, tc.name)
	}

	// Reachable surface + recipient.kinds combinations remain accepted.
	for _, tc := range []struct {
		surfaces []string
		kinds    []string
	}{
		{[]string{messagepolicy.SurfaceAgentToUser}, []string{"user"}},
		{[]string{messagepolicy.SurfaceAgentToConversation}, []string{"conversation"}},
		{[]string{messagepolicy.SurfaceAgentToUser, messagepolicy.SurfaceAgentToConversation}, []string{"user"}},
		{[]string{messagepolicy.SurfaceAgentToUser, messagepolicy.SurfaceAgentToConversation}, []string{"conversation"}},
	} {
		reachSpec := validAuthoringSpec()
		reachSpec.Scope.Surfaces = tc.surfaces
		reachSpec.Scope.Recipient.Kinds = tc.kinds
		_, err = ev.ValidateAuthoring(ctx, reachSpec)
		require.NoError(t, err, "surfaces=%v kinds=%v", tc.surfaces, tc.kinds)
	}

	// Missing on_failed refused with validation_error (Target 4).
	noOnFailed := validAuthoringSpec()
	noOnFailed.OnFailed = ""
	_, err = ev.ValidateAuthoring(ctx, noOnFailed)
	vErr = nil
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, messagepolicy.ErrCodeValidation, vErr.Code)
	assert.Equal(t, "on_failed", vErr.Field)

	// Template attempting to interpolate {message.body} is refused at authoring.
	bodyTmplSpec := validAuthoringSpec()
	bodyTmplSpec.RejectReason = "Rejected body: {message.body}"
	_, err = ev.ValidateAuthoring(ctx, bodyTmplSpec)
	vErr = nil
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, messagepolicy.ErrCodeValidation, vErr.Code)
	assert.Equal(t, "reject_reason", vErr.Field)

	// Every known failure reason in the closed set (including policy_cap_exceeded
	// and facts_unavailable) is accepted as an on_failed_reasons key.
	knownReasons := messagepolicy.KnownFailureReasons()
	assert.Contains(t, knownReasons, messagepolicy.ReasonBodyTruncated)
	assert.Contains(t, knownReasons, messagepolicy.ReasonBudgetExhausted)
	assert.Contains(t, knownReasons, messagepolicy.ReasonPolicyCapExceeded)
	assert.Contains(t, knownReasons, messagepolicy.ReasonFactsUnavailable)
	assert.Contains(t, knownReasons, messagepolicy.ReasonCompileError)
	assert.Contains(t, knownReasons, messagepolicy.ReasonUninitializedProgram)
	assert.Contains(t, knownReasons, messagepolicy.ReasonInvalidCompiledProgram)
	assert.Contains(t, knownReasons, celengine.ReasonMissingInput)
	assert.Contains(t, knownReasons, celengine.ReasonCostLimit)
	assert.Contains(t, knownReasons, celengine.ReasonTimeout)
	assert.Contains(t, knownReasons, celengine.ReasonCanceled)
	assert.Contains(t, knownReasons, celengine.ReasonRuntime)
	assert.Contains(t, knownReasons, celengine.ReasonType)
	assert.Contains(t, knownReasons, ruleengine.ReasonConsumerNotRegistered)
	assert.Contains(t, knownReasons, ruleengine.ReasonDialectNotAllowed)

	allReasonsSpec := validAuthoringSpec()
	allReasonsSpec.OnFailedReasons = make(map[string]string, len(knownReasons))
	for i, r := range knownReasons {
		assert.True(t, messagepolicy.IsValidFailureReason(r), "reason=%s", r)
		if i%2 == 0 {
			allReasonsSpec.OnFailedReasons[r] = messagepolicy.OutcomeAdmit
		} else {
			allReasonsSpec.OnFailedReasons[r] = messagepolicy.OutcomeReject
		}
	}
	_, err = ev.ValidateAuthoring(ctx, allReasonsSpec)
	require.NoError(t, err)

	// Unknown, typo, or blank on_failed_reasons keys are refused with validation_error.
	for _, badReason := range []string{"", "   ", "cost-limit", "budget_exausted", "unknown_reason"} {
		assert.False(t, messagepolicy.IsValidFailureReason(badReason), "badReason=%q", badReason)
		badReasonSpec := validAuthoringSpec()
		badReasonSpec.OnFailedReasons = map[string]string{
			badReason: messagepolicy.OutcomeReject,
		}
		_, err = ev.ValidateAuthoring(ctx, badReasonSpec)
		vErr = nil
		require.ErrorAs(t, err, &vErr, "badReason=%q", badReason)
		assert.Equal(t, messagepolicy.ErrCodeValidation, vErr.Code)
		assert.Equal(t, "on_failed_reasons", vErr.Field)
	}

	// Valid failure reason with invalid outcome is refused with validation_error.
	badOutcomeSpec := validAuthoringSpec()
	badOutcomeSpec.OnFailedReasons = map[string]string{
		celengine.ReasonTimeout: "ignore",
	}
	_, err = ev.ValidateAuthoring(ctx, badOutcomeSpec)
	vErr = nil
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, messagepolicy.ErrCodeValidation, vErr.Code)
	assert.Equal(t, "on_failed_reasons", vErr.Field)
}

func TestTemplateRendering_ContentFree(t *testing.T) {
	rendered := messagepolicy.RenderTemplate(
		"Policy {policy} rejected message to {recipient} (intended=@{judgment.intended}, asks={judgment.asks_choice}, missing={judgment.other}); example JSON: {\"ok\": true}",
		messagepolicy.TemplateData{
			Policy:         "no-prose-choices",
			Recipient:      "alice",
			JudgmentFlags:  map[string]bool{"asks_choice": true},
			JudgmentLabels: map[string]string{"intended": "reviewer"},
		},
	)
	assert.Equal(t,
		`Policy no-prose-choices rejected message to alice (intended=@reviewer, asks=true, missing=); example JSON: {"ok": true}`,
		rendered,
	)
}
