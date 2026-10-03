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

package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveAgentLimit(t *testing.T) {
	vs := &VersionedSettings{
		ActiveProfile: "capped",
		Runtimes: map[string]V1RuntimeConfig{
			"k8s":    {Type: "kubernetes", MaxAgents: 10},
			"docker": {Type: "docker"},
			"neg":    {Type: "docker", MaxAgents: -2},
		},
		Profiles: map[string]V1ProfileConfig{
			"capped":   {Runtime: "docker", MaxAgents: 3},
			"inherit":  {Runtime: "k8s"},
			"override": {Runtime: "k8s", MaxAgents: 4},
			"zero":     {Runtime: "docker", MaxAgents: 0},
			"negative": {Runtime: "k8s", MaxAgents: -1},
			"negrt":    {Runtime: "neg"},
		},
	}
	tests := []struct {
		profile    string
		wantLimit  int
		wantSource string
	}{
		{"capped", 3, "profiles.capped.max_agents"},
		{"inherit", 10, "runtimes.k8s.max_agents"},
		{"override", 4, "profiles.override.max_agents"},
		{"zero", 0, ""},
		// A negative profile value is unset, so the runtime entry applies.
		{"negative", 10, "runtimes.k8s.max_agents"},
		{"negrt", 0, ""},
		{"unknown", 0, ""},
		// Empty does not fall back to the hub's active profile.
		{"", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.profile, func(t *testing.T) {
			limit, source := vs.ResolveAgentLimit(tt.profile)
			assert.Equal(t, tt.wantLimit, limit)
			assert.Equal(t, tt.wantSource, source)
		})
	}

	var nilVS *VersionedSettings
	limit, source := nilVS.ResolveAgentLimit("capped")
	assert.Zero(t, limit)
	assert.Empty(t, source)
}

func TestValidateSettings_MaxAgents(t *testing.T) {
	const okYAML = `schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    max_agents: 10
profiles:
  gke:
    runtime: k8s
    max_agents: 0
`
	errs, err := ValidateSettings([]byte(okYAML), "1")
	require.NoError(t, err)
	assert.Empty(t, errs)

	for _, bad := range []string{
		"schema_version: \"1\"\nruntimes:\n  k8s:\n    type: kubernetes\n    max_agents: -1\n",
		"schema_version: \"1\"\nprofiles:\n  gke:\n    runtime: k8s\n    max_agents: -1\n",
	} {
		errs, err := ValidateSettings([]byte(bad), "1")
		require.NoError(t, err)
		assert.NotEmpty(t, errs, "a negative max_agents must be rejected: %s", bad)
	}
}
