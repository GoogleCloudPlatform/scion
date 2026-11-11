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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ResolveHomeStorage reads only the global block: the removed
// per-profile and per-runtime overrides are never consulted.
func TestResolveHomeStorage_GlobalOnly(t *testing.T) {
	var nilVS *VersionedSettings
	assert.Equal(t, ResolvedHomeStorage{Backend: "local", Leaf: "pod"}, nilVS.ResolveHomeStorage())

	vs := &VersionedSettings{
		ActiveProfile: "gke",
		Server:        &V1ServerConfig{},
		Runtimes:      map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes", HomeStorageBackend: "nfs", HomeStorageLeaf: "broker"}},
		Profiles:      map[string]V1ProfileConfig{"gke": {Runtime: "k8s", HomeStorageBackend: "nfs"}},
	}
	assert.Equal(t, ResolvedHomeStorage{Backend: "local", Leaf: "pod"}, vs.ResolveHomeStorage(),
		"overrides on the profile and runtime entry must be ignored")

	vs.Server.HomeStorage = &V1HomeStorageConfig{Backend: "nfs", Leaf: "broker"}
	assert.Equal(t, ResolvedHomeStorage{
		Backend: "nfs", BackendSource: HomeStorageBackendGlobalSource,
		Leaf: "broker", LeafSource: HomeStorageLeafGlobalSource,
	}, vs.ResolveHomeStorage())

	vs.Server.HomeStorage = &V1HomeStorageConfig{Leaf: "broker"}
	got := vs.ResolveHomeStorage()
	assert.Equal(t, "local", got.Backend)
	assert.Equal(t, "", got.BackendSource)
	assert.Equal(t, "broker", got.Leaf)
}

func TestV1HomeStorageConfig_Validate(t *testing.T) {
	var nilCfg *V1HomeStorageConfig
	assert.NoError(t, nilCfg.Validate())
	assert.NoError(t, (&V1HomeStorageConfig{}).Validate())
	assert.NoError(t, (&V1HomeStorageConfig{Backend: "nfs", Leaf: "broker", StopGraceSeconds: 5}).Validate())
	for _, bad := range []V1HomeStorageConfig{
		{Backend: "NFS"},
		{Backend: "nfs "},
		{Backend: "ceph"},
		{Leaf: "node"},
		{Leaf: "Pod"},
		{StopGraceSeconds: -1},
		{TerminationWaitSeconds: -1},
		{SkeletonMaxBytes: -1},
	} {
		bad := bad
		assert.Error(t, bad.Validate(), "%+v", bad)
	}
}

func TestV1HomeStorageConfig_Defaults(t *testing.T) {
	var nilCfg *V1HomeStorageConfig
	assert.Equal(t, 30, nilCfg.StopGrace())
	assert.Equal(t, 15, nilCfg.TerminationWait())
	assert.Equal(t, int64(256<<20), nilCfg.SkeletonMax())
	c := &V1HomeStorageConfig{StopGraceSeconds: 40, TerminationWaitSeconds: 5, SkeletonMaxBytes: 10}
	assert.Equal(t, 40, c.StopGrace())
	assert.Equal(t, 5, c.TerminationWait())
	assert.Equal(t, int64(10), c.SkeletonMax())
}

func TestValidateSettings_HomeStorage(t *testing.T) {
	const okYAML = `schema_version: "1"
active_profile: local
server:
  home_storage:
    backend: local
    leaf: pod
    stop_grace_seconds: 30
    termination_wait_seconds: 15
    skeleton_max_bytes: 1024
    allow_incomplete_phases: true
runtimes:
  docker:
    type: docker
  k8s:
    type: kubernetes
profiles:
  local:
    runtime: docker
  gke:
    runtime: k8s
`
	errs, err := ValidateSettings([]byte(okYAML), "1")
	require.NoError(t, err)
	assert.Empty(t, errs)
	assert.Empty(t, SettingsWarnings([]byte(okYAML), "1"))

	for _, bad := range []struct{ from, to string }{
		{"    leaf: pod", "    leaf: kubelet"},
		{"    stop_grace_seconds: 30", "    stop_grace_seconds: -3"},
		{"    allow_incomplete_phases: true", "    allow_incomplete_phases: true\n    unknown_key: 1"},
	} {
		doc := strings.Replace(okYAML, bad.from, bad.to, 1)
		require.NotEqual(t, okYAML, doc, bad.from)
		errs, err := ValidateSettings([]byte(doc), "1")
		require.NoError(t, err)
		assert.NotEmpty(t, errs, "expected an error for %q", bad.to)
	}
}

func TestHomeStorageWindowWarnings(t *testing.T) {
	assert.Empty(t, HomeStorageWindowWarnings(nil))
	assert.Empty(t, HomeStorageWindowWarnings(&V1HomeStorageConfig{}), "the defaults (30 + 15) fit")
	w := HomeStorageWindowWarnings(&V1HomeStorageConfig{StopGraceSeconds: 80, TerminationWaitSeconds: 15})
	require.Len(t, w, 1)
	assert.Contains(t, w[0], "95s")
	assert.Contains(t, w[0], "90s")
	warnings := SettingsWarnings([]byte("schema_version: \"1\"\nserver:\n  home_storage:\n    stop_grace_seconds: 100\n"), "1")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "server.home_storage")
}
