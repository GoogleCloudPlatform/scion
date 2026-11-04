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

package brokerconvert

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The planted_values.yaml fixture plants a distinctive value in every place
// a settings value could leak from: harness env values that differ between
// two profiles on one target (so the converter's conflict message would
// quote them), a runtime entry env value holding a kubeconfig path, a
// removed profile-level env value, a context and conflicting registries.
const (
	plantedEnvA       = "PLANTED-ENV-TOKEN-A-c43"
	plantedEnvB       = "PLANTED-ENV-TOKEN-B-c45"
	plantedKubeconfig = "PLANTED-KUBECONFIG-c42"
	plantedProfileEnv = "PLANTED-PROFILE-ENV-c44"
	plantedContext    = "PLANTED-CONTEXT-c41"
	plantedPrefix     = "PLANTED-"
)

func plantedPlan(t *testing.T) *Plan {
	t.Helper()
	p, err := BuildLocal(loadInput(t, "planted_values.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, p.Findings, "the fixture yields findings")
	require.NotEmpty(t, p.settingsInstances, "the fixture yields instances")
	return p
}

func humanOut(t *testing.T, p *Plan, opts HumanOptions) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, WriteHuman(&b, p, opts))
	return b.String()
}

func jsonOut(t *testing.T, p *Plan) string {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, WriteJSON(&b, p))
	return b.String()
}

// section returns the human output between two section headings.
func section(t *testing.T, out, from, to string) string {
	t.Helper()
	i := strings.Index(out, from)
	j := strings.Index(out, to)
	require.True(t, i >= 0 && j > i, "sections %q and %q in order", from, to)
	return out[i:j]
}

func TestRedaction_HumanDefaultRedactsEnvValuesKeepsPasteReadyValues(t *testing.T) {
	out := humanOut(t, plantedPlan(t), HumanOptions{})
	assert.NotContains(t, out, plantedEnvA)
	assert.NotContains(t, out, plantedEnvB)
	assert.NotContains(t, out, plantedKubeconfig)
	assert.NotContains(t, out, plantedProfileEnv)
	assert.Contains(t, out, "API_TOKEN: "+RedactedValue, "the env key is kept, its value redacted")
	assert.Contains(t, out, plantedContext, "the paste-ready block keeps the context")
	assert.Contains(t, out, "reg.example/PLANTED-REGISTRY-A", "the paste-ready block keeps the registry")
	assert.NotContains(t, out, ShowEnvValuesWarning)
	assert.Contains(t, out, TipNote, "the tip note states the redaction and the flag")
}

func TestRedaction_ShowEnvValuesPrintsThemInHumanYAMLOnly(t *testing.T) {
	p := plantedPlan(t)
	// A default render first: redaction must not change the plan itself.
	_ = humanOut(t, p, HumanOptions{})
	out := humanOut(t, p, HumanOptions{ShowEnvValues: true})
	assert.Contains(t, out, ShowEnvValuesWarning)
	yaml := section(t, out, "1. Proposed instances", "2. Profile map")
	assert.Contains(t, yaml, plantedEnvA)
	assert.Contains(t, yaml, plantedEnvB)
	assert.NotContains(t, yaml, RedactedValue)
	// Only the settings block shows them; findings never do.
	assert.NotContains(t, section(t, out, "3. Findings", "4. Capacity"), plantedPrefix)
}

func TestRedaction_JSONCarriesNoSettingsValueOnlyThePresentation(t *testing.T) {
	p := plantedPlan(t)
	out := jsonOut(t, p)
	for _, v := range []string{plantedEnvA, plantedEnvB, plantedKubeconfig, plantedProfileEnv, plantedContext} {
		assert.NotContains(t, out, v)
	}
	var doc struct {
		Instances []map[string]json.RawMessage `json:"instances"`
		Findings  []map[string]json.RawMessage `json:"findings"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	require.NotEmpty(t, doc.Instances)
	for _, inst := range doc.Instances {
		_, settingsShape := inst["runtime_target"]
		assert.False(t, settingsShape, "instances are the publishable presentation, not the settings shape")
		assert.Contains(t, inst, "schemaVersion")
	}
	assert.Contains(t, out, `"envKeys"`, "env keys are published, values never")
	for _, f := range doc.Findings {
		var keys []string
		for k := range f {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t, []string{"code", "severity", "path", "profiles", "text"}, keys)
	}
}

func TestRedaction_FindingsNeverQuoteValues(t *testing.T) {
	p := plantedPlan(t)
	var codes []string
	for _, f := range p.Findings {
		codes = append(codes, f.Code)
		blob := f.Path + " " + f.Text + " " + strings.Join(f.Profiles, " ")
		assert.NotContains(t, blob, plantedPrefix, "finding %s at %s", f.Code, f.Path)
		assert.Equal(t, FindingText(f.Code), f.Text)
	}
	assert.Contains(t, codes, "profile_conflict", "the conflicting values produce a conflict finding")
	assert.Contains(t, codes, "legacy_field_no_effect", "the runtime entry env is reported")
	findingsJSON, err := json.Marshal(p.Findings)
	require.NoError(t, err)
	assert.NotContains(t, string(findingsJSON), plantedPrefix)
	assert.NotContains(t, section(t, humanOut(t, p, HumanOptions{}), "3. Findings", "4. Capacity"), plantedPrefix)
}

func TestFindingText_EveryCodeHasFixedText(t *testing.T) {
	for code := range findingTexts {
		assert.NotEmpty(t, FindingText(code))
	}
	assert.Len(t, findingTexts, 8, "one text per frozen converter code")
	assert.NotEmpty(t, FindingText("some_new_code"))
}

// TestRedaction_PresentationCarriesNoSettingsValue extends the redaction
// test to the snapshot a legacy Runtime Broker publishes to the Hub: no
// planted settings value appears anywhere in it, findings carry no text,
// and every path is the converter's key path.
func TestRedaction_PresentationCarriesNoSettingsValue(t *testing.T) {
	p := plantedPlan(t)
	pres := Presentation(p, testComputedAt)
	b, err := json.Marshal(pres)
	require.NoError(t, err)
	out := string(b)
	for _, v := range []string{plantedEnvA, plantedEnvB, plantedKubeconfig, plantedProfileEnv, plantedContext} {
		assert.NotContains(t, out, v)
	}
	findings, err := json.Marshal(pres.Findings)
	require.NoError(t, err)
	assert.NotContains(t, string(findings), plantedPrefix, "findings never quote a value")
	require.Len(t, pres.Findings, len(p.Findings))
	for i, f := range pres.Findings {
		assert.Equal(t, p.Findings[i].Path, f.Path)
		assert.NotContains(t, out, p.Findings[i].Text)
	}
}
