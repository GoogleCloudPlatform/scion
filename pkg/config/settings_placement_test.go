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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Settings-level tests for the Kubernetes placement defaults
// (ptone/scion#5085). Test IDs are from the placement design's catalogue.

// writePlacementGlobalSettings points HOME at a temp dir holding a global
// settings file with content, with no settings overlay installed.
func writePlacementGlobalSettings(t *testing.T, content string) {
	t.Helper()
	old := GetGlobalSettingsOverlay()
	t.Cleanup(func() { SetGlobalSettingsOverlay(old) })
	SetGlobalSettingsOverlay(nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644))
	// A project found from the working directory must not take part.
	wd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(home))
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

func anyContains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// V1w: PlacementSettingsWarnings and PlacementIgnoredWarnings.
func TestPlacementWarnings_V1w(t *testing.T) {
	runtimes := map[string]V1RuntimeConfig{
		"gke": {
			Type: "kubernetes", GKE: true,
			NodeSelector: map[string]string{
				"scion-workload":                "agents", // paired below
				"pool":                          "a",      // no same-key toleration
				"cloud.google.com/gke-nodepool": "p",      // exempt prefix
				WorkloadIdentityNodeLabel:       "true",   // WI key
			},
			Tolerations: []api.K8sToleration{
				{Key: "scion-workload", Operator: "Equal", Value: "agents", Effect: "NoSchedule"},
				{Operator: "Exists"}, // match-all
			},
		},
		"plain": {Type: "kubernetes", NodeSelector: map[string]string{"pool": "b"}}, // gke false: no pairing
		"dock":  {Type: "docker", NodeSelector: map[string]string{"pool": "c"}},
		"dock2": {Type: "docker"},
	}
	profiles := map[string]V1ProfileConfig{
		"gpu":     {Runtime: "gke", NodeSelector: map[string]string{"gpu": "yes", "scion-workload": "agents"}},
		"onDock":  {Runtime: "dock", RuntimeClassName: "gvisor"},
		"noPlace": {Runtime: "dock2"},
	}

	w := PlacementSettingsWarnings(runtimes, profiles)
	assert.True(t, anyContains(w, `runtimes.gke.node_selector sets "iam.gke.io/gke-metadata-server-enabled"`), "WI-key warning: %v", w)
	assert.True(t, anyContains(w, "runtimes.gke.tolerations[1] has an empty key with operator Exists"), "match-all warning: %v", w)
	assert.True(t, anyContains(w, `runtimes.gke.node_selector key "pool" has no toleration`), "pairing warning: %v", w)
	assert.False(t, anyContains(w, `key "scion-workload" has no toleration`), "paired key warned: %v", w)
	assert.False(t, anyContains(w, `key "cloud.google.com/gke-nodepool"`), "exempt prefix warned: %v", w)
	assert.True(t, anyContains(w, `profiles.gpu.node_selector key "gpu" has no toleration`), "profile pairing warning: %v", w)
	assert.False(t, anyContains(w, "runtimes.plain"), "gke false entry warned: %v", w)
	assert.False(t, anyContains(w, "runtimes.dock"), "non-Kubernetes entry in settings warnings: %v", w)

	ig := PlacementIgnoredWarnings(runtimes, profiles)
	assert.Equal(t, []string{
		`profiles.onDock sets node_selector, tolerations or runtime_class_name but its runtime "dock" is not a Kubernetes runtime; they are ignored`,
		`runtimes.dock sets node_selector, tolerations or runtime_class_name but runtime "dock" is not a Kubernetes runtime; they are ignored`,
	}, ig)
}

// ValidatePlacement: malformed values carry the key path; siblings must
// agree (order-insensitive tolerations, nil and empty selectors equal).
func TestValidatePlacement_ValuesAndSiblings(t *testing.T) {
	runtimes := map[string]V1RuntimeConfig{
		"a": {Type: "kubernetes", NodeSelector: map[string]string{"bad key!": "x"}},
		"b": {Type: "kubernetes", Tolerations: []api.K8sToleration{{Key: "", Operator: "Equal"}}},
		"c": {Type: "kubernetes", RuntimeClassName: "Not_DNS"},
		"s1": {Type: "kubernetes", Context: "ctx", Namespace: "ns",
			Tolerations: []api.K8sToleration{{Key: "x", Operator: "Exists"}, {Key: "y", Value: "v"}}},
		"s2": {Type: "kubernetes", Context: "ctx", Namespace: "ns", NodeSelector: map[string]string{},
			Tolerations: []api.K8sToleration{{Key: "y", Operator: "Equal", Value: "v"}, {Key: "x", Operator: "Exists"}}},
	}
	profiles := map[string]V1ProfileConfig{"p": {Runtime: "a", NodeSelector: map[string]string{"k": "bad value!"}}}
	errs := ValidatePlacement(runtimes, profiles, [][]string{{"s1", "s2"}})
	paths := map[string]bool{}
	for _, e := range errs {
		paths[e.Path] = true
	}
	assert.True(t, paths["runtimes.a.node_selector"], "%v", errs)
	assert.True(t, paths["runtimes.b.tolerations"], "%v", errs)
	assert.True(t, paths["runtimes.c.runtime_class_name"], "%v", errs)
	assert.True(t, paths["profiles.p.node_selector"], "%v", errs)
	assert.False(t, paths["runtimes.s1"], "agreeing siblings reported: %v", errs)
	assert.Len(t, errs, 4)

	runtimes["s2"] = V1RuntimeConfig{Type: "kubernetes", Context: "ctx", Namespace: "ns", RuntimeClassName: "gvisor"}
	errs = ValidatePlacement(runtimes, nil, [][]string{{"s1", "s2"}, {"s2", "s1"}})
	var sib []ValidationError
	for _, e := range errs {
		if e.Path == "runtimes.s1" {
			sib = append(sib, e)
		}
	}
	require.Len(t, sib, 1, "one error per disagreeing pair: %v", errs)
	assert.Equal(t, "runtime entries s1, s2 target the same cluster and namespace with different placement; identity-matched entries must agree (put variants in profiles)", sib[0].Message)
}

// G1a: literal siblings with different placement fail ValidateSettings.
func TestValidateSettings_LiteralSiblingsDisagree_G1a(t *testing.T) {
	data := []byte(`schema_version: "1"
runtimes:
  gke-a:
    type: kubernetes
    context: ctx
    namespace: agents
    node_selector:
      cloud.google.com/gke-nodepool: pool-a
  gke-b:
    type: kubernetes
    context: ctx
    namespace: agents
    node_selector:
      cloud.google.com/gke-nodepool: pool-b
  other:
    type: kubernetes
    context: ctx
    namespace: elsewhere
    runtime_class_name: gvisor
`)
	errs, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	var found bool
	for _, e := range errs {
		if strings.Contains(e.Message, "runtime entries gke-a, gke-b target the same cluster and namespace with different placement") {
			found = true
		}
		assert.NotContains(t, e.Message, "other", "a different namespace is not a sibling: %v", e)
	}
	assert.True(t, found, "want a sibling disagreement error, got %v", errs)

	// Agreeing literal siblings, and the new keys themselves, validate.
	ok := []byte(`schema_version: "1"
runtimes:
  gke-a:
    type: kubernetes
    context: ctx
    namespace: agents
    node_selector:
      cloud.google.com/gke-nodepool: pool-a
    tolerations:
      - key: dedicated
        operator: Equal
        value: agents
        effect: NoSchedule
    runtime_class_name: gvisor
  gke-b:
    type: kubernetes
    context: ctx
    namespace: agents
    node_selector:
      cloud.google.com/gke-nodepool: pool-a
    tolerations:
      - key: dedicated
        value: agents
        effect: NoSchedule
    runtime_class_name: gvisor
profiles:
  gpu:
    runtime: gke-a
    node_selector:
      cloud.google.com/gke-accelerator: nvidia-l4
    tolerations:
      - key: nvidia.com/gpu
        operator: Exists
        effect: NoSchedule
    runtime_class_name: gvisor
`)
	errs, err = ValidateSettings(ok, "1")
	require.NoError(t, err)
	assert.Empty(t, errs)
}

// (17): how the loader treats the spelling of the three keys. The
// snake_case keys load. Keys differing only in letter case also decode
// (the settings decoder matches field names case-insensitively), though
// the schema rejects them. camelCase spellings are not settings keys and
// load nothing.
func TestPlacementKeyCase_17(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantLoads bool
	}{
		{"snake_case", "node_selector:\n      pool: a\n    tolerations:\n      - key: k\n        operator: Exists\n    runtime_class_name: gvisor\n", true},
		{"upper case", "NODE_SELECTOR:\n      pool: a\n    TOLERATIONS:\n      - key: k\n        operator: Exists\n    RUNTIME_CLASS_NAME: gvisor\n", true},
		{"camelCase", "nodeSelector:\n      pool: a\n    runtimeClassName: gvisor\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writePlacementGlobalSettings(t, "schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\n    "+tc.body)
			vs, _, err := LoadGlobalSettingsWithOverlay()
			require.NoError(t, err)
			rt := vs.Runtimes["gke"]
			if tc.wantLoads {
				assert.Equal(t, map[string]string{"pool": "a"}, rt.NodeSelector)
				assert.Equal(t, []api.K8sToleration{{Key: "k", Operator: "Exists"}}, rt.Tolerations)
				assert.Equal(t, "gvisor", rt.RuntimeClassName)
			} else {
				assert.Empty(t, rt.NodeSelector)
				assert.Empty(t, rt.Tolerations)
				assert.Empty(t, rt.RuntimeClassName)
			}
		})
	}

	// Validation accepts only the snake_case keys.
	for _, key := range []string{"NODE_SELECTOR", "nodeSelector", "runtimeClassName"} {
		errs, err := ValidateSettings([]byte("schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\n    "+key+": {}\n"), "1")
		require.NoError(t, err)
		assert.NotEmpty(t, errs, "schema accepted %s", key)
	}
}

// D1: a dotted, slashed label key and a toleration list survive the file
// load and the settings overlay; {} and [] behave as unset.
func TestPlacementSurvivesLoadAndOverlay_D1(t *testing.T) {
	writePlacementGlobalSettings(t, `schema_version: "1"
runtimes:
  gke:
    type: kubernetes
    context: ctx
    node_selector:
      cloud.google.com/gke-nodepool: scion-agents
    tolerations:
      - key: dedicated
        operator: Equal
        value: scion-agents
        effect: NoSchedule
  empty:
    type: kubernetes
    node_selector: {}
    tolerations: []
profiles:
  gpu:
    runtime: gke
    node_selector:
      nvidia.com/gpu.present: "true"
`)
	wantSel := map[string]string{"cloud.google.com/gke-nodepool": "scion-agents"}
	wantTol := []api.K8sToleration{{Key: "dedicated", Operator: "Equal", Value: "scion-agents", Effect: "NoSchedule"}}

	vs, _, err := LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	assert.Equal(t, wantSel, vs.Runtimes["gke"].NodeSelector)
	assert.Equal(t, wantTol, vs.Runtimes["gke"].Tolerations)
	assert.Equal(t, map[string]string{"nvidia.com/gpu.present": "true"}, vs.Profiles["gpu"].NodeSelector)
	assert.Empty(t, vs.Runtimes["empty"].NodeSelector)
	assert.Empty(t, vs.Runtimes["empty"].Tolerations)
	assert.False(t, RuntimeEntryHasPlacement(vs.Runtimes["empty"]), "{} and [] must behave as unset")

	// Overlay (DB-backed settings): stored values replace the file's.
	o := NewSettingsOverlay()
	o.Update(map[string]V1RuntimeConfig{
		"stored": {Type: "kubernetes", NodeSelector: wantSel, Tolerations: wantTol},
		"blank":  {Type: "kubernetes", NodeSelector: map[string]string{}, Tolerations: []api.K8sToleration{}},
	}, map[string]V1ProfileConfig{"p": {Runtime: "stored", NodeSelector: map[string]string{"a.b/c": "d"}}}, nil)
	SetGlobalSettingsOverlay(o)
	vs, _, err = LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	assert.Equal(t, wantSel, vs.Runtimes["stored"].NodeSelector)
	assert.Equal(t, wantTol, vs.Runtimes["stored"].Tolerations)
	assert.Equal(t, map[string]string{"a.b/c": "d"}, vs.Profiles["p"].NodeSelector)
	assert.False(t, RuntimeEntryHasPlacement(vs.Runtimes["blank"]))
	_, fileEntry := vs.Runtimes["gke"]
	assert.False(t, fileEntry, "stored runtimes replace the file's")
}

// GlobalSettingsMentionPlacementKeys matches every spelling, ignoring case.
func TestGlobalSettingsMentionPlacementKeys(t *testing.T) {
	for body, want := range map[string]bool{
		"runtimes:\n  k:\n    nodeSelector: {}\n": true,
		"NODE_SELECTOR: x\n":                      true,
		"runtimeClassName: x\n":                   true,
		"x: tolerations\n":                        true,
		"runtimes:\n  k:\n    namespace: n\n":     false,
	} {
		writePlacementGlobalSettings(t, body)
		assert.Equal(t, want, GlobalSettingsMentionPlacementKeys(), "%q", body)
	}
}

// ConfiguredRuntimeNames: the active overlay's stored runtimes when it
// supplies the runtimes section, otherwise the global file's runtimes;
// never entries that exist only in the embedded defaults; not known (fail
// closed) when the file cannot be stat'd, read or decoded.
func TestConfiguredRuntimeNames(t *testing.T) {
	writePlacementGlobalSettings(t, "schema_version: \"1\"\nruntimes:\n  gke:\n    type: kubernetes\n")
	names, ok := ConfiguredRuntimeNames()
	require.True(t, ok)
	assert.Equal(t, map[string]bool{"gke": true}, names, "the embedded default kubernetes entry is not configured")
	vs, _, err := LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	_, hasDefault := vs.Runtimes["kubernetes"]
	require.True(t, hasDefault, "precondition: the loaded view holds the embedded default entry")

	// DB mode: when the overlay supplies the runtimes section, only its
	// stored runtimes are configured; the file's entries are not in effect.
	o := NewSettingsOverlay()
	o.Update(map[string]V1RuntimeConfig{"stored": {Type: "kubernetes"}}, nil, nil)
	SetGlobalSettingsOverlay(o)
	names, ok = ConfiguredRuntimeNames()
	require.True(t, ok)
	assert.Equal(t, map[string]bool{"stored": true}, names)

	// An active overlay without stored runtimes adds nothing.
	o2 := NewSettingsOverlay()
	o2.Update(nil, map[string]V1ProfileConfig{"p": {Runtime: "gke"}}, nil)
	SetGlobalSettingsOverlay(o2)
	names, ok = ConfiguredRuntimeNames()
	require.True(t, ok)
	assert.Equal(t, map[string]bool{"gke": true}, names)
	SetGlobalSettingsOverlay(nil)

	// No global file: nothing is configured, and that is known.
	home := t.TempDir()
	t.Setenv("HOME", home)
	names, ok = ConfiguredRuntimeNames()
	assert.True(t, ok)
	assert.Empty(t, names)

	// A settings file that cannot be stat'd (a self-referencing symlink;
	// independent of permissions) or parsed: not known.
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.Symlink("settings.yaml", filepath.Join(dir, "settings.yaml")))
	_, ok = ConfiguredRuntimeNames()
	assert.False(t, ok, "unstatable file must fail closed")
	require.NoError(t, os.Remove(filepath.Join(dir, "settings.yaml")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("runtimes: [unclosed\n"), 0o644))
	_, ok = ConfiguredRuntimeNames()
	assert.False(t, ok, "unparsable file must fail closed")
}

// The file's runtime names are read the way the settings load them: a
// top-level key in another letter case still configures its entries.
func TestConfiguredRuntimeNames_KeyCase(t *testing.T) {
	writePlacementGlobalSettings(t, "schema_version: \"1\"\nRuntimes:\n  gke:\n    type: kubernetes\n")
	vs, _, err := LoadGlobalSettingsWithOverlay()
	require.NoError(t, err)
	_, loaded := vs.Runtimes["gke"]
	require.True(t, loaded, "precondition: the loader reads the upper-case key")
	names, ok := ConfiguredRuntimeNames()
	require.True(t, ok)
	assert.True(t, names["gke"], "an entry the loader reads is configured: %v", names)
	assert.False(t, names["kubernetes"], "the embedded default entry is not configured")
}

// GlobalSettingsFileUnstatable: only a stat error other than not-exist.
func TestGlobalSettingsFileUnstatable(t *testing.T) {
	writePlacementGlobalSettings(t, "schema_version: \"1\"\n")
	assert.False(t, GlobalSettingsFileUnstatable(), "readable file")
	home := t.TempDir()
	t.Setenv("HOME", home)
	assert.False(t, GlobalSettingsFileUnstatable(), "no global directory")
	dir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	assert.False(t, GlobalSettingsFileUnstatable(), "no file")
	require.NoError(t, os.Symlink("settings.yaml", filepath.Join(dir, "settings.yaml")))
	assert.True(t, GlobalSettingsFileUnstatable(), "self-referencing symlink")
}
