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

package spec_test

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuestionHash_GoldenDigests(t *testing.T) {
	// Golden 1: binary question with criteria and {topic} param in probe v1.
	// Canonical JSON:
	// {"criteria":{"false":"Unrelated.","true":"Mentions café billing."},"environment":{"name":"probe","version":1},"inputs":["probe.text"],"instructions":"Is probe.text about café?","type":"binary"}
	const wantBinaryGolden = "sha256:7d8e490f4c7ae384fad0a89bfa6700a925c09a11882a9cf6f7f216f0914655e2"

	qBin := spec.Question{
		ID:           "asks_billing",
		Type:         spec.PrimitiveBinary,
		Instructions: "Is probe.text about {topic}?",
		Criteria: &spec.BinaryCriteria{
			True:  "Mentions caf\u00e9 billing.",
			False: "Unrelated.",
		},
	}
	gotBin, err := spec.QuestionHash(qBin, spec.HashContext{
		Params:     map[string]string{"topic": "caf\u00e9"},
		Inputs:     []string{"probe.text"},
		EnvName:    "probe",
		EnvVersion: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, wantBinaryGolden, gotBin)

	// Golden 2: 3-option choice question with abstain in probe v1.
	// Canonical JSON:
	// {"environment":{"name":"probe","version":1},"inputs":["probe.text"],"instructions":"Classify probe.text.","options":[{"description":"Invoices and payments","id":"billing"},{"description":"Bugs and code","id":"engineering"},{"abstain":true,"description":"Insufficient context","id":"cannot_tell"}],"type":"choice"}
	const wantChoiceGolden = "sha256:77b9c642a81138cb79686aedd416f157e6b743e25124137766563775bd846380"

	qChoice := spec.Question{
		ID:           "topic",
		Type:         spec.PrimitiveChoice,
		Instructions: "Classify probe.text.",
		Options: []spec.Option{
			{ID: "billing", Description: "Invoices and payments"},
			{ID: "engineering", Description: "Bugs and code"},
			{ID: "cannot_tell", Description: "Insufficient context", Abstain: true},
		},
	}
	gotChoice, err := spec.QuestionHash(qChoice, spec.HashContext{
		Inputs:     []string{"probe.text"},
		EnvName:    "probe",
		EnvVersion: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, wantChoiceGolden, gotChoice)
}

func TestQuestionHash_StableAcrossKeyOrderAndUnicodeNFC(t *testing.T) {
	// Spec A uses precomposed UTF-8 (NFC: U+00E9) and canonical key order.
	rawA := []byte(`{
		"dialect": "system_one",
		"inputs": ["probe.text"],
		"params": {"alpha": "r\u00e9sum\u00e9", "beta": "caf\u00e9"},
		"spec": {
			"version": 1,
			"questions": [{
				"id": "q_order",
				"type": "choice",
				"instructions": "Compare {alpha} and {beta}.",
				"options": [
					{"id": "first", "description": "Premi\u00e8re option", "abstain": false},
					{"id": "second", "description": "Deuxi\u00e8me option", "abstain": true}
				]
			}]
		}
	}`)

	// Spec B reverses every JSON object key order, omits default "abstain": false,
	// and uses decomposed NFD combining accents (e + U+0301, e + U+0300).
	rawB := []byte(`{
		"spec": {
			"questions": [{
				"options": [
					{"description": "Premie\u0300re option", "id": "first"},
					{"abstain": true, "description": "Deuxie\u0300me option", "id": "second"}
				],
				"instructions": "Compare {alpha} and {beta}.",
				"type": "choice",
				"id": "q_order"
			}],
			"version": 1
		},
		"params": {"beta": "cafe\u0301", "alpha": "re\u0301sume\u0301"},
		"inputs": ["probe.text"],
		"dialect": "system_one"
	}`)

	compA, err := spec.Compile(rawA, spec.CompileOptions{Kind: spec.KindJudgments})
	require.NoError(t, err)
	compB, err := spec.Compile(rawB, spec.CompileOptions{Kind: spec.KindJudgments})
	require.NoError(t, err)

	assert.Equal(t, compA.QuestionHashes["q_order"], compB.QuestionHashes["q_order"])

	// Input field set order is also canonicalized.
	h1, err := spec.QuestionHash(compA.Spec.Questions[0], spec.HashContext{
		Inputs:     []string{"b.field", "a.field"},
		EnvName:    "message_policy",
		EnvVersion: 1,
	})
	require.NoError(t, err)
	h2, err := spec.QuestionHash(compA.Spec.Questions[0], spec.HashContext{
		Inputs:     []string{"a.field", "b.field"},
		EnvName:    "message_policy",
		EnvVersion: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, h1, h2)
}

func TestQuestionHash_ChangesWithOptionOrderAndSemanticInputs(t *testing.T) {
	base := spec.Question{
		ID:           "topic",
		Type:         spec.PrimitiveChoice,
		Instructions: "Pick one for {team}.",
		Options: []spec.Option{
			{ID: "billing", Description: "Invoices"},
			{ID: "engineering", Description: "Code"},
		},
	}
	baseCtx := spec.HashContext{
		Params:     map[string]string{"team": "core"},
		Inputs:     []string{"probe.text"},
		EnvName:    "probe",
		EnvVersion: 1,
	}
	hBase, err := spec.QuestionHash(base, baseCtx)
	require.NoError(t, err)

	// Reversing option order MUST change QuestionHash (provider-design §5.4, AC-PV1).
	reordered := base
	reordered.Options = []spec.Option{
		{ID: "engineering", Description: "Code"},
		{ID: "billing", Description: "Invoices"},
	}
	hReordered, err := spec.QuestionHash(reordered, baseCtx)
	require.NoError(t, err)
	assert.NotEqual(t, hBase, hReordered, "swapping option order must change QuestionHash")

	// Flipping abstain flag changes hash.
	withAbstain := base
	withAbstain.Options = []spec.Option{
		{ID: "billing", Description: "Invoices"},
		{ID: "engineering", Description: "Code", Abstain: true},
	}
	hAbstain, err := spec.QuestionHash(withAbstain, baseCtx)
	require.NoError(t, err)
	assert.NotEqual(t, hBase, hAbstain)

	// Changing a substituted param value changes hash.
	otherParamCtx := baseCtx
	otherParamCtx.Params = map[string]string{"team": "infra"}
	hOtherParam, err := spec.QuestionHash(base, otherParamCtx)
	require.NoError(t, err)
	assert.NotEqual(t, hBase, hOtherParam)

	// Changing ContentEnvironment version changes hash.
	otherEnvCtx := baseCtx
	otherEnvCtx.EnvVersion = 2
	hOtherEnv, err := spec.QuestionHash(base, otherEnvCtx)
	require.NoError(t, err)
	assert.NotEqual(t, hBase, hOtherEnv)
}
