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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/geminidecisions"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRuntime_GeminiDecisionsAdapter_ProvenanceAndHeaderPlacementAcrossOutcomes
// exercises the real geminidecisions.Adapter through runtime.Runtime and
// pkg/systemone/egress over TLS, pinning PV3 design-authority decisions 2, 3,
// 5, and 6 across Success, Uncertain, and Failed outcomes:
//   - Secret appears only in x-goog-api-key, never in Authorization, query,
//     body, Evaluation JSON, error strings, or logs.
//   - ConfiguredModel == inst.Model; ResolvedModel == ""; Attempt.ReportedModel == "";
//     Notes contains model_unreported exactly once; EffectiveResolvedModel() == "".
//   - The same provenance fields hold on Uncertain and Failed via EvaluationError.
//   - Missing usage adds usage_unreported alongside model_unreported.
//   - Documented {0.99, 0.004, 0.003, 0.002} choice example (sum 0.999) passes
//     through both adapter and runtime without renormalization.
func TestRuntime_GeminiDecisionsAdapter_ProvenanceAndHeaderPlacementAcrossOutcomes(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): ai.google.dev/gemini-api/docs/decisions-api & api/decisions.
	const canaryKey = "AIzaSy-runtime-gemini-canary-secret-445566"

	mixedSpec := `{
		"version": 1,
		"form": "portable",
		"questions": [
			{
				"id": "is_damaged",
				"type": "binary",
				"instructions": "Does ` + "`probe.text`" + ` describe physical damage?"
			},
			{
				"id": "department",
				"type": "choice",
				"instructions": "Route ` + "`probe.text`" + ` to a department.",
				"options": [
					{"id": "billing", "description": "Billing inquiries"},
					{"id": "sales", "description": "Sales inquiries"},
					{"id": "support", "description": "Technical support"},
					{"id": "other", "description": "Cannot determine", "abstain": true}
				]
			}
		]
	}`
	compiled := compileTestSpec(t, mixedSpec, spec.KindJudgments)

	var (
		mu         sync.Mutex
		nextStatus int
		nextBody   string
		lastMethod string
		lastPath   string
		lastQuery  string
		lastHdrs   http.Header
		lastBody   []byte
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		mu.Lock()
		lastMethod = r.Method
		lastPath = r.URL.Path
		lastQuery = r.URL.RawQuery
		lastHdrs = r.Header.Clone()
		lastBody = raw
		st := nextStatus
		body := nextBody
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	setMock := func(status int, body string) {
		mu.Lock()
		nextStatus = status
		nextBody = body
		mu.Unlock()
	}

	assertWireIsolation := func(t *testing.T) {
		t.Helper()
		mu.Lock()
		method := lastMethod
		path := lastPath
		query := lastQuery
		hdrs := lastHdrs.Clone()
		body := append([]byte(nil), lastBody...)
		mu.Unlock()

		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1beta/decisions", path)
		assert.Empty(t, query, "Gemini adapter must never send ?key= or any URL query")
		assert.Equal(t, canaryKey, hdrs.Get("x-goog-api-key"))
		assert.Empty(t, hdrs.Get("Authorization"))
		assert.Empty(t, hdrs.Get("X-Admin-Ignored-Header"))
		for name, vals := range hdrs {
			if strings.EqualFold(name, "x-goog-api-key") {
				continue
			}
			for _, v := range vals {
				assert.NotContains(t, v, canaryKey, "credential leaked into header %s", name)
			}
		}
		assert.NotContains(t, string(body), canaryKey)
	}

	assertDecision3Provenance := func(t *testing.T, eval *runtime.Evaluation, logBuf *bytes.Buffer) {
		t.Helper()
		require.NotNil(t, eval)
		assert.Equal(t, "gemini-decision-1", eval.ConfiguredModel)
		assert.Empty(t, eval.ResolvedModel)
		assert.Empty(t, eval.EffectiveResolvedModel())
		assert.False(t, eval.HasModelAliasReported())

		countModelUnreported := 0
		for _, n := range eval.Notes {
			if n == provider.NoteModelUnreported {
				countModelUnreported++
			}
		}
		assert.Equal(t, 1, countModelUnreported, "Evaluation.Notes must contain model_unreported exactly once, got %v", eval.Notes)

		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, "gemini_decisions", eval.Attempts[0].Adapter)
		assert.Equal(t, "0.1.0", eval.Attempts[0].AdapterVersion)
		assert.Equal(t, "gemini-decision-1", eval.Attempts[0].ConfiguredModel)
		assert.Empty(t, eval.Attempts[0].ReportedModel)

		rawEval, err := json.Marshal(eval)
		require.NoError(t, err)
		assert.NotContains(t, string(rawEval), canaryKey)
		assert.NotContains(t, logBuf.String(), canaryKey)
	}

	reg := provider.NewRegistry()
	require.NoError(t, geminidecisions.Register(reg))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	inst := runtime.Instance{
		ID:       "inst-gemini-rt-1",
		Name:     "gemini-main",
		Adapter:  geminidecisions.AdapterName,
		Model:    "gemini-decision-1",
		Endpoint: srv.URL,
		Auth: runtime.InstanceAuth{
			Kind:       "api_key",
			SecretName: "systemone.instance.inst-gemini-rt-1",
			HeaderName: "X-Admin-Ignored-Header",
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

	t.Run("success_documented_0999_choice_and_provenance", func(t *testing.T) {
		setMock(http.StatusOK, `{
			"answers": {
				"is_damaged": {"type": "noul", "noul": 0.99, "confidence": 0.99},
				"department": {
					"type": "choice",
					"choice": "support",
					"probabilities": {"billing": 0.004, "other": 0.002, "sales": 0.003, "support": 0.99},
					"confidence": 0.99
				}
			},
			"usage": {"total_input_tokens": 58, "total_output_tokens": 0}
		}`)

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("The screen arrived cracked."), runtime.EvalOptions{
			Consumer: "probe",
			RuleID:   "gemini-mixed",
			Revision: 1,
		})
		require.NoError(t, err)
		assertWireIsolation(t)
		assertDecision3Provenance(t, dec.Eval, &logBuf)
		assert.Equal(t, []string{provider.NoteModelUnreported}, dec.Eval.Notes)
		require.NotNil(t, dec.Eval.Usage)
		assert.Equal(t, provider.Usage{InputTokens: 58, OutputTokens: 0}, *dec.Eval.Usage)
		assert.False(t, dec.Eval.HasUsageUnreported())

		binRec := dec.Eval.Answers["is_damaged"]
		require.NotNil(t, binRec.Bool)
		assert.True(t, *binRec.Bool)
		assert.Equal(t, []string{provider.NoteClamped001To099}, binRec.Notes)
		assert.InDelta(t, 0.99, binRec.NativeMetrics["gemini.confidence"].Value, 1e-12)

		choiceRec := dec.Eval.Answers["department"]
		require.NotNil(t, choiceRec.Choice)
		assert.Equal(t, "support", *choiceRec.Choice)
		assert.Equal(t, []string{provider.NoteOptionsSortedByProvider}, choiceRec.Notes)
		assert.Equal(t, map[string]float64{
			"billing": 0.004,
			"other":   0.002,
			"sales":   0.003,
			"support": 0.99,
		}, choiceRec.Distribution)
		assert.InDelta(t, 0.99, choiceRec.Metrics[metrics.NamePTop].Value, 1e-12)
		assert.InDelta(t, 0.99, choiceRec.NativeMetrics["gemini.confidence"].Value, 1e-12)
	})

	t.Run("success_missing_usage_records_usage_unreported", func(t *testing.T) {
		setMock(http.StatusOK, `{
			"answers": {
				"is_damaged": {"type": "noul", "noul": 0.12, "confidence": 0.88},
				"department": {
					"type": "choice",
					"choice": "billing",
					"probabilities": {"billing": 0.90, "other": 0.02, "sales": 0.05, "support": 0.03},
					"confidence": 0.90
				}
			}
		}`)

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("Question about invoice #7."), runtime.EvalOptions{
			Consumer: "probe",
			RuleID:   "gemini-missing-usage",
		})
		require.NoError(t, err)
		assertWireIsolation(t)
		assertDecision3Provenance(t, dec.Eval, &logBuf)
		assert.ElementsMatch(t, []string{provider.NoteModelUnreported, provider.NoteUsageUnreported}, dec.Eval.Notes)
		assert.True(t, dec.Eval.HasUsageUnreported())
		assert.Nil(t, dec.Eval.Usage, "missing usage must never present SPI zero tokens as measured usage")
		rawMissingJSON, err := json.Marshal(dec.Eval)
		require.NoError(t, err)
		assert.NotContains(t, string(rawMissingJSON), `"usage"`)
	})

	t.Run("success_string_typed_usage_records_usage_unreported", func(t *testing.T) {
		setMock(http.StatusOK, `{
			"answers": {
				"is_damaged": {"type": "noul", "noul": 0.99, "confidence": 0.99},
				"department": {
					"type": "choice",
					"choice": "support",
					"probabilities": {"billing": 0.004, "other": 0.002, "sales": 0.003, "support": 0.99},
					"confidence": 0.99
				}
			},
			"usage": {"total_input_tokens": "64", "total_output_tokens": "0"}
		}`)

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("Screen cracked."), runtime.EvalOptions{
			Consumer: "probe",
			RuleID:   "gemini-string-usage",
		})
		require.NoError(t, err)
		assertWireIsolation(t)
		assertDecision3Provenance(t, dec.Eval, &logBuf)
		assert.ElementsMatch(t, []string{provider.NoteModelUnreported, provider.NoteUsageUnreported}, dec.Eval.Notes)
		assert.True(t, dec.Eval.HasUsageUnreported())
		assert.Nil(t, dec.Eval.Usage, "unparseable usage must never present SPI zero tokens as measured usage")
	})

	t.Run("uncertain_tie_carries_decision3_provenance_in_evaluation_error", func(t *testing.T) {
		setMock(http.StatusOK, `{
			"answers": {
				"is_damaged": {"type": "noul", "noul": 0.20, "confidence": 0.80},
				"department": {
					"type": "choice",
					"choice": "billing",
					"probabilities": {"billing": 0.45, "other": 0.05, "sales": 0.45, "support": 0.05},
					"confidence": 0.45
				}
			},
			"usage": {"total_input_tokens": 44, "total_output_tokens": 0}
		}`)

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("Pricing or quote question."), runtime.EvalOptions{
			Consumer: "probe",
			RuleID:   "gemini-tie",
		})
		require.Error(t, err)
		assertWireIsolation(t)

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainTie, eval.Reason)
		assertDecision3Provenance(t, eval, &logBuf)
	})

	t.Run("uncertain_abstain_carries_decision3_provenance_in_evaluation_error", func(t *testing.T) {
		setMock(http.StatusOK, `{
			"answers": {
				"is_damaged": {"type": "noul", "noul": 0.10, "confidence": 0.90},
				"department": {
					"type": "choice",
					"choice": "other",
					"probabilities": {"billing": 0.05, "other": 0.85, "sales": 0.05, "support": 0.05},
					"confidence": 0.85
				}
			},
			"usage": {"total_input_tokens": 40, "total_output_tokens": 0}
		}`)

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("Hello there."), runtime.EvalOptions{
			Consumer: "probe",
			RuleID:   "gemini-abstain",
		})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainAbstained, eval.Reason)
		assertDecision3Provenance(t, eval, &logBuf)
	})

	t.Run("failed_400_and_200_safety_blocks_and_malformed_carry_decision3_provenance", func(t *testing.T) {
		failCases := []struct {
			name       string
			status     int
			body       string
			wantReason string
			wantClass  string
		}{
			{
				name:       "safety_block_400_guide_form",
				status:     http.StatusBadRequest,
				body:       `{"error":{"code":"safety","message":"CANARY_ERR_SAFETY_400"}}`,
				wantReason: string(provider.FailContentBlocked),
				wantClass:  "4xx",
			},
			{
				name:       "safety_block_200_reference_form",
				status:     http.StatusOK,
				body:       `{"answers":{},"prompt_feedback":{"block_reason":"blocklist","block_reason_message":"CANARY_ERR_BLOCKLIST_200"}}`,
				wantReason: string(provider.FailContentBlocked),
				wantClass:  "2xx",
			},
			{
				name:       "malformed_200_sum_098",
				status:     http.StatusOK,
				body:       `{"answers":{"is_damaged":{"type":"noul","noul":0.9,"confidence":0.9},"department":{"type":"choice","choice":"support","confidence":0.9,"probabilities":{"billing":0.04,"other":0.02,"sales":0.02,"support":0.90}}},"usage":{"total_input_tokens":30}}`,
				wantReason: string(provider.FailMalformedResponse),
				wantClass:  "2xx",
			},
			{
				name:       "auth_403_unrestricted_key",
				status:     http.StatusForbidden,
				body:       `{"error":{"code":403,"message":"CANARY_ERR_403_KEY","status":"PERMISSION_DENIED"}}`,
				wantReason: string(provider.FailAuth),
				wantClass:  "4xx",
			},
		}

		for _, fc := range failCases {
			t.Run(fc.name, func(t *testing.T) {
				setMock(fc.status, fc.body)
				_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("test input"), runtime.EvalOptions{
					Consumer: "probe",
					RuleID:   "gemini-fail-" + fc.name,
				})
				require.Error(t, err)
				assert.NotContains(t, err.Error(), "CANARY_ERR_")
				assert.NotContains(t, err.Error(), canaryKey)
				assertWireIsolation(t)

				eval, ok := runtime.EvaluationFrom(err)
				require.True(t, ok)
				assert.Equal(t, runtime.ClassFailed, eval.Class)
				assert.Equal(t, fc.wantReason, eval.Reason)
				assert.Equal(t, fc.wantClass, eval.Attempts[0].HTTPStatusClass)
				assertDecision3Provenance(t, eval, &logBuf)
			})
		}
	})

	t.Run("list_models_returns_unsupported_without_network", func(t *testing.T) {
		_, err := rt.ListModels(context.Background(), inst)
		require.Error(t, err)
		pe, ok := provider.AsProviderError(err)
		require.True(t, ok)
		assert.Equal(t, provider.FailUnsupported, pe.Reason)
	})

	t.Run("default_endpoint_resolves_generativelanguage_googleapis_com", func(t *testing.T) {
		var gotHost, gotPath, gotKey, gotAuth string
		defSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHost = r.Host
			gotPath = r.URL.Path
			gotKey = r.Header.Get("x-goog-api-key")
			gotAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{
				"answers": {
					"is_damaged": {"type": "noul", "noul": 0.99, "confidence": 0.99},
					"department": {"type": "choice", "choice": "support", "confidence": 0.99, "probabilities": {"billing": 0.004, "other": 0.002, "sales": 0.003, "support": 0.99}}
				},
				"usage": {"total_input_tokens": 22, "total_output_tokens": 0}
			}`)
		}))
		defer defSrv.Close()

		defInst := inst
		defInst.Endpoint = ""
		defRt := runtime.New(
			runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(defInst),
				Credentials: runtime.StaticCredential(canaryKey),
			},
			runtime.WithEgressOptionsForTesting(
				egress.WithDialerForTesting(staticPublicResolver{}, hostRoutingDialer{
					targets: map[string]string{"443": defSrv.Listener.Addr().String()},
				}),
				egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
			),
		)
		defer defRt.CloseIdleConnections()

		dec, err := defRt.Evaluate(context.Background(), compiled, runtime.ProbeInput("check default endpoint"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.Equal(t, "generativelanguage.googleapis.com", gotHost)
		assert.Equal(t, "/v1beta/decisions", gotPath)
		assert.Equal(t, canaryKey, gotKey)
		assert.Empty(t, gotAuth)
		assertDecision3Provenance(t, dec.Eval, &logBuf)
	})
}

func TestRuntime_GeminiDecisionsAdapter_ADCBearerErrorMappingAndCanaryRedaction(t *testing.T) {
	const (
		canaryToken  = "fake-adc-token-CANARY-gemini-rt"
		quotaProject = "paid-gemini-proj-99"
	)

	binSpec := `{
		"version": 1,
		"form": "portable",
		"questions": [
			{"id": "is_damaged", "type": "binary", "instructions": "Does ` + "`probe.text`" + ` describe damage?"}
		],
		"output": {"question": "is_damaged"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	var (
		mu       sync.Mutex
		hits     int
		nextCode int
		nextBody string
		lastHdrs http.Header
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		mu.Lock()
		hits++
		lastHdrs = r.Header.Clone()
		code := nextCode
		body := nextBody
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	reg := provider.NewRegistry()
	require.NoError(t, geminidecisions.Register(reg))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var tokenFn func(ctx context.Context, inst runtime.Instance) (string, error)
	inst := runtime.Instance{
		ID:       "inst-gemini-adc-1",
		Name:     "gemini-adc",
		Adapter:  geminidecisions.AdapterName,
		Model:    "gemini-decision-1",
		Endpoint: srv.URL,
		Auth: runtime.InstanceAuth{
			Kind:   "google_access_token",
			Scopes: []string{provider.GoogleScopeCloudPlatform},
		},
		Options:  map[string]string{provider.OptionQuotaProject: quotaProject},
		Enabled:  true,
		Revision: 1,
	}

	rt := runtime.New(
		runtime.Config{
			Registry:  reg,
			Instances: runtime.StaticInstances(inst),
			TokenSource: func(ctx context.Context, inst runtime.Instance) (string, error) {
				return tokenFn(ctx, inst)
			},
			Logger: logger,
		},
		runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(srv.URL, srv.Client())),
	)
	defer rt.CloseIdleConnections()

	assertNoCanaryLeak := func(t *testing.T, eval *runtime.Evaluation, evalErr error) {
		t.Helper()
		if evalErr != nil {
			assert.NotContains(t, evalErr.Error(), canaryToken)
		}
		if eval != nil {
			raw, mErr := json.Marshal(eval)
			require.NoError(t, mErr)
			assert.NotContains(t, string(raw), canaryToken)
		}
		assert.NotContains(t, logBuf.String(), canaryToken)
	}

	t.Run("success_adc_bearer_and_quota_project_wire_headers", func(t *testing.T) {
		mu.Lock()
		hits = 0
		nextCode = http.StatusOK
		nextBody = `{"answers":{"is_damaged":{"type":"noul","noul":0.99,"confidence":0.99}},"usage":{"total_input_tokens":40,"total_output_tokens":0}}`
		mu.Unlock()
		tokenFn = func(context.Context, runtime.Instance) (string, error) {
			return canaryToken, nil
		}

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("cracked glass"), runtime.EvalOptions{Consumer: "probe"})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		require.NotNil(t, dec.Eval)
		require.Len(t, dec.Eval.Attempts, 1)
		assert.Equal(t, 1, dec.Eval.Attempts[0].Requests)
		assert.Equal(t, "gemini-decision-1", dec.Eval.ConfiguredModel)
		assert.Empty(t, dec.Eval.ResolvedModel)
		assert.True(t, dec.Eval.HasModelUnreported())

		mu.Lock()
		gotHits := hits
		hdrs := lastHdrs.Clone()
		mu.Unlock()
		assert.Equal(t, 1, gotHits)
		assert.Equal(t, "Bearer "+canaryToken, hdrs.Get("Authorization"))
		assert.Equal(t, quotaProject, hdrs.Get("X-Goog-User-Project"))
		assert.Empty(t, hdrs.Get("x-goog-api-key"))
		for name, vals := range hdrs {
			if strings.EqualFold(name, "Authorization") {
				continue
			}
			for _, v := range vals {
				assert.NotContains(t, v, canaryToken)
			}
		}
		assertNoCanaryLeak(t, dec.Eval, nil)
	})

	t.Run("pre_call_token_auth_error_records_zero_requests", func(t *testing.T) {
		mu.Lock()
		hits = 0
		mu.Unlock()
		tokenFn = func(context.Context, runtime.Instance) (string, error) {
			return "", egress.ErrAuth
		}

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("cracked glass"), runtime.EvalOptions{Consumer: "probe"})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailAuth), eval.Reason)
		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, 0, eval.Attempts[0].Requests, "pre-call auth failure must record requests=0")
		assert.Empty(t, eval.Attempts[0].HTTPStatusClass)

		mu.Lock()
		gotHits := hits
		mu.Unlock()
		assert.Equal(t, 0, gotHits, "provider must receive 0 requests on pre-call credential failure")
		assertNoCanaryLeak(t, eval, err)
	})

	t.Run("pre_call_token_unavailable_error_records_zero_requests_and_scrubs_canary", func(t *testing.T) {
		mu.Lock()
		hits = 0
		mu.Unlock()
		tokenFn = func(context.Context, runtime.Instance) (string, error) {
			return "", io.ErrUnexpectedEOF
		}

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("cracked glass"), runtime.EvalOptions{Consumer: "probe"})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailUnavailable), eval.Reason)
		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, 0, eval.Attempts[0].Requests, "pre-call token source outage must record requests=0")

		mu.Lock()
		gotHits := hits
		mu.Unlock()
		assert.Equal(t, 0, gotHits)
		assertNoCanaryLeak(t, eval, err)
	})

	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run("provider_"+http.StatusText(statusCode)+"_maps_to_auth_with_one_request", func(t *testing.T) {
			mu.Lock()
			hits = 0
			nextCode = statusCode
			nextBody = `{"error":{"code":403,"message":"API not enabled on quota project; token ` + canaryToken + `","status":"PERMISSION_DENIED"}}`
			mu.Unlock()
			tokenFn = func(context.Context, runtime.Instance) (string, error) {
				return canaryToken, nil
			}

			_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("cracked glass"), runtime.EvalOptions{Consumer: "probe"})
			require.Error(t, err)
			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok)
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailAuth), eval.Reason)
			require.Len(t, eval.Attempts, 1)
			assert.Equal(t, 1, eval.Attempts[0].Requests, "provider 401/403 must record requests=1")
			assert.Equal(t, "4xx", eval.Attempts[0].HTTPStatusClass)

			mu.Lock()
			gotHits := hits
			mu.Unlock()
			assert.Equal(t, 1, gotHits)
			assertNoCanaryLeak(t, eval, err)
		})
	}
}
