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
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

type mpFakeAdapter struct {
	name       string
	binaryOnly bool
	invokeFn   func(ctx context.Context, tr provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error)
}

func (a *mpFakeAdapter) Name() string    { return a.name }
func (a *mpFakeAdapter) Version() string { return "1.0.0-test" }
func (a *mpFakeAdapter) Capabilities(provider.InstanceView) (provider.Capabilities, error) {
	prims := map[string]provider.PrimitiveLimits{
		spec.PrimitiveBinary: {Supported: true, DistributionCompleteness: spec.CompletenessComplete},
	}
	if !a.binaryOnly {
		prims[spec.PrimitiveChoice] = provider.PrimitiveLimits{Supported: true, MinOptions: 2, MaxOptions: 16, DistributionCompleteness: spec.CompletenessComplete}
	}
	return provider.Capabilities{
		Primitives:             prims,
		MaxQuestionsPerRequest: 8,
		MaxInputTokens:         8192,
		Truncation:             provider.TruncationNever,
		SideEffectFree:         true,
		AuthScheme:             provider.AuthScheme{Kind: provider.AuthKindBearer},
	}, nil
}
func (a *mpFakeAdapter) Validate(provider.InstanceView, provider.RuleSpec) error { return nil }
func (a *mpFakeAdapter) Invoke(ctx context.Context, tr provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
	return a.invokeFn(ctx, tr, inst, req)
}

func newTestEvaluatorWithSystemOne(
	t *testing.T,
	enabled func() bool,
	invokeFn func(ctx context.Context, tr provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error),
) (*messagepolicy.Evaluator, *ruleengine.Engine) {
	t.Helper()
	reg := provider.NewRegistry()
	reg.MustRegister(&mpFakeAdapter{name: "fake-mp", invokeFn: invokeFn})
	inst := systemoneruntime.Instance{
		ID:       "inst-mp-1",
		Name:     "mp-inst",
		Adapter:  "fake-mp",
		Model:    "fake-model-1",
		Endpoint: "https://fake.example.test",
		Auth:     systemoneruntime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-mp-1"},
		Enabled:  true,
		Revision: 3,
	}
	rt := systemoneruntime.New(systemoneruntime.Config{
		Registry:    reg,
		Instances:   systemoneruntime.StaticInstances(inst),
		Credentials: systemoneruntime.StaticCredential("sk-test-secret-never-logged"),
	})
	t.Cleanup(rt.CloseIdleConnections)

	eng := ruleengine.NewEngine()
	celDialect, err := ruleengine.NewCELDialect()
	require.NoError(t, err)
	celEng, err := messagepolicy.NewCELEngine()
	require.NoError(t, err)
	require.NoError(t, celDialect.RegisterEnvironment(celEng))
	require.NoError(t, eng.Register(celDialect))

	if enabled == nil {
		enabled = func() bool { return true }
	}
	require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
		Runtime: rt,
		Enabled: enabled,
	})))
	require.NoError(t, eng.AllowConsumerDialects(
		messagepolicy.ConsumerName,
		ruleengine.Grant(ruleengine.DialectCEL),
		ruleengine.GrantIO(systemone.DialectName),
	))
	require.NoError(t, systemone.RegisterConsumerEnvironment(eng, messagepolicy.ConsumerName, messagepolicy.SystemOneContentEnv()))

	ev, err := messagepolicy.New(eng)
	require.NoError(t, err)
	return ev, eng
}

func TestSystemOneContentEnv_MatchesDesignSpec(t *testing.T) {
	env := messagepolicy.SystemOneContentEnv()
	require.NoError(t, env.Validate())
	assert.Equal(t, messagepolicy.EnvironmentName, env.Name)
	assert.Equal(t, 1, env.Version)
	assert.Equal(t, 24*1024, env.MaxTotalBytes)

	wantFields := []struct {
		name     string
		maxBytes int
		trust    spec.TrustLabel
	}{
		{"message.body", 16384, spec.TrustUntrusted},
		{"message.type", 64, spec.TrustUntrusted},
		{"sender.slug", 128, spec.TrustTrusted},
		{"recipient.slug", 128, spec.TrustTrusted},
		{"recipient.kind", 16, spec.TrustTrusted},
		{"conversation.unmentioned_agent_slugs", 2048, spec.TrustTrusted},
	}
	require.Len(t, env.Fields, len(wantFields))
	for _, wf := range wantFields {
		f, ok := env.Field(wf.name)
		require.True(t, ok, "missing field %s", wf.name)
		assert.Equal(t, spec.FieldKindText, f.Kind)
		assert.Equal(t, wf.maxBytes, f.MaxBytes)
		assert.Equal(t, wf.trust, f.Trust)
	}
	// Dynamic choice sets are omitted in v1 (refused at registration in P2; deferred to M3 + PV8).
	assert.Empty(t, env.ChoiceSets)
}

func TestValidateAuthoring_SystemOneMatrix(t *testing.T) {
	ctx := context.Background()
	ev, _ := newTestEvaluatorWithSystemOne(t, nil, func(context.Context, provider.Transport, provider.InstanceView, provider.ModelRequest) (provider.ModelResult, error) {
		t.Fatal("authoring must never invoke provider")
		return provider.ModelResult{}, nil
	})

	validS1Rule := `{
		"dialect": "system_one",
		"inputs": ["message.body"],
		"on_oversize": "reject",
		"spec": {
			"version": 1,
			"binding": {"instance": "mp-inst"},
			"questions": [
				{"id": "asks_choice", "type": "binary", "instructions": "Does the message ask the recipient to choose among alternatives?"},
				{
					"id": "topic",
					"type": "choice",
					"instructions": "Classify topic.",
					"options": [
						{"id": "in_scope", "description": "In scope"},
						{"id": "out_of_scope", "description": "Out of scope"},
						{"id": "unsure", "description": "Unsure", "abstain": true}
					]
				}
			]
		}
	}`

	t.Run("valid_system_one_rule_accepted", func(t *testing.T) {
		sp := validAuthoringSpec()
		sp.SystemOneRule = validS1Rule
		sp.ViolationRule = `recipient.kind != "agent" && !message.has_choices && judgment_flags.asks_choice`
		sp.OnUncertain = messagepolicy.OutcomeAdmit
		sp.OnFailed = messagepolicy.OutcomeReject
		sp.OnFailedReasons = map[string]string{
			string(provider.FailNoEligibleInstance): messagepolicy.OutcomeReject,
			string(provider.FailCapacity):           messagepolicy.OutcomeAdmit,
			string(provider.FailInputTooLarge):      messagepolicy.OutcomeReject,
			systemone.ReasonDialectDisabled:         messagepolicy.OutcomeReject,
		}
		mode, err := ev.ValidateAuthoring(ctx, sp)
		require.NoError(t, err)
		assert.Equal(t, messagepolicy.ModeShadow, mode)
		assert.Equal(t, []string{"mp-inst"}, messagepolicy.ExtractSystemOneRefs(sp.SystemOneRule))
	})

	t.Run("cel_only_without_on_uncertain_still_accepted", func(t *testing.T) {
		sp := validAuthoringSpec()
		sp.OnUncertain = ""
		mode, err := ev.ValidateAuthoring(ctx, sp)
		require.NoError(t, err)
		assert.Equal(t, messagepolicy.ModeShadow, mode)
	})

	t.Run("missing_on_uncertain_refused_with_400_validation", func(t *testing.T) {
		sp := validAuthoringSpec()
		sp.SystemOneRule = validS1Rule
		sp.OnUncertain = ""
		_, err := ev.ValidateAuthoring(ctx, sp)
		var vErr *messagepolicy.ValidationError
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, messagepolicy.ErrCodeValidation, vErr.Code)
		assert.Equal(t, "on_uncertain", vErr.Field)
	})

	t.Run("undeclared_input_field_refused", func(t *testing.T) {
		sp := validAuthoringSpec()
		sp.OnUncertain = messagepolicy.OutcomeReject
		sp.SystemOneRule = `{
			"dialect": "system_one",
			"inputs": ["secret.api_key"],
			"spec": {
				"version": 1,
				"binding": {"instance": "mp-inst"},
				"questions": [{"id": "q1", "type": "binary", "instructions": "Check."}]
			}
		}`
		_, err := ev.ValidateAuthoring(ctx, sp)
		var vErr *messagepolicy.ValidationError
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, messagepolicy.ErrCodeInvalidRule, vErr.Code)
		assert.Equal(t, "system_one_rule", vErr.Field)
		assert.Equal(t, string(spec.CategoryInput), vErr.Category)
	})

	t.Run("score_question_refused_as_not_yet_supported", func(t *testing.T) {
		sp := validAuthoringSpec()
		sp.OnUncertain = messagepolicy.OutcomeReject
		sp.SystemOneRule = `{
			"dialect": "system_one",
			"inputs": ["message.body"],
			"spec": {
				"version": 1,
				"binding": {"instance": "mp-inst"},
				"questions": [{
					"id": "severity",
					"type": "score",
					"instructions": "Rate severity.",
					"levels": [
						{"id": "low", "description": "Low"},
						{"id": "high", "description": "High"}
					]
				}]
			}
		}`
		_, err := ev.ValidateAuthoring(ctx, sp)
		var vErr *messagepolicy.ValidationError
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, messagepolicy.ErrCodeNotYetSupported, vErr.Code)
		assert.Equal(t, "system_one_rule", vErr.Field)
		assert.Equal(t, string(spec.CategoryCapability), vErr.Category)
	})

	t.Run("threshold_uncertain_if_refused_as_not_yet_supported", func(t *testing.T) {
		sp := validAuthoringSpec()
		sp.OnUncertain = messagepolicy.OutcomeReject
		sp.SystemOneRule = `{
			"dialect": "system_one",
			"inputs": ["message.body"],
			"spec": {
				"version": 1,
				"binding": {"instance": "mp-inst"},
				"questions": [{"id": "asks_choice", "type": "binary", "instructions": "Check."}],
				"uncertain_if": [{"question": "asks_choice", "metric": "p_top", "below": 0.7}]
			}
		}`
		_, err := ev.ValidateAuthoring(ctx, sp)
		var vErr *messagepolicy.ValidationError
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, messagepolicy.ErrCodeNotYetSupported, vErr.Code)
		assert.Equal(t, "system_one_rule", vErr.Field)
		assert.Equal(t, string(spec.CategoryCapability), vErr.Category)
	})

	t.Run("over_16kib_system_one_rule_refused", func(t *testing.T) {
		sp := validAuthoringSpec()
		sp.OnUncertain = messagepolicy.OutcomeReject
		sp.SystemOneRule = `{"spec": "` + strings.Repeat("x", messagepolicy.MaxSystemOneRuleBytes) + `"}`
		_, err := ev.ValidateAuthoring(ctx, sp)
		var vErr *messagepolicy.ValidationError
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, messagepolicy.ErrCodeInvalidRule, vErr.Code)
		assert.Equal(t, "system_one_rule", vErr.Field)
		assert.Equal(t, string(spec.CategoryLimit), vErr.Category)
	})
}

func TestEvaluatePolicy_SystemOneShortCircuitAndProjection(t *testing.T) {
	ctx := context.Background()
	const secretCanary = "TOP_SECRET_MESSAGE_BODY_CANARY_9911"

	var (
		mode        atomic.Int32 // 0=success(violation), 1=uncertain(abstain), 2=failed(unavailable)
		seenInputs  []string
		invokeCount atomic.Int32
	)
	ev, _ := newTestEvaluatorWithSystemOne(t, nil, func(_ context.Context, _ provider.Transport, _ provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
		invokeCount.Add(1)
		seenInputs = seenInputs[:0]
		for _, f := range req.Input {
			seenInputs = append(seenInputs, f.Name)
		}
		switch mode.Load() {
		case 1:
			dist := map[string]float64{"in_scope": 0.1, "out_of_scope": 0.1, "unsure": 0.8}
			choice := "unsure"
			return provider.ModelResult{
				ResolvedModel: "fake-model-1",
				HTTPStatus:    200,
				Answers: map[string]provider.Answer{
					"topic": {
						QuestionID:   "topic",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       &choice,
						Distribution: dist,
					},
				},
			}, nil
		case 2:
			return provider.ModelResult{}, provider.NewProviderError(provider.FailUnavailable, false, 0, 503, "upstream 503")
		default:
			dist, val, _, _ := provider.BinaryDistribution(0.94)
			return provider.ModelResult{
				ResolvedModel: "", // triggers model_unreported note
				HTTPStatus:    200,
				Notes:         []string{provider.NoteModelUnreported},
				Answers: map[string]provider.Answer{
					"asks_choice": {
						QuestionID:   "asks_choice",
						Primitive:    spec.PrimitiveBinary,
						Status:       provider.AnswerStatusAnswered,
						Bool:         val,
						Distribution: dist,
					},
				},
			}, nil
		}
	})

	proseRule := `{
		"dialect": "system_one",
		"inputs": ["message.body"],
		"on_oversize": "truncate",
		"spec": {
			"version": 1,
			"binding": {"instance": "mp-inst"},
			"questions": [{"id": "asks_choice", "type": "binary", "instructions": "Does the message ask the recipient to choose among alternatives?"}]
		}
	}`
	facts := messagepolicy.Facts{
		Surface:   messagepolicy.SurfaceAgentToUser,
		Sender:    messagepolicy.SenderFacts{Kind: "agent", ID: "ag-1", Slug: "agent-alpha", ProjectID: "proj-1"},
		Recipient: messagepolicy.RecipientFacts{Kind: "user", ID: "u-1", Slug: "alice"},
		Message: messagepolicy.MessageFacts{
			Type:       "message",
			Body:       "Would you prefer option A or option B? " + secretCanary,
			HasChoices: false,
		},
	}

	// 1. Prose-choices violation + content-free EvaluationProjection check.
	mode.Store(0)
	cls := ev.EvaluatePolicy(
		ctx, "pol-1", 4,
		`recipient.kind != "agent" && !message.has_choices && judgment_flags.asks_choice && size(judgment_scores) == 0`,
		proseRule, "proj-1", facts,
		[]messagepolicy.EvalOption{messagepolicy.WithBudget(messagepolicy.NewSendBudget())},
		[]messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(time.Second)},
	)
	assert.Equal(t, messagepolicy.ClassViolation, cls.Class)
	assert.Equal(t, []string{"message.body"}, seenInputs, "only referenced input field must reach provider")
	require.NotNil(t, cls.Evaluation)
	assert.Equal(t, "proj-1", cls.Evaluation.ProjectID)
	assert.Equal(t, "pol-1", cls.Evaluation.Rule.RuleID)
	assert.Equal(t, int64(4), cls.Evaluation.Rule.Revision)

	rawProj := messagepolicy.MarshalEvaluationProjection(cls.Evaluation)
	require.NotEmpty(t, rawProj)
	projJSON := string(rawProj)
	assert.NotContains(t, projJSON, secretCanary, "projection must be content-free")
	if cls.Evaluation.Input.SHA256 != "" {
		assert.NotContains(t, projJSON, cls.Evaluation.Input.SHA256, "projection must omit input sha256 per consumer design §5.8")
	}
	assert.NotContains(t, projJSON, `"sha256":`, "projection must not have an input sha256 field per consumer design §5.8")
	assert.NotContains(t, projJSON, "sk-test-secret", "projection must never contain credentials")

	var proj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(rawProj, &proj))
	assert.Equal(t, "success", proj.Class)
	assert.Equal(t, "fake-model-1", proj.ConfiguredModel)
	assert.True(t, proj.ModelUnreported)
	assert.Equal(t, "mp-inst", proj.Instance)
	assert.False(t, proj.Truncated)
	require.Contains(t, proj.Answers, "asks_choice")
	require.NotNil(t, proj.Answers["asks_choice"].Bool)
	assert.True(t, *proj.Answers["asks_choice"].Bool)

	// 2. Uncertain short-circuits BEFORE CEL (even with a violation_rule that would throw if evaluated).
	mode.Store(1)
	abstainRule := `{
		"dialect": "system_one",
		"inputs": ["message.body"],
		"spec": {
			"version": 1,
			"binding": {"instance": "mp-inst"},
			"questions": [{
				"id": "topic",
				"type": "choice",
				"instructions": "Topic?",
				"options": [
					{"id": "in_scope", "description": "In"},
					{"id": "out_of_scope", "description": "Out"},
					{"id": "unsure", "description": "Unsure", "abstain": true}
				]
			}]
		}
	}`
	clsUnc := ev.EvaluatePolicy(
		ctx, "pol-2", 1,
		`judgment_labels.nonexistent_key == "boom"`,
		abstainRule, "proj-1", facts,
		nil, []messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(time.Second)},
	)
	assert.Equal(t, messagepolicy.ClassUncertain, clsUnc.Class)
	assert.Equal(t, provider.UncertainAbstained, clsUnc.Reason)
	require.NotNil(t, clsUnc.Evaluation)

	// 3. Failed short-circuits BEFORE CEL.
	mode.Store(2)
	clsFail := ev.EvaluatePolicy(
		ctx, "pol-3", 1,
		`judgment_labels.nonexistent_key == "boom"`,
		abstainRule, "proj-1", facts,
		nil, []messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(time.Second)},
	)
	assert.Equal(t, messagepolicy.ClassFailed, clsFail.Class)
	assert.Equal(t, string(provider.FailUnavailable), clsFail.Reason)
	require.NotNil(t, clsFail.Evaluation)

	// 4. Oversize truncate sets Truncated: true in projection.
	mode.Store(0)
	bigFacts := facts
	bigFacts.Message.Body = strings.Repeat("a", messagepolicy.MaxBodyBytes+500)
	clsTrunc := ev.EvaluatePolicy(
		ctx, "pol-4", 1,
		`judgment_flags.asks_choice`,
		proseRule, "proj-1", bigFacts,
		nil, []messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(time.Second)},
	)
	assert.Equal(t, messagepolicy.ClassViolation, clsTrunc.Class)
	projTrunc := messagepolicy.ProjectEvaluation(clsTrunc.Evaluation)
	require.NotNil(t, projTrunc)
	assert.True(t, projTrunc.Truncated)
}

func TestEvaluatePolicy_BinaryOnlyOnBinaryOnlyAdapterAndStaticClosedChoice(t *testing.T) {
	ctx := context.Background()

	facts := messagepolicy.Facts{
		Surface: messagepolicy.SurfaceAgentToConversation,
		Message: messagepolicy.MessageFacts{
			Body: "Could you pick option A or B?",
			Type: "input-needed",
		},
		Conversation: messagepolicy.ConversationFacts{
			UnmentionedAgentSlugs: []string{"alice-agent", "bob-agent"},
		},
	}

	// BuildSystemOneInput must NOT populate any dynamic choice sets (removed in v1).
	builtInput := messagepolicy.BuildSystemOneInput(facts)
	assert.Empty(t, builtInput.ChoiceSets, "v1 message_policy input must not carry dynamic choice sets")

	// 1. Binary-only System One rule on an adapter WITHOUT the choice primitive
	//    must evaluate normally (NOT fail with unsupported).
	var seenChoiceSets map[string][]string
	regBin := provider.NewRegistry()
	regBin.MustRegister(&mpFakeAdapter{
		name:       "fake-binary-only",
		binaryOnly: true,
		invokeFn: func(_ context.Context, _ provider.Transport, _ provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
			seenChoiceSets = req.ChoiceSets
			dist, val, _, _ := provider.BinaryDistribution(0.94)
			return provider.ModelResult{
				ResolvedModel: "bin-model-1",
				HTTPStatus:    200,
				Answers: map[string]provider.Answer{
					"asks_choice": {
						QuestionID:   "asks_choice",
						Primitive:    spec.PrimitiveBinary,
						Status:       provider.AnswerStatusAnswered,
						Bool:         val,
						Distribution: dist,
					},
				},
			}, nil
		},
	})
	instBin := systemoneruntime.Instance{
		ID:       "inst-bin-1",
		Name:     "bin-inst",
		Adapter:  "fake-binary-only",
		Model:    "bin-model-1",
		Endpoint: "https://bin.example.test",
		Auth:     systemoneruntime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-bin-1"},
		Enabled:  true,
		Revision: 1,
	}
	rtBin := systemoneruntime.New(systemoneruntime.Config{
		Registry:    regBin,
		Instances:   systemoneruntime.StaticInstances(instBin),
		Credentials: systemoneruntime.StaticCredential("sk-bin-only"),
	})
	t.Cleanup(rtBin.CloseIdleConnections)

	engBin := ruleengine.NewEngine()
	celDialectBin, err := ruleengine.NewCELDialect()
	require.NoError(t, err)
	celEngBin, err := messagepolicy.NewCELEngine()
	require.NoError(t, err)
	require.NoError(t, celDialectBin.RegisterEnvironment(celEngBin))
	require.NoError(t, engBin.Register(celDialectBin))
	require.NoError(t, engBin.Register(systemone.NewDialect(systemone.DialectConfig{
		Runtime: rtBin,
		Enabled: func() bool { return true },
	})))
	require.NoError(t, engBin.AllowConsumerDialects(
		messagepolicy.ConsumerName,
		ruleengine.Grant(ruleengine.DialectCEL),
		ruleengine.GrantIO(systemone.DialectName),
	))
	require.NoError(t, systemone.RegisterConsumerEnvironment(engBin, messagepolicy.ConsumerName, messagepolicy.SystemOneContentEnv()))

	evBin, err := messagepolicy.New(engBin)
	require.NoError(t, err)

	binaryRule := `{
		"dialect": "system_one",
		"inputs": ["message.body"],
		"spec": {
			"version": 1,
			"binding": {"instance": "bin-inst"},
			"questions": [{"id": "asks_choice", "type": "binary", "instructions": "Does the message ask the user to choose?"}]
		}
	}`
	clsBin := evBin.EvaluatePolicy(
		ctx, "pol-bin-only", 1,
		`judgment_flags.asks_choice`,
		binaryRule, "proj-1", facts,
		nil, []messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(time.Second)},
	)
	assert.Equal(t, messagepolicy.ClassViolation, clsBin.Class, "binary-only rule on binary-only adapter must evaluate normally, not unsupported")
	assert.Empty(t, clsBin.Reason)
	assert.Empty(t, seenChoiceSets)
	assert.True(t, clsBin.Judgments.Flags["asks_choice"])

	// 2. Static closed-choice rule evaluates normally on an adapter that supports PrimitiveChoice.
	evChoice, _ := newTestEvaluatorWithSystemOne(t, nil, func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		chosen := "out_of_scope"
		return provider.ModelResult{
			ResolvedModel: "fake-model-1",
			HTTPStatus:    200,
			Answers: map[string]provider.Answer{
				"topic": {
					QuestionID: "topic",
					Primitive:  spec.PrimitiveChoice,
					Status:     provider.AnswerStatusAnswered,
					Choice:     &chosen,
					Distribution: map[string]float64{
						"in_scope":     0.05,
						"out_of_scope": 0.95,
					},
				},
			},
		}, nil
	})
	staticChoiceRule := `{
		"dialect": "system_one",
		"inputs": ["message.body"],
		"spec": {
			"version": 1,
			"binding": {"instance": "mp-inst"},
			"questions": [{
				"id": "topic",
				"type": "choice",
				"instructions": "Classify topic scope.",
				"options": [
					{"id": "in_scope", "description": "Within project scope"},
					{"id": "out_of_scope", "description": "Outside project scope"}
				]
			}]
		}
	}`
	clsChoice := evChoice.EvaluatePolicy(
		ctx, "pol-static-choice", 1,
		`judgment_labels.topic == "out_of_scope"`,
		staticChoiceRule, "proj-1", facts,
		nil, []messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(time.Second)},
	)
	assert.Equal(t, messagepolicy.ClassViolation, clsChoice.Class)
	assert.Equal(t, "out_of_scope", clsChoice.Judgments.Labels["topic"])
}

func TestValidateAuthoring_CELOnlyRejectsSystemOneFailureReasons(t *testing.T) {
	ctx := context.Background()
	ev, _ := newTestEvaluatorWithSystemOne(t, nil, func(context.Context, provider.Transport, provider.InstanceView, provider.ModelRequest) (provider.ModelResult, error) {
		t.Fatal("authoring must never invoke provider")
		return provider.ModelResult{}, nil
	})

	for _, badKey := range []string{string(provider.FailCapacity), string(provider.FailNoEligibleInstance), systemone.ReasonDialectDisabled} {
		t.Run("cel_only_rejects_"+badKey, func(t *testing.T) {
			sp := messagepolicy.AuthoringSpec{
				Name:            "cel-only-bad-reason",
				Scope:           messagepolicy.Scope{Surfaces: []string{messagepolicy.SurfaceAgentToUser}},
				ViolationRule:   `!message.has_choices`,
				OnFailed:        messagepolicy.OutcomeAdmit,
				OnFailedReasons: map[string]string{badKey: messagepolicy.OutcomeReject},
			}
			_, err := ev.ValidateAuthoring(ctx, sp)
			require.Error(t, err)
			var ve *messagepolicy.ValidationError
			require.ErrorAs(t, err, &ve)
			assert.Equal(t, messagepolicy.ErrCodeValidation, ve.Code)
			assert.Equal(t, "on_failed_reasons", ve.Field)
		})
	}
}

func TestEvaluatePolicy_DialectDisabledRealCompilePath(t *testing.T) {
	ctx := context.Background()
	var providerCalls atomic.Int32
	var enabled atomic.Bool
	enabled.Store(false)

	ev, _ := newTestEvaluatorWithSystemOne(t, enabled.Load, func(context.Context, provider.Transport, provider.InstanceView, provider.ModelRequest) (provider.ModelResult, error) {
		providerCalls.Add(1)
		t.Fatal("disabled dialect must make 0 provider calls")
		return provider.ModelResult{}, nil
	})

	const s1Rule = `{
		"dialect": "system_one",
		"inputs": ["message.body"],
		"spec": {
			"version": 1,
			"binding": {"instance": "mp-inst"},
			"questions": [{"id": "asks_choice", "type": "binary", "instructions": "Asks choice?"}]
		}
	}`

	// Authoring still returns unsupported_dialect when disabled.
	_, authErr := ev.ValidateAuthoring(ctx, messagepolicy.AuthoringSpec{
		Name:            "disabled-author",
		Scope:           messagepolicy.Scope{Surfaces: []string{messagepolicy.SurfaceAgentToUser}},
		ViolationRule:   `judgment_flags.asks_choice`,
		SystemOneRule:   s1Rule,
		OnUncertain:     messagepolicy.OutcomeReject,
		OnFailed:        messagepolicy.OutcomeAdmit,
		OnFailedReasons: map[string]string{systemone.ReasonDialectDisabled: messagepolicy.OutcomeReject},
	})
	require.Error(t, authErr)
	var ve *messagepolicy.ValidationError
	require.ErrorAs(t, authErr, &ve)
	assert.Equal(t, messagepolicy.ErrCodeUnsupportedDialect, ve.Code)

	// Real gate evaluation path (no forged handle): returns ClassFailed,
	// Reason="dialect_disabled", AdmissionError=false, and ResolveOutcome respects
	// on_failed_reasons["dialect_disabled"] = reject over on_failed = admit.
	facts := messagepolicy.Facts{
		Surface: messagepolicy.SurfaceAgentToUser,
		Message: messagepolicy.MessageFacts{Body: "Pick A or B?"},
	}
	cls := ev.EvaluatePolicy(
		ctx, "pol-disabled-real", 1,
		`judgment_flags.asks_choice`,
		s1Rule, "proj-1", facts,
		nil, []messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(time.Second)},
	)
	assert.Equal(t, messagepolicy.ClassFailed, cls.Class)
	assert.Equal(t, systemone.ReasonDialectDisabled, cls.Reason)
	assert.False(t, cls.AdmissionError, "disabled dialect must not be flagged as a server admission error")
	outcome := messagepolicy.ResolveOutcome(
		cls.Class, cls.Reason,
		messagepolicy.OutcomeReject,
		messagepolicy.OutcomeAdmit,
		map[string]string{systemone.ReasonDialectDisabled: messagepolicy.OutcomeReject},
	)
	assert.Equal(t, messagepolicy.OutcomeReject, outcome)
	assert.Zero(t, providerCalls.Load(), "disabled dialect must make 0 provider calls")
}
