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
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// loadInput reads a settings fixture as the effective legacy settings and
// its raw bytes, as the command does.
func loadInput(t *testing.T, name string) Input {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	vs := &config.VersionedSettings{}
	require.NoError(t, yamlv3.Unmarshal(raw, vs))
	return Input{Settings: vs, RawSettings: raw}
}

// golden compares got with testdata/<name>, rewriting it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden file; run the test with -update")
	assert.Equal(t, string(want), string(got))
}

func render(t *testing.T, p *Plan) (human, js []byte) {
	t.Helper()
	var h, j bytes.Buffer
	require.NoError(t, WriteHuman(&h, p, HumanOptions{}))
	require.NoError(t, WriteJSON(&j, p))
	return h.Bytes(), j.Bytes()
}

func TestBuildLocal_GoldenAndDeterministic(t *testing.T) {
	p, err := BuildLocal(loadInput(t, "four_profiles.yaml"))
	require.NoError(t, err)
	human, js := render(t, p)

	// A second build of the same input is byte-identical.
	p2, err := BuildLocal(loadInput(t, "four_profiles.yaml"))
	require.NoError(t, err)
	human2, js2 := render(t, p2)
	assert.Equal(t, string(human), string(human2))
	assert.Equal(t, string(js), string(js2))

	golden(t, "four_profiles.nohub.txt", human)
	golden(t, "four_profiles.nohub.json", js)

	// The podman profile has no flat target: blocking, unmapped, exit 3.
	var podman *ProfileMapping
	for i := range p.ProfileMap {
		if p.ProfileMap[i].Profile == "legacy-podman" {
			podman = &p.ProfileMap[i]
		}
	}
	require.NotNil(t, podman)
	assert.Empty(t, podman.Instance)
	assert.Equal(t, config.ConversionUnsupportedFlatTargetType, podman.BlockedBy)
	assert.Equal(t, 4, p.Summary.ProfilesInScope)
	assert.Equal(t, 3, p.Summary.MappedProfiles)
	assert.Equal(t, 3, len(p.Instances), "two differing docker profiles split; one kubernetes instance")
	assert.Equal(t, CapacityNeedsHub, p.Capacity.Status)
	assert.Nil(t, p.Agents)
	assert.Nil(t, p.Projects)
	assert.Equal(t, ResultBlockingFindings, p.Summary.Result)
	assert.Equal(t, ExitBlocked, p.ExitCode())
	assert.Contains(t, string(human), "(no mapping: unsupported_flat_target_type)")
}

// The JSON field names are the CLI contract (schemaVersion 1).
func TestPlanJSON_FieldNamesFrozen(t *testing.T) {
	p, err := BuildLocal(loadInput(t, "four_profiles.yaml"))
	require.NoError(t, err)
	_, js := render(t, p)
	var doc map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(js, &doc))
	var keys []string
	for k := range doc {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{
		"schemaVersion", "tipNote", "instances", "profileMap", "findings",
		"capacity", "agents", "projects", "summary",
	}, keys, "--skip-hub omits only legacyBrokerId and legacyBroker")
	assert.JSONEq(t, `1`, string(doc["schemaVersion"]))
	assert.JSONEq(t, `null`, string(doc["agents"]))
}

func TestBuildLocal_RefusesConfiguredInstances(t *testing.T) {
	_, err := BuildLocal(loadInput(t, "configured_instances.yaml"))
	assert.ErrorIs(t, err, ErrInstancesConfigured)
}

func TestBuildLocal_NothingToConvert(t *testing.T) {
	p, err := BuildLocal(Input{Settings: &config.VersionedSettings{}})
	require.NoError(t, err)
	assert.Equal(t, ResultNothingToConvert, p.Summary.Result)
	assert.Equal(t, ExitClean, p.ExitCode())
	human, _ := render(t, p)
	assert.Contains(t, string(human), "Nothing to convert")
}

func TestBuildLocal_ProfilesInScope(t *testing.T) {
	in := loadInput(t, "four_profiles.yaml")
	in.Profiles = []string{"k8s", "k8s"}
	p, err := BuildLocal(in)
	require.NoError(t, err)
	require.Len(t, p.ProfileMap, 1)
	assert.Equal(t, "k8s", p.ProfileMap[0].Profile)
	assert.NotEmpty(t, p.ProfileMap[0].Instance)
	assert.Len(t, p.Instances, 1)
	assert.Equal(t, ExitClean, p.ExitCode())
}

func TestBuildLocal_ValidSplitChoice(t *testing.T) {
	in := loadInput(t, "split_choice.yaml")
	choices, err := ParseChoices([]byte("- target_group: k8s-agents\n  split: [fast]\n"))
	require.NoError(t, err)
	in.Choices = choices
	p, err := BuildLocal(in)
	require.NoError(t, err)
	assert.Equal(t, 0, p.Summary.BlockingFindings)
	got := map[string]string{}
	for _, m := range p.ProfileMap {
		got[m.Profile] = m.Instance
	}
	assert.Equal(t, map[string]string{"fast": "k8s-agents-fast", "other": "k8s-agents", "slow": "k8s-agents"}, got)
	assert.Equal(t, ExitClean, p.ExitCode())
}

func TestBuildLocal_InvalidChoiceIsBlockingNotError(t *testing.T) {
	in := loadInput(t, "split_choice.yaml")
	choices, err := ParseChoices([]byte(`[{"target_group": "no-such-group", "split": ["fast"]}]`))
	require.NoError(t, err)
	in.Choices = choices
	p, err := BuildLocal(in)
	require.NoError(t, err)
	var found bool
	for _, f := range p.Findings {
		if f.Path == "choices[0]" && f.Severity == config.ConversionSeverityBlocking {
			found = true
		}
	}
	assert.True(t, found, "an invalid choice is a blocking finding at choices[0]: %+v", p.Findings)
	assert.Equal(t, ExitBlocked, p.ExitCode())
}

func TestParseChoices_Shape(t *testing.T) {
	_, err := ParseChoices([]byte("- target_group: g\n  no_such_key: x\n"))
	assert.Error(t, err, "an unknown key in the file is an error")
	_, err = ParseChoices([]byte("target_group: g\n"))
	assert.Error(t, err, "the file is a list")
	got, err := ParseChoices(nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	got, err = ParseChoices([]byte(`[{"target_group":"g","field":"shared_dir_size","chosen_profile":"a"}]`))
	require.NoError(t, err)
	assert.Equal(t, []config.ConversionChoice{{TargetGroup: "g", Field: "shared_dir_size", ChosenProfile: "a"}}, got)
}
