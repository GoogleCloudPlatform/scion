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

package jevwire_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultRegistry_RegistersTypeSafeAdapterAndModelLister(t *testing.T) {
	got, ok := provider.Lookup(jevwire.AdapterNameTypeSafe)
	require.True(t, ok, "typesafe must register in provider.DefaultRegistry()")
	assert.Equal(t, "typesafe", got.Name())
	assert.NotEmpty(t, got.Version())

	_, isLister := got.(provider.ModelLister)
	assert.True(t, isLister, "typesafe profile must implement optional provider.ModelLister")

	caps, err := got.Capabilities(provider.InstanceView{Name: "jev-prod", Model: "jev-1.14.0"})
	require.NoError(t, err)
	assert.True(t, caps.SideEffectFree)
	assert.True(t, caps.ReturnsDistribution)
	assert.False(t, caps.Abstains)
	assert.Equal(t, provider.TruncationNever, caps.Truncation)
	assert.Equal(t, 64, caps.MaxQuestionsPerRequest)
	assert.Equal(t, 32768, caps.MaxInputTokens)
	assert.Equal(t, provider.AuthBearer, caps.AuthScheme)
	assert.Equal(t, jevwire.TypeSafeProviderRetention, caps.ProviderRetention)
	require.Len(t, caps.NativeMetrics, 1)
	assert.Equal(t, provider.MetricDecl{
		Name:        "typesafe.confidence",
		Unit:        metrics.UnitFraction,
		Min:         0,
		Max:         1,
		HigherMeans: "more_concentrated",
		Source:      metrics.SourceProviderDoc,
	}, caps.NativeMetrics[0])
}

func TestValidate_AcceptsPortableBinaryAndChoiceAndRefusesUnsupported(t *testing.T) {
	adapter := jevwire.NewTypeSafe()
	inst := provider.InstanceView{Name: "jev-prod", Model: "jev-1.14.0"}

	validRule := spec.Spec{
		Version: 1,
		Questions: []spec.Question{
			{
				ID:           "billing",
				Type:         spec.PrimitiveBinary,
				Instructions: "Is this message about billing?",
			},
			{
				ID:           "tone",
				Type:         spec.PrimitiveChoice,
				Instructions: "What is the tone of this message?",
				Options: []spec.Option{
					{ID: "angry", Description: "An upset or hostile message"},
					{ID: "calm", Description: "A neutral or polite message"},
				},
			},
		},
	}
	require.NoError(t, adapter.Validate(inst, validRule))

	scoreRule := spec.Spec{
		Version: 1,
		Questions: []spec.Question{
			{
				ID:           "urgency",
				Type:         spec.PrimitiveScore,
				Instructions: "How urgent?",
			},
		},
	}
	err := adapter.Validate(inst, scoreRule)
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryCapability, ce.Category)
}

func TestEncodeRequest_GoldensAndModelRequired(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): OpenAPI 0.2.0 SystemOneRequest schema.
	req := provider.ModelRequest{
		Rule: spec.Spec{
			Version: 1,
			Questions: []spec.Question{
				{
					ID:           "billing",
					Type:         spec.PrimitiveBinary,
					Instructions: "Is this message about billing?",
				},
				{
					ID:           "asks_choice",
					Type:         spec.PrimitiveBinary,
					Instructions: "Does `<message>` ask for a pick?",
					Criteria: &spec.BinaryCriteria{
						True:  "Presents alternatives and asks for a pick.",
						False: "No pick among alternatives is requested.",
					},
				},
				{
					ID:           "route_target",
					Type:         spec.PrimitiveChoice,
					Instructions: "Select the target queue.",
					Options: []spec.Option{
						{ID: "zebra", Description: "Zebra queue (first in declared order)"},
						{ID: "alpha", Description: "Alpha queue (second in declared order)"},
						{ID: "abstain_opt", Description: "Cannot tell from text", Abstain: true},
					},
				},
			},
		},
		Input: []provider.InputField{
			{
				Name:  spec.ProbeFieldText,
				Kind:  provider.FieldKindText,
				Value: "I was charged twice for my subscription this month.",
				Trust: provider.TrustUntrusted,
			},
		},
	}

	_, _, err := jevwire.EncodeRequest("", req)
	require.Error(t, err)
	pe, ok := provider.AsProviderError(err)
	require.True(t, ok)
	assert.Equal(t, provider.FailInvalidRequest, pe.Reason)

	encoded, qs, err := jevwire.EncodeRequest("jev-1.14.0", req)
	require.NoError(t, err)
	require.Len(t, qs, 3)
	assert.False(t, bytes.HasPrefix(encoded, []byte{0xEF, 0xBB, 0xBF}), "must not carry UTF-8 BOM")

	wantGolden := `{"model":"jev-1.14.0","state":{"probe.text":"I was charged twice for my subscription this month."},"questions":{"billing":{"type":"noul","instructions":"Is this message about billing?"},"asks_choice":{"type":"noul","instructions":"Does ` + "`<message>`" + ` ask for a pick?","criteria":{"true":"Presents alternatives and asks for a pick.","false":"No pick among alternatives is requested."}},"route_target":{"type":"choice","instructions":"Select the target queue.","criteria":{"zebra":"Zebra queue (first in declared order)","alpha":"Alpha queue (second in declared order)","abstain_opt":"Cannot tell from text"}}}}`
	assert.Equal(t, wantGolden, string(encoded))
}

func TestInvoke_NoDangerZoneHeadersAndCredentialSeparation(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): OpenAPI 0.2.0 NoulAnswer + ChoiceAnswer examples.
	const secretKey = "sk-typesafe-unit-secret-001122"

	var (
		capturedHeaders http.Header
		capturedBody    []byte
		capturedMethod  string
		capturedPath    string
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		capturedHeaders = r.Header.Clone()
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)

		w.Header().Set("X-TypeSafe-Request-Id", "req_synth_001")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
			"model": "jev-latest",
			"answers": {
				"billing": {"type": "noul", "noul": 0.98},
				"tone": {
					"type": "choice",
					"choice": "calm",
					"confidence": 0.7,
					"probabilities": {"angry": 0.15, "calm": 0.8, "excited": 0.05}
				}
			},
			"usage": {"input_tokens": 120, "output_tokens": 12}
		}`)
	}))
	defer srv.Close()

	transport := egress.NewClient(
		egress.StaticBearer(secretKey),
		egress.WithLoopbackServerForTesting(srv.URL, srv.Client()),
	)

	adapter := jevwire.NewTypeSafe()
	inst := provider.InstanceView{
		Name:     "jev-prod",
		Revision: 3,
		Model:    "jev-1.14.0",
		Endpoint: srv.URL,
		Options: map[string]string{
			// Even if Options contains danger-zone keys, adapter must never send them.
			jevwire.HeaderDangerZoneImages:      "true",
			jevwire.HeaderDangerZoneLongContext: "true",
		},
	}

	req := provider.ModelRequest{
		Rule: spec.Spec{
			Version: 1,
			Questions: []spec.Question{
				{ID: "billing", Type: spec.PrimitiveBinary, Instructions: "Is this message about billing?"},
				{
					ID:           "tone",
					Type:         spec.PrimitiveChoice,
					Instructions: "What is the tone of this message?",
					Options: []spec.Option{
						{ID: "angry", Description: "An upset or hostile message"},
						{ID: "calm", Description: "A neutral or polite message"},
						{ID: "excited", Description: "An enthusiastic or eager message"},
					},
				},
			},
		},
		Input: []provider.InputField{
			{
				Name:  spec.ProbeFieldText,
				Kind:  provider.FieldKindText,
				Value: "I was charged twice for my subscription this month. Please refund the extra charge.",
				Trust: provider.TrustUntrusted,
			},
		},
	}

	res, err := adapter.Invoke(context.Background(), transport, inst, req)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, capturedMethod)
	assert.Equal(t, "/v1/systemone", capturedPath)
	assert.Equal(t, "Bearer "+secretKey, capturedHeaders.Get("Authorization"))
	assert.Equal(t, "application/json", capturedHeaders.Get("Content-Type"))
	assert.Empty(t, capturedHeaders.Get("Content-Encoding"))
	assert.Empty(t, capturedHeaders.Get(jevwire.HeaderDangerZoneImages))
	assert.Empty(t, capturedHeaders.Get(jevwire.HeaderDangerZoneLongContext))
	for hdr := range capturedHeaders {
		assert.False(t, strings.Contains(strings.ToLower(hdr), "danger-zone"), "unexpected danger-zone header %q", hdr)
	}
	assert.NotContains(t, string(capturedBody), secretKey)

	assert.Equal(t, "jev-latest", res.ResolvedModel)
	assert.Equal(t, "req_synth_001", res.ProviderRequestID)
	assert.Equal(t, http.StatusOK, res.HTTPStatus)
	assert.Equal(t, provider.Usage{InputTokens: 120, OutputTokens: 12}, res.Usage)
	assert.Contains(t, res.Notes, provider.NoteModelAliasReported)

	billingAns, ok := res.Answers["billing"]
	require.True(t, ok)
	require.NotNil(t, billingAns.Bool)
	assert.True(t, *billingAns.Bool)
	assert.False(t, billingAns.Tied)
	assert.InDelta(t, 0.98, billingAns.Distribution["true"], 1e-12)
	assert.InDelta(t, 0.02, billingAns.Distribution["false"], 1e-12)
	assert.Empty(t, billingAns.NativeMetrics)

	toneAns, ok := res.Answers["tone"]
	require.True(t, ok)
	require.NotNil(t, toneAns.Choice)
	assert.Equal(t, "calm", *toneAns.Choice)
	assert.False(t, toneAns.Tied)
	assert.Equal(t, map[string]float64{"angry": 0.15, "calm": 0.8, "excited": 0.05}, toneAns.Distribution)
	assert.Equal(t, provider.Metric{
		Value:  0.7,
		Unit:   metrics.UnitFraction,
		Min:    0,
		Max:    1,
		Source: metrics.SourceProviderDoc,
	}, toneAns.NativeMetrics["typesafe.confidence"])
}

func TestInvoke_ErrorTableAndNoRetryAndNoBodyLeak(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): provider-design §7.4 error table.
	cases := []struct {
		name           string
		status         int
		headers        map[string]string
		body           string
		dropConn       bool
		wantReason     provider.FailReason
		wantRetryable  bool
		wantRetryAfter time.Duration
	}{
		{
			name:          "401_auth",
			status:        http.StatusUnauthorized,
			body:          `{"detail":"CANARY_401_INVALID_KEY"}`,
			wantReason:    provider.FailAuth,
			wantRetryable: false,
		},
		{
			name:          "400_max_tokens_exceeded_string",
			status:        http.StatusBadRequest,
			body:          `{"detail":"max_tokens_exceeded: CANARY_400_PROMPT"}`,
			wantReason:    provider.FailInputTooLarge,
			wantRetryable: false,
		},
		{
			name:          "400_max_tokens_exceeded_object",
			status:        http.StatusBadRequest,
			body:          `{"detail":{"code":"max_tokens_exceeded","echo":"CANARY_400_OBJ"}}`,
			wantReason:    provider.FailInputTooLarge,
			wantRetryable: false,
		},
		{
			name:          "400_other_rule_violation",
			status:        http.StatusBadRequest,
			body:          `{"detail":"CANARY_400_RULE_VIOLATION"}`,
			wantReason:    provider.FailInvalidRequest,
			wantRetryable: false,
		},
		{
			name:          "413_payload_too_large",
			status:        http.StatusRequestEntityTooLarge,
			body:          `{"detail":"CANARY_413_TOO_LARGE"}`,
			wantReason:    provider.FailInputTooLarge,
			wantRetryable: false,
		},
		{
			name:          "415_unsupported_media_type",
			status:        http.StatusUnsupportedMediaType,
			body:          `{"detail":"CANARY_415_COMPRESSION"}`,
			wantReason:    provider.FailInvalidRequest,
			wantRetryable: false,
		},
		{
			name:          "422_validation_error_list",
			status:        http.StatusUnprocessableEntity,
			body:          `{"detail":[{"loc":["body","questions"],"msg":"CANARY_422_SCHEMA","type":"value_error"}]}`,
			wantReason:    provider.FailInvalidRequest,
			wantRetryable: false,
		},
		{
			name:           "429_retry_after_seconds",
			status:         http.StatusTooManyRequests,
			headers:        map[string]string{"Retry-After": "4"},
			body:           `{"detail":"CANARY_429_ORG_QUOTA"}`,
			wantReason:     provider.FailRateLimited,
			wantRetryable:  true,
			wantRetryAfter: 4 * time.Second,
		},
		{
			name:           "429_retry_after_ms",
			status:         http.StatusTooManyRequests,
			headers:        map[string]string{"retry-after-ms": "350"},
			body:           `{"detail":"CANARY_429_MS"}`,
			wantReason:     provider.FailRateLimited,
			wantRetryable:  true,
			wantRetryAfter: 350 * time.Millisecond,
		},
		{
			name:          "529_overloaded",
			status:        529,
			body:          `{"detail":"CANARY_529_OVERLOADED"}`,
			wantReason:    provider.FailUnavailable,
			wantRetryable: true,
		},
		{
			name:          "503_service_unavailable",
			status:        http.StatusServiceUnavailable,
			body:          `{"detail":"CANARY_503_DOWN"}`,
			wantReason:    provider.FailUnavailable,
			wantRetryable: true,
		},
		{
			name:          "408_request_timeout",
			status:        http.StatusRequestTimeout,
			body:          `{"detail":"CANARY_408_UPSTREAM"}`,
			wantReason:    provider.FailUnavailable,
			wantRetryable: true,
		},
		{
			name:          "connection_failure",
			dropConn:      true,
			wantReason:    provider.FailUnavailable,
			wantRetryable: true,
		},
	}

	baseReq := provider.ModelRequest{
		Rule: spec.Spec{
			Version: 1,
			Questions: []spec.Question{
				{ID: "billing", Type: spec.PrimitiveBinary, Instructions: "Is this message about billing?"},
			},
		},
		Input: []provider.InputField{
			{Name: spec.ProbeFieldText, Kind: provider.FieldKindText, Value: "synthetic text", Trust: provider.TrustUntrusted},
		},
	}

	adapter := jevwire.NewTypeSafe()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if tc.dropConn {
					hj, ok := w.(http.Hijacker)
					if ok {
						conn, _, err := hj.Hijack()
						if err == nil {
							_ = conn.Close()
							return
						}
					}
				}
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			transport := egress.NewClient(
				egress.StaticBearer("sk-err-secret-canary"),
				egress.WithLoopbackServerForTesting(srv.URL, srv.Client()),
			)
			inst := provider.InstanceView{Name: "jev-prod", Model: "jev-1.14.0", Endpoint: srv.URL}

			_, err := adapter.Invoke(context.Background(), transport, inst, baseReq)
			require.Error(t, err)
			assert.Equal(t, int32(1), hits.Load(), "P1 adapter must never retry inside Invoke")

			pe, ok := provider.AsProviderError(err)
			require.True(t, ok)
			assert.Equal(t, tc.wantReason, pe.Reason)
			assert.Equal(t, tc.wantRetryable, pe.Retryable)
			if tc.wantRetryAfter > 0 {
				assert.Equal(t, tc.wantRetryAfter, pe.RetryAfter)
			}
			assert.NotContains(t, pe.Message, "CANARY_")
			assert.NotContains(t, err.Error(), "CANARY_")
			assert.NotContains(t, err.Error(), "sk-err-secret-canary")
		})
	}
}

func TestListModels_SuccessAndErrors(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): OpenAPI 0.2.0 GET /v1/models ModelMetadataList.
	var capturedHeaders http.Header
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/v1/models", r.URL.Path)
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{
			"models": [
				{"name": "jev-latest", "description": "General-purpose system one model.", "release_date": "2026-09-15"},
				{"name": "jev-1.14.0", "description": "Pinned Jev 1.14.0 release.", "release_date": "2026-09-15"}
			]
		}`)
	}))
	defer srv.Close()

	transport := egress.NewClient(
		egress.StaticBearer("sk-models-key"),
		egress.WithLoopbackServerForTesting(srv.URL, srv.Client()),
	)

	adapter := jevwire.NewTypeSafe()
	models, err := adapter.ListModels(context.Background(), transport, provider.InstanceView{
		Name:     "jev-prod",
		Model:    "jev-1.14.0",
		Endpoint: srv.URL,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"jev-latest", "jev-1.14.0"}, models)
	assert.Equal(t, "Bearer sk-models-key", capturedHeaders.Get("Authorization"))
	assert.Empty(t, capturedHeaders.Get(jevwire.HeaderDangerZoneImages))
	assert.Empty(t, capturedHeaders.Get(jevwire.HeaderDangerZoneLongContext))

	// Profile without ModelsPath returns FailUnsupported.
	noListProfile := jevwire.TypeSafeProfile()
	noListProfile.ModelsPath = ""
	_, err = jevwire.NewAdapter(noListProfile).ListModels(context.Background(), transport, provider.InstanceView{Endpoint: srv.URL})
	require.Error(t, err)
	pe, ok := provider.AsProviderError(err)
	require.True(t, ok)
	assert.Equal(t, provider.FailUnsupported, pe.Reason)

	// Malformed GET /v1/models payloads fail as malformed_response.
	for _, badBody := range []string{
		`{}`,
		`{"data":[{"id":"jev-latest"}]}`,
		`{"models":[{"name":"","description":"d","release_date":"2026-09-15"}]}`,
		`{"models":[{"name":"jev-latest","release_date":"2026-09-15"}]}`,
		`{"models":[{"name":"jev-latest","description":"d","release_date":""}]}`,
		`{"models":[],"models":[]}`,
	} {
		_, decErr := jevwire.DecodeModelsResponse(http.StatusOK, []byte(badBody))
		require.Error(t, decErr, "expected error for body %s", badBody)
		peBad, ok := provider.AsProviderError(decErr)
		require.True(t, ok)
		assert.Equal(t, provider.FailMalformedResponse, peBad.Reason)
	}
}

func TestDecodeResponse_TiesConfidenceAndProfileFlags(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): provider-design §4.3, §6.3, §7.4 (U3, U4).
	qs := []spec.Question{
		{ID: "b", Type: spec.PrimitiveBinary, Instructions: "Binary tie check."},
		{
			ID:           "c",
			Type:         spec.PrimitiveChoice,
			Instructions: "Choice tie check.",
			Options: []spec.Option{
				{ID: "angry", Description: "Upset"},
				{ID: "calm", Description: "Neutral"},
				{ID: "excited", Description: "Eager"},
			},
		},
	}

	// Exact binary 0.5 tie + top-two choice tie within 1e-9 with choice set to one of the tied top options.
	tiePayload := []byte(`{
		"model": "jev-1.14.0",
		"answers": {
			"b": {"type": "noul", "noul": 0.5, "confidence": 0.99},
			"c": {
				"type": "choice",
				"choice": "calm",
				"confidence": 0.50,
				"probabilities": {"angry": 0.45, "calm": 0.4500000004, "excited": 0.0999999996}
			}
		},
		"usage": {"input_tokens": 50, "output_tokens": 8}
	}`)

	res, err := jevwire.DecodeResponse(jevwire.TypeSafeProfile(), qs, http.StatusOK, "  req_tie_123 \r\n", tiePayload)
	require.NoError(t, err)
	assert.Equal(t, "jev-1.14.0", res.ResolvedModel)
	assert.Empty(t, res.Notes, "versioned model id must not add model_alias_reported note")
	assert.Equal(t, "req_tie_123", res.ProviderRequestID)

	bAns := res.Answers["b"]
	assert.Equal(t, provider.AnswerStatusAnswered, bAns.Status)
	assert.True(t, bAns.Tied)
	assert.Nil(t, bAns.Bool)
	assert.Equal(t, map[string]float64{"true": 0.5, "false": 0.5}, bAns.Distribution)
	assert.Empty(t, bAns.NativeMetrics, "confidence on noul must be ignored")

	cAns := res.Answers["c"]
	assert.Equal(t, provider.AnswerStatusAnswered, cAns.Status)
	assert.True(t, cAns.Tied)
	assert.Nil(t, cAns.Choice)
	assert.Contains(t, cAns.Notes, jevwire.NoteConfidenceFormulaDeviation, "U4: confidence formula deviation must be noted, not rejected")
	assert.InDelta(t, 0.50, cAns.NativeMetrics[jevwire.NativeMetricTypeSafeConfidence].Value, 1e-12)

	// Choice tie with choice == null: rejected on TypeSafeProfile (ChoiceNullOnTie=false), accepted when ChoiceNullOnTie=true.
	nullChoiceTie := []byte(`{
		"model": "jev-1.14.0",
		"answers": {
			"c": {
				"type": "choice",
				"choice": null,
				"confidence": 0.175,
				"probabilities": {"angry": 0.45, "calm": 0.45, "excited": 0.10}
			}
		},
		"usage": {"input_tokens": 40, "output_tokens": 5}
	}`)
	_, err = jevwire.DecodeResponse(jevwire.TypeSafeProfile(), qs[1:], http.StatusOK, "", nullChoiceTie)
	require.Error(t, err)
	pe, ok := provider.AsProviderError(err)
	require.True(t, ok)
	assert.Equal(t, provider.FailMalformedResponse, pe.Reason)

	nullTieProfile := jevwire.TypeSafeProfile()
	nullTieProfile.ChoiceNullOnTie = true
	resNullOk, err := jevwire.DecodeResponse(nullTieProfile, qs[1:], http.StatusOK, "", nullChoiceTie)
	require.NoError(t, err)
	assert.True(t, resNullOk.Answers["c"].Tied)
	assert.Nil(t, resNullOk.Answers["c"].Choice)

	// Choice tie where choice names a non-top option -> malformed_response even on tie.
	nonTopTie := []byte(`{
		"model": "jev-1.14.0",
		"answers": {
			"c": {
				"type": "choice",
				"choice": "excited",
				"confidence": 0.175,
				"probabilities": {"angry": 0.45, "calm": 0.45, "excited": 0.10}
			}
		},
		"usage": {"input_tokens": 40, "output_tokens": 5}
	}`)
	_, err = jevwire.DecodeResponse(nullTieProfile, qs[1:], http.StatusOK, "", nonTopTie)
	require.Error(t, err)
	pe, ok = provider.AsProviderError(err)
	require.True(t, ok)
	assert.Equal(t, provider.FailMalformedResponse, pe.Reason)
}

func TestDecodeResponse_MalformedCasesNeverRenormalize(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): provider-design §6.3, §7.4 F6.
	qs := []spec.Question{
		{
			ID:           "tone",
			Type:         spec.PrimitiveChoice,
			Instructions: "What is the tone?",
			Options: []spec.Option{
				{ID: "angry", Description: "Upset"},
				{ID: "calm", Description: "Neutral"},
				{ID: "excited", Description: "Eager"},
			},
		},
	}

	malformedPayloads := []struct {
		name string
		body string
	}{
		{
			name: "missing_answer_key",
			body: `{"model":"jev-1.14.0","answers":{},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "extra_answer_key",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"calm","confidence":0.7,"probabilities":{"angry":0.15,"calm":0.8,"excited":0.05}},"extra":{"type":"noul","noul":0.5}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "missing_probability_key",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"calm","confidence":0.7,"probabilities":{"angry":0.2,"calm":0.8}}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "extra_probability_key",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"calm","confidence":0.7,"probabilities":{"angry":0.15,"calm":0.8,"excited":0.05,"other":0.0}}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "sum_0_9",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"calm","confidence":0.7,"probabilities":{"angry":0.15,"calm":0.70,"excited":0.05}}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "bare_nan",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"calm","confidence":0.7,"probabilities":{"angry":NaN,"calm":0.8,"excited":0.2}}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "choice_not_argmax",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"angry","confidence":0.7,"probabilities":{"angry":0.15,"calm":0.8,"excited":0.05}}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "wrong_answer_type",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"noul","noul":0.8}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "missing_confidence",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"calm","probabilities":{"angry":0.15,"calm":0.8,"excited":0.05}}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "confidence_out_of_range",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"calm","confidence":1.2,"probabilities":{"angry":0.15,"calm":0.8,"excited":0.05}}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "missing_model",
			body: `{"answers":{"tone":{"type":"choice","choice":"calm","confidence":0.7,"probabilities":{"angry":0.15,"calm":0.8,"excited":0.05}}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
		{
			name: "missing_usage",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"calm","confidence":0.7,"probabilities":{"angry":0.15,"calm":0.8,"excited":0.05}}}}`,
		},
		{
			name: "duplicate_json_probability_key",
			body: `{"model":"jev-1.14.0","answers":{"tone":{"type":"choice","choice":"calm","confidence":0.7,"probabilities":{"angry":0.1,"angry":0.15,"calm":0.8,"excited":0.05}}},"usage":{"input_tokens":10,"output_tokens":2}}`,
		},
	}

	for _, tc := range malformedPayloads {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jevwire.DecodeResponse(jevwire.TypeSafeProfile(), qs, http.StatusOK, "req_bad", []byte(tc.body))
			require.Error(t, err)
			pe, ok := provider.AsProviderError(err)
			require.True(t, ok)
			assert.Equal(t, provider.FailMalformedResponse, pe.Reason)
			assert.False(t, pe.Retryable)
		})
	}
}

func TestInvoke_OversizeBodyAndContextCancellation(t *testing.T) {
	// > 1 MiB response body fails as FailMalformedResponse (AC-4).
	oversizeSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte("x"), int(egress.MaxResponseBytes)+64))
	}))
	defer oversizeSrv.Close()

	transport := egress.NewClient(
		egress.StaticBearer("sk-oversize-key"),
		egress.WithLoopbackServerForTesting(oversizeSrv.URL, oversizeSrv.Client()),
	)
	adapter := jevwire.NewTypeSafe()
	inst := provider.InstanceView{Name: "jev-prod", Model: "jev-1.14.0", Endpoint: oversizeSrv.URL}
	req := provider.ModelRequest{
		Rule: spec.Spec{
			Version:   1,
			Questions: []spec.Question{{ID: "q", Type: spec.PrimitiveBinary, Instructions: "Check."}},
		},
		Input: []provider.InputField{{Name: spec.ProbeFieldText, Kind: provider.FieldKindText, Value: "hi", Trust: provider.TrustUntrusted}},
	}

	_, err := adapter.Invoke(context.Background(), transport, inst, req)
	require.Error(t, err)
	pe, ok := provider.AsProviderError(err)
	require.True(t, ok)
	assert.Equal(t, provider.FailMalformedResponse, pe.Reason)

	// Canceled caller context maps to FailCanceled.
	ctxCanceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = adapter.Invoke(ctxCanceled, transport, inst, req)
	require.Error(t, err)
	pe, ok = provider.AsProviderError(err)
	require.True(t, ok)
	assert.Equal(t, provider.FailCanceled, pe.Reason)
}
