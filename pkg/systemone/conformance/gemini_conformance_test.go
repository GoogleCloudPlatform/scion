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
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/conformance"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/geminidecisions"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func geminiSuiteConfig() conformance.SuiteConfig {
	return conformance.SuiteConfig{
		Auth:                 egress.HeaderPlacement(geminidecisions.HeaderAPIKey),
		ForbiddenHeaders:     []string{"Authorization"},
		DataChannelInspector: inspectGeminiRequestBody,
	}
}

func inspectGeminiRequestBody(t testing.TB, body []byte, expectedInputs []string, expectedQIDs []string) {
	t.Helper()
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &top))
	assert.ElementsMatch(t, []string{"model", "input", "questions"}, mapKeys(top),
		"Gemini Decisions request body must carry only model, input, and questions (PV3 brief §5.2, AC-2)")

	// Verify input is serialized as a JSON string whose decoded object keys
	// match expectedInputs in sorted lexicographical order.
	var inputStr string
	require.NoError(t, json.Unmarshal(top["input"], &inputStr),
		"Gemini Decisions top-level input must be a serialized JSON string")

	var inputObj map[string]string
	require.NoError(t, json.Unmarshal([]byte(inputStr), &inputObj),
		"Gemini Decisions input string must contain a valid JSON object of input fields")
	assert.ElementsMatch(t, expectedInputs, mapKeys(inputObj),
		"Gemini Decisions input JSON must carry only the referenced input fields (AC-2, AC-3)")

	// Also verify key ordering inside inputStr is lexicographically sorted.
	dec := json.NewDecoder(strings.NewReader(inputStr))
	tok, err := dec.Token()
	require.NoError(t, err)
	require.Equal(t, json.Delim('{'), tok)
	var observedOrder []string
	for dec.More() {
		kTok, kErr := dec.Token()
		require.NoError(t, kErr)
		k, ok := kTok.(string)
		require.True(t, ok)
		observedOrder = append(observedOrder, k)
		var val string
		require.NoError(t, dec.Decode(&val))
	}
	wantSorted := append([]string(nil), expectedInputs...)
	sort.Strings(wantSorted)
	if len(wantSorted) == 0 {
		assert.Empty(t, observedOrder)
	} else {
		assert.Equal(t, wantSorted, observedOrder, "Gemini input JSON object keys must be in sorted order")
	}

	var questions map[string]any
	require.NoError(t, json.Unmarshal(top["questions"], &questions))
	assert.ElementsMatch(t, expectedQIDs, mapKeys(questions),
		"Gemini Decisions request questions must carry only the requested question IDs")
}

// TestGeminiConformance_FixturesG1ToG8 executes the offline conformance harness
// over synthetic-from-docs fixtures G1–G8 against the registered
// "gemini_decisions" adapter using header credential placement (PV3 brief §§4,
// 5, 7; engineering handoff).
func TestGeminiConformance_FixturesG1ToG8(t *testing.T) {
	adapter, ok := provider.Lookup(geminidecisions.AdapterName)
	require.True(t, ok, "gemini_decisions adapter must be registered in provider.DefaultRegistry()")

	fixtureFiles := []string{
		"testdata/gemini/g1_binary.json",
		"testdata/gemini/g2_binary_criteria.json",
		"testdata/gemini/g3_choice.json",
		"testdata/gemini/g4_mixed_provenance.json",
		"testdata/gemini/g5a_safety_block_400.json",
		"testdata/gemini/g5b_safety_block_200.json",
		"testdata/gemini/g6_errors.json",
		"testdata/gemini/g7_malformed.json",
		"testdata/gemini/g8_request_goldens.json",
	}
	expectedOrder := []string{"G1", "G2", "G3", "G4", "G5a", "G5b", "G6", "G7", "G8"}
	cfg := geminiSuiteConfig()

	for i, rel := range fixtureFiles {
		bundle := conformance.LoadBundle(t, filepath.FromSlash(rel))
		require.Equal(t, expectedOrder[i], bundle.FixtureID)
		conformance.RunBundle(t, adapter, bundle, cfg)
	}
}

// TestGeminiConformance_AllFixtureFilesCarrySyntheticDisclaimer guards that all
// 9 Gemini fixture files (G1–G8, with G5a and G5b) carry RequiredSyntheticNotice
// and documentation source citations.
func TestGeminiConformance_AllFixtureFilesCarrySyntheticDisclaimer(t *testing.T) {
	matches, err := filepath.Glob(filepath.FromSlash("testdata/gemini/*.json"))
	require.NoError(t, err)
	require.Len(t, matches, 9, "expected all 9 Gemini synthetic fixtures G1-G8 (including G5a, G5b)")

	for _, path := range matches {
		bundle := conformance.LoadBundle(t, path)
		assert.Equal(t, geminidecisions.AdapterName, bundle.Adapter, "unexpected adapter in %s", path)
		conformance.ValidateProvenance(t, path, bundle.Provenance)
	}
}

// TestGeminiConformance_CapabilitiesAndUntrustedInputIsolation verifies
// declared capabilities (including header:x-goog-api-key and gemini.confidence)
// and proves that untrusted input appears exclusively inside the serialized
// "input" string channel and never in model, questions, instructions, criteria,
// headers, or URL.
func TestGeminiConformance_CapabilitiesAndUntrustedInputIsolation(t *testing.T) {
	adapter, ok := provider.Lookup(geminidecisions.AdapterName)
	require.True(t, ok)

	_, implementsModelLister := adapter.(provider.ModelLister)
	assert.False(t, implementsModelLister, "gemini_decisions must not implement ModelLister (U-G1)")

	inst := provider.InstanceView{
		Name:     "gemini-conformance",
		Revision: 1,
		Model:    "gemini-decision-1",
	}
	conformance.RunCapabilitiesCheck(t, adapter, inst, "gemini")

	compiled, err := spec.Compile([]byte(`{
		"dialect": "system_one",
		"inputs": ["probe.text"],
		"params": {"domain": "hardware"},
		"spec": {
			"version": 1,
			"form": "portable",
			"questions": [
				{
					"id": "is_damaged",
					"type": "binary",
					"instructions": "Does `+"`probe.text`"+` report {domain} damage?",
					"criteria": {
						"true": "Reports physical {domain} damage.",
						"false": "Does not report physical {domain} damage."
					}
				},
				{
					"id": "department",
					"type": "choice",
					"instructions": "Route `+"`probe.text`"+` to a department.",
					"options": [
						{"id": "billing", "description": "Billing questions"},
						{"id": "support", "description": "Technical support"}
					]
				}
			]
		}
	}`), spec.CompileOptions{Kind: spec.KindJudgments})
	require.NoError(t, err)
	require.NoError(t, adapter.Validate(inst, compiled.Spec))

	mockOK := conformance.MockHTTPResponse{
		StatusCode: 200,
		RawBody:    `{"answers":{"is_damaged":{"type":"noul","noul":0.99,"confidence":0.99},"department":{"type":"choice","choice":"support","confidence":0.95,"probabilities":{"billing":0.05,"support":0.95}}},"usage":{"total_input_tokens":64,"total_output_tokens":0}}`,
	}

	conformance.RunUntrustedInputIsolationCheck(
		t,
		adapter,
		inst,
		compiled.Spec,
		mockOK,
		geminiSuiteConfig(),
		func(t testing.TB, rawReq []byte) string {
			t.Helper()
			var parsed struct {
				Model     string          `json:"model"`
				Input     string          `json:"input"`
				Questions json.RawMessage `json:"questions"`
			}
			require.NoError(t, json.Unmarshal(rawReq, &parsed))
			assert.NotEmpty(t, parsed.Input)
			return parsed.Model + "\n" + string(parsed.Questions)
		},
	)
}
