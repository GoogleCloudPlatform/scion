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
	"encoding/json"
	"regexp"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func perDirNFSBlock() *V1SharedDirStorageConfig {
	return &V1SharedDirStorageConfig{
		Backend: "local",
		NFS: &V1NFSConfig{
			MountRoot: "/mnt/nfs",
			Shares:    []V1NFSShare{{ID: "share-1", PVName: "pv-1"}},
		},
	}
}

// A settings file that uses only the per-dir key on a runtime entry and no
// schema_version is still read as v1, so the key is not dropped.
func TestDetectSettingsFormat_PerDirKeyIsV1Indicator(t *testing.T) {
	raw := map[string]interface{}{
		"runtimes": map[string]interface{}{
			"k8s": map[string]interface{}{"shared_dir_storage_backends": map[string]interface{}{"notes": "nfs"}},
		},
	}
	assert.True(t, hasV1RuntimeIndicators(raw))
}

// The settings schema itself rejects per-dir keys that are not shared dir
// names.
func TestSettingsSchema_SharedDirStorageBackendsKeyPattern(t *testing.T) {
	doc := func(key string) []byte {
		return []byte(`schema_version: "1"
runtimes:
  k8s:
    type: kubernetes
    shared_dir_storage_backends:
      ` + key + `: local
profiles:
  gke:
    runtime: k8s
    shared_dir_storage_backends:
      ` + key + `: local
`)
	}
	errs, err := validateAgainstSchema(doc("build-cache"), "1", settingsSchemaFiles)
	require.NoError(t, err)
	assert.Empty(t, errs)
	for _, key := range []string{"Bad_Name", "-lead", "trail-", "with.dot"} {
		errs, err := validateAgainstSchema(doc(`"`+key+`"`), "1", settingsSchemaFiles)
		require.NoError(t, err)
		assert.Len(t, errs, 2, "%s: both entries are rejected by the schema: %v", key, errs)
	}
}

func TestSharedDirNamePatternMatchesValidateSharedDirs(t *testing.T) {
	re := regexp.MustCompile(SharedDirNamePattern)
	for _, name := range []string{"a", "build-cache", "a1-b2", "x--y", "", "-a", "a-", "A", "a_b", "a.b", "a/b"} {
		want := api.ValidateSharedDirs([]api.SharedDir{{Name: name}}) == nil
		assert.Equal(t, want, re.MatchString(name), "%q", name)
	}
}

// The JSON schema repeats the shared dir name pattern as a literal; all three
// shared_dir_storage_backends entries (runtime, profile and flat Runtime
// Broker instance runtime_target) must use SharedDirNamePattern.
func TestSettingsSchema_SharedDirStorageBackendsPatternMatchesConstant(t *testing.T) {
	data, err := schemasFS.ReadFile(settingsSchemaFiles["1"])
	require.NoError(t, err)
	var doc interface{}
	require.NoError(t, json.Unmarshal(data, &doc))
	var patterns []string
	var walk func(v interface{})
	walk = func(v interface{}) {
		switch n := v.(type) {
		case map[string]interface{}:
			for k, child := range n {
				if k == "shared_dir_storage_backends" {
					entry, _ := child.(map[string]interface{})
					names, _ := entry["propertyNames"].(map[string]interface{})
					pattern, _ := names["pattern"].(string)
					patterns = append(patterns, pattern)
				}
				walk(child)
			}
		case []interface{}:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(doc)
	require.Len(t, patterns, 3, "runtime, profile and flat Runtime Broker instance entries")
	for _, p := range patterns {
		assert.Equal(t, SharedDirNamePattern, p)
	}
}
