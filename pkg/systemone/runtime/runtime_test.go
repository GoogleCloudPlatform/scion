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

package runtime_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAdapter is an in-memory provider.Adapter (and optional ModelLister) for
// testing runtime execution, classification, and provenance without jevwire.
type fakeAdapter struct {
	name       string
	version    string
	caps       provider.Capabilities
	validateFn func(inst provider.InstanceView, rule provider.RuleSpec) error
	invokeFn   func(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error)

	mu       sync.Mutex
	calls    int
	lastInst provider.InstanceView
	lastReq  provider.ModelRequest
}

func newFakeAdapter(invokeFn func(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error)) *fakeAdapter {
	return &fakeAdapter{
		name:    "fake-typesafe",
		version: "0.1.0-test",
		caps: provider.Capabilities{
			Primitives: map[string]provider.PrimitiveLimits{
				spec.PrimitiveBinary: {
					Supported:                true,
					DistributionCompleteness: spec.CompletenessComplete,
				},
				spec.PrimitiveChoice: {
					Supported:                true,
					MinOptions:               2,
					MaxOptions:               64,
					DistributionCompleteness: spec.CompletenessComplete,
				},
			},
			MaxQuestionsPerRequest: 64,
			MaxInputTokens:         32768,
			Truncation:             provider.TruncationNever,
			ReturnsDistribution:    true,
			NativeMetrics: []provider.MetricDecl{
				{
					Name:   "typesafe.confidence",
					Unit:   metrics.UnitFraction,
					Min:    0,
					Max:    1,
					Source: metrics.SourceProviderDoc,
				},
			},
			SideEffectFree: true,
			AuthScheme:     provider.AuthBearer,
		},
		invokeFn: invokeFn,
	}
}

func (a *fakeAdapter) Name() string    { return a.name }
func (a *fakeAdapter) Version() string { return a.version }
func (a *fakeAdapter) Capabilities(provider.InstanceView) (provider.Capabilities, error) {
	return a.caps, nil
}
func (a *fakeAdapter) Validate(inst provider.InstanceView, rule provider.RuleSpec) error {
	if a.validateFn != nil {
		return a.validateFn(inst, rule)
	}
	return nil
}
func (a *fakeAdapter) Invoke(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
	a.mu.Lock()
	a.calls++
	a.lastInst = inst
	a.lastReq = req
	a.mu.Unlock()
	return a.invokeFn(ctx, t, inst, req)
}

func (a *fakeAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *fakeAdapter) recordedRequest() (provider.InstanceView, provider.ModelRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastInst, a.lastReq
}

type fakeModelListerAdapter struct {
	*fakeAdapter
	modelsFn func(ctx context.Context, t provider.Transport, inst provider.InstanceView) ([]string, error)
}

func (a *fakeModelListerAdapter) ListModels(ctx context.Context, t provider.Transport, inst provider.InstanceView) ([]string, error) {
	return a.modelsFn(ctx, t, inst)
}

func defaultTestInstance() runtime.Instance {
	return runtime.Instance{
		ID:       "inst-uuid-1",
		Name:     "jev-primary",
		Adapter:  "fake-typesafe",
		Model:    "jev-1.14.0",
		Endpoint: "https://api.typesafe.ai",
		Location: "us",
		Auth: runtime.InstanceAuth{
			Kind:       "api_key",
			SecretName: "systemone.instance.inst-uuid-1",
		},
		Enabled:  true,
		Revision: 3,
	}
}

func compileTestSpec(t *testing.T, raw string, kind spec.Kind) *spec.Compiled {
	t.Helper()
	c, err := spec.Compile([]byte(raw), spec.CompileOptions{Kind: kind})
	require.NoError(t, err)
	return c
}

func boolPtr(v bool) *bool    { return &v }
func strPtr(v string) *string { return &v }

const mixedSpecJSON = `{
  "version": 1,
  "questions": [
    {
      "id": "asks_choice",
      "type": "binary",
      "instructions": "Does probe.text ask for a choice?"
    },
    {
      "id": "topic",
      "type": "choice",
      "instructions": "Which topic is probe.text about?",
      "options": [
        {"id": "billing", "description": "Invoices and payments"},
        {"id": "engineering", "description": "Code and bugs"},
        {"id": "cannot_tell", "description": "Insufficient information", "abstain": true}
      ]
    }
  ]
}`

// ---------------------------------------------------------------------------
// AC-6: Success, Provenance, Portable & Native Metrics, Alias Note
// ---------------------------------------------------------------------------

func TestRuntime_EvaluateSuccessProvenanceAndMetrics(t *testing.T) {
	const probeContent = "SENSITIVE_SYNTHETIC_PROBE_TEXT_DO_NOT_RECORD_IN_EVAL"

	adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
		distBin, valBin, tiedBin, err := provider.BinaryDistribution(0.85)
		if err != nil || tiedBin {
			t.Fatalf("unexpected binary dist error: %v", err)
		}
		return provider.ModelResult{
			ResolvedModel:     "jev-1.14.0",
			ProviderRequestID: "req-ts-98765",
			HTTPStatus:        200,
			Notes:             []string{provider.NoteModelAliasReported},
			Usage: provider.Usage{
				InputTokens:  42,
				OutputTokens: 2,
			},
			Answers: map[string]provider.Answer{
				"asks_choice": {
					QuestionID:   "asks_choice",
					Primitive:    spec.PrimitiveBinary,
					Status:       provider.AnswerStatusAnswered,
					Bool:         valBin,
					Distribution: distBin,
				},
				"topic": {
					QuestionID: "topic",
					Primitive:  spec.PrimitiveChoice,
					Status:     provider.AnswerStatusAnswered,
					Choice:     strPtr("engineering"),
					Distribution: map[string]float64{
						"billing":     0.15,
						"engineering": 0.80,
						"cannot_tell": 0.05,
					},
					NativeMetrics: map[string]provider.Metric{
						"typesafe.confidence": {
							Value:  0.70,
							Unit:   metrics.UnitFraction,
							Min:    0,
							Max:    1,
							Source: metrics.SourceProviderDoc,
						},
					},
				},
			},
		}, nil
	})

	reg := provider.NewRegistry()
	reg.MustRegister(adapter)

	inst := defaultTestInstance()
	inst.Model = "jev-latest"

	rt := runtime.New(runtime.Config{
		Registry:    reg,
		Instances:   runtime.StaticInstances(inst),
		Credentials: runtime.StaticCredential("sk-never-touch-eval"),
	})

	compiled := compileTestSpec(t, mixedSpecJSON, spec.KindJudgments)
	// Pass an extra unreferenced field alongside probe.text to verify stripping.
	in := runtime.Input{
		Fields: []provider.InputField{
			{Name: spec.ProbeFieldText, Kind: spec.FieldKindText, Value: probeContent, Trust: spec.TrustTrusted /* must be coerced to env's untrusted */},
			{Name: "unreferenced.extra", Kind: spec.FieldKindText, Value: "UNREFERENCED_PAYLOAD", Trust: spec.TrustUntrusted},
		},
	}

	dec, err := rt.Evaluate(context.Background(), compiled, in, runtime.EvalOptions{
		Consumer:  "probe",
		RuleID:    "rule-1",
		Revision:  7,
		ProjectID: "proj-1",
	})
	require.NoError(t, err)
	assert.Equal(t, spec.KindJudgments, dec.Kind)
	require.NotNil(t, dec.Eval)
	assert.Equal(t, 1, adapter.callCount(), "must make exactly one attempt")

	// Verify only referenced probe.text reached ModelRequest, with env's Untrusted label.
	gotView, gotReq := adapter.recordedRequest()
	assert.Equal(t, "jev-primary", gotView.Name)
	assert.Equal(t, "jev-latest", gotView.Model)
	require.Len(t, gotReq.Input, 1)
	assert.Equal(t, spec.ProbeFieldText, gotReq.Input[0].Name)
	assert.Equal(t, probeContent, gotReq.Input[0].Value)
	assert.Equal(t, spec.TrustUntrusted, gotReq.Input[0].Trust)
	assert.Equal(t, []string{"asks_choice", "topic"}, gotReq.Questions)

	eval := dec.Eval
	assert.NotEmpty(t, eval.EvalID)
	assert.Equal(t, runtime.ClassSuccess, eval.Class)
	assert.Empty(t, eval.Reason)
	assert.Equal(t, "jev-latest", eval.ConfiguredModel)
	assert.Equal(t, "jev-1.14.0", eval.ResolvedModel)
	assert.True(t, eval.HasModelAliasReported())
	assert.Empty(t, eval.EffectiveResolvedModel(), "alias note makes EffectiveResolvedModel empty")
	assert.Equal(t, compiled.ContentHash, eval.Rule.ContentHash)
	assert.Equal(t, "system_one", eval.Rule.Dialect)

	// Attempt provenance
	require.Len(t, eval.Attempts, 1)
	att := eval.Attempts[0]
	assert.Equal(t, "jev-primary", att.Instance)
	assert.Equal(t, int64(3), att.InstanceRevision)
	assert.Equal(t, "fake-typesafe", att.Adapter)
	assert.Equal(t, "0.1.0-test", att.AdapterVersion)
	assert.Equal(t, "jev-latest", att.ConfiguredModel)
	assert.Equal(t, "jev-1.14.0", att.ReportedModel)
	assert.Equal(t, "req-ts-98765", att.ProviderRequestID)
	assert.Equal(t, 1, att.Requests)
	assert.Equal(t, 0, att.Retries)
	assert.Equal(t, runtime.AttemptSuccess, att.Outcome)
	assert.Equal(t, "2xx", att.HTTPStatusClass)

	// Per-question answers, portable metrics, and native metrics
	require.Len(t, eval.Answers, 2)
	binRec := eval.Answers["asks_choice"]
	require.NotNil(t, binRec.Bool)
	assert.True(t, *binRec.Bool)
	assert.InDelta(t, 0.85, binRec.Metrics[metrics.NamePTop].Value, 1e-9)
	assert.InDelta(t, 0.70, binRec.Metrics[metrics.NameMargin].Value, 1e-9)
	assert.Equal(t, metrics.SourceScion, binRec.Metrics[metrics.NameNormEntropy].Source)

	choiceRec := eval.Answers["topic"]
	require.NotNil(t, choiceRec.Choice)
	assert.Equal(t, "engineering", *choiceRec.Choice)
	assert.False(t, choiceRec.Abstained)
	assert.InDelta(t, 0.80, choiceRec.Metrics[metrics.NamePTop].Value, 1e-9)
	assert.InDelta(t, 0.65, choiceRec.Metrics[metrics.NameMargin].Value, 1e-9)
	assert.InDelta(t, 0.70, choiceRec.NativeMetrics["typesafe.confidence"].Value, 1e-9)
	assert.Equal(t, metrics.SourceProviderDoc, choiceRec.NativeMetrics["typesafe.confidence"].Source)

	// InputFingerprint is content-free: probe text and secret never appear in serialized Evaluation.
	require.Len(t, eval.Input.Fields, 1)
	assert.Equal(t, spec.ProbeFieldText, eval.Input.Fields[0].Name)
	assert.Equal(t, len(probeContent), eval.Input.Fields[0].Bytes)
	assert.True(t, strings.HasPrefix(eval.Input.SHA256, "sha256:"))

	rawEvalJSON, err := json.Marshal(eval)
	require.NoError(t, err)
	assert.NotContains(t, string(rawEvalJSON), probeContent)
	assert.NotContains(t, string(rawEvalJSON), "UNREFERENCED_PAYLOAD")
	assert.NotContains(t, string(rawEvalJSON), "sk-never-touch-eval")
}

// ---------------------------------------------------------------------------
// AC-3: Built-in probe environment 16 KiB limit & conservative bytes pre-check
// ---------------------------------------------------------------------------

func TestRuntime_AC3_ProbeOversizeAndBytesPrecheck(t *testing.T) {
	binaryFilterJSON := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check text."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binaryFilterJSON, spec.KindFilter)

	t.Run("exact_16KiB_succeeds", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.9)
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		text16KiB := strings.Repeat("a", spec.ProbeMaxBytes)
		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(text16KiB), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, 1, adapter.callCount())
	})

	t.Run("over_16KiB_returns_input_too_large_without_calling_adapter", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			t.Fatal("adapter must not be called when input exceeds 16 KiB")
			return provider.ModelResult{}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		oversize := strings.Repeat("x", spec.ProbeMaxBytes+1)
		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(oversize), runtime.EvalOptions{})
		require.Error(t, err)
		assert.Equal(t, 0, adapter.callCount())

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
		require.Len(t, eval.Input.Fields, 1)
		assert.Equal(t, spec.ProbeMaxBytes+1, eval.Input.Fields[0].Bytes)
	})

	t.Run("over_conservative_bytes_precheck_returns_input_too_large", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			t.Fatal("adapter must not be called when input exceeds MaxInputTokens bytes pre-check")
			return provider.ModelResult{}, nil
		})
		adapter.caps.MaxInputTokens = 100 // 100 tokens * 16 bytes/token = 1600 bytes loose pre-check ceiling
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		overPrecheck := strings.Repeat("b", 100*runtime.BytesPerTokenPrecheck+1)
		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(overPrecheck), runtime.EvalOptions{})
		require.Error(t, err)
		assert.Equal(t, 0, adapter.callCount())

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
	})

	t.Run("multibyte_cjk_under_16_bytes_per_token_is_not_falsely_rejected_on_non_silent_adapter", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.9)
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
				},
			}, nil
		})
		adapter.caps.MaxInputTokens = 100 // 600 bytes (e.g. 200 3-byte CJK runes) fits well under 100 * 16 = 1600
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		cjkText := strings.Repeat("語", 200) // 600 UTF-8 bytes
		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(cjkText), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, 1, adapter.callCount())
	})

	t.Run("silent_truncation_adapter_enforces_strict_1_byte_per_token", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			t.Fatal("silent-truncation adapter must reject unprovable non-truncation")
			return provider.ModelResult{}, nil
		})
		adapter.caps.Truncation = provider.TruncationSilent
		adapter.caps.MaxInputTokens = 200
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(strings.Repeat("c", 201)), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
	})

	t.Run("provider_usage_truncated_true_returns_input_too_large", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.8)
			tr := true
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Usage:         provider.Usage{InputTokens: 512, Truncated: &tr},
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("short"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
	})
}

// ---------------------------------------------------------------------------
// AC-6: Abstain, Tie, Optional vs Required, Route & Filter Projections
// ---------------------------------------------------------------------------

func TestRuntime_AC6_UncertainAbstainAndTieAndOptionalQuestions(t *testing.T) {
	t.Run("portable_abstain_option_yields_uncertain_abstained", func(t *testing.T) {
		choiceSpec := `{
		  "version": 1,
		  "questions": [{
		    "id": "route_q",
		    "type": "choice",
		    "instructions": "Pick target.",
		    "options": [
		      {"id": "alpha", "description": "Alpha team"},
		      {"id": "beta", "description": "Beta team"},
		      {"id": "unsure", "description": "Cannot tell", "abstain": true}
		    ]
		  }],
		  "output": {"question": "route_q"}
		}`
		compiled := compileTestSpec(t, choiceSpec, spec.KindRoute)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"route_q": {
						QuestionID: "route_q",
						Primitive:  spec.PrimitiveChoice,
						Status:     provider.AnswerStatusAnswered,
						Choice:     strPtr("unsure"),
						Distribution: map[string]float64{
							"alpha":  0.10,
							"beta":   0.20,
							"unsure": 0.70,
						},
					},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("ambiguous"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainAbstained, eval.Reason)
		assert.True(t, eval.Answers["route_q"].Abstained)
		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, runtime.AttemptUncertain, eval.Attempts[0].Outcome)
	})

	t.Run("adapter_native_abstained_status_yields_uncertain_abstained", func(t *testing.T) {
		binSpec := `{
		  "version": 1,
		  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
		  "output": {"question": "q1"}
		}`
		compiled := compileTestSpec(t, binSpec, spec.KindJudgmentBool)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {
						QuestionID: "q1",
						Primitive:  spec.PrimitiveBinary,
						Status:     provider.AnswerStatusAbstained,
					},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("text"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainAbstained, eval.Reason)
		assert.True(t, eval.Answers["q1"].Abstained)
	})

	t.Run("choice_exact_top_tie_yields_uncertain_tie", func(t *testing.T) {
		choiceSpec := `{
		  "version": 1,
		  "questions": [{
		    "id": "route_q",
		    "type": "choice",
		    "instructions": "Pick target.",
		    "options": [
		      {"id": "alpha", "description": "Alpha team"},
		      {"id": "beta", "description": "Beta team"}
		    ]
		  }],
		  "output": {"question": "route_q"}
		}`
		compiled := compileTestSpec(t, choiceSpec, spec.KindRoute)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"route_q": {
						QuestionID: "route_q",
						Primitive:  spec.PrimitiveChoice,
						Status:     provider.AnswerStatusAnswered,
						Tied:       true,
						Choice:     nil,
						Distribution: map[string]float64{
							"alpha": 0.50,
							"beta":  0.50,
						},
					},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("50-50"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainTie, eval.Reason)
		assert.True(t, eval.Answers["route_q"].Tied)
	})

	t.Run("binary_05_tie_yields_uncertain_tie", func(t *testing.T) {
		binSpec := `{
		  "version": 1,
		  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
		  "output": {"question": "q1"}
		}`
		compiled := compileTestSpec(t, binSpec, spec.KindFilter)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, tied, _ := provider.BinaryDistribution(0.5)
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Tied: tied, Distribution: dist},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("coinflip"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainTie, eval.Reason)
	})

	t.Run("optional_question_uncertain_or_refused_does_not_alter_required_success", func(t *testing.T) {
		optSpec := `{
		  "version": 1,
		  "questions": [
		    {"id": "req_q", "type": "binary", "instructions": "Required question."},
		    {"id": "opt_q", "type": "binary", "instructions": "Optional question."}
		  ],
		  "required": ["req_q"],
		  "output": {"question": "req_q"}
		}`
		compiled := compileTestSpec(t, optSpec, spec.KindFilter)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.95)
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"req_q": {QuestionID: "req_q", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
					"opt_q": {QuestionID: "opt_q", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusRefused},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("ok"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, runtime.ClassSuccess, dec.Eval.Class)
		assert.Equal(t, provider.AnswerStatusRefused, dec.Eval.Answers["opt_q"].Status)
		assert.False(t, dec.Eval.Answers["opt_q"].Required)
	})
}

func TestRuntime_AC6_ProjectionsAndMalformedResponses(t *testing.T) {
	routeSpec := `{
	  "version": 1,
	  "questions": [{
	    "id": "dest",
	    "type": "choice",
	    "instructions": "Select destination.",
	    "options": [
	      {"id": "billing", "description": "Billing queue"},
	      {"id": "support", "description": "Support queue"}
	    ]
	  }],
	  "output": {"question": "dest"}
	}`

	t.Run("route_valid_candidate_succeeds", func(t *testing.T) {
		compiled := compileTestSpec(t, routeSpec, spec.KindRoute)
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("support"),
						Distribution: map[string]float64{"billing": 0.1, "support": 0.9},
					},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("help"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.Equal(t, "support", dec.String)
	})

	t.Run("filter_choice_match_projection", func(t *testing.T) {
		filterChoiceSpec := `{
		  "version": 1,
		  "questions": [{
		    "id": "dest",
		    "type": "choice",
		    "instructions": "Select destination.",
		    "options": [
		      {"id": "billing", "description": "Billing queue"},
		      {"id": "support", "description": "Support queue"}
		    ]
		  }],
		  "output": {"question": "dest", "match": ["billing"]}
		}`
		compiled := compileTestSpec(t, filterChoiceSpec, spec.KindFilter)
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 0.85, "support": 0.15},
					},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("invoice"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
	})

	malformedCases := []struct {
		name   string
		noDist bool
		result provider.ModelResult
	}{
		{
			name:   "route_answer_outside_candidate_set",
			noDist: true,
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID: "dest",
						Primitive:  spec.PrimitiveChoice,
						Status:     provider.AnswerStatusAnswered,
						Choice:     strPtr("evil_injected_target"),
					},
				},
			},
		},
		{
			name: "incomplete_distribution_missing_key",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 1.0},
					},
				},
			},
		},
		{
			name: "non_normalized_distribution_sum_09",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 0.7, "support": 0.2},
					},
				},
			},
		},
		{
			name: "nan_probability",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": math.NaN(), "support": 0.2},
					},
				},
			},
		},
		{
			name: "choice_not_argmax",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 0.2, "support": 0.8},
					},
				},
			},
		},
		{
			name: "missing_required_answer",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers:       map[string]provider.Answer{},
			},
		},
		{
			name: "extra_unknown_question_key",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 0.8, "support": 0.2},
					},
					"unexpected_q": {
						QuestionID: "unexpected_q",
						Primitive:  spec.PrimitiveChoice,
						Status:     provider.AnswerStatusAnswered,
					},
				},
			},
		},
		{
			name: "wrong_primitive_type",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID: "dest",
						Primitive:  spec.PrimitiveBinary,
						Status:     provider.AnswerStatusAnswered,
						Bool:       boolPtr(true),
					},
				},
			},
		},
	}

	for _, tc := range malformedCases {
		t.Run(tc.name, func(t *testing.T) {
			compiled := compileTestSpec(t, routeSpec, spec.KindRoute)
			adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				return tc.result, nil
			})
			if tc.noDist {
				adapter.caps.ReturnsDistribution = false
				lim := adapter.caps.Primitives[spec.PrimitiveChoice]
				lim.DistributionCompleteness = spec.CompletenessNone
				adapter.caps.Primitives[spec.PrimitiveChoice] = lim
			}
			reg := provider.NewRegistry()
			reg.MustRegister(adapter)
			rt := runtime.New(runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(defaultTestInstance()),
				Credentials: runtime.StaticCredential("sk-test"),
			})

			_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("test"), runtime.EvalOptions{})
			require.Error(t, err)
			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok, "Evaluation must be present on every non-cancel path")
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailMalformedResponse), eval.Reason)
		})
	}
}

// ---------------------------------------------------------------------------
// AC-6: Full closed 17-reason FailReason classification + panic recovery +
// Evaluation present on every non-cancel path
// ---------------------------------------------------------------------------

func TestRuntime_AC6_AllClosedFailReasonsAndPanicAndRefusal(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	// Verify every member of the closed 17-value FailReason vocabulary surfaces
	// with Evaluation present on every non-cancel path.
	for _, reason := range provider.AllFailReasons() {
		t.Run("provider_reason_"+string(reason), func(t *testing.T) {
			adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				return provider.ModelResult{}, provider.NewProviderError(reason, reason.RetryableLater(), 2*time.Second, 503, "synthetic failure")
			})
			reg := provider.NewRegistry()
			reg.MustRegister(adapter)
			rt := runtime.New(runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(defaultTestInstance()),
				Credentials: runtime.StaticCredential("sk-test"),
			})

			_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("probe"), runtime.EvalOptions{})
			require.Error(t, err)
			assert.Equal(t, 1, adapter.callCount(), "P1 must not retry even when Retryable is true")

			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok, "Evaluation must be present for reason %s", reason)
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(reason), eval.Reason)
			require.Len(t, eval.Attempts, 1)
			assert.Equal(t, runtime.AttemptFailed, eval.Attempts[0].Outcome)
			assert.Equal(t, string(reason), eval.Attempts[0].Reason)
		})
	}

	t.Run("required_question_refused_yields_provider_refused", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusRefused},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("probe"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailProviderRefused), eval.Reason)
	})

	t.Run("disabled_or_missing_instance_yields_no_eligible_instance", func(t *testing.T) {
		inst := defaultTestInstance()
		inst.Enabled = false
		reg := provider.NewRegistry()
		reg.MustRegister(newFakeAdapter(nil))
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(inst),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("probe"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailNoEligibleInstance), eval.Reason)
	})

	t.Run("adapter_panic_recovered_as_failed_internal_without_leaking_panic_payload", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			panic("PANIC_PAYLOAD_DO_NOT_ECHO")
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("probe"), runtime.EvalOptions{})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "PANIC_PAYLOAD_DO_NOT_ECHO")

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInternal), eval.Reason)
	})
}

func TestRuntime_ListModels(t *testing.T) {
	t.Run("supported_adapter_returns_models", func(t *testing.T) {
		base := newFakeAdapter(nil)
		ml := &fakeModelListerAdapter{
			fakeAdapter: base,
			modelsFn: func(_ context.Context, tr provider.Transport, inst provider.InstanceView) ([]string, error) {
				require.NotNil(t, tr)
				assert.Equal(t, "jev-primary", inst.Name)
				return []string{"jev-1.14.0", "jev-latest"}, nil
			},
		}
		reg := provider.NewRegistry()
		reg.MustRegister(ml)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Credentials: runtime.StaticCredential("sk-test"),
		})

		models, err := rt.ListModels(context.Background(), defaultTestInstance())
		require.NoError(t, err)
		assert.Equal(t, []string{"jev-1.14.0", "jev-latest"}, models)
	})

	t.Run("adapter_without_model_lister_returns_unsupported", func(t *testing.T) {
		reg := provider.NewRegistry()
		reg.MustRegister(newFakeAdapter(nil))
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.ListModels(context.Background(), defaultTestInstance())
		require.Error(t, err)
		pe, ok := provider.AsProviderError(err)
		require.True(t, ok)
		assert.Equal(t, provider.FailUnsupported, pe.Reason)
	})

	t.Run("adapter_panic_in_list_models_recovered_as_failed_internal", func(t *testing.T) {
		base := newFakeAdapter(nil)
		ml := &fakeModelListerAdapter{
			fakeAdapter: base,
			modelsFn: func(_ context.Context, _ provider.Transport, _ provider.InstanceView) ([]string, error) {
				panic("LIST_MODELS_PANIC_DO_NOT_ECHO")
			},
		}
		reg := provider.NewRegistry()
		reg.MustRegister(ml)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Credentials: runtime.StaticCredential("sk-test"),
		})

		models, err := rt.ListModels(context.Background(), defaultTestInstance())
		require.Error(t, err)
		assert.Nil(t, models)
		assert.NotContains(t, err.Error(), "LIST_MODELS_PANIC_DO_NOT_ECHO")
		pe, ok := provider.AsProviderError(err)
		require.True(t, ok)
		assert.Equal(t, provider.FailInternal, pe.Reason)
	})

	t.Run("sanitizes_and_caps_listed_model_names", func(t *testing.T) {
		raw := []string{
			"  jev-1.14.0  ",
			"",
			"   ",
			"bad\ninjected-log",
			"bad\x1b[31mterminal-escape",
			"bad internal space",
			"語-non-ascii",
			strings.Repeat("m", runtime.MaxProviderIdentifierBytes+1),
			strings.Repeat("k", runtime.MaxProviderIdentifierBytes),
		}
		for i := 0; i < 300; i++ {
			raw = append(raw, fmt.Sprintf("jev-extra-%03d", i))
		}

		base := newFakeAdapter(nil)
		ml := &fakeModelListerAdapter{
			fakeAdapter: base,
			modelsFn: func(_ context.Context, _ provider.Transport, _ provider.InstanceView) ([]string, error) {
				return raw, nil
			},
		}
		reg := provider.NewRegistry()
		reg.MustRegister(ml)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Credentials: runtime.StaticCredential("sk-test"),
		})

		models, err := rt.ListModels(context.Background(), defaultTestInstance())
		require.NoError(t, err)
		require.Len(t, models, runtime.MaxListedModels)
		assert.Equal(t, "jev-1.14.0", models[0])
		assert.Equal(t, strings.Repeat("k", runtime.MaxProviderIdentifierBytes), models[1])
		assert.Equal(t, "jev-extra-000", models[2])
		for _, m := range models {
			assert.NotContains(t, m, "\n")
			assert.NotContains(t, m, "\x1b")
			assert.NotContains(t, m, " ")
			assert.LessOrEqual(t, len(m), runtime.MaxProviderIdentifierBytes)
		}
	})
}

type staticPublicResolver struct{}

func (staticPublicResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

type hostRoutingDialer struct {
	targets map[string]string
}

func (d hostRoutingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	target, ok := d.targets[port]
	if !ok {
		return nil, fmt.Errorf("unexpected port %s", port)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, target)
}

func TestRuntime_TransportCacheKeyIsolatesEndpointAndAuth(t *testing.T) {
	var mu sync.Mutex
	var authHeadersA, authHeadersB []string

	srvA := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeadersA = append(authHeadersA, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srvA.Close()

	srvB := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeadersB = append(authHeadersB, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srvB.Close()

	_, portA, err := net.SplitHostPort(srvA.Listener.Addr().String())
	require.NoError(t, err)
	_, portB, err := net.SplitHostPort(srvB.Listener.Addr().String())
	require.NoError(t, err)

	base := newFakeAdapter(nil)
	ml := &fakeModelListerAdapter{
		fakeAdapter: base,
		modelsFn: func(ctx context.Context, tr provider.Transport, inst provider.InstanceView) ([]string, error) {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, inst.Endpoint+"/v1/models", nil)
			if reqErr != nil {
				return nil, reqErr
			}
			resp, doErr := tr.Do(req)
			if doErr != nil {
				return nil, doErr
			}
			_ = resp.Body.Close()
			return []string{"jev-1.14.0"}, nil
		},
	}
	reg := provider.NewRegistry()
	reg.MustRegister(ml)

	rt := runtime.New(
		runtime.Config{
			Registry: reg,
			Credentials: func(_ context.Context, inst runtime.Instance) (string, error) {
				return fmt.Sprintf("key[%s|%s|%s|%s|%s]",
					inst.Auth.SecretName,
					inst.Endpoint,
					inst.Auth.HeaderName,
					inst.Auth.Audience,
					strings.Join(inst.Auth.Scopes, ","),
				), nil
			},
		},
		runtime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(staticPublicResolver{}, hostRoutingDialer{
				targets: map[string]string{
					portA: srvA.Listener.Addr().String(),
					portB: srvB.Listener.Addr().String(),
				},
			}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)
	defer rt.CloseIdleConnections()

	endpointA := "https://provider-a.example.test:" + portA
	endpointB := "https://provider-b.example.test:" + portB

	// Case 1: Identical (ID, Name, Revision, Endpoint), different SecretName.
	instSecretA := runtime.Instance{
		ID:       "same-id",
		Name:     "same-name",
		Revision: 1,
		Adapter:  "fake-typesafe",
		Endpoint: endpointA,
		Enabled:  true,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "secret-A"},
	}
	instSecretB := instSecretA
	instSecretB.Auth.SecretName = "secret-B"

	_, err = rt.ListModels(context.Background(), instSecretA)
	require.NoError(t, err)
	_, err = rt.ListModels(context.Background(), instSecretB)
	require.NoError(t, err)

	// Case 2: Identical (ID, Name, Revision, Auth), different Endpoint.
	instEndB := instSecretA
	instEndB.Endpoint = endpointB
	_, err = rt.ListModels(context.Background(), instEndB)
	require.NoError(t, err)

	// Case 3: Identical (ID, Name, Revision, Endpoint, SecretName), different Audience / Scopes.
	instAud := instSecretA
	instAud.Auth.Audience = "aud-2"
	instAud.Auth.Scopes = []string{"scope-x"}
	_, err = rt.ListModels(context.Background(), instAud)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{
		fmt.Sprintf("Bearer key[secret-A|%s|||]", endpointA),
		fmt.Sprintf("Bearer key[secret-B|%s|||]", endpointA),
		fmt.Sprintf("Bearer key[secret-A|%s||aud-2|scope-x]", endpointA),
	}, authHeadersA, "instances with identical (id, name, revision) must never share bearer credentials")
	require.Equal(t, []string{
		fmt.Sprintf("Bearer key[secret-A|%s|||]", endpointB),
	}, authHeadersB, "different endpoint must never reuse credential bound to previous endpoint")
}

func TestRuntime_ForgetInstanceEvictsAllKeysForID(t *testing.T) {
	var mu sync.Mutex
	var seenTransports []provider.Transport

	base := newFakeAdapter(nil)
	ml := &fakeModelListerAdapter{
		fakeAdapter: base,
		modelsFn: func(_ context.Context, tr provider.Transport, _ provider.InstanceView) ([]string, error) {
			mu.Lock()
			seenTransports = append(seenTransports, tr)
			mu.Unlock()
			return []string{"jev-1.14.0"}, nil
		},
	}
	reg := provider.NewRegistry()
	reg.MustRegister(ml)

	rt := runtime.New(runtime.Config{
		Registry: reg,
		Credentials: func(_ context.Context, inst runtime.Instance) (string, error) {
			return "tok-" + inst.Auth.SecretName, nil
		},
	})
	defer rt.CloseIdleConnections()

	lastTransport := func() provider.Transport {
		mu.Lock()
		defer mu.Unlock()
		return seenTransports[len(seenTransports)-1]
	}

	targetA := runtime.Instance{
		ID:       "inst-target",
		Name:     "target",
		Revision: 1,
		Adapter:  "fake-typesafe",
		Endpoint: "https://provider-a.example.test",
		Enabled:  true,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "secret-a"},
	}
	// Same ID + Revision, different Endpoint and SecretName (distinct transportKey).
	targetB := targetA
	targetB.Endpoint = "https://provider-b.example.test"
	targetB.Auth.SecretName = "secret-b"

	other := runtime.Instance{
		ID:       "inst-other",
		Name:     "other",
		Revision: 1,
		Adapter:  "fake-typesafe",
		Endpoint: "https://provider-other.example.test",
		Enabled:  true,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "secret-other"},
	}

	_, err := rt.ListModels(context.Background(), targetA)
	require.NoError(t, err)
	trTargetA1 := lastTransport()

	_, err = rt.ListModels(context.Background(), targetA)
	require.NoError(t, err)
	trTargetA2 := lastTransport()
	require.Same(t, trTargetA1, trTargetA2, "transport for targetA should be cached before ForgetInstance")

	_, err = rt.ListModels(context.Background(), targetB)
	require.NoError(t, err)
	trTargetB1 := lastTransport()
	require.NotSame(t, trTargetA1, trTargetB1)

	_, err = rt.ListModels(context.Background(), other)
	require.NoError(t, err)
	trOther1 := lastTransport()

	// Blank ID is a no-op.
	rt.ForgetInstance("   ")
	_, err = rt.ListModels(context.Background(), targetA)
	require.NoError(t, err)
	require.Same(t, trTargetA1, lastTransport(), "blank ForgetInstance must not evict cached entries")

	// Evict all keys belonging to inst-target regardless of endpoint/auth fields.
	rt.ForgetInstance("inst-target")

	_, err = rt.ListModels(context.Background(), targetA)
	require.NoError(t, err)
	require.NotSame(t, trTargetA1, lastTransport(), "targetA transport must be evicted after ForgetInstance")

	_, err = rt.ListModels(context.Background(), targetB)
	require.NoError(t, err)
	require.NotSame(t, trTargetB1, lastTransport(), "targetB transport with same ID must also be evicted")

	_, err = rt.ListModels(context.Background(), other)
	require.NoError(t, err)
	require.Same(t, trOther1, lastTransport(), "unrelated instance transport must remain cached")
}

type wrappedAdapterError struct {
	pe    *provider.ProviderError
	cause error
}

func (e wrappedAdapterError) Error() string { return e.pe.Error() }
func (e wrappedAdapterError) Unwrap() []error {
	return []error{e.pe, e.cause}
}

func TestRuntime_WrappedEgressErrorOverridesProviderErrorReason(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	cases := []struct {
		name     string
		sentinel error
		want     provider.FailReason
	}{
		{name: "wrapped_redirect_blocked", sentinel: egress.ErrRedirectBlocked, want: provider.FailEgressRefused},
		{name: "wrapped_blocked_destination", sentinel: egress.ErrBlockedDestination, want: provider.FailEgressRefused},
		{name: "wrapped_scheme_not_allowed", sentinel: egress.ErrSchemeNotAllowed, want: provider.FailEgressRefused},
		{name: "wrapped_invalid_endpoint", sentinel: egress.ErrInvalidEndpoint, want: provider.FailEgressRefused},
		{name: "wrapped_response_too_large", sentinel: egress.ErrResponseTooLarge, want: provider.FailMalformedResponse},
		{name: "wrapped_credential_unavailable", sentinel: egress.ErrCredentialUnavailable, want: provider.FailUnavailable},
		{name: "wrapped_auth", sentinel: egress.ErrAuth, want: provider.FailAuth},
		{name: "wrapped_invalid_placement", sentinel: egress.ErrInvalidPlacement, want: provider.FailInternal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				// Deliberately wrap inside a ProviderError carrying a conflicting Reason
				// to verify runtime's errors.Is egress table wins before pe.Reason is read.
				return provider.ModelResult{}, wrappedAdapterError{
					pe:    provider.NewProviderError(provider.FailInternal, false, 0, 502, "adapter wrapper"),
					cause: tc.sentinel,
				}
			})
			reg := provider.NewRegistry()
			reg.MustRegister(adapter)
			rt := runtime.New(runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(defaultTestInstance()),
				Credentials: runtime.StaticCredential("sk-test"),
			})

			_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("probe"), runtime.EvalOptions{})
			require.Error(t, err)
			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok)
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(tc.want), eval.Reason)
			require.Len(t, eval.Attempts, 1)
			assert.Equal(t, string(tc.want), eval.Attempts[0].Reason)
			assert.Equal(t, "5xx", eval.Attempts[0].HTTPStatusClass)
		})
	}
}

func TestRuntime_SanitizesProviderIdentifiersAndFiltersNotes(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	t.Run("oversize_64KiB_and_newline_identifiers_and_arbitrary_notes_dropped", func(t *testing.T) {
		oversizeModel := strings.Repeat("M", 64<<10) + "\nlevel=ERROR msg=injected"
		newlineAnsModel := "jev-ans\r\nspoofed_field=1"
		oversizeReqID := strings.Repeat("R", 200) + "\ninjected"
		arbitraryTopNote := "arbitrary_echoed_user_prompt_secret"
		arbitraryAnsNote := "arbitrary_echoed_answer_note"

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.9)
			return provider.ModelResult{
				ResolvedModel:     oversizeModel,
				ProviderRequestID: oversizeReqID,
				HTTPStatus:        200,
				Notes:             []string{arbitraryTopNote, provider.NoteClamped001To099},
				Answers: map[string]provider.Answer{
					"q1": {
						QuestionID:    "q1",
						Primitive:     spec.PrimitiveBinary,
						Status:        provider.AnswerStatusAnswered,
						Bool:          val,
						Distribution:  dist,
						ReportedModel: newlineAnsModel,
						Notes:         []string{arbitraryAnsNote, provider.NoteConfidenceFormulaDeviation},
					},
				},
			}, nil
		})

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
			Logger:      logger,
		})

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hello"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		require.NotNil(t, dec.Eval)

		// Violating ResolvedModel/ReportedModel/ProviderRequestID are dropped and NoteModelUnreported is recorded.
		assert.Empty(t, dec.Eval.ResolvedModel)
		assert.Empty(t, dec.Eval.EffectiveResolvedModel())
		require.Len(t, dec.Eval.Attempts, 1)
		assert.Empty(t, dec.Eval.Attempts[0].ReportedModel)
		assert.Empty(t, dec.Eval.Attempts[0].ProviderRequestID)
		assert.Equal(t, []string{provider.NoteClamped001To099, provider.NoteModelUnreported}, dec.Eval.Notes)

		rec := dec.Eval.Answers["q1"]
		assert.Empty(t, rec.ReportedModel)
		assert.Equal(t, []string{provider.NoteConfidenceFormulaDeviation, provider.NoteModelUnreported}, rec.Notes)

		evalJSON, err := json.Marshal(dec.Eval)
		require.NoError(t, err)
		jsonStr := string(evalJSON)
		logs := logBuf.String()
		require.NotEmpty(t, logs)

		for _, forbidden := range []string{
			"MMMMMMMM",
			"msg=injected",
			"jev-ans",
			"spoofed_field",
			"RRRRRRRR",
			arbitraryTopNote,
			arbitraryAnsNote,
		} {
			assert.NotContains(t, jsonStr, forbidden, "Evaluation JSON leaked unsanitized value %q", forbidden)
			assert.NotContains(t, logs, forbidden, "debug logs leaked unsanitized value %q", forbidden)
		}
	})

	t.Run("trimmed_valid_identifier_preserved_and_error_path_sanitized", func(t *testing.T) {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		okAdapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.8)
			return provider.ModelResult{
				ResolvedModel:     "  jev-1.14.0  ",
				ProviderRequestID: "  req_valid_123  ",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist, ReportedModel: " jev-1.14.0 "},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(okAdapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
			Logger:      logger,
		})

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("ok"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.Equal(t, "jev-1.14.0", dec.Eval.ResolvedModel)
		assert.Equal(t, "jev-1.14.0", dec.Eval.EffectiveResolvedModel())
		assert.Equal(t, "jev-1.14.0", dec.Eval.Attempts[0].ReportedModel)
		assert.Equal(t, "req_valid_123", dec.Eval.Attempts[0].ProviderRequestID)
		assert.Equal(t, "jev-1.14.0", dec.Eval.Answers["q1"].ReportedModel)
		assert.Empty(t, dec.Eval.Notes)

		// Error path with hostile ResolvedModel + note must also scrub both.
		errAdapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "bad-err-model\ninjected",
				Notes:         []string{"arbitrary_err_secret"},
				HTTPStatus:    503,
			}, provider.NewProviderError(provider.FailUnavailable, true, 0, 503, "upstream unavailable")
		})
		regErr := provider.NewRegistry()
		regErr.MustRegister(errAdapter)
		rtErr := runtime.New(runtime.Config{
			Registry:    regErr,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
			Logger:      logger,
		})

		_, err = rtErr.Evaluate(context.Background(), compiled, runtime.ProbeInput("err"), runtime.EvalOptions{})
		require.Error(t, err)
		evalErr, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Empty(t, evalErr.ResolvedModel)
		assert.Empty(t, evalErr.Attempts[0].ReportedModel)
		assert.Equal(t, []string{provider.NoteModelUnreported}, evalErr.Notes)

		errJSON, mErr := json.Marshal(evalErr)
		require.NoError(t, mErr)
		assert.NotContains(t, string(errJSON), "bad-err-model")
		assert.NotContains(t, string(errJSON), "arbitrary_err_secret")
		assert.NotContains(t, logBuf.String(), "bad-err-model")
		assert.NotContains(t, logBuf.String(), "arbitrary_err_secret")
	})
}

func TestRuntime_BinarySubUnitSumArgmaxRule(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	cases := []struct {
		name      string
		pTrue     float64
		pFalse    float64
		givenBool *bool
		wantFail  bool
		wantBool  bool
	}{
		{
			name:      "sub_1_sum_within_tolerance_nil_bool_derives_true_by_argmax",
			pTrue:     0.4998,
			pFalse:    0.4994,
			givenBool: nil,
			wantBool:  true,
		},
		{
			name:      "sub_1_sum_within_tolerance_matching_true_bool_succeeds",
			pTrue:     0.4998,
			pFalse:    0.4994,
			givenBool: boolPtr(true),
			wantBool:  true,
		},
		{
			name:      "sub_1_sum_within_tolerance_contradictory_false_bool_rejected",
			pTrue:     0.4998,
			pFalse:    0.4994,
			givenBool: boolPtr(false),
			wantFail:  true,
		},
		{
			name:      "sub_1_sum_within_tolerance_false_argmax_derives_false",
			pTrue:     0.4994,
			pFalse:    0.4998,
			givenBool: nil,
			wantBool:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				return provider.ModelResult{
					ResolvedModel: "jev-1.14.0",
					Answers: map[string]provider.Answer{
						"q1": {
							QuestionID: "q1",
							Primitive:  spec.PrimitiveBinary,
							Status:     provider.AnswerStatusAnswered,
							Bool:       tc.givenBool,
							Distribution: map[string]float64{
								provider.BinaryTrueKey:  tc.pTrue,
								provider.BinaryFalseKey: tc.pFalse,
							},
						},
					},
				}, nil
			})
			reg := provider.NewRegistry()
			reg.MustRegister(adapter)
			rt := runtime.New(runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(defaultTestInstance()),
				Credentials: runtime.StaticCredential("sk-test"),
			})

			dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("check"), runtime.EvalOptions{})
			if tc.wantFail {
				require.Error(t, err)
				eval, ok := runtime.EvaluationFrom(err)
				require.True(t, ok)
				assert.Equal(t, runtime.ClassFailed, eval.Class)
				assert.Equal(t, string(provider.FailMalformedResponse), eval.Reason)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantBool, dec.Bool)
		})
	}
}

func TestRuntime_DeadlineCauseWinsOverSubsequentCallerCancel(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	callerCtx, callerCancel := context.WithCancel(context.Background())
	defer callerCancel()

	adapter := newFakeAdapter(func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		<-attemptCtx.Done()
		// Cancel the parent caller context AFTER the runtime's own deadline has
		// already expired on attemptCtx, before returning to Evaluate.
		callerCancel()
		return provider.ModelResult{}, attemptCtx.Err()
	})

	reg := provider.NewRegistry()
	reg.MustRegister(adapter)
	rt := runtime.New(runtime.Config{
		Registry:    reg,
		Instances:   runtime.StaticInstances(defaultTestInstance()),
		Credentials: runtime.StaticCredential("sk-test"),
		MaxEvalTime: 35 * time.Millisecond,
	})

	_, err := rt.Evaluate(callerCtx, compiled, runtime.ProbeInput("race"), runtime.EvalOptions{})
	require.Error(t, err)
	eval, ok := runtime.EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, runtime.ClassFailed, eval.Class)
	assert.Equal(t, string(provider.FailTimeout), eval.Reason, "runtime deadline cause must win over a subsequent caller cancel")
	require.Len(t, eval.Attempts, 1)
	assert.Equal(t, string(provider.FailTimeout), eval.Attempts[0].Reason)
}

type slowSuccessInstanceSource struct {
	inst runtime.Instance
	hook func(resolveCtx context.Context)
}

func (s slowSuccessInstanceSource) GetInstance(ctx context.Context, name string) (runtime.Instance, bool, error) {
	if s.hook != nil {
		s.hook(ctx)
	}
	if name != s.inst.Name {
		return runtime.Instance{}, false, nil
	}
	return s.inst, true, nil
}

func (s slowSuccessInstanceSource) ListInstances(ctx context.Context) ([]runtime.Instance, error) {
	if s.hook != nil {
		s.hook(ctx)
	}
	return []runtime.Instance{s.inst}, nil
}

func TestRuntime_EngineMaxEvalTimeMarginAndLatch(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	t.Run("margin_subtracts_injected_margin_from_engine_max_eval_time", func(t *testing.T) {
		cases := []struct {
			name         string
			engineBudget time.Duration
			margin       time.Duration
			wantMaxDur   time.Duration // engineBudget - max(0, margin)
		}{
			{
				name:         "short_budget_uses_5pct_margin",
				engineBudget: 160 * time.Millisecond, // 5% = 8ms < 10ms -> 152ms
				margin:       8 * time.Millisecond,
				wantMaxDur:   152 * time.Millisecond,
			},
			{
				name:         "longer_budget_caps_margin_at_10ms",
				engineBudget: 400 * time.Millisecond, // 5% = 20ms > 10ms -> 390ms
				margin:       10 * time.Millisecond,
				wantMaxDur:   390 * time.Millisecond,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var observedRemaining time.Duration
					adapter := newFakeAdapter(func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
						dl, ok := attemptCtx.Deadline()
						require.True(t, ok)
						observedRemaining = time.Until(dl)
						<-attemptCtx.Done()
						return provider.ModelResult{}, attemptCtx.Err()
					})

					reg := provider.NewRegistry()
					reg.MustRegister(adapter)
					rt := runtime.New(runtime.Config{
						Registry:    reg,
						Instances:   runtime.StaticInstances(defaultTestInstance()),
						Credentials: runtime.StaticCredential("sk-test"),
					})

					before := time.Now()
					engineCtx, cancel := context.WithTimeoutCause(context.Background(), tc.engineBudget, fmt.Errorf("engine timeout"))
					defer cancel()

					_, err := rt.Evaluate(engineCtx, compiled, runtime.ProbeInput("margin"), runtime.EvalOptions{
						EngineMaxEvalTime:    tc.engineBudget,
						EngineDeadlineMargin: tc.margin,
					})
					elapsed := time.Since(before)

					require.Error(t, err)
					assert.LessOrEqual(t, observedRemaining, tc.wantMaxDur,
						"attemptCtx deadline must incorporate the injected EngineDeadlineMargin")
					assert.NoError(t, engineCtx.Err(),
						"runtime timeout (elapsed=%v) must fire before engine context deadline (%v)", elapsed, tc.engineBudget)

					eval, ok := runtime.EvaluationFrom(err)
					require.True(t, ok)
					assert.Equal(t, runtime.ClassFailed, eval.Class)
					assert.Equal(t, string(provider.FailTimeout), eval.Reason)
				})
			})
		}
	})

	t.Run("margin_ge_engine_max_eval_time_immediate_fail_timeout", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			engineBudget time.Duration
			margin       time.Duration
		}{
			{name: "margin_equals_budget", engineBudget: 5 * time.Millisecond, margin: 5 * time.Millisecond},
			{name: "margin_exceeds_budget", engineBudget: 5 * time.Millisecond, margin: 10 * time.Millisecond},
		} {
			t.Run(tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var called bool
					adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
						called = true
						return provider.ModelResult{}, fmt.Errorf("adapter must not be invoked")
					})

					reg := provider.NewRegistry()
					reg.MustRegister(adapter)
					rt := runtime.New(runtime.Config{
						Registry:    reg,
						Instances:   runtime.StaticInstances(defaultTestInstance()),
						Credentials: runtime.StaticCredential("sk-test"),
					})

					_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("immediate-timeout"), runtime.EvalOptions{
						EngineMaxEvalTime:    tc.engineBudget,
						EngineDeadlineMargin: tc.margin,
					})
					require.Error(t, err)
					assert.False(t, called, "provider must not be invoked when margin >= EngineMaxEvalTime")

					eval, ok := runtime.EvaluationFrom(err)
					require.True(t, ok)
					assert.Equal(t, runtime.ClassFailed, eval.Class)
					assert.Equal(t, string(provider.FailTimeout), eval.Reason)
					assert.Empty(t, eval.Attempts)
				})
			})
		}
	})

	t.Run("negative_margin_treated_as_zero", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			var observedRemaining time.Duration
			adapter := newFakeAdapter(func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				dl, ok := attemptCtx.Deadline()
				require.True(t, ok)
				observedRemaining = time.Until(dl)
				<-attemptCtx.Done()
				return provider.ModelResult{}, attemptCtx.Err()
			})

			reg := provider.NewRegistry()
			reg.MustRegister(adapter)
			rt := runtime.New(runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(defaultTestInstance()),
				Credentials: runtime.StaticCredential("sk-test"),
			})

			_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("neg-margin"), runtime.EvalOptions{
				EngineMaxEvalTime:    100 * time.Millisecond,
				EngineDeadlineMargin: -10 * time.Millisecond,
			})
			require.Error(t, err)
			assert.Equal(t, 100*time.Millisecond, observedRemaining,
				"negative EngineDeadlineMargin must be treated as 0 and not extend EngineMaxEvalTime")

			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok)
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailTimeout), eval.Reason)
		})
	})

	t.Run("engine_max_eval_time_non_positive_ignores_margin", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			engineBudget time.Duration
		}{
			{name: "zero_engine_max_eval_time", engineBudget: 0},
			{name: "negative_engine_max_eval_time", engineBudget: -10 * time.Millisecond},
		} {
			t.Run(tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var observedRemaining time.Duration
					adapter := newFakeAdapter(func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
						dl, ok := attemptCtx.Deadline()
						require.True(t, ok)
						observedRemaining = time.Until(dl)
						dist, val, _, _ := provider.BinaryDistribution(0.9)
						return provider.ModelResult{
							ResolvedModel: "jev-1.14.0",
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

					reg := provider.NewRegistry()
					reg.MustRegister(adapter)
					rt := runtime.New(runtime.Config{
						Registry:    reg,
						Instances:   runtime.StaticInstances(defaultTestInstance()),
						Credentials: runtime.StaticCredential("sk-test"),
						MaxEvalTime: 500 * time.Millisecond,
					})

					dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("no-engine-bound"), runtime.EvalOptions{
						EngineMaxEvalTime:    tc.engineBudget,
						EngineDeadlineMargin: 10 * time.Second,
					})
					require.NoError(t, err)
					assert.True(t, dec.Bool)
					assert.Equal(t, 500*time.Millisecond, observedRemaining,
						"EngineDeadlineMargin must be ignored when EngineMaxEvalTime <= 0")
				})
			})
		}
	})

	t.Run("latch_preserves_timeout_when_parent_context_expires_during_slow_invoke_unwind", func(t *testing.T) {
		for _, returnNilErr := range []bool{false, true} {
			name := "adapter_returns_err"
			if returnNilErr {
				name = "adapter_returns_nil_result"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					engineCtx, cancel := context.WithTimeoutCause(context.Background(), 60*time.Millisecond, fmt.Errorf("engine timeout"))
					defer cancel()

					adapter := newFakeAdapter(func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
						<-attemptCtx.Done()
						// Wait until the outer engine context also expires before returning.
						<-engineCtx.Done()
						if returnNilErr {
							dist, val, _, _ := provider.BinaryDistribution(0.9)
							return provider.ModelResult{
								ResolvedModel: "jev-1.14.0",
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
						}
						return provider.ModelResult{}, attemptCtx.Err()
					})

					reg := provider.NewRegistry()
					reg.MustRegister(adapter)
					rt := runtime.New(runtime.Config{
						Registry:    reg,
						Instances:   runtime.StaticInstances(defaultTestInstance()),
						Credentials: runtime.StaticCredential("sk-test"),
					})

					_, err := rt.Evaluate(engineCtx, compiled, runtime.ProbeInput("latch"), runtime.EvalOptions{
						EngineMaxEvalTime:    60 * time.Millisecond,
						EngineDeadlineMargin: 3 * time.Millisecond,
					})
					require.Error(t, err)
					eval, ok := runtime.EvaluationFrom(err)
					require.True(t, ok)
					assert.Equal(t, runtime.ClassFailed, eval.Class)
					assert.Equal(t, string(provider.FailTimeout), eval.Reason,
						"once runtime deadline sentinel fires, result must stay timeout even after parent context expires")
					require.Len(t, eval.Attempts, 1)
					assert.Equal(t, string(provider.FailTimeout), eval.Attempts[0].Reason)
				})
			})
		}
	})

	t.Run("latch_preserves_timeout_across_stages_after_resolve_when_parent_context_expires", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			engineCtx, cancel := context.WithTimeoutCause(context.Background(), 60*time.Millisecond, fmt.Errorf("engine timeout"))
			defer cancel()

			instSrc := slowSuccessInstanceSource{
				inst: defaultTestInstance(),
				hook: func(resolveCtx context.Context) {
					// Runtime deadline fires first on resolveCtx; then parent engineCtx
					// also expires before GetInstance returns an instance to Step 3-5.
					<-resolveCtx.Done()
					<-engineCtx.Done()
				},
			}

			adapter := newFakeAdapter(nil)
			reg := provider.NewRegistry()
			reg.MustRegister(adapter)
			rt := runtime.New(runtime.Config{
				Registry:    reg,
				Instances:   instSrc,
				Credentials: runtime.StaticCredential("sk-test"),
			})

			_, err := rt.Evaluate(engineCtx, compiled, runtime.ProbeInput("stage-latch"), runtime.EvalOptions{
				EngineMaxEvalTime:    60 * time.Millisecond,
				EngineDeadlineMargin: 3 * time.Millisecond,
			})
			require.Error(t, err)
			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok)
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailTimeout), eval.Reason,
				"latched runtime deadline from Step 2 must not be overwritten by expired parent context in Step 5")
			assert.Empty(t, eval.Attempts, "provider must not be invoked after deadline expired during resolve")
		})
	})
}

// ---------------------------------------------------------------------------
// PV3 G0: Header auth support, placement wiring, transport cache isolation,
// float-slack choice runtime check, and TypeSafe bearer/default-endpoint regression
// ---------------------------------------------------------------------------

type dynamicCapsAdapter struct {
	*fakeAdapter
	capsFn func(inst provider.InstanceView) (provider.Capabilities, error)
}

func (a *dynamicCapsAdapter) Capabilities(inst provider.InstanceView) (provider.Capabilities, error) {
	if a.capsFn != nil {
		return a.capsFn(inst)
	}
	return a.fakeAdapter.Capabilities(inst)
}

func (a *dynamicCapsAdapter) ListModels(ctx context.Context, tr provider.Transport, inst provider.InstanceView) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, inst.Endpoint+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := tr.Do(req)
	if err != nil {
		return nil, err
	}
	_ = resp.Body.Close()
	return []string{"model-1"}, nil
}

func TestRuntime_InstanceAuthSupportMatrix(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	geminiHeaderScheme := provider.AuthScheme{
		Kind:       provider.AuthKindHeader,
		HeaderName: "x-goog-api-key",
	}

	cases := []struct {
		name          string
		adapterScheme provider.AuthScheme
		instAuthKind  string
		instScopes    []string
		wantFail      provider.FailReason
	}{
		{name: "bearer_adapter_with_empty_inst_auth", adapterScheme: provider.AuthBearer, instAuthKind: "", wantFail: ""},
		{name: "bearer_adapter_with_api_key_inst_auth", adapterScheme: provider.AuthBearer, instAuthKind: "api_key", wantFail: ""},
		{name: "bearer_adapter_with_bearer_inst_auth", adapterScheme: provider.AuthBearer, instAuthKind: "bearer", wantFail: ""},
		{name: "bearer_adapter_with_none_inst_auth_rejected", adapterScheme: provider.AuthBearer, instAuthKind: "none", wantFail: provider.FailUnsupported},
		{name: "header_adapter_with_empty_inst_auth", adapterScheme: geminiHeaderScheme, instAuthKind: "", wantFail: ""},
		{name: "header_adapter_with_api_key_inst_auth", adapterScheme: geminiHeaderScheme, instAuthKind: "api_key", wantFail: ""},
		{name: "header_adapter_with_none_inst_auth_rejected", adapterScheme: geminiHeaderScheme, instAuthKind: "none", wantFail: provider.FailUnsupported},
		{name: "header_adapter_with_bearer_inst_auth_rejected", adapterScheme: geminiHeaderScheme, instAuthKind: "bearer", wantFail: provider.FailUnsupported},
		{name: "header_adapter_with_google_token_inst_auth_rejected", adapterScheme: geminiHeaderScheme, instAuthKind: "google_access_token", wantFail: provider.FailUnsupported},
		{name: "header_adapter_with_empty_header_name_rejected_internal", adapterScheme: provider.AuthScheme{Kind: provider.AuthKindHeader, HeaderName: ""}, instAuthKind: "api_key", wantFail: provider.FailInternal},
		{name: "header_adapter_with_reserved_authorization_rejected_internal", adapterScheme: provider.AuthScheme{Kind: provider.AuthKindHeader, HeaderName: "Authorization"}, instAuthKind: "api_key", wantFail: provider.FailInternal},
		{name: "header_adapter_with_reserved_host_rejected_internal", adapterScheme: provider.AuthScheme{Kind: provider.AuthKindHeader, HeaderName: "Host"}, instAuthKind: "api_key", wantFail: provider.FailInternal},
		{name: "header_adapter_with_crlf_header_name_rejected_internal", adapterScheme: provider.AuthScheme{Kind: provider.AuthKindHeader, HeaderName: "x-key\r\nEvil: 1"}, instAuthKind: "api_key", wantFail: provider.FailInternal},
		{name: "none_adapter_with_empty_inst_auth", adapterScheme: provider.AuthNone, instAuthKind: "", wantFail: ""},
		{name: "none_adapter_with_none_inst_auth", adapterScheme: provider.AuthNone, instAuthKind: "none", wantFail: ""},
		{name: "none_adapter_with_api_key_inst_auth_rejected", adapterScheme: provider.AuthNone, instAuthKind: "api_key", wantFail: provider.FailUnsupported},
		{name: "google_access_token_adapter_with_matching_inst_auth_and_explicit_scope", adapterScheme: provider.AuthScheme{Kind: provider.AuthKindGoogleAccessToken}, instAuthKind: "google_access_token", instScopes: []string{"https://www.googleapis.com/auth/synthetic.test"}, wantFail: ""},
		{name: "google_access_token_adapter_without_scopes_rejected_invalid_request", adapterScheme: provider.AuthScheme{Kind: provider.AuthKindGoogleAccessToken}, instAuthKind: "google_access_token", wantFail: provider.FailInvalidRequest},
		{name: "google_access_token_adapter_with_api_key_rejected", adapterScheme: provider.AuthScheme{Kind: provider.AuthKindGoogleAccessToken}, instAuthKind: "api_key", wantFail: provider.FailUnsupported},
		{name: "google_id_token_adapter_rejected_unsupported", adapterScheme: provider.AuthScheme{Kind: provider.AuthKindGoogleIDToken}, instAuthKind: "google_id_token", wantFail: provider.FailUnsupported},
		{name: "azure_entra_adapter_rejected_unsupported", adapterScheme: provider.AuthScheme{Kind: provider.AuthKindAzureEntra}, instAuthKind: "azure_entra", wantFail: provider.FailUnsupported},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			base := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				dist, val, _, _ := provider.BinaryDistribution(0.9)
				return provider.ModelResult{
					Answers: map[string]provider.Answer{
						"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
					},
				}, nil
			})
			base.caps.AuthScheme = tc.adapterScheme
			adapter := &dynamicCapsAdapter{fakeAdapter: base}

			reg := provider.NewRegistry()
			reg.MustRegister(adapter)

			inst := defaultTestInstance()
			inst.Endpoint = srv.URL
			inst.Auth = runtime.InstanceAuth{
				Kind:       tc.instAuthKind,
				SecretName: "systemone.instance.inst-uuid-1",
				Scopes:     tc.instScopes,
			}

			var credSrc runtime.CredentialSource
			if tc.adapterScheme.Kind != provider.AuthKindNone {
				credSrc = runtime.StaticCredential("sk-matrix-key")
			}

			rt := runtime.New(
				runtime.Config{
					Registry:    reg,
					Instances:   runtime.StaticInstances(inst),
					Credentials: credSrc,
				},
				runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(srv.URL, srv.Client())),
			)
			defer rt.CloseIdleConnections()

			dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hello"), runtime.EvalOptions{})
			if tc.wantFail == "" {
				require.NoError(t, err)
				assert.True(t, dec.Bool)
				assert.Equal(t, 1, adapter.callCount())

				models, listErr := rt.ListModels(context.Background(), inst)
				require.NoError(t, listErr)
				assert.Equal(t, []string{"model-1"}, models)
			} else {
				require.Error(t, err)
				assert.Equal(t, 0, adapter.callCount(), "adapter must not be invoked when auth support check fails")
				eval, ok := runtime.EvaluationFrom(err)
				require.True(t, ok)
				assert.Equal(t, runtime.ClassFailed, eval.Class)
				assert.Equal(t, string(tc.wantFail), eval.Reason)

				_, listErr := rt.ListModels(context.Background(), inst)
				require.Error(t, listErr)
				pe, ok := provider.AsProviderError(listErr)
				require.True(t, ok)
				assert.Equal(t, tc.wantFail, pe.Reason)
			}
		})
	}
}

func TestRuntime_HeaderPlacementWireIsolationFloatBoundaryAndUnreportedModelProvenance(t *testing.T) {
	const canaryKey = "AIzaSy-runtime-header-canary-secret-442200"

	var (
		gotHeaders http.Header
		gotQuery   string
		gotBody    []byte
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotQuery = r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	choiceSpec := `{
	  "version": 1,
	  "questions": [{
	    "id": "department",
	    "type": "choice",
	    "instructions": "Route probe.text to a department.",
	    "options": [
	      {"id": "billing", "description": "Billing"},
	      {"id": "sales", "description": "Sales"},
	      {"id": "support", "description": "Support"},
	      {"id": "other", "description": "Other"}
	    ]
	  }],
	  "output": {"question": "department"}
	}`
	compiled := compileTestSpec(t, choiceSpec, spec.KindRoute)

	adapter := newFakeAdapter(func(ctx context.Context, tr provider.Transport, inst provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, inst.Endpoint+"/v1beta/decisions", strings.NewReader(`{"model":"gemini-decision-1"}`))
		if err != nil {
			return provider.ModelResult{}, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := tr.Do(httpReq)
		if err != nil {
			return provider.ModelResult{}, err
		}
		_ = resp.Body.Close()

		return provider.ModelResult{
			ResolvedModel: "", // Gemini omits model in response (PV3 brief r1.2)
			HTTPStatus:    resp.StatusCode,
			Notes:         []string{provider.NoteModelUnreported},
			Answers: map[string]provider.Answer{
				"department": {
					QuestionID: "department",
					Primitive:  spec.PrimitiveChoice,
					Status:     provider.AnswerStatusAnswered,
					Choice:     strPtr("support"),
					// Documented Gemini choice probabilities summing to 0.999 in float64.
					Distribution: map[string]float64{
						"support": 0.99,
						"billing": 0.004,
						"sales":   0.003,
						"other":   0.002,
					},
					Notes: []string{provider.NoteOptionsSortedByProvider},
				},
			},
		}, nil
	})
	adapter.name = "fake-gemini"
	adapter.caps.AuthScheme = provider.AuthScheme{
		Kind:       provider.AuthKindHeader,
		HeaderName: "x-goog-api-key",
	}

	reg := provider.NewRegistry()
	reg.MustRegister(adapter)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	inst := runtime.Instance{
		ID:       "inst-gem-1",
		Name:     "gemini-primary",
		Adapter:  "fake-gemini",
		Model:    "gemini-decision-1",
		Endpoint: srv.URL,
		Auth: runtime.InstanceAuth{
			Kind:       "api_key",
			SecretName: "systemone.instance.inst-gem-1",
			// Even if Instance.Auth.HeaderName carries another string, placement
			// comes strictly from the adapter's declared Capabilities.AuthScheme.
			HeaderName: "X-Ignored-Instance-Header",
		},
		Enabled:  true,
		Revision: 1,
	}

	rt := runtime.New(
		runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(inst),
			Credentials: runtime.StaticCredential(canaryKey),
			Logger:      logger,
		},
		runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(srv.URL, srv.Client())),
	)
	defer rt.CloseIdleConnections()

	dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("broken screen"), runtime.EvalOptions{
		Consumer: "probe",
		RuleID:   "route-dept",
	})
	require.NoError(t, err)
	assert.Equal(t, "support", dec.String)

	// Wire placement assertions: secret ONLY in x-goog-api-key, never Authorization or X-Ignored-Instance-Header.
	assert.Equal(t, canaryKey, gotHeaders.Get("x-goog-api-key"))
	assert.Empty(t, gotHeaders.Get("Authorization"))
	assert.Empty(t, gotHeaders.Get("X-Ignored-Instance-Header"))
	for hdr, vals := range gotHeaders {
		if strings.EqualFold(hdr, "x-goog-api-key") {
			continue
		}
		for _, v := range vals {
			assert.NotContains(t, v, canaryKey, "header %s leaked secret", hdr)
		}
	}
	assert.NotContains(t, gotQuery, canaryKey)
	assert.NotContains(t, string(gotBody), canaryKey)

	// Provenance & float-boundary assertions (PV3 brief r1.2 & §5.3).
	eval := dec.Eval
	require.NotNil(t, eval)
	assert.Equal(t, "gemini-decision-1", eval.ConfiguredModel)
	assert.Empty(t, eval.ResolvedModel)
	assert.Empty(t, eval.EffectiveResolvedModel())
	assert.Equal(t, []string{provider.NoteModelUnreported}, eval.Notes, "mergeNotes must dedupe model_unreported to once")
	require.Len(t, eval.Attempts, 1)
	assert.Equal(t, "gemini-decision-1", eval.Attempts[0].ConfiguredModel)
	assert.Empty(t, eval.Attempts[0].ReportedModel)

	deptRec := eval.Answers["department"]
	assert.Equal(t, map[string]float64{
		"support": 0.99,
		"billing": 0.004,
		"sales":   0.003,
		"other":   0.002,
	}, deptRec.Distribution, "runtime must preserve exact probabilities without renormalization")
	assert.InDelta(t, 0.99, deptRec.Metrics[metrics.NamePTop].Value, 1e-12)
	assert.InDelta(t, 0.986, deptRec.Metrics[metrics.NameMargin].Value, 1e-12)

	evalJSON, err := json.Marshal(eval)
	require.NoError(t, err)
	assert.NotContains(t, string(evalJSON), canaryKey)
	assert.NotContains(t, logBuf.String(), canaryKey)
}

func TestRuntime_TransportCacheKeyIsolatesCredentialPlacement(t *testing.T) {
	const canaryKey = "sk-placement-cache-canary-9090"

	type wireAuthSnapshot struct {
		authorization string
		googKey       string
		altKey        string
	}
	var (
		mu        sync.Mutex
		wireCalls []wireAuthSnapshot
		seenTrs   []provider.Transport
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		wireCalls = append(wireCalls, wireAuthSnapshot{
			authorization: r.Header.Get("Authorization"),
			googKey:       r.Header.Get("x-goog-api-key"),
			altKey:        r.Header.Get("x-alt-api-key"),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	base := newFakeAdapter(nil)
	base.name = "dynamic-placement"
	dyn := &dynamicCapsAdapter{
		fakeAdapter: base,
		capsFn: func(inst provider.InstanceView) (provider.Capabilities, error) {
			caps := base.caps
			switch inst.Options["mode"] {
			case "goog":
				caps.AuthScheme = provider.AuthScheme{Kind: provider.AuthKindHeader, HeaderName: "x-goog-api-key"}
			case "alt":
				caps.AuthScheme = provider.AuthScheme{Kind: provider.AuthKindHeader, HeaderName: "x-alt-api-key"}
			default:
				caps.AuthScheme = provider.AuthBearer
			}
			return caps, nil
		},
	}

	reg := provider.NewRegistry()
	// Register a wrapper implementing both dynamic Capabilities and transport pointer capture.
	wrapper := &placementRecordingAdapter{
		dynamicCapsAdapter: dyn,
		onTransport: func(tr provider.Transport) {
			mu.Lock()
			seenTrs = append(seenTrs, tr)
			mu.Unlock()
		},
	}
	reg.MustRegister(wrapper)

	rt := runtime.New(
		runtime.Config{
			Registry:    reg,
			Credentials: runtime.StaticCredential(canaryKey),
		},
		runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(srv.URL, srv.Client())),
	)
	defer rt.CloseIdleConnections()

	// Identical ID, Name, Revision, Endpoint, and InstanceAuth across all calls;
	// ONLY the adapter-declared placement changes.
	makeInst := func(mode string) runtime.Instance {
		return runtime.Instance{
			ID:       "inst-shared-cache-id",
			Name:     "shared-name",
			Revision: 1,
			Adapter:  "dynamic-placement",
			Endpoint: srv.URL,
			Enabled:  true,
			Auth: runtime.InstanceAuth{
				Kind:       "api_key",
				SecretName: "systemone.instance.inst-shared-cache-id",
			},
			Options: map[string]string{"mode": mode},
		}
	}

	_, err := rt.ListModels(context.Background(), makeInst("bearer"))
	require.NoError(t, err)
	_, err = rt.ListModels(context.Background(), makeInst("bearer"))
	require.NoError(t, err)
	_, err = rt.ListModels(context.Background(), makeInst("goog"))
	require.NoError(t, err)
	_, err = rt.ListModels(context.Background(), makeInst("goog"))
	require.NoError(t, err)
	_, err = rt.ListModels(context.Background(), makeInst("alt"))
	require.NoError(t, err)

	mu.Lock()
	trs := append([]provider.Transport(nil), seenTrs...)
	calls := append([]wireAuthSnapshot(nil), wireCalls...)
	mu.Unlock()

	require.Len(t, trs, 5)
	assert.Same(t, trs[0], trs[1], "same placement must reuse cached transport")
	assert.NotSame(t, trs[0], trs[2], "switching bearer -> header:x-goog-api-key must yield distinct cached transport")
	assert.Same(t, trs[2], trs[3], "same header placement must reuse cached transport")
	assert.NotSame(t, trs[2], trs[4], "switching header:x-goog-api-key -> header:x-alt-api-key must yield distinct cached transport")
	assert.NotSame(t, trs[0], trs[4], "bearer and header:x-alt-api-key must not share transport")

	require.Equal(t, []wireAuthSnapshot{
		{authorization: "Bearer " + canaryKey},
		{authorization: "Bearer " + canaryKey},
		{googKey: canaryKey},
		{googKey: canaryKey},
		{altKey: canaryKey},
	}, calls)
}

type placementRecordingAdapter struct {
	*dynamicCapsAdapter
	onTransport func(provider.Transport)
}

func (a *placementRecordingAdapter) ListModels(ctx context.Context, tr provider.Transport, inst provider.InstanceView) ([]string, error) {
	if a.onTransport != nil {
		a.onTransport(tr)
	}
	return a.dynamicCapsAdapter.ListModels(ctx, tr, inst)
}

func TestRuntime_TypeSafeBearerAndDefaultEndpointRegression(t *testing.T) {
	const typeSafeSecret = "sk-typesafe-regression-canary-112233"

	type recordedWire struct {
		method string
		host   string
		path   string
		auth   string
		goog   string
	}
	var (
		mu   sync.Mutex
		reqs []recordedWire
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, recordedWire{
			method: r.Method,
			host:   r.Host,
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			goog:   r.Header.Get("x-goog-api-key"),
		})
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"models":[{"name":"jev-1.14.0","description":"Pinned release.","release_date":"2026-09-15"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"model":"jev-1.14.0","answers":{"q1":{"type":"noul","noul":0.95}},"usage":{"input_tokens":20,"output_tokens":2}}`))
	}))
	defer srv.Close()

	reg := provider.NewRegistry()
	require.NoError(t, jevwire.Register(reg))

	// Instance omits Endpoint ("") so TypeSafe resolves jevwire.DefaultTypeSafeEndpoint ("https://api.typesafe.ai").
	inst := runtime.Instance{
		ID:       "inst-ts-default",
		Name:     "typesafe-default",
		Adapter:  jevwire.AdapterNameTypeSafe,
		Model:    "jev-1.14.0",
		Endpoint: "",
		Auth: runtime.InstanceAuth{
			Kind:       "api_key",
			SecretName: "systemone.instance.inst-ts-default",
		},
		Enabled:  true,
		Revision: 1,
	}

	rt := runtime.New(
		runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(inst),
			Credentials: runtime.StaticCredential(typeSafeSecret),
		},
		runtime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(staticPublicResolver{}, hostRoutingDialer{
				targets: map[string]string{"443": srv.Listener.Addr().String()},
			}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)
	defer rt.CloseIdleConnections()

	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("check default endpoint"), runtime.EvalOptions{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)

	models, err := rt.ListModels(context.Background(), inst)
	require.NoError(t, err)
	assert.Equal(t, []string{"jev-1.14.0"}, models)

	mu.Lock()
	gotReqs := append([]recordedWire(nil), reqs...)
	mu.Unlock()

	require.Equal(t, []recordedWire{
		{
			method: http.MethodPost,
			host:   "api.typesafe.ai",
			path:   "/v1/systemone",
			auth:   "Bearer " + typeSafeSecret,
			goog:   "",
		},
		{
			method: http.MethodGet,
			host:   "api.typesafe.ai",
			path:   "/v1/models",
			auth:   "Bearer " + typeSafeSecret,
			goog:   "",
		},
	}, gotReqs)
}

func TestRuntime_SupportedAuthSelectionMatrixAndTokenSourceBranching(t *testing.T) {
	const (
		canaryAPIKey = "AIzaSy-fake-key-CANARY-matrix"
		canaryToken  = "fake-adc-token-CANARY-matrix"
		testScope    = "https://www.googleapis.com/auth/synthetic.test"
	)

	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	type wireCall struct {
		authHeader   string
		googKey      string
		quotaProject string
	}

	var (
		mu        sync.Mutex
		wireCalls []wireCall
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		wireCalls = append(wireCalls, wireCall{
			authHeader:   r.Header.Get("Authorization"),
			googKey:      r.Header.Get("x-goog-api-key"),
			quotaProject: r.Header.Get("x-goog-user-project"),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	adapter := newFakeAdapter(func(ctx context.Context, tr provider.Transport, inst provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, inst.Endpoint+"/v1beta/decisions", strings.NewReader(`{}`))
		if err != nil {
			return provider.ModelResult{}, err
		}
		resp, err := tr.Do(req)
		if err != nil {
			return provider.ModelResult{}, err
		}
		_ = resp.Body.Close()
		dist, val, _, _ := provider.BinaryDistribution(0.9)
		return provider.ModelResult{
			HTTPStatus: resp.StatusCode,
			Answers: map[string]provider.Answer{
				"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
			},
		}, nil
	})
	adapter.name = "multi-auth-gemini"
	adapter.caps.AuthScheme = provider.AuthScheme{Kind: provider.AuthKindHeader, HeaderName: "x-goog-api-key"}
	adapter.caps.SupportedAuth = map[string]provider.AuthScheme{
		"api_key": {
			Kind:       provider.AuthKindHeader,
			HeaderName: "x-goog-api-key",
		},
		"google_access_token": {
			Kind: provider.AuthKindGoogleAccessToken,
		},
	}

	reg := provider.NewRegistry()
	reg.MustRegister(adapter)

	var (
		credCalls  int
		tokenCalls int
		gotScopes  []string
	)
	rt := runtime.New(
		runtime.Config{
			Registry: reg,
			Credentials: func(_ context.Context, inst runtime.Instance) (string, error) {
				credCalls++
				return canaryAPIKey, nil
			},
			TokenSource: func(_ context.Context, inst runtime.Instance) (string, error) {
				tokenCalls++
				gotScopes = append([]string(nil), inst.Auth.Scopes...)
				return canaryToken, nil
			},
		},
		runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(srv.URL, srv.Client())),
	)
	defer rt.CloseIdleConnections()

	resetCounters := func() {
		mu.Lock()
		wireCalls = nil
		mu.Unlock()
		credCalls = 0
		tokenCalls = 0
		gotScopes = nil
	}

	t.Run("api_key_selects_header_and_never_sends_quota_project", func(t *testing.T) {
		resetCounters()
		inst := runtime.Instance{
			ID:       "inst-1",
			Name:     "gem-key",
			Revision: 1,
			Adapter:  "multi-auth-gemini",
			Model:    "gemini-decision-1",
			Endpoint: srv.URL,
			Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-1"},
			Options:  map[string]string{provider.OptionQuotaProject: "ignored-on-api-key"},
			Enabled:  true,
		}
		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{Instance: &inst})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, 1, credCalls)
		assert.Equal(t, 0, tokenCalls)
		require.Len(t, wireCalls, 1)
		assert.Equal(t, canaryAPIKey, wireCalls[0].googKey)
		assert.Empty(t, wireCalls[0].authHeader)
		assert.Empty(t, wireCalls[0].quotaProject, "quota_project must never be sent on api_key requests")
	})

	t.Run("google_access_token_with_quota_project_selects_bearer_and_explicit_provenance", func(t *testing.T) {
		resetCounters()
		inst := runtime.Instance{
			ID:       "inst-2",
			Name:     "gem-adc-qp",
			Revision: 1,
			Adapter:  "multi-auth-gemini",
			Model:    "gemini-decision-1",
			Endpoint: srv.URL,
			Auth:     runtime.InstanceAuth{Kind: "google_access_token", Scopes: []string{testScope}},
			Options:  map[string]string{provider.OptionQuotaProject: "paid-gemini-proj-42"},
			Enabled:  true,
		}
		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{Instance: &inst})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, 0, credCalls)
		assert.Equal(t, 1, tokenCalls)
		assert.Equal(t, []string{testScope}, gotScopes)
		require.NotNil(t, dec.Eval.Auth)
		assert.Equal(t, "explicit: paid-gemini-proj-42", dec.Eval.Auth.QuotaProject)
		require.Len(t, wireCalls, 1)
		assert.Equal(t, "Bearer "+canaryToken, wireCalls[0].authHeader)
		assert.Equal(t, "paid-gemini-proj-42", wireCalls[0].quotaProject)
		assert.Empty(t, wireCalls[0].googKey)
	})

	t.Run("google_access_token_without_quota_project_omits_header_and_records_implicit", func(t *testing.T) {
		resetCounters()
		inst := runtime.Instance{
			ID:       "inst-3",
			Name:     "gem-adc-no-qp",
			Revision: 1,
			Adapter:  "multi-auth-gemini",
			Model:    "gemini-decision-1",
			Endpoint: srv.URL,
			Auth:     runtime.InstanceAuth{Kind: "google_access_token", Scopes: []string{testScope}},
			Enabled:  true,
		}
		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{Instance: &inst})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, 0, credCalls)
		assert.Equal(t, 1, tokenCalls)
		require.NotNil(t, dec.Eval.Auth)
		assert.Equal(t, provider.QuotaProjectImplicit, dec.Eval.Auth.QuotaProject)
		require.Len(t, wireCalls, 1)
		assert.Equal(t, "Bearer "+canaryToken, wireCalls[0].authHeader)
		assert.Empty(t, wireCalls[0].quotaProject)
		assert.Empty(t, wireCalls[0].googKey)
	})

	t.Run("google_access_token_without_scopes_fails_pre_call_invalid_request", func(t *testing.T) {
		resetCounters()
		inst := runtime.Instance{
			ID:       "inst-no-scopes",
			Name:     "gem-adc-no-scopes",
			Revision: 1,
			Adapter:  "multi-auth-gemini",
			Model:    "gemini-decision-1",
			Endpoint: srv.URL,
			Auth:     runtime.InstanceAuth{Kind: "google_access_token"},
			Enabled:  true,
		}
		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{Instance: &inst})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInvalidRequest), eval.Reason)
		assert.Empty(t, eval.Attempts)
		assert.Empty(t, wireCalls)
	})

	for _, badKind := range []string{"bearer", "none", "google_id_token", "azure_entra", "custom_kind"} {
		t.Run("unsupported_kind_"+badKind, func(t *testing.T) {
			resetCounters()
			inst := runtime.Instance{
				ID:       "inst-bad-" + badKind,
				Name:     "gem-bad",
				Revision: 1,
				Adapter:  "multi-auth-gemini",
				Model:    "gemini-decision-1",
				Endpoint: srv.URL,
				Auth:     runtime.InstanceAuth{Kind: badKind},
				Enabled:  true,
			}
			_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{Instance: &inst})
			require.Error(t, err)
			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok)
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailUnsupported), eval.Reason)
			assert.Empty(t, eval.Attempts, "pre-call unsupported auth must make 0 attempts")
			assert.Empty(t, wireCalls, "must make 0 provider HTTP requests")
			assert.Equal(t, 0, credCalls)
			assert.Equal(t, 0, tokenCalls)
		})
	}

	t.Run("invalid_quota_project_fails_pre_call_invalid_request", func(t *testing.T) {
		resetCounters()
		inst := runtime.Instance{
			ID:       "inst-bad-qp",
			Name:     "gem-bad-qp",
			Revision: 1,
			Adapter:  "multi-auth-gemini",
			Model:    "gemini-decision-1",
			Endpoint: srv.URL,
			Auth:     runtime.InstanceAuth{Kind: "google_access_token", Scopes: []string{testScope}},
			Options:  map[string]string{provider.OptionQuotaProject: "INVALID_PROJECT!"},
			Enabled:  true,
		}
		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{Instance: &inst})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInvalidRequest), eval.Reason)
		assert.Empty(t, eval.Attempts)
		assert.Empty(t, wireCalls)
	})
}

func TestRuntime_TransportCacheKeyIncludesQuotaProjectAndExcludesToken(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	type wireSnap struct {
		auth string
		qp   string
	}
	var (
		mu      sync.Mutex
		wires   []wireSnap
		seenTrs []provider.Transport
		tokSeq  int
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		wires = append(wires, wireSnap{
			auth: r.Header.Get("Authorization"),
			qp:   r.Header.Get("X-Goog-User-Project"),
		})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	base := newFakeAdapter(func(ctx context.Context, tr provider.Transport, inst provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		mu.Lock()
		seenTrs = append(seenTrs, tr)
		mu.Unlock()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, inst.Endpoint+"/v1beta/decisions", strings.NewReader(`{}`))
		if err != nil {
			return provider.ModelResult{}, err
		}
		resp, err := tr.Do(req)
		if err != nil {
			return provider.ModelResult{}, err
		}
		_ = resp.Body.Close()
		dist, val, _, _ := provider.BinaryDistribution(0.9)
		return provider.ModelResult{
			HTTPStatus: resp.StatusCode,
			Answers: map[string]provider.Answer{
				"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
			},
		}, nil
	})
	base.name = "adc-cache-test"
	base.caps.SupportedAuth = map[string]provider.AuthScheme{
		"google_access_token": {
			Kind: provider.AuthKindGoogleAccessToken,
		},
	}

	reg := provider.NewRegistry()
	reg.MustRegister(base)

	rt := runtime.New(
		runtime.Config{
			Registry: reg,
			TokenSource: func(_ context.Context, _ runtime.Instance) (string, error) {
				mu.Lock()
				tokSeq++
				tok := "fake-adc-token-CANARY-" + string(rune('0'+tokSeq))
				mu.Unlock()
				return tok, nil
			},
		},
		runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(srv.URL, srv.Client())),
	)
	defer rt.CloseIdleConnections()

	makeInst := func(qp string) runtime.Instance {
		var opts map[string]string
		if qp != "" {
			opts = map[string]string{provider.OptionQuotaProject: qp}
		}
		return runtime.Instance{
			ID:       "inst-adc-cache",
			Name:     "gem-adc",
			Revision: 1,
			Adapter:  "adc-cache-test",
			Model:    "gemini-decision-1",
			Endpoint: srv.URL,
			Auth: runtime.InstanceAuth{
				Kind:   "google_access_token",
				Scopes: []string{"https://www.googleapis.com/auth/synthetic.test"},
			},
			Options: opts,
			Enabled: true,
		}
	}

	// Call 1 & Call 2: same quotaProject, rotating token -> MUST reuse identical provider.Transport pointer while sending rotated token!
	_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("one"), runtime.EvalOptions{Instance: ptr(makeInst("project-alpha-1"))})
	require.NoError(t, err)
	_, err = rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("two"), runtime.EvalOptions{Instance: ptr(makeInst("project-alpha-1"))})
	require.NoError(t, err)

	// Call 3: different quotaProject -> MUST allocate distinct provider.Transport!
	_, err = rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("three"), runtime.EvalOptions{Instance: ptr(makeInst("project-bravo-2"))})
	require.NoError(t, err)

	// Call 4: empty quotaProject -> MUST allocate distinct provider.Transport with no header!
	_, err = rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("four"), runtime.EvalOptions{Instance: ptr(makeInst(""))})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seenTrs, 4)
	assert.Same(t, seenTrs[0], seenTrs[1], "rotating access token must NOT bust transport cache")
	assert.NotSame(t, seenTrs[0], seenTrs[2], "different quota_project must isolate transport cache")
	assert.NotSame(t, seenTrs[0], seenTrs[3], "empty vs non-empty quota_project must isolate transport cache")
	assert.NotSame(t, seenTrs[2], seenTrs[3])

	require.Equal(t, []wireSnap{
		{auth: "Bearer fake-adc-token-CANARY-1", qp: "project-alpha-1"},
		{auth: "Bearer fake-adc-token-CANARY-2", qp: "project-alpha-1"},
		{auth: "Bearer fake-adc-token-CANARY-3", qp: "project-bravo-2"},
		{auth: "Bearer fake-adc-token-CANARY-4", qp: ""},
	}, wires)
}

func ptr[T any](v T) *T { return &v }
