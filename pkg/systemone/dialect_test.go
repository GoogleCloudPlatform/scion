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

package systemone_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const testSystemOneConsumer = "system_one_test"

func newTestTypeSafeRuntime(t *testing.T, srv *httptest.Server, maxEval time.Duration) *runtime.Runtime {
	t.Helper()
	inst := runtime.Instance{
		ID:       "inst-1",
		Name:     "ts-primary",
		Adapter:  jevwire.AdapterNameTypeSafe,
		Model:    "jev-1.14.0",
		Endpoint: srv.URL,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-1"},
		Enabled:  true,
		Revision: 3,
	}
	rt := runtime.New(
		runtime.Config{
			Instances:   runtime.StaticInstances(inst),
			Credentials: runtime.StaticCredential("sk-offline-test"),
			MaxEvalTime: maxEval,
		},
		runtime.WithEgressOptionsForTesting(
			egress.WithLoopbackServerForTesting(srv.URL, srv.Client()),
		),
	)
	t.Cleanup(rt.CloseIdleConnections)
	return rt
}

type dialectFuncAdapter struct {
	name     string
	invokeFn func(ctx context.Context, tr provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error)
}

func (a *dialectFuncAdapter) Name() string    { return a.name }
func (a *dialectFuncAdapter) Version() string { return "v1-test" }
func (a *dialectFuncAdapter) Capabilities(_ provider.InstanceView) (provider.Capabilities, error) {
	return provider.Capabilities{
		Primitives: map[string]provider.PrimitiveLimits{
			spec.PrimitiveBinary: {
				Supported:                true,
				DistributionCompleteness: spec.CompletenessComplete,
			},
			spec.PrimitiveChoice: {
				Supported:                true,
				MinOptions:               2,
				MaxOptions:               16,
				DistributionCompleteness: spec.CompletenessComplete,
			},
		},
		MaxQuestionsPerRequest: 8,
		MaxInputTokens:         8192,
		Truncation:             provider.TruncationReported,
		ReturnsDistribution:    true,
		AuthScheme:             provider.AuthScheme{Kind: provider.AuthKindBearer},
		SideEffectFree:         true,
	}, nil
}
func (a *dialectFuncAdapter) Validate(_ provider.InstanceView, _ spec.Spec) error { return nil }
func (a *dialectFuncAdapter) Invoke(ctx context.Context, tr provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
	return a.invokeFn(ctx, tr, inst, req)
}

func newFakeAdapterRuntime(
	t *testing.T,
	maxEval time.Duration,
	invokeFn func(ctx context.Context, tr provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error),
) *runtime.Runtime {
	t.Helper()
	reg := provider.NewRegistry()
	reg.MustRegister(&dialectFuncAdapter{name: "fake-adapter", invokeFn: invokeFn})
	inst := runtime.Instance{
		ID:       "inst-fake",
		Name:     "fake-primary",
		Adapter:  "fake-adapter",
		Model:    "fake-model-1",
		Endpoint: "https://fake.example.test",
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-fake"},
		Enabled:  true,
		Revision: 1,
	}
	rt := runtime.New(runtime.Config{
		Registry:    reg,
		Instances:   runtime.StaticInstances(inst),
		Credentials: runtime.StaticCredential("sk-fake"),
		MaxEvalTime: maxEval,
	})
	t.Cleanup(rt.CloseIdleConnections)
	return rt
}

func TestDialect_DeclaresAndCompileEnvironmentSelection(t *testing.T) {
	dDefault := systemone.NewDialect(systemone.DialectConfig{})
	assert.Equal(t, "system_one", dDefault.Name())
	caps := dDefault.Declares()
	assert.True(t, caps.PerformsIO)
	assert.Equal(t, runtime.DefaultMaxEvalTime, caps.MaxEvalTime)
	assert.Equal(t, []ruleengine.Kind{
		ruleengine.KindFilter,
		ruleengine.Kind(spec.KindRoute),
		ruleengine.Kind(spec.KindJudgmentBool),
		ruleengine.Kind(spec.KindJudgmentChoice),
		ruleengine.Kind(spec.KindJudgments),
	}, caps.ResultKinds)

	dClamped := systemone.NewDialect(systemone.DialectConfig{
		MaxEvalTime: 90 * time.Second,
	})
	assert.Equal(t, runtime.MaxAllowedEvalTime, dClamped.Declares().MaxEvalTime)

	eng := ruleengine.NewEngine()
	require.NoError(t, eng.Register(dDefault))
	require.NoError(t, eng.AllowConsumerDialects("admin-probe", ruleengine.GrantIO(systemone.DialectName)))
	require.NoError(t, eng.AllowConsumerDialects("probe", ruleengine.GrantIO(systemone.DialectName)))
	require.NoError(t, eng.AllowConsumerDialects(testSystemOneConsumer, ruleengine.GrantIO(systemone.DialectName)))

	rule := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Is urgent?"}],
	  "output": {"question": "q1"}
	}`

	// EnvironmentRef.Name selects the content environment (probe reserved to "probe" consumer); ResultType is ignored.
	compiled, err := eng.Compile(context.Background(), systemone.DialectName, rule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer:   spec.ProbeEnvironmentName,
		Name:       spec.ProbeEnvironmentName,
		ResultType: celengine.String(), // ignored by system_one
	})
	require.NoError(t, err)
	assert.False(t, compiled.IsZero())
	assert.Equal(t, spec.ProbeEnvironmentName, compiled.Consumer())
	assert.Equal(t, systemone.DialectName, compiled.Dialect())
	assert.Equal(t, ruleengine.KindFilter, compiled.Kind())

	// Empty or non-probe EnvironmentRef.Name is rejected with CategoryInput.
	for _, badEnv := range []string{"", "message", "trigger"} {
		_, err := eng.Compile(context.Background(), systemone.DialectName, rule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: "probe",
			Name:     badEnv,
		})
		require.Error(t, err, "env.Name=%q must fail closed in P1", badEnv)
		ce, ok := spec.AsCompileError(err)
		require.True(t, ok, "expected *spec.CompileError, got %T: %v", err, err)
		assert.Equal(t, spec.CategoryInput, ce.Category)
	}

	// MaxRuleBytes is enforced when positive.
	_, err = eng.Compile(context.Background(), systemone.DialectName, rule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer:     testSystemOneConsumer,
		Name:         spec.ProbeEnvironmentName,
		MaxRuleBytes: 16,
	})
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryLimit, ce.Category)
}

func TestDialect_EngineSuccessAndUncertainMatchProbeShape(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer sk-offline-test", r.Header.Get("Authorization"))
		var body struct {
			Model     string         `json:"model"`
			Questions map[string]any `json:"questions"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "jev-1.14.0", body.Model)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-TypeSafe-Request-Id", "req_synth_dialect_01")
		// Switch response based on question IDs so one server covers binary, judgments, and abstain.
		if len(body.Questions) == 2 {
			_, _ = w.Write([]byte(`{
			  "model": "jev-1.14.0",
			  "answers": {
			    "is_bug": {
			      "type": "noul",
			      "noul": 0.88
			    },
			    "topic": {
			      "type": "choice",
			      "choice": "billing",
			      "confidence": 0.79,
			      "probabilities": {"billing": 0.75, "auth": 0.15, "other": 0.10}
			    }
			  },
			  "usage": {"input_tokens": 24, "output_tokens": 4}
			}`))
			return
		}
		if _, hasAbstain := body.Questions["abstain_q"]; hasAbstain {
			_, _ = w.Write([]byte(`{
			  "model": "jev-1.14.0",
			  "answers": {
			    "abstain_q": {
			      "type": "choice",
			      "choice": "unsure",
			      "confidence": 0.62,
			      "probabilities": {"billing": 0.20, "auth": 0.18, "unsure": 0.62}
			    }
			  },
			  "usage": {"input_tokens": 18, "output_tokens": 2}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
		  "model": "jev-1.14.0",
		  "answers": {
		    "q1": {
		      "type": "noul",
		      "noul": 0.91
		    }
		  },
		  "usage": {"input_tokens": 12, "output_tokens": 1}
		}`))
	}))
	defer srv.Close()

	rt := newTestTypeSafeRuntime(t, srv, 3*time.Second)
	eng := ruleengine.NewEngine()
	require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
		Runtime:     rt,
		MaxEvalTime: 3 * time.Second,
	})))
	for _, cons := range []string{"trigger", "probe", "message"} {
		require.NoError(t, eng.AllowConsumerDialects(cons, ruleengine.GrantIO(systemone.DialectName), ruleengine.Grant(ruleengine.DialectCEL)))
	}
	for _, cons := range []string{"trigger", "message"} {
		require.NoError(t, systemone.RegisterConsumerEnvironment(eng, cons, spec.ContentEnvironment{
			Name:          cons + "_env",
			Version:       1,
			Fields:        []spec.FieldDecl{{Name: spec.ProbeFieldText, Kind: spec.FieldKindText, MaxBytes: 16384, Trust: spec.TrustUntrusted}},
			MaxTotalBytes: 16384,
		}))
	}

	t.Run("binary_filter_success", func(t *testing.T) {
		rawRule := `{
		  "dialect": "system_one",
		  "inputs": ["probe.text"],
		  "spec": {
		    "version": 1,
		    "questions": [{"id": "q1", "type": "binary", "instructions": "Does the text report an outage?"}],
		    "output": {"question": "q1"}
		  }
		}`
		c, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: "trigger",
			Name:     "trigger_env",
		})
		require.NoError(t, err)

		dec, err := eng.Evaluate(context.Background(), c, ruleengine.Input{
			Payload: systemone.Request{Input: systemone.ProbeInput("synthetic outage text"), ProjectID: "proj-test"},
		}, ruleengine.Budget{CostLimit: 1}) // CostLimit ignored by system_one
		require.NoError(t, err)
		assert.Equal(t, ruleengine.KindFilter, dec.Kind)
		assert.True(t, dec.Bool)

		eval, ok := dec.Detail.(*systemone.Evaluation)
		require.True(t, ok, "Decision.Detail must hold *systemone.Evaluation")
		assert.Equal(t, systemone.ClassSuccess, eval.Class)
		assert.Equal(t, "trigger", eval.Rule.Consumer)
		assert.Equal(t, "system_one", eval.Rule.Dialect)
		assert.Equal(t, "jev-1.14.0", eval.ConfiguredModel)
		assert.Equal(t, "jev-1.14.0", eval.ResolvedModel)
		require.Contains(t, eval.Answers, "q1")
		ans := eval.Answers["q1"]
		require.NotNil(t, ans.Bool)
		assert.True(t, *ans.Bool)
		assert.InDelta(t, 0.91, ans.Distribution["true"], 1e-6)
		assert.Contains(t, ans.Metrics, "p_top")
		assert.Contains(t, ans.Metrics, "margin")
		assert.Contains(t, ans.Metrics, "norm_entropy")
		assert.Empty(t, ans.NativeMetrics, "noul binary answer has no typesafe.confidence")
		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, "ts-primary", eval.Attempts[0].Instance)
		assert.Equal(t, "success", eval.Attempts[0].Outcome)
		assert.Equal(t, "2xx", eval.Attempts[0].HTTPStatusClass)
	})

	t.Run("multi_question_judgments_matches_direct_probe_shape", func(t *testing.T) {
		rawRule := `{
		  "version": 1,
		  "questions": [
		    {"id": "is_bug", "type": "binary", "instructions": "Is this a bug report?"},
		    {
		      "id": "topic",
		      "type": "choice",
		      "instructions": "Classify topic.",
		      "options": [
		        {"id": "billing", "description": "Billing"},
		        {"id": "auth", "description": "Auth"},
		        {"id": "other", "description": "Other"}
		      ]
		    }
		  ]
		}`
		input := systemone.ProbeInput("invoice calculation looks wrong")

		// Direct probe-style evaluation via Runtime.Evaluate.
		directSpec, err := spec.Compile([]byte(rawRule), spec.CompileOptions{Kind: spec.KindJudgments})
		require.NoError(t, err)
		directDec, err := rt.Evaluate(context.Background(), directSpec, input, runtime.EvalOptions{Consumer: "probe"})
		require.NoError(t, err)

		// Engine-path evaluation via ruleengine.Engine.
		c, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.Kind(spec.KindJudgments), ruleengine.EnvironmentRef{
			Consumer: "probe",
			Name:     spec.ProbeEnvironmentName,
		})
		require.NoError(t, err)
		engDec, err := eng.Evaluate(context.Background(), c, ruleengine.Input{
			Payload: systemone.Request{Input: input, ProjectID: "proj-test"},
		}, ruleengine.Budget{})
		require.NoError(t, err)

		engEval, ok := engDec.Detail.(*systemone.Evaluation)
		require.True(t, ok)
		assert.Equal(t, directDec.Eval.Class, engEval.Class)
		assert.Equal(t, directDec.Eval.Rule.ContentHash, engEval.Rule.ContentHash)
		assert.Equal(t, directDec.Eval.ConfiguredModel, engEval.ConfiguredModel)
		assert.Equal(t, directDec.Eval.ResolvedModel, engEval.ResolvedModel)
		assert.Equal(t, directDec.Eval.Input, engEval.Input)
		assert.Equal(t, directDec.Eval.Answers, engEval.Answers)
		assert.Contains(t, engEval.Answers["topic"].NativeMetrics, "typesafe.confidence")
		require.Len(t, engEval.Attempts, 1)
	})

	t.Run("uncertain_carries_evaluation_in_error_cause", func(t *testing.T) {
		rawRule := `{
		  "dialect": "system_one",
		  "inputs": ["probe.text"],
		  "spec": {
		    "version": 1,
		    "questions": [{
		      "id": "abstain_q",
		      "type": "choice",
		      "instructions": "Route request or abstain.",
		      "options": [
		        {"id": "billing", "description": "Billing"},
		        {"id": "auth", "description": "Auth"},
		        {"id": "unsure", "description": "Unsure", "abstain": true}
		      ]
		    }],
		    "output": {"question": "abstain_q"}
		  }
		}`
		c, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.Kind(spec.KindRoute), ruleengine.EnvironmentRef{
			Consumer: "message",
			Name:     "message_env",
		})
		require.NoError(t, err)

		dec, err := eng.Evaluate(context.Background(), c, ruleengine.Input{
			Payload: systemone.Request{Input: systemone.ProbeInput("ambiguous synthetic text"), ProjectID: "proj-test"},
		}, ruleengine.Budget{})
		require.Error(t, err)
		assert.Equal(t, ruleengine.Decision{}, dec, "Engine.Evaluate zeroes Decision on error")

		var uncErr *ruleengine.UncertainError
		require.ErrorAs(t, err, &uncErr)
		assert.Equal(t, provider.UncertainAbstained, uncErr.Reason)

		eval, ok := systemone.EvaluationFrom(err)
		require.True(t, ok, "EvaluationFrom must extract *Evaluation from UncertainError.Cause")
		assert.Equal(t, systemone.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainAbstained, eval.Reason)
		assert.Contains(t, eval.Answers, "abstain_q")
		assert.True(t, eval.Answers["abstain_q"].Abstained)
	})
}

func TestDialect_MissingOrWrongPayloadReturnsFailedInternalWithoutProviderCall(t *testing.T) {
	var calls atomic.Int32
	rt := newFakeAdapterRuntime(t, 3*time.Second, func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		calls.Add(1)
		return provider.ModelResult{}, errors.New("unexpected call")
	})

	eng := ruleengine.NewEngine()
	require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{Runtime: rt})))
	require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName)))

	rawRule := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	c, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: spec.ProbeEnvironmentName,
		Name:     spec.ProbeEnvironmentName,
	})
	require.NoError(t, err)

	ptr := systemone.ProbeInput("wrong pointer type")
	badInputs := []struct {
		name  string
		input ruleengine.Input
	}{
		{name: "nil_payload", input: ruleengine.Input{}},
		{name: "activation_only", input: ruleengine.Input{Activation: map[string]any{"system_one.input": ptr}}},
		{name: "pointer_payload", input: ruleengine.Input{Payload: &ptr}},
		{name: "string_payload", input: ruleengine.Input{Payload: "raw string"}},
	}

	for _, tc := range badInputs {
		t.Run(tc.name, func(t *testing.T) {
			_, err := eng.Evaluate(context.Background(), c, tc.input, ruleengine.Budget{})
			require.Error(t, err)
			var failedErr *ruleengine.FailedError
			require.ErrorAs(t, err, &failedErr)
			assert.Equal(t, string(provider.FailInternal), failedErr.Reason)

			eval, ok := systemone.EvaluationFrom(err)
			require.True(t, ok)
			assert.Equal(t, systemone.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailInternal), eval.Reason)
			assert.Empty(t, eval.Attempts)
		})
	}
	assert.Zero(t, calls.Load(), "invalid Payload must never invoke provider")
}

func TestDialect_ExperimentGateToggleWithoutRestart(t *testing.T) {
	var calls atomic.Int32
	var enabled atomic.Bool
	enabled.Store(true)

	rt := newFakeAdapterRuntime(t, 3*time.Second, func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		calls.Add(1)
		dist, val, _, _ := provider.BinaryDistribution(0.95)
		return provider.ModelResult{
			ResolvedModel: "fake-model-1",
			HTTPStatus:    200,
			Answers: map[string]provider.Answer{
				"q1": {
					QuestionID:   "q1",
					Primitive:    spec.PrimitiveBinary,
					Status:       provider.AnswerStatusAnswered,
					Bool:         val,
					Distribution: dist,
				},
			},
		}, nil
	})

	eng := ruleengine.NewEngine()
	require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
		Runtime: rt,
		Enabled: enabled.Load,
	})))
	require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName), ruleengine.Grant(ruleengine.DialectCEL)))

	rawRule := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`

	// 1. Enabled = true: Supports is true, Compile and Evaluate succeed.
	assert.True(t, eng.Supports(spec.ProbeEnvironmentName, systemone.DialectName))
	compiledWhileOn, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: spec.ProbeEnvironmentName,
		Name:     spec.ProbeEnvironmentName,
	})
	require.NoError(t, err)

	dec, err := eng.Evaluate(context.Background(), compiledWhileOn, ruleengine.Input{
		Payload: systemone.Request{Input: systemone.ProbeInput("check while on"), ProjectID: "proj-test"},
	}, ruleengine.Budget{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, int32(1), calls.Load())

	// 2. Flip Enabled -> false in the same Engine without re-registering.
	enabled.Store(false)
	assert.True(t, eng.Supports(spec.ProbeEnvironmentName, systemone.DialectName),
		"Engine.Supports reports registration and consumer grant only and stays true when disabled")

	_, err = eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: " " + spec.ProbeEnvironmentName + " ",
		Name:     spec.ProbeEnvironmentName,
	})
	require.Error(t, err)
	var unsupCompile *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, err, &unsupCompile)
	assert.Equal(t, systemone.DialectName, unsupCompile.Dialect)
	assert.Equal(t, spec.ProbeEnvironmentName, unsupCompile.Consumer)
	assert.Equal(t, ruleengine.ReasonUnknownDialect, unsupCompile.Reason)

	// Disabled Compile error message must match an unregistered dialect byte-for-byte
	// for an identically granted consumer.
	unregEng := ruleengine.NewEngine()
	require.NoError(t, unregEng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName)))
	_, unregErr := unregEng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: spec.ProbeEnvironmentName,
		Name:     spec.ProbeEnvironmentName,
	})
	require.Error(t, unregErr)
	assert.Equal(t, unregErr.Error(), err.Error())

	_, err = eng.Evaluate(context.Background(), compiledWhileOn, ruleengine.Input{
		Payload: systemone.Request{Input: systemone.ProbeInput("must not reach provider"), ProjectID: "proj-test"},
	}, ruleengine.Budget{})
	require.Error(t, err)
	var failedErr *ruleengine.FailedError
	require.ErrorAs(t, err, &failedErr)
	assert.Equal(t, systemone.ReasonDialectDisabled, failedErr.Reason)
	var unsupCause *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, failedErr.Cause, &unsupCause)
	assert.Equal(t, systemone.DialectName, unsupCause.Dialect)
	assert.Equal(t, int32(1), calls.Load(), "Evaluate while disabled must make zero provider calls")

	// 3. Flip Enabled -> true again: Compile and previously compiled rule both work.
	enabled.Store(true)
	_, err = eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: spec.ProbeEnvironmentName,
		Name:     spec.ProbeEnvironmentName,
	})
	require.NoError(t, err)

	dec2, err := eng.Evaluate(context.Background(), compiledWhileOn, ruleengine.Input{
		Payload: systemone.Request{Input: systemone.ProbeInput("back on"), ProjectID: "proj-test"},
	}, ruleengine.Budget{})
	require.NoError(t, err)
	assert.True(t, dec2.Bool)
	assert.Equal(t, int32(2), calls.Load())
}

func TestDialect_EngineWrapShapesPreserveEvaluationProvenance(t *testing.T) {
	rawRule := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`

	t.Run("margin_allows_runtime_timeout_to_pass_through_before_engine_context_expires", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rt := newFakeAdapterRuntime(t, 3*time.Second, func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				// Under synctest fake time the margin only guarantees deadline ordering, so
				// assert the exact 120ms - min(10ms, 5%) = 114ms deadline explicitly.
				dl, ok := attemptCtx.Deadline()
				assert.True(t, ok)
				assert.Equal(t, 114*time.Millisecond, time.Until(dl),
					"runtime deadline must sit min(10ms, 5%) before the engine deadline")
				<-attemptCtx.Done()
				return provider.ModelResult{}, attemptCtx.Err()
			})
			eng := ruleengine.NewEngine()
			require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
				Runtime:     rt,
				MaxEvalTime: 3 * time.Second,
			})))
			require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName)))
			c, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
				Consumer: spec.ProbeEnvironmentName,
				Name:     spec.ProbeEnvironmentName,
			})
			require.NoError(t, err)

			_, err = eng.Evaluate(context.Background(), c, ruleengine.Input{
				Payload: systemone.Request{Input: systemone.ProbeInput("hang"), ProjectID: "proj-test"},
			}, ruleengine.Budget{MaxEvalTime: 120 * time.Millisecond})
			require.Error(t, err)

			var failedErr *ruleengine.FailedError
			require.ErrorAs(t, err, &failedErr)
			assert.Equal(t, celengine.ReasonTimeout, failedErr.Reason)

			eval, ok := systemone.EvaluationFrom(err)
			require.True(t, ok)
			assert.Equal(t, systemone.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailTimeout), eval.Reason)
		})
	})

	t.Run("wrap_shape_1_caller_cancel_joins_caller_err_and_dialect_evaluation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			arrived := make(chan struct{})
			rt := newFakeAdapterRuntime(t, 3*time.Second, func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				close(arrived)
				<-attemptCtx.Done()
				return provider.ModelResult{}, attemptCtx.Err()
			})
			eng := ruleengine.NewEngine()
			require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
				Runtime:     rt,
				MaxEvalTime: 3 * time.Second,
			})))
			require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName)))
			c, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
				Consumer: spec.ProbeEnvironmentName,
				Name:     spec.ProbeEnvironmentName,
			})
			require.NoError(t, err)

			callerCtx, cancel := context.WithCancel(context.Background())
			cancelDone := make(chan struct{})
			go func() {
				defer close(cancelDone)
				<-arrived
				cancel()
			}()

			_, err = eng.Evaluate(callerCtx, c, ruleengine.Input{
				Payload: systemone.Request{Input: systemone.ProbeInput("cancel me"), ProjectID: "proj-test"},
			}, ruleengine.Budget{})
			<-cancelDone
			require.Error(t, err)

			var failedErr *ruleengine.FailedError
			require.ErrorAs(t, err, &failedErr)
			assert.Equal(t, celengine.ReasonCanceled, failedErr.Reason)
			assert.ErrorIs(t, err, context.Canceled)

			eval, ok := systemone.EvaluationFrom(err)
			require.True(t, ok, "EvaluationFrom must reach *Evaluation inside FailedError{canceled, Join(callerErr, dialectErr)}")
			assert.Equal(t, systemone.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailCanceled), eval.Reason)
			require.Len(t, eval.Attempts, 1)
			assert.Equal(t, string(provider.FailCanceled), eval.Attempts[0].Reason)
		})
	})

	t.Run("wrap_shape_2_engine_timeout_joins_cel_eval_error_and_dialect_evaluation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// Force d.Evaluate to return AFTER ruleengine's evalCtx has also expired so
			// Engine.Evaluate hits the errDialectEvalTimeout branch and wraps with
			// FailedError{timeout, errors.Join(&celengine.EvalError{Reason: "timeout"}, dialectErr)}.
			rt := newFakeAdapterRuntime(t, 60*time.Millisecond, func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				dl, ok := attemptCtx.Deadline()
				assert.True(t, ok)
				assert.Equal(t, 57*time.Millisecond, time.Until(dl),
					"runtime deadline must sit min(10ms, 5%) before the 60ms engine deadline")
				<-attemptCtx.Done()
				// Advance the fake clock past the 3ms runtime margin so Engine's 60ms evalCtx also expires.
				time.Sleep(15 * time.Millisecond)
				return provider.ModelResult{}, attemptCtx.Err()
			})
			eng := ruleengine.NewEngine()
			require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
				Runtime:     rt,
				MaxEvalTime: 60 * time.Millisecond,
			})))
			require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName)))
			c, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
				Consumer: spec.ProbeEnvironmentName,
				Name:     spec.ProbeEnvironmentName,
			})
			require.NoError(t, err)

			_, err = eng.Evaluate(context.Background(), c, ruleengine.Input{
				Payload: systemone.Request{Input: systemone.ProbeInput("slow unwind"), ProjectID: "proj-test"},
			}, ruleengine.Budget{})
			require.Error(t, err)

			var failedErr *ruleengine.FailedError
			require.ErrorAs(t, err, &failedErr)
			assert.Equal(t, celengine.ReasonTimeout, failedErr.Reason)

			var celEvalErr *celengine.EvalError
			require.ErrorAs(t, err, &celEvalErr, "expected outer Engine timeout wrap containing *celengine.EvalError")
			assert.Equal(t, celengine.ReasonTimeout, celEvalErr.Reason)

			eval, ok := systemone.EvaluationFrom(err)
			require.True(t, ok, "EvaluationFrom must reach *Evaluation inside FailedError{timeout, Join(EvalError{timeout}, dialectErr)}")
			assert.Equal(t, systemone.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailTimeout), eval.Reason,
				"outer and inner timeout reasons must agree via latched runtime sentinel")
		})
	})

	t.Run("wrap_shape_3_budget_tracker_exhaustion_wraps_err_budget_exhausted_and_evaluation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rt := newFakeAdapterRuntime(t, 3*time.Second, func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				<-attemptCtx.Done()
				return provider.ModelResult{}, attemptCtx.Err()
			})
			eng := ruleengine.NewEngine()
			require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
				Runtime:     rt,
				MaxEvalTime: 3 * time.Second,
			})))
			require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName)))
			c, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
				Consumer: spec.ProbeEnvironmentName,
				Name:     spec.ProbeEnvironmentName,
			})
			require.NoError(t, err)

			tracker := ruleengine.NewBudgetTracker(80 * time.Millisecond)
			_, err = eng.Evaluate(context.Background(), c, ruleengine.Input{
				Payload: systemone.Request{Input: systemone.ProbeInput("budget bound"), ProjectID: "proj-test"},
			}, ruleengine.Budget{Tracker: tracker})
			require.Error(t, err)
			assert.ErrorIs(t, err, ruleengine.ErrBudgetExhausted)

			eval, ok := systemone.EvaluationFrom(err)
			require.True(t, ok, "EvaluationFrom must reach *Evaluation inside fmt.Errorf(\"%%w: %%w\", ErrBudgetExhausted, dialectErr)")
			assert.Equal(t, systemone.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailTimeout), eval.Reason)
		})
	})
}

func TestDialect_DenyByDefaultBlocksUnregisteredEmptyAndNonIOConsumersWithoutProviderCall(t *testing.T) {
	var calls atomic.Int32
	rt := newFakeAdapterRuntime(t, 3*time.Second, func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		calls.Add(1)
		return provider.ModelResult{}, errors.New("unexpected provider call")
	})

	eng := ruleengine.NewEngine()
	require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
		Runtime: rt,
		Enabled: func() bool { return true },
	})))

	const rawRule = `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	ctx := context.Background()

	// 1. Empty consumer rejected with empty_consumer.
	assert.False(t, eng.Supports("", systemone.DialectName))
	_, err := eng.Compile(ctx, systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "",
		Name:     spec.ProbeEnvironmentName,
	})
	var unsup *ruleengine.UnsupportedDialectError
	require.ErrorAs(t, err, &unsup)
	assert.Equal(t, ruleengine.ReasonEmptyConsumer, unsup.Reason)

	// 2. Unregistered consumer rejected with consumer_not_registered.
	assert.False(t, eng.Supports("unregistered_consumer", systemone.DialectName))
	_, err = eng.Compile(ctx, systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "unregistered_consumer",
		Name:     spec.ProbeEnvironmentName,
	})
	unsup = nil
	require.ErrorAs(t, err, &unsup)
	assert.Equal(t, ruleengine.ReasonConsumerNotRegistered, unsup.Reason)

	// 3. Pure Grant without AllowIO rejected with io_not_allowed.
	require.NoError(t, eng.AllowConsumerDialects("pure_only_consumer", ruleengine.Grant(systemone.DialectName)))
	assert.False(t, eng.Supports("pure_only_consumer", systemone.DialectName))
	_, err = eng.Compile(ctx, systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "pure_only_consumer",
		Name:     spec.ProbeEnvironmentName,
	})
	unsup = nil
	require.ErrorAs(t, err, &unsup)
	assert.Equal(t, ruleengine.ReasonIONotAllowed, unsup.Reason)

	// 4. Compiled under GrantIO, then downgraded to pure Grant -> Evaluate fails with io_not_allowed (zero provider calls).
	require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName)))
	compiled, err := eng.Compile(ctx, systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: spec.ProbeEnvironmentName,
		Name:     spec.ProbeEnvironmentName,
	})
	require.NoError(t, err)

	require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.Grant(systemone.DialectName)))
	_, err = eng.Evaluate(ctx, compiled, ruleengine.Input{
		Payload: systemone.Request{Input: systemone.ProbeInput("must not reach provider"), ProjectID: "proj-test"},
	}, ruleengine.Budget{})
	unsup = nil
	require.ErrorAs(t, err, &unsup)
	assert.Equal(t, ruleengine.ReasonIONotAllowed, unsup.Reason)
	assert.Zero(t, calls.Load(), "denied admission must make zero provider calls")
}

func TestDialect_ConsumerAttributionUsesCompiledHandleConsumer(t *testing.T) {
	rt := newFakeAdapterRuntime(t, 3*time.Second, func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		dist, val, _, _ := provider.BinaryDistribution(0.90)
		return provider.ModelResult{
			ResolvedModel: "fake-model-1",
			HTTPStatus:    200,
			Answers: map[string]provider.Answer{
				"q1": {
					QuestionID:   "q1",
					Primitive:    spec.PrimitiveBinary,
					Status:       provider.AnswerStatusAnswered,
					Bool:         val,
					Distribution: dist,
				},
			},
		}, nil
	})

	eng := ruleengine.NewEngine()
	require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{Runtime: rt})))
	require.NoError(t, eng.AllowConsumerDialects("consumer_a", ruleengine.GrantIO(systemone.DialectName)))
	require.NoError(t, eng.AllowConsumerDialects("consumer_b", ruleengine.GrantIO(systemone.DialectName)))
	require.NoError(t, systemone.RegisterConsumerEnvironment(eng, "consumer_a", spec.ContentEnvironment{
		Name:          "consumer_a_env",
		Version:       1,
		Fields:        []spec.FieldDecl{{Name: spec.ProbeFieldText, Kind: spec.FieldKindText, MaxBytes: 16384, Trust: spec.TrustUntrusted}},
		MaxTotalBytes: 16384,
	}))

	const rawRule = `{
	  "dialect": "system_one",
	  "inputs": ["probe.text"],
	  "spec": {
	    "version": 1,
	    "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	    "output": {"question": "q1"}
	  }
	}`
	ctx := context.Background()

	compiledA, err := eng.Compile(ctx, systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "consumer_a",
		Name:     "consumer_a_env",
	})
	require.NoError(t, err)

	// Rebind compiledA's internal rep to consumer_b: evaluation attribution and internal-failure
	// provenance must both reflect c.Consumer() ("consumer_b"), never a stale rep consumer.
	reboundB := ruleengine.NewCompiled("consumer_b", systemone.DialectName, compiledA.Kind(), compiledA.Rep())

	dec, err := eng.Evaluate(ctx, reboundB, ruleengine.Input{
		Payload: systemone.Request{Input: systemone.ProbeInput("check attribution"), ProjectID: "proj-test"},
	}, ruleengine.Budget{})
	require.NoError(t, err)
	evalOK, ok := dec.Detail.(*systemone.Evaluation)
	require.True(t, ok)
	assert.Equal(t, "consumer_b", evalOK.Rule.Consumer)

	_, err = eng.Evaluate(ctx, reboundB, ruleengine.Input{}, ruleengine.Budget{})
	require.Error(t, err)
	evalFail, ok := systemone.EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, "consumer_b", evalFail.Rule.Consumer)
}

func TestDialect_EngineDeadlineMarginCappedAt10ms(t *testing.T) {
	const rawRule = `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`

	synctest.Test(t, func(t *testing.T) {
		var observedRemaining time.Duration
		rt := newFakeAdapterRuntime(t, 3*time.Second, func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dl, ok := attemptCtx.Deadline()
			require.True(t, ok)
			observedRemaining = time.Until(dl)
			<-attemptCtx.Done()
			return provider.ModelResult{}, attemptCtx.Err()
		})
		eng := ruleengine.NewEngine()
		require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{
			Runtime:     rt,
			MaxEvalTime: 3 * time.Second,
		})))
		require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName)))
		c, err := eng.Compile(context.Background(), systemone.DialectName, rawRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: spec.ProbeEnvironmentName,
			Name:     spec.ProbeEnvironmentName,
		})
		require.NoError(t, err)

		_, err = eng.Evaluate(context.Background(), c, ruleengine.Input{
			Payload: systemone.Request{Input: systemone.ProbeInput("cap-check"), ProjectID: "proj-test"},
		}, ruleengine.Budget{MaxEvalTime: 400 * time.Millisecond})
		require.Error(t, err)
		assert.Equal(t, 390*time.Millisecond, observedRemaining,
			"dialect must inject ruleengine.EngineDeadlineMargin (400ms - 10ms cap = 390ms)")

		eval, ok := systemone.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, systemone.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailTimeout), eval.Reason)
	})
}

func TestDialect_ConsumerEnvironmentRegistrationAndCompileMatrix(t *testing.T) {
	ctx := context.Background()

	customEnv := spec.ContentEnvironment{
		Name:    "test_consumer_env",
		Version: 2,
		Fields: []spec.FieldDecl{
			{Name: "doc.title", Kind: spec.FieldKindText, MaxBytes: 256, Trust: spec.TrustTrusted},
			{Name: "doc.body", Kind: spec.FieldKindText, MaxBytes: 16384, Trust: spec.TrustUntrusted},
			{Name: "doc.meta", Kind: spec.FieldKindJSON, MaxBytes: 4096, Trust: spec.TrustUntrusted},
		},
		MaxTotalBytes: 18 * 1024,
	}

	t.Run("choice_sets_refused_fail_closed_at_registration", func(t *testing.T) {
		eng := ruleengine.NewEngine()
		d := systemone.NewDialect(systemone.DialectConfig{})
		require.NoError(t, eng.Register(d))
		require.NoError(t, eng.AllowConsumerDialects("cs_consumer", ruleengine.GrantIO(systemone.DialectName)))

		envWithCS := customEnv.Clone()
		envWithCS.Name = "test_cs_env"
		envWithCS.ChoiceSets = []spec.ChoiceSetDecl{
			{Name: "reviewers", MaxItems: 16, MaxLabelBytes: 64},
		}
		err := systemone.RegisterConsumerEnvironment(eng, "cs_consumer", envWithCS)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dynamic choice sets are not yet supported")
		_, exists := d.Environment("test_cs_env")
		assert.False(t, exists, "refused environment must not be registered")

		// Probe environment remains registered and unaffected.
		_, probeOK := d.Environment(spec.ProbeEnvironmentName)
		assert.True(t, probeOK)
	})

	t.Run("consumer_grant_enforced_when_binding_environment", func(t *testing.T) {
		eng := ruleengine.NewEngine()
		d := systemone.NewDialect(systemone.DialectConfig{})
		require.NoError(t, eng.Register(d))

		// Unregistered consumer cannot bind an environment.
		err := systemone.RegisterConsumerEnvironment(eng, "unregistered", customEnv)
		var unsup *ruleengine.UnsupportedDialectError
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, ruleengine.ReasonConsumerNotRegistered, unsup.Reason)

		// Pure Grant without AllowIO cannot bind an environment.
		require.NoError(t, eng.AllowConsumerDialects("pure_consumer", ruleengine.Grant(systemone.DialectName)))
		err = systemone.RegisterConsumerEnvironment(eng, "pure_consumer", customEnv)
		unsup = nil
		require.ErrorAs(t, err, &unsup)
		assert.Equal(t, ruleengine.ReasonIONotAllowed, unsup.Reason)

		// Consumer with GrantIO can bind its environment.
		require.NoError(t, eng.AllowConsumerDialects("granted_consumer", ruleengine.GrantIO(systemone.DialectName)))
		require.NoError(t, systemone.RegisterConsumerEnvironment(eng, "granted_consumer", customEnv))

		// Duplicate registration is rejected.
		require.Error(t, systemone.RegisterConsumerEnvironment(eng, "granted_consumer", customEnv))

		// Built-in probe stays registered alongside customEnv.
		gotProbe, ok := d.Environment(spec.ProbeEnvironmentName)
		require.True(t, ok)
		assert.Equal(t, spec.ProbeEnvironment(), gotProbe)
		gotCustom, ok := d.Environment(customEnv.Name)
		require.True(t, ok)
		assert.Equal(t, customEnv, gotCustom)
	})

	t.Run("compile_rejects_unknown_env_undeclared_field_and_max_input_tokens", func(t *testing.T) {
		reg := provider.NewRegistry()
		reg.MustRegister(&dialectFuncAdapter{
			name: "tight-adapter",
			invokeFn: func(context.Context, provider.Transport, provider.InstanceView, provider.ModelRequest) (provider.ModelResult, error) {
				return provider.ModelResult{}, errors.New("unexpected invoke")
			},
		})
		// Instance limits MaxInputTokens to 512 -> 512*16 = 8192 bytes ceiling.
		// doc.title (256) fits; doc.body (16384) exceeds 8192.
		smallInst := runtime.Instance{
			ID:       "inst-small",
			Name:     "small-inst",
			Adapter:  "tight-adapter",
			Model:    "fake-model-1",
			Endpoint: "https://fake.example.test",
			Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-small"},
			Limits:   runtime.InstanceLimits{MaxInputTokens: 512},
			Enabled:  true,
			Revision: 1,
		}
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(smallInst),
			Credentials: runtime.StaticCredential("sk-fake"),
		})
		t.Cleanup(rt.CloseIdleConnections)

		eng := ruleengine.NewEngine()
		require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{Runtime: rt})))
		require.NoError(t, eng.AllowConsumerDialects(testSystemOneConsumer, ruleengine.GrantIO(systemone.DialectName)))
		require.NoError(t, systemone.RegisterConsumerEnvironment(eng, testSystemOneConsumer, customEnv))

		// Fits: doc.title (256 bytes <= 512*16) on small-inst.
		okRule := `{
		  "dialect": "system_one",
		  "inputs": ["doc.title"],
		  "on_oversize": "truncate",
		  "spec": {
		    "version": 1,
		    "binding": {"instance": "small-inst"},
		    "questions": [{"id": "q1", "type": "binary", "instructions": "Check title."}],
		    "output": {"question": "q1"}
		  }
		}`
		compiled, err := eng.Compile(ctx, systemone.DialectName, okRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: testSystemOneConsumer,
			Name:     customEnv.Name,
		})
		require.NoError(t, err)
		assert.False(t, compiled.IsZero())

		// Too large for bound instance MaxInputTokens: doc.body (16384 > 512*16 = 8192).
		tooLargeRule := `{
		  "dialect": "system_one",
		  "inputs": ["doc.body"],
		  "spec": {
		    "version": 1,
		    "binding": {"instance": "small-inst"},
		    "questions": [{"id": "q1", "type": "binary", "instructions": "Check body."}],
		    "output": {"question": "q1"}
		  }
		}`
		_, err = eng.Compile(ctx, systemone.DialectName, tooLargeRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: testSystemOneConsumer,
			Name:     customEnv.Name,
		})
		require.Error(t, err)
		ce, ok := spec.AsCompileError(err)
		require.True(t, ok)
		assert.Equal(t, spec.CategoryLimit, ce.Category)
		assert.Equal(t, "inputs", ce.Path)

		// Undeclared field rejected with CategoryInput.
		undeclaredRule := `{
		  "dialect": "system_one",
		  "inputs": ["doc.secret"],
		  "spec": {
		    "version": 1,
		    "binding": {"instance": "small-inst"},
		    "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
		    "output": {"question": "q1"}
		  }
		}`
		_, err = eng.Compile(ctx, systemone.DialectName, undeclaredRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: testSystemOneConsumer,
			Name:     customEnv.Name,
		})
		require.Error(t, err)
		ce, ok = spec.AsCompileError(err)
		require.True(t, ok)
		assert.Equal(t, spec.CategoryInput, ce.Category)

		// Unknown environment rejected with CategoryInput.
		_, err = eng.Compile(ctx, systemone.DialectName, okRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: testSystemOneConsumer,
			Name:     "nonexistent_env",
		})
		require.Error(t, err)
		ce, ok = spec.AsCompileError(err)
		require.True(t, ok)
		assert.Equal(t, spec.CategoryInput, ce.Category)

		// Unknown bound instance rejected with CategoryBinding.
		badBindRule := `{
		  "dialect": "system_one",
		  "inputs": ["doc.title"],
		  "spec": {
		    "version": 1,
		    "binding": {"instance": "missing-inst"},
		    "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
		    "output": {"question": "q1"}
		  }
		}`
		_, err = eng.Compile(ctx, systemone.DialectName, badBindRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
			Consumer: testSystemOneConsumer,
			Name:     customEnv.Name,
		})
		require.Error(t, err)
		ce, ok = spec.AsCompileError(err)
		require.True(t, ok)
		assert.Equal(t, spec.CategoryBinding, ce.Category)
	})
}

type secretPolicyTestAdapter struct {
	dialectFuncAdapter
	allowedSecrets []string
	defaultEP      string
}

func (a *secretPolicyTestAdapter) AllowedSecretNames() []string { return a.allowedSecrets }
func (a *secretPolicyTestAdapter) DefaultEndpoint() string      { return a.defaultEP }

type staticPublicTestResolver struct{}

func (staticPublicTestResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

type targetRoutingTestDialer struct {
	targetAddr string
}

func (d targetRoutingTestDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, d.targetAddr)
}

func TestDialect_RequestForwardsEvaluationContextAndResolvesProjectSecret(t *testing.T) {
	ctx := context.Background()
	const pinnedOrigin = "https://secret-policy.example.test"

	var (
		capturedAuthHeader string
		providerCalls      atomic.Int32
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		capturedAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {"q1": {"type": "noul", "noul": 0.93}},
			"usage": {"input_tokens": 11, "output_tokens": 1}
		}`))
	}))
	defer srv.Close()

	reg := provider.NewRegistry()
	reg.MustRegister(&secretPolicyTestAdapter{
		dialectFuncAdapter: dialectFuncAdapter{
			name: "secret-policy-adapter",
			invokeFn: func(callCtx context.Context, tr provider.Transport, inst provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				req, err := http.NewRequestWithContext(callCtx, http.MethodPost, inst.Endpoint+"/eval", nil)
				if err != nil {
					return provider.ModelResult{}, err
				}
				resp, err := tr.Do(req)
				if err != nil {
					return provider.ModelResult{}, err
				}
				defer func() { _ = resp.Body.Close() }()
				dist, val, _, _ := provider.BinaryDistribution(0.93)
				return provider.ModelResult{
					ResolvedModel: "jev-1.14.0",
					HTTPStatus:    resp.StatusCode,
					Answers: map[string]provider.Answer{
						"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
					},
				}, nil
			},
		},
		allowedSecrets: []string{"GEMINI_API_KEY"},
		defaultEP:      pinnedOrigin,
	})

	projectInst := runtime.Instance{
		ID:       "inst-proj-sec",
		Name:     "proj-secret-inst",
		Adapter:  "secret-policy-adapter",
		Model:    "jev-1.14.0",
		Endpoint: pinnedOrigin,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "GEMINI_API_KEY"},
		Options:  map[string]string{provider.OptionSecretScope: provider.SecretScopeProject},
		Enabled:  true,
		Revision: 2,
	}
	hubInst := runtime.Instance{
		ID:       "inst-hub-sec",
		Name:     "hub-secret-inst",
		Adapter:  "secret-policy-adapter",
		Model:    "jev-1.14.0",
		Endpoint: pinnedOrigin,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "GEMINI_API_KEY"},
		Options:  map[string]string{provider.OptionSecretScope: provider.SecretScopeHub},
		Enabled:  true,
		Revision: 1,
	}

	credSource := func(reqCtx context.Context, inst runtime.Instance) (string, error) {
		scope, isNamed := provider.SecretRefModeFromOptions(inst.Options)
		if isNamed && scope == provider.SecretScopeProject {
			pid := runtime.EvalProjectIDFromContext(reqCtx)
			if pid == "proj-alpha" {
				runtime.RecordSecretRefProvenance(reqCtx, inst.Auth.SecretName, provider.SecretScopeProject, 4)
				return "sk-project-alpha-secret", nil
			}
			return "", egress.ErrAuth
		}
		if isNamed && scope == provider.SecretScopeHub {
			runtime.RecordSecretRefProvenance(reqCtx, inst.Auth.SecretName, provider.SecretScopeHub, 1)
			return "sk-hub-shared-secret", nil
		}
		return "sk-owned-secret", nil
	}

	rt := runtime.New(
		runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(projectInst, hubInst),
			Credentials: credSource,
		},
		runtime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(staticPublicTestResolver{}, targetRoutingTestDialer{targetAddr: srv.Listener.Addr().String()}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)
	t.Cleanup(rt.CloseIdleConnections)

	eng := ruleengine.NewEngine()
	require.NoError(t, eng.Register(systemone.NewDialect(systemone.DialectConfig{Runtime: rt})))
	require.NoError(t, eng.AllowConsumerDialects("trigger", ruleengine.GrantIO(systemone.DialectName)))
	require.NoError(t, systemone.RegisterConsumerEnvironment(eng, "trigger", spec.ContentEnvironment{
		Name:          "trigger_secret_env",
		Version:       1,
		Fields:        []spec.FieldDecl{{Name: spec.ProbeFieldText, Kind: spec.FieldKindText, MaxBytes: 16384, Trust: spec.TrustUntrusted}},
		MaxTotalBytes: 16384,
	}))

	projRule := `{
		"dialect": "system_one",
		"inputs": ["probe.text"],
		"spec": {
			"version": 1,
			"binding": {"instance": "proj-secret-inst"},
			"questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
			"output": {"question": "q1"}
		}
	}`
	compiledProj, err := eng.Compile(ctx, systemone.DialectName, projRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "trigger",
		Name:     "trigger_secret_env",
	})
	require.NoError(t, err)

	// 1. Bare Input (no ProjectID) fails closed with no_eligible_instance (0 provider calls).
	_, err = eng.Evaluate(ctx, compiledProj, ruleengine.Input{
		Payload: systemone.ProbeInput("hello"),
	}, ruleengine.Budget{})
	require.Error(t, err)
	var failedErr *ruleengine.FailedError
	require.ErrorAs(t, err, &failedErr)
	assert.Equal(t, string(provider.FailNoEligibleInstance), failedErr.Reason)
	assert.Zero(t, providerCalls.Load())

	// 2. Request carrying authorized ProjectID, RuleID, Revision, and ElementIndex=0 succeeds
	//    and resolves the project-scoped named secret.
	elemZero := 0
	dec, err := eng.Evaluate(ctx, compiledProj, ruleengine.Input{
		Payload: systemone.Request{
			Input:        systemone.ProbeInput("hello project"),
			ProjectID:    "proj-alpha",
			RuleID:       "trig-99",
			Revision:     7,
			ElementIndex: &elemZero,
		},
	}, ruleengine.Budget{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, int32(1), providerCalls.Load())
	assert.Equal(t, "Bearer sk-project-alpha-secret", capturedAuthHeader)

	eval, ok := dec.Detail.(*systemone.Evaluation)
	require.True(t, ok)
	assert.Equal(t, "proj-alpha", eval.ProjectID)
	assert.Equal(t, "trigger", eval.Rule.Consumer)
	assert.Equal(t, "trig-99", eval.Rule.RuleID)
	assert.Equal(t, int64(7), eval.Rule.Revision)
	require.NotNil(t, eval.Rule.ElementIndex)
	assert.Equal(t, 0, *eval.Rule.ElementIndex)
	require.NotNil(t, eval.Auth)
	require.NotNil(t, eval.Auth.SecretRef)
	assert.Equal(t, provider.SecretScopeProject, eval.Auth.SecretRef.ScopeUsed)
	assert.Equal(t, 4, eval.Auth.SecretVersion)

	// 3. Hub-only named secret works with Request carrying authorized ProjectID.
	hubRule := `{
		"dialect": "system_one",
		"inputs": ["probe.text"],
		"spec": {
			"version": 1,
			"binding": {"instance": "hub-secret-inst"},
			"questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
			"output": {"question": "q1"}
		}
	}`
	compiledHub, err := eng.Compile(ctx, systemone.DialectName, hubRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "trigger",
		Name:     "trigger_secret_env",
	})
	require.NoError(t, err)
	decHub, err := eng.Evaluate(ctx, compiledHub, ruleengine.Input{
		Payload: systemone.Request{
			Input:     systemone.ProbeInput("hello hub"),
			ProjectID: "proj-alpha",
		},
	}, ruleengine.Budget{})
	require.NoError(t, err)
	assert.True(t, decHub.Bool)
	assert.Equal(t, int32(2), providerCalls.Load())
	assert.Equal(t, "Bearer sk-hub-shared-secret", capturedAuthHeader)
}

func TestDialect_RequireProjectEveryInputShapeAndEnvironmentOwnership(t *testing.T) {
	ctx := context.Background()
	var providerCalls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.14.0","answers":{"q1":{"type":"noul","noul":0.9}},"usage":{"input_tokens":10,"output_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)

	inst := runtime.Instance{
		ID:       "inst-r1-o5",
		Name:     "ts-r1",
		Adapter:  jevwire.AdapterNameTypeSafe,
		Model:    "jev-1.14.0",
		Endpoint: srv.URL,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-r1-o5"},
		Enabled:  true,
		Revision: 1,
	}
	rt := runtime.New(
		runtime.Config{
			Instances:        runtime.StaticInstances(inst),
			ProjectAllowlist: runtime.StaticProjectAllowlist(map[string][]string{"proj-allowed": {"ts-r1"}}),
			Credentials:      runtime.StaticCredential("sk-offline-r1-o5"),
		},
		runtime.WithEgressOptionsForTesting(
			egress.WithLoopbackServerForTesting(srv.URL, srv.Client()),
		),
	)
	t.Cleanup(rt.CloseIdleConnections)

	eng := ruleengine.NewEngine()
	d := systemone.NewDialect(systemone.DialectConfig{Runtime: rt})
	require.NoError(t, eng.Register(d))
	require.NoError(t, eng.AllowConsumerDialects(spec.ProbeEnvironmentName, ruleengine.GrantIO(systemone.DialectName)))
	require.NoError(t, eng.AllowConsumerDialects("consumer_a", ruleengine.GrantIO(systemone.DialectName)))
	require.NoError(t, eng.AllowConsumerDialects("consumer_b", ruleengine.GrantIO(systemone.DialectName)))

	envA := spec.ContentEnvironment{
		Name:    "env_a",
		Version: 1,
		Fields: []spec.FieldDecl{
			{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 256, Trust: spec.TrustUntrusted},
		},
		MaxTotalBytes: 512,
	}
	require.NoError(t, systemone.RegisterConsumerEnvironment(eng, "consumer_a", envA))

	probeRule := `{
		"version": 1,
		"binding": {"instance": "ts-r1"},
		"questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
		"output": {"question": "q1"}
	}`
	consumerRule := `{
		"dialect": "system_one",
		"inputs": ["msg.body"],
		"spec": {
			"version": 1,
			"binding": {"instance": "ts-r1"},
			"questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
			"output": {"question": "q1"}
		}
	}`

	// O5: Environment ownership — consumer_b cannot compile against consumer_a's env,
	// and non-probe consumers cannot compile against the reserved "probe" env.
	_, err := eng.Compile(ctx, systemone.DialectName, consumerRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "consumer_b",
		Name:     "env_a",
	})
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryInput, ce.Category)
	assert.Contains(t, ce.Message, "owned by consumer")

	_, err = eng.Compile(ctx, systemone.DialectName, probeRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "consumer_a",
		Name:     spec.ProbeEnvironmentName,
	})
	require.Error(t, err)
	ce, ok = spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryInput, ce.Category)
	assert.Contains(t, ce.Message, "owned by consumer")

	// Compile probe rule under "probe" consumer and consumer rule under "consumer_a".
	compiledProbe, err := eng.Compile(ctx, systemone.DialectName, probeRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: spec.ProbeEnvironmentName,
		Name:     spec.ProbeEnvironmentName,
	})
	require.NoError(t, err)

	compiledConsumer, err := eng.Compile(ctx, systemone.DialectName, consumerRule, ruleengine.KindFilter, ruleengine.EnvironmentRef{
		Consumer: "consumer_a",
		Name:     "env_a",
	})
	require.NoError(t, err)

	consumerInput := systemone.Input{
		Fields: []provider.InputField{{Name: "msg.body", Kind: spec.FieldKindText, Value: "hello"}},
	}

	// R1: Every accepted input shape (bare Input and Request with empty ProjectID,
	// on both probe and registered consumer environments) MUST fail closed with
	// no_eligible_instance and 0 provider calls when no authorized ProjectID is supplied.
	for _, tc := range []struct {
		name     string
		compiled ruleengine.Compiled
		payload  any
	}{
		{"bare_input_probe_env", compiledProbe, systemone.ProbeInput("hello")},
		{"bare_input_consumer_env", compiledConsumer, consumerInput},
		{"empty_project_request_probe_env", compiledProbe, systemone.Request{Input: systemone.ProbeInput("hello")}},
		{"empty_project_request_consumer_env", compiledConsumer, systemone.Request{Input: consumerInput}},
		{"unlisted_project_request_consumer_env", compiledConsumer, systemone.Request{Input: consumerInput, ProjectID: "proj-unlisted"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, evalErr := eng.Evaluate(ctx, tc.compiled, ruleengine.Input{Payload: tc.payload}, ruleengine.Budget{})
			require.Error(t, evalErr)
			var fe *ruleengine.FailedError
			require.ErrorAs(t, evalErr, &fe)
			assert.Equal(t, string(provider.FailNoEligibleInstance), fe.Reason)
			assert.Zero(t, providerCalls.Load(), "fail-closed must make 0 provider calls")
		})
	}

	// Authorized allowlisted project succeeds on the consumer-owned environment.
	dec, err := eng.Evaluate(ctx, compiledConsumer, ruleengine.Input{
		Payload: systemone.Request{Input: consumerInput, ProjectID: "proj-allowed"},
	}, ruleengine.Budget{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, int32(1), providerCalls.Load())
}
