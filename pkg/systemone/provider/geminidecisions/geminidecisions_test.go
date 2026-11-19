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

package geminidecisions_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/geminidecisions"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultRegistry_RegistersGeminiDecisionsWithoutModelLister(t *testing.T) {
	got, ok := provider.Lookup(geminidecisions.AdapterName)
	require.True(t, ok, "gemini_decisions must register in provider.DefaultRegistry()")
	assert.Equal(t, "gemini_decisions", got.Name())
	assert.Equal(t, "0.1.0", got.Version())

	_, isLister := got.(provider.ModelLister)
	assert.False(t, isLister, "gemini_decisions must not implement provider.ModelLister in PV3 (U-G1)")

	// Empty or whitespace model in Capabilities must return FailInvalidRequest.
	for _, badModel := range []string{"", "   ", "gemini\nmodel"} {
		_, err := got.Capabilities(provider.InstanceView{Name: "gem-prod", Model: badModel})
		require.Error(t, err, "expected error for model %q", badModel)
		pe, ok := provider.AsProviderError(err)
		require.True(t, ok)
		assert.Equal(t, provider.FailInvalidRequest, pe.Reason)
	}

	caps, err := got.Capabilities(provider.InstanceView{Name: "gem-prod", Model: "gemini-decision-1"})
	require.NoError(t, err)
	assert.True(t, caps.SideEffectFree)
	assert.True(t, caps.ReturnsDistribution)
	assert.False(t, caps.Abstains)
	assert.Equal(t, provider.TruncationNever, caps.Truncation)
	assert.Equal(t, 64, caps.MaxQuestionsPerRequest)
	assert.Equal(t, 262144, caps.MaxInputTokens)
	assert.Equal(t, provider.AuthScheme{
		Kind:       provider.AuthKindHeader,
		HeaderName: "x-goog-api-key",
	}, caps.AuthScheme)
	assert.Equal(t, "header:x-goog-api-key", caps.AuthScheme.String())
	assert.Equal(t, geminidecisions.ProviderRetention, caps.ProviderRetention)
	assert.Equal(t, provider.PrimitiveLimits{
		Supported:                true,
		DistributionCompleteness: provider.CompletenessComplete,
	}, caps.Primitives[spec.PrimitiveBinary])
	assert.Equal(t, provider.PrimitiveLimits{
		Supported:                true,
		MinOptions:               2,
		MaxOptions:               64,
		DistributionCompleteness: provider.CompletenessComplete,
	}, caps.Primitives[spec.PrimitiveChoice])
	_, hasScore := caps.Primitives[spec.PrimitiveScore]
	assert.False(t, hasScore, "score primitive is deferred to PV9")

	require.Len(t, caps.NativeMetrics, 1)
	assert.Equal(t, provider.MetricDecl{
		Name:        "gemini.confidence",
		Unit:        metrics.UnitFraction,
		Min:         0,
		Max:         1,
		HigherMeans: "more_probable_top_outcome",
		Source:      metrics.SourceProviderDoc,
	}, caps.NativeMetrics[0])

	// Capabilities returns defensive copies.
	caps.Primitives[spec.PrimitiveBinary] = provider.PrimitiveLimits{}
	caps.NativeMetrics[0].Name = "mutated"
	capsAgain, err := got.Capabilities(provider.InstanceView{Name: "gem-prod", Model: "gemini-decision-1"})
	require.NoError(t, err)
	assert.True(t, capsAgain.Primitives[spec.PrimitiveBinary].Supported)
	assert.Equal(t, "gemini.confidence", capsAgain.NativeMetrics[0].Name)
}

func TestValidate_AcceptsPortableBinaryAndChoiceAndRejectsEmptyModelOrScore(t *testing.T) {
	adapter := geminidecisions.New()
	inst := provider.InstanceView{Name: "gem-prod", Model: "gemini-decision-1"}

	validRule := spec.Spec{
		Version: 1,
		Questions: []spec.Question{
			{
				ID:           "is_damaged",
				Type:         spec.PrimitiveBinary,
				Instructions: "Does `probe.text` report physical damage?",
			},
			{
				ID:           "department",
				Type:         spec.PrimitiveChoice,
				Instructions: "Route `probe.text` to a department.",
				Options: []spec.Option{
					{ID: "billing", Description: "Billing questions"},
					{ID: "support", Description: "Technical support"},
				},
			},
		},
	}
	require.NoError(t, adapter.Validate(inst, validRule))

	// Empty model fails Validate via Capabilities.
	err := adapter.Validate(provider.InstanceView{Name: "gem-prod", Model: ""}, validRule)
	require.Error(t, err)

	// Score rule fails Validate with CategoryCapability.
	scoreRule := spec.Spec{
		Version: 1,
		Questions: []spec.Question{
			{ID: "urgency", Type: spec.PrimitiveScore, Instructions: "How urgent?"},
		},
	}
	err = adapter.Validate(inst, scoreRule)
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryCapability, ce.Category)
}

func TestEncodeRequest_SortedInputStringBinaryCriteriaAndDeclaredChoiceOrder(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): ai.google.dev/gemini-api/docs/decisions-api & api/decisions.
	req := provider.ModelRequest{
		Rule: spec.Spec{
			Version: 1,
			Questions: []spec.Question{
				{
					ID:           "is_damaged",
					Type:         spec.PrimitiveBinary,
					Instructions: "Does the customer describe physical damage?",
				},
				{
					ID:           "is_defect",
					Type:         spec.PrimitiveBinary,
					Instructions: "Is the issue a manufacturing defect under <policy>?",
					Criteria: &spec.BinaryCriteria{
						True:  "Caused by manufacturing or assembly defect.",
						False: "Accidental user damage or normal wear.",
					},
				},
				{
					ID:           "department",
					Type:         spec.PrimitiveChoice,
					Instructions: "Which department should handle this?",
					Options: []spec.Option{
						{ID: "support", Description: "Technical support"},
						{ID: "billing", Description: "Billing & invoices"},
						{ID: "other", Description: "Other inquiries", Abstain: true},
					},
				},
			},
		},
		// Supply inputs out of alphabetical order; EncodeRequest must sort keys in serialized JSON string.
		Input: []provider.InputField{
			{Name: "z_field", Kind: provider.FieldKindText, Value: "second", Trust: provider.TrustUntrusted},
			{Name: "a_field", Kind: provider.FieldKindText, Value: "first <item> & note", Trust: provider.TrustUntrusted},
		},
	}

	_, _, err := geminidecisions.EncodeRequest("", req)
	require.Error(t, err)
	pe, ok := provider.AsProviderError(err)
	require.True(t, ok)
	assert.Equal(t, provider.FailInvalidRequest, pe.Reason)

	encoded, qs, err := geminidecisions.EncodeRequest("gemini-decision-1", req)
	require.NoError(t, err)
	require.Len(t, qs, 3)
	assert.False(t, bytes.HasPrefix(encoded, []byte{0xEF, 0xBB, 0xBF}), "must not carry UTF-8 BOM")

	wantGolden := `{"model":"gemini-decision-1","input":"{\"a_field\":\"first <item> & note\",\"z_field\":\"second\"}","questions":{"is_damaged":{"type":"noul","instructions":"Does the customer describe physical damage?"},"is_defect":{"type":"noul","instructions":"Is the issue a manufacturing defect under <policy>?","criteria":{"true_description":"Caused by manufacturing or assembly defect.","false_description":"Accidental user damage or normal wear."}},"department":{"type":"choice","instructions":"Which department should handle this?","criteria":{"support":"Technical support","billing":"Billing & invoices","other":"Other inquiries"}}}}`
	assert.Equal(t, wantGolden, string(encoded))

	// Verify input decodes as a single JSON string whose inner JSON keys are sorted.
	var parsed struct {
		Model     string          `json:"model"`
		Input     string          `json:"input"`
		Questions json.RawMessage `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(encoded, &parsed))
	assert.Equal(t, `{"a_field":"first <item> & note","z_field":"second"}`, parsed.Input)
}

func TestDecodeResponse_Documented0999ChoiceBinaryNotesTiesSafetyBlockAndUsage(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): ai.google.dev/gemini-api/docs/decisions-api & api/decisions.
	qs := []spec.Question{
		{
			ID:           "is_damaged",
			Type:         spec.PrimitiveBinary,
			Instructions: "Is the item damaged?",
		},
		{
			ID:           "department",
			Type:         spec.PrimitiveChoice,
			Instructions: "Route to department.",
			Options: []spec.Option{
				{ID: "billing", Description: "Billing"},
				{ID: "sales", Description: "Sales"},
				{ID: "support", Description: "Support"},
				{ID: "other", Description: "Other"},
			},
		},
	}

	docPayload := []byte(`{
		"answers": {
			"is_damaged": {
				"type": "noul",
				"noul": 0.99,
				"confidence": 0.99
			},
			"department": {
				"type": "choice",
				"choice": "support",
				"probabilities": {
					"billing": 0.004,
					"other": 0.002,
					"sales": 0.003,
					"support": 0.99
				},
				"confidence": 0.99
			}
		},
		"usage": {
			"total_input_tokens": 64,
			"total_output_tokens": 0,
			"total_tokens": 64
		},
		"unknown_future_field": {"ignored": true}
	}`)

	res, err := geminidecisions.DecodeResponse(qs, http.StatusOK, docPayload)
	require.NoError(t, err)
	assert.Empty(t, res.ResolvedModel, "Gemini Decisions response omits model; ResolvedModel must be empty")
	assert.Empty(t, res.ProviderRequestID)
	assert.Equal(t, http.StatusOK, res.HTTPStatus)
	assert.Equal(t, provider.Usage{InputTokens: 64, OutputTokens: 0}, res.Usage)
	assert.Equal(t, []string{provider.NoteModelUnreported}, res.Notes)

	binAns := res.Answers["is_damaged"]
	require.NotNil(t, binAns.Bool)
	assert.True(t, *binAns.Bool)
	assert.False(t, binAns.Tied)
	assert.InDelta(t, 0.99, binAns.Distribution["true"], 1e-12)
	assert.InDelta(t, 0.01, binAns.Distribution["false"], 1e-12)
	assert.Equal(t, []string{provider.NoteClamped001To099}, binAns.Notes)
	assert.Equal(t, provider.Metric{
		Value:  0.99,
		Unit:   metrics.UnitFraction,
		Min:    0,
		Max:    1,
		Source: metrics.SourceProviderDoc,
	}, binAns.NativeMetrics["gemini.confidence"])

	choiceAns := res.Answers["department"]
	require.NotNil(t, choiceAns.Choice)
	assert.Equal(t, "support", *choiceAns.Choice)
	assert.False(t, choiceAns.Tied)
	assert.Equal(t, map[string]float64{
		"billing": 0.004,
		"other":   0.002,
		"sales":   0.003,
		"support": 0.99,
	}, choiceAns.Distribution, "0.999 documented sum must pass without renormalization")
	assert.Equal(t, []string{provider.NoteOptionsSortedByProvider}, choiceAns.Notes)
	assert.Equal(t, provider.Metric{
		Value:  0.99,
		Unit:   metrics.UnitFraction,
		Min:    0,
		Max:    1,
		Source: metrics.SourceProviderDoc,
	}, choiceAns.NativeMetrics["gemini.confidence"])

	// Missing usage or missing total_input_tokens -> usage_unreported note, never presented as measured zero.
	for _, missingUsageBody := range []string{
		`{"answers":{"is_damaged":{"type":"noul","noul":0.99,"confidence":0.99}}}`,
		`{"answers":{"is_damaged":{"type":"noul","noul":0.99,"confidence":0.99}},"usage":{}}`,
		`{"answers":{"is_damaged":{"type":"noul","noul":0.99,"confidence":0.99}},"usage":{"total_output_tokens":0}}`,
	} {
		uRes, uErr := geminidecisions.DecodeResponse(qs[:1], http.StatusOK, []byte(missingUsageBody))
		require.NoError(t, uErr)
		assert.Equal(t, provider.Usage{InputTokens: 0, OutputTokens: 0}, uRes.Usage)
		assert.ElementsMatch(t, []string{provider.NoteModelUnreported, provider.NoteUsageUnreported}, uRes.Notes)
	}

	// Tie + confidence formula deviation observations on binary and choice.
	tieAndDeviation := []byte(`{
		"answers": {
			"is_damaged": {"type": "noul", "noul": 0.5, "confidence": 0.8},
			"department": {
				"type": "choice",
				"choice": "billing",
				"probabilities": {"billing": 0.45, "other": 0.05, "sales": 0.05, "support": 0.4500000004},
				"confidence": 0.6
			}
		},
		"usage": {"total_input_tokens": 30}
	}`)
	tieRes, err := geminidecisions.DecodeResponse(qs, http.StatusOK, tieAndDeviation)
	require.NoError(t, err)
	assert.Equal(t, []string{provider.NoteModelUnreported}, tieRes.Notes)
	assert.True(t, tieRes.Answers["is_damaged"].Tied)
	assert.Nil(t, tieRes.Answers["is_damaged"].Bool)
	assert.ElementsMatch(t, []string{provider.NoteClamped001To099, provider.NoteConfidenceFormulaDeviation}, tieRes.Answers["is_damaged"].Notes)
	assert.True(t, tieRes.Answers["department"].Tied)
	assert.Nil(t, tieRes.Answers["department"].Choice)
	assert.ElementsMatch(t, []string{provider.NoteOptionsSortedByProvider, provider.NoteConfidenceFormulaDeviation}, tieRes.Answers["department"].Notes)

	// 200 safety block via prompt_feedback.block_reason -> content_blocked (even if partial answers are present).
	for _, blockedBody := range []string{
		`{"answers":{},"prompt_feedback":{"block_reason":"blocklist","block_reason_message":"CANARY_BLOCK_MSG"}}`,
		`{"prompt_feedback":{"block_reason":"safety","block_reason_message":"CANARY_SAFETY_MSG"}}`,
		`{"answers":{"is_damaged":{"type":"noul","noul":0.9,"confidence":0.9}},"prompt_feedback":{"block_reason":"prohibited_content","block_reason_message":"CANARY_PARTIAL_MSG"}}`,
	} {
		_, bErr := geminidecisions.DecodeResponse(qs, http.StatusOK, []byte(blockedBody))
		require.Error(t, bErr)
		pe, ok := provider.AsProviderError(bErr)
		require.True(t, ok)
		assert.Equal(t, provider.FailContentBlocked, pe.Reason)
		assert.False(t, pe.Retryable)
		assert.Equal(t, http.StatusOK, pe.HTTPStatus)
		assert.NotContains(t, pe.Message, "CANARY_")
		assert.NotContains(t, bErr.Error(), "CANARY_")
	}
}

func TestDecodeResponse_MalformedResponsesFailClosed(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): brief §5.3 & §7 G7.
	qs := []spec.Question{
		{
			ID:           "department",
			Type:         spec.PrimitiveChoice,
			Instructions: "Route.",
			Options: []spec.Option{
				{ID: "billing", Description: "Billing"},
				{ID: "support", Description: "Support"},
			},
		},
	}
	binQs := []spec.Question{
		{ID: "is_damaged", Type: spec.PrimitiveBinary, Instructions: "Damaged?"},
	}

	malformed := []struct {
		name string
		qs   []spec.Question
		body string
	}{
		{
			name: "missing_answer",
			qs:   qs,
			body: `{"answers":{},"usage":{"total_input_tokens":10}}`,
		},
		{
			name: "extra_answer",
			qs:   qs,
			body: `{"answers":{"department":{"type":"choice","choice":"support","confidence":0.8,"probabilities":{"billing":0.2,"support":0.8}},"extra":{"type":"noul","noul":0.5,"confidence":0.5}}}`,
		},
		{
			name: "extra_probability_key",
			qs:   qs,
			body: `{"answers":{"department":{"type":"choice","choice":"support","confidence":0.8,"probabilities":{"billing":0.2,"support":0.8,"sales":0.0}}}}`,
		},
		{
			name: "sum_098_materially_below",
			qs:   qs,
			body: `{"answers":{"department":{"type":"choice","choice":"support","confidence":0.8,"probabilities":{"billing":0.18,"support":0.80}}}}`,
		},
		{
			name: "sum_just_outside_float_slack",
			qs:   qs,
			body: `{"answers":{"department":{"type":"choice","choice":"support","confidence":0.5,"probabilities":{"billing":0.5,"support":0.498999999}}}}`,
		},
		{
			name: "bare_nan",
			qs:   qs,
			body: `{"answers":{"department":{"type":"choice","choice":"support","confidence":0.8,"probabilities":{"billing":NaN,"support":0.8}}}}`,
		},
		{
			name: "noul_above_1",
			qs:   binQs,
			body: `{"answers":{"is_damaged":{"type":"noul","noul":1.2,"confidence":0.9}}}`,
		},
		{
			name: "non_argmax_choice",
			qs:   qs,
			body: `{"answers":{"department":{"type":"choice","choice":"billing","confidence":0.8,"probabilities":{"billing":0.2,"support":0.8}}}}`,
		},
		{
			name: "non_top_choice_on_tie",
			qs: []spec.Question{{
				ID: "c", Type: spec.PrimitiveChoice, Instructions: "Route.",
				Options: []spec.Option{{ID: "a", Description: "A"}, {ID: "b", Description: "B"}, {ID: "c", Description: "C"}},
			}},
			body: `{"answers":{"c":{"type":"choice","choice":"c","confidence":0.45,"probabilities":{"a":0.45,"b":0.45,"c":0.10}}}}`,
		},
		{
			name: "null_choice_non_tie",
			qs:   qs,
			body: `{"answers":{"department":{"type":"choice","choice":null,"confidence":0.8,"probabilities":{"billing":0.2,"support":0.8}}}}`,
		},
		{
			name: "null_choice_on_tie",
			qs:   qs,
			body: `{"answers":{"department":{"type":"choice","choice":null,"confidence":0.5,"probabilities":{"billing":0.5,"support":0.5}}}}`,
		},
		{
			name: "wrong_type_noul_for_choice",
			qs:   qs,
			body: `{"answers":{"department":{"type":"noul","noul":0.8,"confidence":0.8}}}`,
		},
		{
			name: "wrong_type_choice_for_binary",
			qs:   binQs,
			body: `{"answers":{"is_damaged":{"type":"choice","choice":"yes","confidence":0.8,"probabilities":{"yes":0.8,"no":0.2}}}}`,
		},
		{
			name: "missing_confidence_choice",
			qs:   qs,
			body: `{"answers":{"department":{"type":"choice","choice":"support","probabilities":{"billing":0.2,"support":0.8}}}}`,
		},
		{
			name: "missing_confidence_binary",
			qs:   binQs,
			body: `{"answers":{"is_damaged":{"type":"noul","noul":0.9}}}`,
		},
		{
			name: "confidence_out_of_range",
			qs:   binQs,
			body: `{"answers":{"is_damaged":{"type":"noul","noul":0.9,"confidence":1.1}}}`,
		},
		{
			name: "negative_usage_tokens",
			qs:   binQs,
			body: `{"answers":{"is_damaged":{"type":"noul","noul":0.9,"confidence":0.9}},"usage":{"total_input_tokens":-1}}`,
		},
		{
			name: "duplicate_json_key",
			qs:   binQs,
			body: `{"answers":{"is_damaged":{"type":"noul","noul":0.9,"noul":0.8,"confidence":0.9}}}`,
		},
	}

	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			_, err := geminidecisions.DecodeResponse(tc.qs, http.StatusOK, []byte(tc.body))
			require.Error(t, err)
			pe, ok := provider.AsProviderError(err)
			require.True(t, ok)
			assert.Equal(t, provider.FailMalformedResponse, pe.Reason)
			assert.False(t, pe.Retryable)
		})
	}
}

func TestInvoke_HeaderPlacementWireGuardsAndErrorMapping(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): brief §4, §5.2, §5.4.
	const canaryKey = "AIzaSy-synthetic-gemini-unit-canary-key-9988"

	t.Run("wire_guards_header_only_and_no_authorization", func(t *testing.T) {
		var (
			capturedMethod  string
			capturedPath    string
			capturedQuery   string
			capturedHeaders http.Header
			capturedBody    []byte
		)
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedMethod = r.Method
			capturedPath = r.URL.Path
			capturedQuery = r.URL.RawQuery
			capturedHeaders = r.Header.Clone()
			var err error
			capturedBody, err = io.ReadAll(r.Body)
			require.NoError(t, err)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"answers":{"is_damaged":{"type":"noul","noul":0.99,"confidence":0.99}},"usage":{"total_input_tokens":28,"total_output_tokens":0}}`)
		}))
		defer srv.Close()

		tr := egress.NewClient(
			egress.StaticBearer(canaryKey),
			egress.WithCredentialPlacement(egress.HeaderPlacement(geminidecisions.HeaderAPIKey)),
			egress.WithLoopbackServerForTesting(srv.URL, srv.Client()),
		)

		adapter := geminidecisions.New()
		inst := provider.InstanceView{
			Name:     "gemini-main",
			Revision: 1,
			Model:    "gemini-decision-1",
			Endpoint: srv.URL,
		}
		req := provider.ModelRequest{
			Rule: spec.Spec{
				Version:   1,
				Questions: []spec.Question{{ID: "is_damaged", Type: spec.PrimitiveBinary, Instructions: "Damaged?"}},
			},
			Input: []provider.InputField{
				{Name: spec.ProbeFieldText, Kind: provider.FieldKindText, Value: "The box arrived crushed.", Trust: provider.TrustUntrusted},
			},
		}

		res, err := adapter.Invoke(context.Background(), tr, inst, req)
		require.NoError(t, err)

		assert.Equal(t, http.MethodPost, capturedMethod)
		assert.Equal(t, "/v1beta/decisions", capturedPath)
		assert.Empty(t, capturedQuery, "must never pass ?key= or any query string")
		assert.Equal(t, canaryKey, capturedHeaders.Get("x-goog-api-key"))
		assert.Empty(t, capturedHeaders.Get("Authorization"))
		assert.Equal(t, "application/json", capturedHeaders.Get("Content-Type"))
		assert.Empty(t, capturedHeaders.Get("Content-Encoding"))
		for hdr, vals := range capturedHeaders {
			if strings.EqualFold(hdr, "x-goog-api-key") {
				continue
			}
			for _, v := range vals {
				assert.NotContains(t, v, canaryKey, "unexpected secret in header %s", hdr)
			}
		}
		assert.NotContains(t, string(capturedBody), canaryKey)

		assert.Empty(t, res.ResolvedModel)
		assert.Equal(t, []string{provider.NoteModelUnreported}, res.Notes)
		assert.Equal(t, provider.Usage{InputTokens: 28, OutputTokens: 0}, res.Usage)
	})

	t.Run("error_mapping_table_content_free_and_no_retry", func(t *testing.T) {
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
				name:          "400_safety_string_code",
				status:        http.StatusBadRequest,
				body:          `{"error":{"code":"safety","message":"CANARY_400_SAFETY"}}`,
				wantReason:    provider.FailContentBlocked,
				wantRetryable: false,
			},
			{
				name:          "400_prohibited_content_string_code",
				status:        http.StatusBadRequest,
				body:          `{"error":{"code":"prohibited_content","message":"CANARY_400_PROHIBITED"}}`,
				wantReason:    provider.FailContentBlocked,
				wantRetryable: false,
			},
			{
				name:          "400_blocklist_string_code",
				status:        http.StatusBadRequest,
				body:          `{"error":{"code":"blocklist","message":"CANARY_400_BLOCKLIST"}}`,
				wantReason:    provider.FailContentBlocked,
				wantRetryable: false,
			},
			{
				name:          "400_image_safety_string_code",
				status:        http.StatusBadRequest,
				body:          `{"error":{"code":"image_safety","message":"CANARY_400_IMG_SAFETY"}}`,
				wantReason:    provider.FailContentBlocked,
				wantRetryable: false,
			},
			{
				name:          "400_content_blocked_string_code",
				status:        http.StatusBadRequest,
				body:          `{"error":{"code":"content_blocked","message":"CANARY_400_BLOCKED"}}`,
				wantReason:    provider.FailContentBlocked,
				wantRetryable: false,
			},
			{
				name:          "400_integer_invalid_argument",
				status:        http.StatusBadRequest,
				body:          `{"error":{"code":400,"message":"CANARY_400_INVALID_ARG","status":"INVALID_ARGUMENT"}}`,
				wantReason:    provider.FailInvalidRequest,
				wantRetryable: false,
			},
			{
				name:          "401_unauthenticated",
				status:        http.StatusUnauthorized,
				body:          `{"error":{"code":401,"message":"CANARY_401_KEY","status":"UNAUTHENTICATED"}}`,
				wantReason:    provider.FailAuth,
				wantRetryable: false,
			},
			{
				name:          "403_unrestricted_key_permission_denied",
				status:        http.StatusForbidden,
				body:          `{"error":{"code":403,"message":"CANARY_403_UNRESTRICTED_KEY","status":"PERMISSION_DENIED"}}`,
				wantReason:    provider.FailAuth,
				wantRetryable: false,
			},
			{
				name:          "404_model_not_found",
				status:        http.StatusNotFound,
				body:          `{"error":{"code":404,"message":"CANARY_404_MODEL","status":"NOT_FOUND"}}`,
				wantReason:    provider.FailInvalidRequest,
				wantRetryable: false,
			},
			{
				name:           "429_retry_after_header_preferred",
				status:         http.StatusTooManyRequests,
				headers:        map[string]string{"Retry-After": "6"},
				body:           `{"error":{"code":429,"message":"CANARY_429_HDR","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"2s"}]}}`,
				wantReason:     provider.FailRateLimited,
				wantRetryable:  true,
				wantRetryAfter: 6 * time.Second,
			},
			{
				name:           "429_retry_info_fallback",
				status:         http.StatusTooManyRequests,
				body:           `{"error":{"code":429,"message":"CANARY_429_DETAIL","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"3.5s"}]}}`,
				wantReason:     provider.FailRateLimited,
				wantRetryable:  true,
				wantRetryAfter: 3500 * time.Millisecond,
			},
			{
				name:          "500_internal",
				status:        http.StatusInternalServerError,
				body:          `{"error":{"code":500,"message":"CANARY_500_INTERNAL","status":"INTERNAL"}}`,
				wantReason:    provider.FailUnavailable,
				wantRetryable: true,
			},
			{
				name:          "503_unavailable",
				status:        http.StatusServiceUnavailable,
				body:          `{"error":{"code":503,"message":"CANARY_503_UNAVAIL","status":"UNAVAILABLE"}}`,
				wantReason:    provider.FailUnavailable,
				wantRetryable: true,
			},
			{
				name:          "504_gateway_timeout",
				status:        http.StatusGatewayTimeout,
				body:          `{"error":{"code":504,"message":"CANARY_504_DEADLINE","status":"DEADLINE_EXCEEDED"}}`,
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

		adapter := geminidecisions.New()
		baseReq := provider.ModelRequest{
			Rule: spec.Spec{
				Version:   1,
				Questions: []spec.Question{{ID: "q", Type: spec.PrimitiveBinary, Instructions: "Check."}},
			},
			Input: []provider.InputField{{Name: spec.ProbeFieldText, Kind: provider.FieldKindText, Value: "test", Trust: provider.TrustUntrusted}},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var hits atomic.Int32
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					if tc.dropConn {
						if hj, ok := w.(http.Hijacker); ok {
							if conn, _, err := hj.Hijack(); err == nil {
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

				tr := egress.NewClient(
					egress.StaticBearer(canaryKey),
					egress.WithCredentialPlacement(egress.HeaderPlacement(geminidecisions.HeaderAPIKey)),
					egress.WithLoopbackServerForTesting(srv.URL, srv.Client()),
				)
				inst := provider.InstanceView{Name: "gemini-main", Model: "gemini-decision-1", Endpoint: srv.URL}

				errMeta, err := adapter.Invoke(context.Background(), tr, inst, baseReq)
				require.Error(t, err)
				assert.Equal(t, int32(1), hits.Load(), "adapter must never retry inside Invoke")
				assert.Contains(t, errMeta.Notes, provider.NoteModelUnreported, "error return metadata must preserve model_unreported")

				pe, ok := provider.AsProviderError(err)
				require.True(t, ok)
				assert.Equal(t, tc.wantReason, pe.Reason)
				assert.Equal(t, tc.wantRetryable, pe.Retryable)
				if tc.wantRetryAfter > 0 {
					assert.Equal(t, tc.wantRetryAfter, pe.RetryAfter)
				}
				assert.NotContains(t, pe.Message, "CANARY_")
				assert.NotContains(t, err.Error(), "CANARY_")
				assert.NotContains(t, err.Error(), canaryKey)
			})
		}
	})

	t.Run("egress_sentinels_pass_through_unwrapped", func(t *testing.T) {
		adapter := geminidecisions.New()
		req := provider.ModelRequest{
			Rule: spec.Spec{
				Version:   1,
				Questions: []spec.Question{{ID: "q", Type: spec.PrimitiveBinary, Instructions: "Check."}},
			},
			Input: []provider.InputField{{Name: spec.ProbeFieldText, Kind: provider.FieldKindText, Value: "test", Trust: provider.TrustUntrusted}},
		}

		redirectSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://redirect.example/CANARY_REDIRECT")
			w.WriteHeader(http.StatusFound)
		}))
		defer redirectSrv.Close()

		tr := egress.NewClient(
			egress.StaticBearer(canaryKey),
			egress.WithCredentialPlacement(egress.HeaderPlacement(geminidecisions.HeaderAPIKey)),
			egress.WithLoopbackServerForTesting(redirectSrv.URL, redirectSrv.Client()),
		)

		_, err := adapter.Invoke(context.Background(), tr, provider.InstanceView{
			Name: "gemini-main", Model: "gemini-decision-1", Endpoint: redirectSrv.URL,
		}, req)
		require.Error(t, err)
		assert.True(t, egress.IsRedirectBlocked(err))
		_, isPE := provider.AsProviderError(err)
		assert.False(t, isPE)

		// Endpoint with query (?key=...) fails closed via egress.ErrInvalidEndpoint.
		_, err = adapter.Invoke(context.Background(), tr, provider.InstanceView{
			Name: "gemini-main", Model: "gemini-decision-1", Endpoint: redirectSrv.URL + "?key=leaked",
		}, req)
		require.Error(t, err)
		assert.True(t, egress.IsInvalidEndpoint(err))
	})
}
