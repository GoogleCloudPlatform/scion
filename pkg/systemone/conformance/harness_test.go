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

package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/conformance"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubHeaderAdapter struct{}

func (stubHeaderAdapter) Name() string    { return "gemini_decisions" }
func (stubHeaderAdapter) Version() string { return "0.1.0-test" }

func (stubHeaderAdapter) Capabilities(inst provider.InstanceView) (provider.Capabilities, error) {
	if inst.Model == "" {
		return provider.Capabilities{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "model is required")
	}
	return provider.Capabilities{
		Primitives: map[string]provider.PrimitiveLimits{
			spec.PrimitiveBinary: {
				Supported:                true,
				DistributionCompleteness: provider.CompletenessComplete,
			},
			spec.PrimitiveChoice: {
				Supported:                true,
				MinOptions:               2,
				MaxOptions:               64,
				DistributionCompleteness: provider.CompletenessComplete,
			},
		},
		MaxQuestionsPerRequest: 64,
		MaxInputTokens:         262144,
		Truncation:             provider.TruncationNever,
		ReturnsDistribution:    true,
		NativeMetrics: []provider.MetricDecl{
			{
				Name:        "gemini.confidence",
				Unit:        metrics.UnitFraction,
				Min:         0,
				Max:         1,
				HigherMeans: "more_probable_top_outcome",
				Source:      metrics.SourceProviderDoc,
			},
		},
		Abstains:       false,
		SideEffectFree: true,
		AuthScheme: provider.AuthScheme{
			Kind:       provider.AuthKindHeader,
			HeaderName: "x-goog-api-key",
		},
	}, nil
}

func (a stubHeaderAdapter) Validate(inst provider.InstanceView, rule provider.RuleSpec) error {
	caps, err := a.Capabilities(inst)
	if err != nil {
		return err
	}
	cp := rule
	return spec.ValidatePortable(&cp, caps.SpecLimits())
}

func (stubHeaderAdapter) Invoke(ctx context.Context, tr provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
	inputMap := make(map[string]string, len(req.Input))
	for _, f := range req.Input {
		inputMap[f.Name] = f.Value
	}
	inputJSON, err := json.Marshal(inputMap)
	if err != nil {
		return provider.ModelResult{}, err
	}
	wirePayload, err := json.Marshal(struct {
		Model     string          `json:"model"`
		Input     string          `json:"input"`
		Questions []spec.Question `json:"questions"`
	}{
		Model:     inst.Model,
		Input:     string(inputJSON),
		Questions: req.Rule.Questions,
	})
	if err != nil {
		return provider.ModelResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, inst.Endpoint+"/v1beta/decisions", bytes.NewReader(wirePayload))
	if err != nil {
		return provider.ModelResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := tr.Do(httpReq)
	if err != nil {
		return provider.ModelResult{}, err
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return provider.ModelResult{HTTPStatus: resp.StatusCode},
			provider.NewProviderError(provider.FailAuth, false, 0, resp.StatusCode, "provider authentication rejected")
	}

	choice := "support"
	return provider.ModelResult{
		ResolvedModel: "",
		HTTPStatus:    resp.StatusCode,
		Usage:         provider.Usage{InputTokens: 48, OutputTokens: 0},
		Notes:         []string{provider.NoteModelUnreported},
		Answers: map[string]provider.Answer{
			"department": {
				QuestionID: "department",
				Primitive:  spec.PrimitiveChoice,
				Status:     provider.AnswerStatusAnswered,
				Choice:     &choice,
				Distribution: map[string]float64{
					"support": 0.99,
					"billing": 0.004,
					"sales":   0.003,
					"other":   0.002,
				},
				NativeMetrics: map[string]provider.Metric{
					"gemini.confidence": {
						Value:  0.99,
						Unit:   metrics.UnitFraction,
						Min:    0,
						Max:    1,
						Source: metrics.SourceProviderDoc,
					},
				},
				Notes: []string{provider.NoteOptionsSortedByProvider},
			},
		},
	}, nil
}

func TestHarness_SuiteConfigHeaderPlacementCapabilitiesAndIsolation(t *testing.T) {
	adapter := stubHeaderAdapter{}
	inst := provider.InstanceView{
		Name:     "gemini-conformance-stub",
		Revision: 1,
		Model:    "gemini-decision-1",
	}

	conformance.RunCapabilitiesCheck(t, adapter, inst)

	cfg := conformance.SuiteConfig{
		Auth: egress.HeaderPlacement("x-goog-api-key"),
	}

	deptRule := spec.Spec{
		Version: 1,
		Form:    spec.FormPortable,
		Questions: []spec.Question{
			{
				ID:           "department",
				Type:         spec.PrimitiveChoice,
				Instructions: "Route `probe.text` to a department.",
				Options: []spec.Option{
					{ID: "billing", Description: "Billing questions"},
					{ID: "sales", Description: "Sales inquiries"},
					{ID: "support", Description: "Technical support"},
					{ID: "other", Description: "Anything else"},
				},
			},
		},
	}

	choice := "support"
	bundle := conformance.FixtureBundle{
		FixtureID:   "G0-HARNESS",
		Description: "Harness self-test for SuiteConfig.Auth header placement and 0.999 float-boundary choice example.",
		Adapter:     "gemini_decisions",
		Provenance: conformance.Provenance{
			Notice:  conformance.RequiredSyntheticNotice,
			Sources: []string{"https://ai.google.dev/gemini-api/docs/decisions-api"},
		},
		InvokeCases: []conformance.InvokeCase{
			{
				Name:         "header_auth_and_0999_choice_distribution",
				Model:        "gemini-decision-1",
				ExpectedPath: "/v1beta/decisions",
				Request: provider.ModelRequest{
					Rule: deptRule,
					Input: []provider.InputField{
						{Name: spec.ProbeFieldText, Kind: provider.FieldKindText, Value: "screen cracked", Trust: provider.TrustUntrusted},
					},
				},
				MockResponse: conformance.MockHTTPResponse{
					StatusCode: http.StatusOK,
					RawBody:    `{"answers":{}}`,
				},
				ExpectResult: &conformance.ExpectedResult{
					ResolvedModel: "",
					HTTPStatus:    http.StatusOK,
					Usage:         provider.Usage{InputTokens: 48, OutputTokens: 0},
					Notes:         []string{provider.NoteModelUnreported},
					Answers: map[string]conformance.ExpectedAnswer{
						"department": {
							Primitive: spec.PrimitiveChoice,
							Status:    provider.AnswerStatusAnswered,
							Choice:    &choice,
							Distribution: map[string]float64{
								"support": 0.99,
								"billing": 0.004,
								"sales":   0.003,
								"other":   0.002,
							},
							NativeMetrics: map[string]metrics.Metric{
								"gemini.confidence": {
									Value:  0.99,
									Unit:   metrics.UnitFraction,
									Min:    0,
									Max:    1,
									Source: metrics.SourceProviderDoc,
								},
							},
							Notes: []string{provider.NoteOptionsSortedByProvider},
						},
					},
				},
			},
			{
				Name:         "header_auth_401_error_never_leaks_canary",
				Model:        "gemini-decision-1",
				ExpectedPath: "/v1beta/decisions",
				Request: provider.ModelRequest{
					Rule: deptRule,
					Input: []provider.InputField{
						{Name: spec.ProbeFieldText, Kind: provider.FieldKindText, Value: "screen cracked", Trust: provider.TrustUntrusted},
					},
				},
				MockResponse: conformance.MockHTTPResponse{
					StatusCode: http.StatusUnauthorized,
					RawBody:    `{"error":{"code":401,"message":"CANARY_401_UNRESTRICTED_KEY"}}`,
				},
				ExpectError: &conformance.ExpectedError{
					Reason:          provider.FailAuth,
					Retryable:       false,
					HTTPStatus:      http.StatusUnauthorized,
					ErrorBodyCanary: "CANARY_401_UNRESTRICTED_KEY",
				},
			},
		},
	}

	conformance.RunBundle(t, adapter, bundle, cfg)

	conformance.RunUntrustedInputIsolationCheck(
		t,
		adapter,
		inst,
		deptRule,
		conformance.MockHTTPResponse{StatusCode: http.StatusOK, RawBody: `{}`},
		cfg,
		func(t testing.TB, rawReq []byte) string {
			t.Helper()
			var parsed struct {
				Model     string          `json:"model"`
				Questions json.RawMessage `json:"questions"`
			}
			require.NoError(t, json.Unmarshal(rawReq, &parsed))
			assert.Equal(t, "gemini-decision-1", parsed.Model)
			return parsed.Model + "\n" + string(parsed.Questions)
		},
	)
}

func TestHarness_ZeroSuiteConfigAuthDefaultsToBearer(t *testing.T) {
	adapter, ok := provider.Lookup(jevwire.AdapterNameTypeSafe)
	require.True(t, ok)

	bundle := conformance.LoadBundle(t, filepath.FromSlash("testdata/typesafe/f1_noul.json"))
	// Omitting SuiteConfig.Auth (zero Placement{}) continues to default to Bearer.
	conformance.RunBundle(t, adapter, bundle, conformance.SuiteConfig{})
}
