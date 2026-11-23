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
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/conformance"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func typeSafeSuiteConfig() conformance.SuiteConfig {
	return conformance.SuiteConfig{
		Auth: egress.BearerPlacement,
		ForbiddenHeaders: []string{
			jevwire.HeaderDangerZoneImages,
			jevwire.HeaderDangerZoneLongContext,
		},
		ForbiddenHeaderSubstrings: []string{"danger-zone"},
		DataChannelInspector:      inspectJevWireRequestBody,
	}
}

func inspectJevWireRequestBody(t testing.TB, body []byte, expectedInputs []string, expectedQIDs []string) {
	t.Helper()
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &top))
	assert.ElementsMatch(t, []string{"model", "state", "questions"}, mapKeys(top),
		"Jev request body must carry only model, state, and questions (AC-2)")

	var state map[string]any
	require.NoError(t, json.Unmarshal(top["state"], &state))
	assert.ElementsMatch(t, expectedInputs, mapKeys(state),
		"Jev request state must carry only the referenced input fields (AC-2, AC-3)")

	var questions map[string]any
	require.NoError(t, json.Unmarshal(top["questions"], &questions))
	assert.ElementsMatch(t, expectedQIDs, mapKeys(questions),
		"Jev request questions must carry only the requested question IDs")
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestTypeSafeConformance_FixturesF1ToF7 executes the offline conformance
// harness over synthetic-from-docs fixtures F1–F7 against the registered
// "typesafe" jevwire profile (P1 brief §3, §7 AC-6 & AC-PV1; provider-design
// §7.4, §9, §10 PV1, §12).
func TestTypeSafeConformance_FixturesF1ToF7(t *testing.T) {
	adapter, ok := provider.Lookup(jevwire.AdapterNameTypeSafe)
	require.True(t, ok, "typesafe adapter must be registered in provider.DefaultRegistry()")

	fixtureFiles := []string{
		"testdata/typesafe/f1_noul.json",
		"testdata/typesafe/f2_choice.json",
		"testdata/typesafe/f3_mixed.json",
		"testdata/typesafe/f4_models_and_aliases.json",
		"testdata/typesafe/f5_errors.json",
		"testdata/typesafe/f6_malformed.json",
		"testdata/typesafe/f7_request_goldens.json",
	}

	expectedOrder := []string{"F1", "F2", "F3", "F4", "F5", "F6", "F7"}
	cfg := typeSafeSuiteConfig()

	for i, rel := range fixtureFiles {
		bundle := conformance.LoadBundle(t, filepath.FromSlash(rel))
		require.Equal(t, expectedOrder[i], bundle.FixtureID)
		conformance.RunBundle(t, adapter, bundle, cfg)
	}
}

// TestTypeSafeConformance_AllFixtureFilesCarrySyntheticDisclaimer guards that
// all 7 TypeSafe fixtures exist under testdata/typesafe/ and that every JSON
// fixture file across all provider subdirectories under testdata/ cites its
// documentation source and explicitly states RequiredSyntheticNotice.
func TestTypeSafeConformance_AllFixtureFilesCarrySyntheticDisclaimer(t *testing.T) {
	typeSafeMatches, err := filepath.Glob(filepath.FromSlash("testdata/typesafe/*.json"))
	require.NoError(t, err)
	require.Len(t, typeSafeMatches, 7, "expected all 7 TypeSafe synthetic fixtures F1-F7")
	for _, path := range typeSafeMatches {
		bundle := conformance.LoadBundle(t, path)
		assert.Equal(t, jevwire.AdapterNameTypeSafe, bundle.Adapter, "unexpected adapter in %s", path)
		conformance.ValidateProvenance(t, path, bundle.Provenance)
	}

	allMatches, err := filepath.Glob(filepath.FromSlash("testdata/*/*.json"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(allMatches), len(typeSafeMatches))
	for _, path := range allMatches {
		bundle := conformance.LoadBundle(t, path)
		conformance.ValidateProvenance(t, path, bundle.Provenance)
	}
}

// TestTypeSafeConformance_CapabilitiesAndUntrustedInputIsolation verifies
// provider-design §12 AC-6 (sentinel in untrusted input appears only in the
// state data channel, never in instructions/criteria/options) and AC-10
// (declared capabilities).
func TestTypeSafeConformance_CapabilitiesAndUntrustedInputIsolation(t *testing.T) {
	adapter, ok := provider.Lookup(jevwire.AdapterNameTypeSafe)
	require.True(t, ok)

	inst := provider.InstanceView{
		Name:     "jev-conformance",
		Revision: 1,
		Model:    "jev-1.14.0",
	}
	conformance.RunCapabilitiesCheck(t, adapter, inst)

	// Compile through spec.Compile to verify full compile -> adapter.Validate -> Invoke compatibility.
	compiled, err := spec.Compile([]byte(`{
		"dialect": "system_one",
		"inputs": ["probe.text"],
		"params": {"domain": "billing"},
		"spec": {
			"version": 1,
			"form": "portable",
			"questions": [
				{
					"id": "billing",
					"type": "binary",
					"instructions": "Is `+"`probe.text`"+` about {domain}?",
					"criteria": {
						"true": "Concerns {domain}.",
						"false": "Does not concern {domain}."
					}
				},
				{
					"id": "tone",
					"type": "choice",
					"instructions": "Classify the tone of `+"`probe.text`"+`.",
					"options": [
						{"id": "angry", "description": "Upset or hostile"},
						{"id": "calm", "description": "Neutral or polite"}
					]
				}
			]
		}
	}`), spec.CompileOptions{Kind: spec.KindJudgments})
	require.NoError(t, err)
	require.NoError(t, adapter.Validate(inst, compiled.Spec))

	mockOK := conformance.MockHTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{"x-typesafe-request-id": "req_synth_sentinel"},
		RawBody:    `{"model":"jev-1.14.0","answers":{"billing":{"type":"noul","noul":0.1},"tone":{"type":"choice","choice":"calm","confidence":0.8,"probabilities":{"angry":0.1,"calm":0.9}}},"usage":{"input_tokens":80,"output_tokens":10}}`,
	}

	conformance.RunUntrustedInputIsolationCheck(
		t,
		adapter,
		inst,
		compiled.Spec,
		mockOK,
		typeSafeSuiteConfig(),
		func(t testing.TB, rawReq []byte) string {
			t.Helper()
			var parsed struct {
				Model     string          `json:"model"`
				Questions json.RawMessage `json:"questions"`
			}
			require.NoError(t, json.Unmarshal(rawReq, &parsed))
			return parsed.Model + "\n" + string(parsed.Questions)
		},
	)
}
