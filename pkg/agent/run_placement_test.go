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

package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// placementGlobalHome points HOME at a temp dir; content, when non-empty,
// is written as the global settings file. No settings overlay is installed.
func placementGlobalHome(t *testing.T, content string) string {
	t.Helper()
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	if content != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644))
	}
	return dir
}

func managerOn(runtimeName string) *AgentManager {
	return &AgentManager{Runtime: &runtime.MockRuntime{NameFunc: func() string { return runtimeName }}}
}

// (12): a start whose placement the agent runtime server did not resolve
// fails on Kubernetes when placement is configured on a Kubernetes entry
// or profile, and proceeds without it otherwise.
func TestApplyStartPlacement_NilHandOff_12(t *testing.T) {
	agentBlock := &api.KubernetesConfig{Namespace: "ns", NodeSelector: map[string]string{"own": "v"}}

	cases := []struct {
		name     string
		settings string
		runtime  string
		wantErr  string
	}{
		{"entry placement", "schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\n    runtime_class_name: gvisor\n", "kubernetes", placementUnresolvedCause},
		{"profile placement", "schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\nprofiles:\n  p:\n    runtime: gke\n    tolerations:\n      - key: k\n        operator: Exists\n", "kubernetes", placementUnresolvedCause},
		{"profile placement on an undefined entry", "schema_version: \"1\"\nprofiles:\n  p:\n    runtime: missing\n    node_selector:\n      a: b\n", "kubernetes", placementUnresolvedCause},
		{"placement only on a docker entry", "schema_version: \"1\"\nruntimes:\n  dock:\n    type: docker\n    node_selector:\n      a: b\n", "kubernetes", ""},
		{"no placement", "schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\n", "kubernetes", ""},
		{"not Kubernetes", "schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\n    runtime_class_name: gvisor\n", "docker", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			placementGlobalHome(t, tc.settings)
			out, _, err := managerOn(tc.runtime).applyStartPlacement(api.StartOptions{Name: "a"}, agentBlock)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantErr, err.Error())
				return
			}
			require.NoError(t, err)
			assert.Equal(t, agentBlock, out, "proceeds with the agent's own block unchanged")
			assert.NotSame(t, agentBlock, out, "a copy, never the agent's block itself")
		})
	}
}

// (12b): a Kubernetes start without resolved placement fails with a fixed
// cause when the global settings cannot be read.
func TestApplyStartPlacement_NilHandOffUnreadableGlobal_12b(t *testing.T) {
	dir := placementGlobalHome(t, "")
	// A directory where the settings file is expected cannot be read.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "settings.yaml"), 0o755))
	_, _, err := managerOn("kubernetes").applyStartPlacement(api.StartOptions{Name: "a"}, nil)
	require.Error(t, err)
	assert.Equal(t, placementGlobalUnreadableCause, err.Error())

	// Not Kubernetes: unaffected.
	_, _, err = managerOn("docker").applyStartPlacement(api.StartOptions{Name: "a"}, nil)
	assert.NoError(t, err)
}

// A global settings file that cannot be stat'd (the loader treats it as
// missing) fails closed with the same fixed cause; a legacy-format file that
// mentions a placement key gets the legacy cause.
func TestApplyStartPlacement_NilHandOffUnstatableAndLegacy(t *testing.T) {
	dir := placementGlobalHome(t, "")
	require.NoError(t, os.Symlink("settings.yaml", filepath.Join(dir, "settings.yaml")))
	_, _, err := managerOn("kubernetes").applyStartPlacement(api.StartOptions{Name: "a"}, nil)
	require.Error(t, err)
	assert.Equal(t, placementGlobalUnreadableCause, err.Error())

	placementGlobalHome(t, "harnesses:\n  claude:\n    image: x\n# runtimeClassName: set here\n")
	_, _, err = managerOn("kubernetes").applyStartPlacement(api.StartOptions{Name: "a"}, nil)
	require.Error(t, err)
	assert.Equal(t, placementLegacyGlobalCause, err.Error())
}

// A resolved placement is applied over the agent's block on Kubernetes,
// with the integrity checks as a backstop, and dropped elsewhere.
func TestApplyStartPlacement_Resolved(t *testing.T) {
	placementGlobalHome(t, "schema_version: \"1\"\n")
	rp := &api.ResolvedPlacement{
		NodeSelector: map[string]string{"pool": "agents"},
		Tolerations:  []api.K8sToleration{{Key: "dedicated", Operator: "Exists", Effect: "NoSchedule"}},
	}
	opts := api.StartOptions{Name: "a", ResolvedPlacement: rp, PlacementResolved: true}
	agentBlock := &api.KubernetesConfig{Namespace: "ns", NodeSelector: map[string]string{"pool": "mine", "extra": "x"}}

	out, warnings, err := managerOn("kubernetes").applyStartPlacement(opts, agentBlock)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"pool": "agents", "extra": "x"}, out.NodeSelector)
	assert.Equal(t, rp.Tolerations, out.Tolerations)
	assert.Equal(t, "ns", out.Namespace)
	assert.Len(t, warnings, 1, "the agent's value for a key the placement sets is ignored with a warning")
	assert.Equal(t, map[string]string{"pool": "mine", "extra": "x"}, agentBlock.NodeSelector, "input not mutated")

	_, _, err = managerOn("kubernetes").applyStartPlacement(opts, &api.KubernetesConfig{
		Tolerations: []api.K8sToleration{{Operator: "Exists"}}})
	require.Error(t, err, "match-all agent toleration refused while placement is in effect")

	out, warnings, err = managerOn("docker").applyStartPlacement(opts, agentBlock)
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.Equal(t, agentBlock, out, "dropped off Kubernetes")

	// Resolved with nothing configured: the agent's block as is.
	out, _, err = managerOn("kubernetes").applyStartPlacement(api.StartOptions{Name: "a", PlacementResolved: true}, agentBlock)
	require.NoError(t, err)
	assert.Equal(t, agentBlock, out)
}
