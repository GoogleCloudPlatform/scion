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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

func newTestChainEvaluator(t *testing.T) *messagepolicy.Evaluator {
	t.Helper()
	celV1, err := messagepolicy.NewCELEngine()
	require.NoError(t, err)
	celV2, err := messagepolicy.NewChainCELEngine()
	require.NoError(t, err)
	dial, err := ruleengine.NewCELDialect(celV1, celV2)
	require.NoError(t, err)
	eng := ruleengine.NewEngine()
	require.NoError(t, eng.Register(dial))
	require.NoError(t, eng.AllowConsumerDialects(
		messagepolicy.ConsumerName,
		ruleengine.Grant(ruleengine.DialectCEL),
	))
	ev, err := messagepolicy.New(eng)
	require.NoError(t, err)
	return ev
}

func validChainAuthoringSpec() messagepolicy.AuthoringSpec {
	return messagepolicy.AuthoringSpec{
		Name: "p1-cel-chain",
		Mode: messagepolicy.ModeEnforce,
		Scope: messagepolicy.Scope{
			Surfaces: []string{messagepolicy.SurfaceAgentToUser},
		},
		HasSteps: true,
		Steps: []messagepolicy.ChainStepSpec{
			{
				ID:      "only_user",
				Role:    messagepolicy.StepRoleGuard,
				Dialect: messagepolicy.StepDialectCEL,
				Expr:    `surface == "agent_to_user"`,
			},
			{
				ID:      "no_secret",
				Role:    messagepolicy.StepRoleViolation,
				Dialect: messagepolicy.StepDialectCEL,
				Expr:    `message.body.contains("SECRET")`,
				Reason:  "Policy {policy} blocked secret content for {recipient}.",
			},
		},
		OnFailed:     messagepolicy.OutcomeReject,
		RejectReason: "Default chain reject for {policy}.",
	}
}

// TestChainEnv_NestedMapPrototype verifies the P1 prototype choice (SP1 §7):
// chain: map(string, dyn) supports nested dot/bracket access and has() over
// JSON-shaped per-step maps.
func TestChainEnv_NestedMapPrototype(t *testing.T) {
	celV2, err := messagepolicy.NewChainCELEngine()
	require.NoError(t, err)
	kind := celengine.Kind{Name: "filter", Result: celengine.Bool()}

	prog, err := celV2.Compile(
		`has(chain.tone) && chain.tone.flags["urgent"] == true && chain["tone"].labels["sev"] == "high"`,
		kind,
	)
	require.NoError(t, err)

	act := messagepolicy.NewChainBaseActivation(messagepolicy.Facts{
		Surface: messagepolicy.SurfaceAgentToUser,
	}).Map()
	act["chain"] = map[string]any{
		"tone": map[string]any{
			"flags":  map[string]any{"urgent": true},
			"labels": map[string]any{"sev": "high"},
			"scores": map[string]any{},
		},
	}

	res, _, err := prog.Eval(context.Background(), act)
	require.NoError(t, err)
	b, err := res.Bool()
	require.NoError(t, err)
	assert.True(t, b)
}

func TestChainAuthoring_Validation(t *testing.T) {
	ctx := context.Background()
	ev := newTestChainEvaluator(t)

	// Happy path: valid CEL-only guard + violation on enforce + E1/E2 surfaces.
	okSpec := validChainAuthoringSpec()
	okSpec.Scope.Surfaces = []string{
		messagepolicy.SurfaceAgentToUser,
		messagepolicy.SurfaceAgentToConversation,
	}
	mode, err := ev.ValidateAuthoring(ctx, okSpec)
	require.NoError(t, err)
	assert.Equal(t, messagepolicy.ModeEnforce, mode)

	cases := []struct {
		name     string
		mutate   func(*messagepolicy.AuthoringSpec)
		wantCode string
		wantFld  string
	}{
		{
			name: "mix steps and violation_rule",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.ViolationRule = "true"
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps",
		},
		{
			name: "mix steps and system_one_rule",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.SystemOneRule = `{"version":1}`
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps",
		},
		{
			name: "empty steps",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps = nil
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps",
		},
		{
			name: "exceeds max steps cap (9)",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps = make([]messagepolicy.ChainStepSpec, messagepolicy.MaxChainSteps+1)
				for i := range s.Steps {
					s.Steps[i] = messagepolicy.ChainStepSpec{
						ID:      "s" + string(rune('a'+i)),
						Role:    messagepolicy.StepRoleViolation,
						Dialect: messagepolicy.StepDialectCEL,
						Expr:    "false",
					}
				}
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps",
		},
		{
			name: "P1 rejects shadow mode",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Mode = messagepolicy.ModeShadow
			},
			wantCode: messagepolicy.ErrCodeNotYetSupported,
			wantFld:  "mode",
		},
		{
			name: "P1 rejects empty mode defaulting to shadow",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Mode = ""
			},
			wantCode: messagepolicy.ErrCodeNotYetSupported,
			wantFld:  "mode",
		},
		{
			name: "P1 rejects agent_dm surface",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Scope.Surfaces = []string{messagepolicy.SurfaceAgentDM}
			},
			wantCode: messagepolicy.ErrCodeNotYetSupported,
			wantFld:  "scope.surfaces",
		},
		{
			name: "P1 rejects mention_delivery surface",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Scope.Surfaces = []string{messagepolicy.SurfaceMentionDelivery}
			},
			wantCode: messagepolicy.ErrCodeNotYetSupported,
			wantFld:  "scope.surfaces",
		},
		{
			name: "non-token step id with spaces",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[0].ID = "free text id"
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps[0].id",
		},
		{
			name: "non-token step id uppercase",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[0].ID = "BadID"
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps[0].id",
		},
		{
			name: "non-token step id too long",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[0].ID = "a" + strings.Repeat("b", 32)
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps[0].id",
		},
		{
			name: "duplicate step id",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[1].ID = s.Steps[0].ID
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps[1].id",
		},
		{
			name: "role dialect mismatch guard with system_one",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[0].Dialect = messagepolicy.StepDialectSystemOne
			},
			wantCode: messagepolicy.ErrCodeRoleDialectMismatch,
			wantFld:  "steps[0].dialect",
		},
		{
			name: "role dialect mismatch judgment with cel",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[0].Role = messagepolicy.StepRoleJudgment
				s.Steps[0].Dialect = messagepolicy.StepDialectCEL
			},
			wantCode: messagepolicy.ErrCodeRoleDialectMismatch,
			wantFld:  "steps[0].dialect",
		},
		{
			name: "P1 rejects judgment step",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[0] = messagepolicy.ChainStepSpec{
					ID:        "tone",
					Role:      messagepolicy.StepRoleJudgment,
					Dialect:   messagepolicy.StepDialectSystemOne,
					SystemOne: json.RawMessage(`{"version":1}`),
				}
			},
			wantCode: messagepolicy.ErrCodeNotYetSupported,
			wantFld:  "steps[0].role",
		},
		{
			name: "CEL step with system_one payload rejected",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[0].SystemOne = json.RawMessage(`{"version":1}`)
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps[0].system_one",
		},
		{
			name: "CEL step with verdict_mapping rejected",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[0].HasVerdictMapping = true
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps[0].verdict_mapping",
		},
		{
			name: "no verdict-capable step (guards only)",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps = []messagepolicy.ChainStepSpec{s.Steps[0]}
			},
			wantCode: messagepolicy.ErrCodeChainHasNoVerdict,
			wantFld:  "steps",
		},
		{
			name: "reserved legacy name judgment_flags",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[1].Expr = `judgment_flags["x"] == true`
			},
			wantCode: messagepolicy.ErrCodeReservedName,
			wantFld:  "steps[1].expr",
		},
		{
			name: "reserved legacy name judgment_labels",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[1].Expr = `judgment_labels["x"] == "y"`
			},
			wantCode: messagepolicy.ErrCodeReservedName,
			wantFld:  "steps[1].expr",
		},
		{
			name: "forward/any chain dot reference in P1",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[1].Expr = `chain.tone.flags["urgent"] == true`
			},
			wantCode: messagepolicy.ErrCodeForwardReference,
			wantFld:  "steps[1].expr",
		},
		{
			name: "forward/any chain bracket reference in P1",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[1].Expr = `chain["tone"].flags["urgent"] == true`
			},
			wantCode: messagepolicy.ErrCodeForwardReference,
			wantFld:  "steps[1].expr",
		},
		{
			name: "dynamic chain reference",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[1].Expr = `chain[surface].flags["urgent"] == true`
			},
			wantCode: messagepolicy.ErrCodeDynamicStepReference,
			wantFld:  "steps[1].expr",
		},
		{
			name: "bare chain size reference",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[1].Expr = `size(chain) > 0`
			},
			wantCode: messagepolicy.ErrCodeDynamicStepReference,
			wantFld:  "steps[1].expr",
		},
		{
			name: "step reason template rejects message.body",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[1].Reason = "Blocked: {message.body}"
			},
			wantCode: messagepolicy.ErrCodeValidation,
			wantFld:  "steps[1].reason",
		},
		{
			name: "non-bool CEL expression",
			mutate: func(s *messagepolicy.AuthoringSpec) {
				s.Steps[1].Expr = `"not a bool"`
			},
			wantCode: messagepolicy.ErrCodeInvalidRule,
			wantFld:  "steps[1].expr",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp := validChainAuthoringSpec()
			tc.mutate(&sp)
			_, err := ev.ValidateAuthoring(ctx, sp)
			var valErr *messagepolicy.ValidationError
			require.ErrorAs(t, err, &valErr)
			assert.Equal(t, tc.wantCode, valErr.Code)
			assert.Equal(t, tc.wantFld, valErr.Field)
		})
	}
}

func TestEvaluateChain_GuardSkipViolationRejectAndContentFreeTrace(t *testing.T) {
	ctx := context.Background()
	ev := newTestChainEvaluator(t)

	steps := []messagepolicy.ChainStepInput{
		{
			ID:      "only_user",
			Role:    messagepolicy.StepRoleGuard,
			Dialect: messagepolicy.StepDialectCEL,
			Expr:    `surface == "agent_to_user"`,
		},
		{
			ID:          "no_secret",
			Role:        messagepolicy.StepRoleViolation,
			Dialect:     messagepolicy.StepDialectCEL,
			Expr:        `message.body.contains("TOP_SECRET_TOKEN")`,
			Reason:      "AUTHOR_CUSTOM_REASON_DO_NOT_PUT_IN_TRACE",
			Remediation: "AUTHOR_CUSTOM_REMEDIATION_DO_NOT_PUT_IN_TRACE",
		},
	}

	t.Run("guard false -> skip_policy and second step unexecuted", func(t *testing.T) {
		facts := messagepolicy.Facts{
			Surface: messagepolicy.SurfaceAgentToConversation,
			Message: messagepolicy.MessageFacts{Body: "TOP_SECRET_TOKEN"},
		}
		cls := ev.EvaluateChain(ctx, "pol-1", 1, steps, "chain default", "", facts, messagepolicy.WithBudget(messagepolicy.NewSendBudget()))
		assert.Equal(t, messagepolicy.ClassPass, cls.Class)
		require.NotNil(t, cls.ChainProvenance)
		assert.Equal(t, 1, cls.ChainProvenance.Through)
		require.NotNil(t, cls.ChainProvenance.Deciding)
		assert.Equal(t, "only_user", cls.ChainProvenance.Deciding.ID)
		assert.Equal(t, messagepolicy.StepRoleGuard, cls.ChainProvenance.Deciding.Role)
		assert.Equal(t, "fail", cls.ChainProvenance.EngineClass)
		assert.Equal(t, messagepolicy.HandlingSkipPolicy, cls.ChainProvenance.ConsumerHandling)
		assert.Equal(t, messagepolicy.ChainVerdictSkipPolicy, cls.ChainProvenance.Verdict)
		assert.Equal(t, []int{1}, cls.ChainProvenance.Unexecuted)
		require.Len(t, cls.ChainProvenance.Steps, 1)

		raw := string(messagepolicy.MarshalChainProvenance(cls.ChainProvenance))
		assert.NotContains(t, raw, "TOP_SECRET_TOKEN")
		assert.NotContains(t, raw, "AUTHOR_CUSTOM_REASON")
		assert.NotContains(t, raw, "AUTHOR_CUSTOM_REMEDIATION")
	})

	t.Run("guard true + violation true -> ClassViolation with step reason and content-free trace", func(t *testing.T) {
		facts := messagepolicy.Facts{
			Surface: messagepolicy.SurfaceAgentToUser,
			Message: messagepolicy.MessageFacts{Body: "contains TOP_SECRET_TOKEN"},
		}
		cls := ev.EvaluateChain(ctx, "pol-1", 1, steps, "chain default", "", facts, messagepolicy.WithBudget(messagepolicy.NewSendBudget()))
		assert.Equal(t, messagepolicy.ClassViolation, cls.Class)
		assert.Equal(t, "AUTHOR_CUSTOM_REASON_DO_NOT_PUT_IN_TRACE", cls.DecidingReason)
		assert.Equal(t, "AUTHOR_CUSTOM_REMEDIATION_DO_NOT_PUT_IN_TRACE", cls.DecidingRemediation)
		require.NotNil(t, cls.ChainProvenance)
		assert.Equal(t, 2, cls.ChainProvenance.Through)
		require.NotNil(t, cls.ChainProvenance.Deciding)
		assert.Equal(t, "no_secret", cls.ChainProvenance.Deciding.ID)
		assert.Equal(t, "pass", cls.ChainProvenance.EngineClass)
		assert.Equal(t, messagepolicy.HandlingRejectViolation, cls.ChainProvenance.ConsumerHandling)
		assert.Equal(t, messagepolicy.ChainVerdictReject, cls.ChainProvenance.Verdict)

		raw := string(messagepolicy.MarshalChainProvenance(cls.ChainProvenance))
		assert.NotContains(t, raw, "TOP_SECRET_TOKEN")
		assert.NotContains(t, raw, "AUTHOR_CUSTOM_REASON")
		assert.NotContains(t, raw, "AUTHOR_CUSTOM_REMEDIATION")
	})

	t.Run("guard true + violation false -> allow", func(t *testing.T) {
		facts := messagepolicy.Facts{
			Surface: messagepolicy.SurfaceAgentToUser,
			Message: messagepolicy.MessageFacts{Body: "clean message"},
		}
		cls := ev.EvaluateChain(ctx, "pol-1", 1, steps, "chain default", "", facts, messagepolicy.WithBudget(messagepolicy.NewSendBudget()))
		assert.Equal(t, messagepolicy.ClassPass, cls.Class)
		require.NotNil(t, cls.ChainProvenance)
		assert.Equal(t, messagepolicy.ChainVerdictAllow, cls.ChainProvenance.Verdict)
		assert.Equal(t, messagepolicy.HandlingContinue, cls.ChainProvenance.ConsumerHandling)
		assert.Nil(t, cls.ChainProvenance.Deciding)
		assert.Empty(t, cls.ChainProvenance.Unexecuted)
	})

	t.Run("budget exhausted -> failed/budget_exhausted", func(t *testing.T) {
		tracker := ruleengine.NewBudgetTracker(time.Nanosecond)
		tracker.Charge(time.Millisecond)
		facts := messagepolicy.Facts{Surface: messagepolicy.SurfaceAgentToUser}
		cls := ev.EvaluateChain(ctx, "pol-1", 1, steps, "chain default", "", facts, messagepolicy.WithBudget(tracker))
		assert.Equal(t, messagepolicy.ClassFailed, cls.Class)
		assert.Equal(t, messagepolicy.ReasonBudgetExhausted, cls.Reason)
	})

	t.Run("non-CEL step rejected fail-closed with no PendingStep", func(t *testing.T) {
		badSteps := []messagepolicy.ChainStepInput{
			{ID: "j1", Role: messagepolicy.StepRoleJudgment, Dialect: messagepolicy.StepDialectSystemOne},
		}
		cls := ev.EvaluateChain(ctx, "pol-1", 1, badSteps, "", "", messagepolicy.Facts{})
		assert.Equal(t, messagepolicy.ClassFailed, cls.Class)
		assert.Equal(t, messagepolicy.ReasonChainUnsupported, cls.Reason)
	})
}

// TestBuildChainEnricherMap_JSONShapedOnly verifies SEC-S2-L1: any chain.*
// additions map contains strictly JSON-shaped values (maps, slices, strings,
// numbers, bools; no structs or []byte).
func TestBuildChainEnricherMap_JSONShapedOnly(t *testing.T) {
	yes := true
	high := "high"
	eval := &systemone.Evaluation{
		Class: systemone.ClassSuccess,
		Answers: map[string]systemone.AnswerRecord{
			"is_urgent": {QuestionID: "is_urgent", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: &yes},
			"severity":  {QuestionID: "severity", Primitive: spec.PrimitiveChoice, Status: provider.AnswerStatusAnswered, Choice: &high},
		},
	}
	view := fakeJudgmentView{
		items: map[int]ruleengine.Judgment{
			0: {Step: 0, Dialect: systemone.DialectName, Detail: eval},
		},
	}
	steps := []messagepolicy.ChainStepInput{
		{ID: "tone", Role: messagepolicy.StepRoleJudgment, Dialect: messagepolicy.StepDialectSystemOne},
	}
	m := messagepolicy.BuildChainEnricherMap(steps, view)
	assertJSONShaped(t, m)

	raw, err := json.Marshal(m)
	require.NoError(t, err)
	assert.JSONEq(t, `{"tone":{"flags":{"is_urgent":true},"labels":{"severity":"high"},"scores":{}}}`, string(raw))
}

type fakeJudgmentView struct {
	items map[int]ruleengine.Judgment
}

func (f fakeJudgmentView) Get(step int) (ruleengine.Judgment, bool) {
	j, ok := f.items[step]
	return j, ok
}

func (f fakeJudgmentView) Steps() []int {
	out := make([]int, 0, len(f.items))
	for k := range f.items {
		out = append(out, k)
	}
	return out
}

func assertJSONShaped(t *testing.T, v any) {
	t.Helper()
	if v == nil {
		return
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int64, reflect.Float64:
		return
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			t.Fatalf("disallowed []byte value in chain enricher map")
		}
		for i := 0; i < rv.Len(); i++ {
			assertJSONShaped(t, rv.Index(i).Interface())
		}
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			t.Fatalf("non-string map key %s in chain enricher map", rv.Type().Key())
		}
		iter := rv.MapRange()
		for iter.Next() {
			assertJSONShaped(t, iter.Value().Interface())
		}
	default:
		t.Fatalf("disallowed non-JSON-shaped kind %s (%T) in chain enricher map", rv.Kind(), v)
	}
}
