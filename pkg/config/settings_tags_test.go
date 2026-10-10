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
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// settingsTagAllowList lists intentional json/yaml/koanf tag disagreements
// on structs reachable from VersionedSettings, keyed "pkg.Type.Field". Each
// entry must say why. It is empty today.
var settingsTagAllowList = map[string]string{}

// TestVersionedSettings_TagsAgree checks that the json, yaml and koanf tag
// names agree on every field of every struct reachable from
// VersionedSettings. The three tags serve three paths: koanf loads
// settings.yaml, yaml.v3 writes it (SaveVersionedSettings), and json backs
// the schema and the hub. A field whose tags disagree is silently dropped by
// whichever path reads the other name. CloudRunConfig had no yaml tags, so
// SaveVersionedSettings wrote "projectid" and the next load lost it
// (ptone/scion#3022). The pkg/api types reached from settings had no koanf
// tags, so koanf fell back to case-insensitive Go field names and dropped
// every multi-word key, e.g. volumes[].read_only and shared_dirs[].read_only.
// An exported field must carry a non-empty json tag (or json:"-"). An
// embedded struct field (untagged under encoding/json, ",squash" for koanf)
// or a third-party type with untagged fields (e.g. metav1.Duration) goes in
// settingsTagAllowList with the reason.
func TestVersionedSettings_TagsAgree(t *testing.T) {
	var problems []string
	matched := map[string]bool{}
	seen := map[reflect.Type]bool{}

	var walk func(tp reflect.Type)
	walk = func(tp reflect.Type) {
		for tp.Kind() == reflect.Pointer || tp.Kind() == reflect.Slice || tp.Kind() == reflect.Array || tp.Kind() == reflect.Map {
			tp = tp.Elem()
		}
		if tp.Kind() != reflect.Struct || seen[tp] {
			return
		}
		seen[tp] = true
		for i := 0; i < tp.NumField(); i++ {
			f := tp.Field(i)
			if !f.IsExported() {
				continue
			}
			j := tagName(f, "json")
			y := tagName(f, "yaml")
			k := tagName(f, "koanf")
			// Every exported field needs an explicit json name (or "-"):
			// with all three tags empty they would trivially "agree" while
			// koanf and yaml fall back to the Go field name.
			if j != "-" && (j == "" || j != y || j != k) {
				key := tp.PkgPath()[strings.LastIndex(tp.PkgPath(), "/")+1:] + "." + tp.Name() + "." + f.Name
				if _, ok := settingsTagAllowList[key]; ok {
					matched[key] = true
				} else {
					problems = append(problems, key+`: json="`+j+`" yaml="`+y+`" koanf="`+k+`"`)
				}
			}
			walk(f.Type)
		}
	}
	walk(reflect.TypeOf(VersionedSettings{}))

	var stale []string
	for key := range settingsTagAllowList {
		if !matched[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(problems)
	sort.Strings(stale)
	require.Empty(t, problems, "json, yaml and koanf tag names must agree on settings structs (or be allow-listed in settingsTagAllowList):\n  %s", strings.Join(problems, "\n  "))
	require.Empty(t, stale, "settingsTagAllowList entries no longer match a mismatch; remove them:\n  %s", strings.Join(stale, "\n  "))
}

// tagName returns the name part of a struct tag ("" when absent).
func tagName(f reflect.StructField, key string) string {
	return strings.Split(f.Tag.Get(key), ",")[0]
}

// TestSaveVersionedSettings_CloudRunRoundTrip saves runtimes.*.cloudrun
// through SaveVersionedSettings and loads it back (ptone/scion#3022).
func TestSaveVersionedSettings_CloudRunRoundTrip(t *testing.T) {
	want := CloudRunConfig{
		ProjectID:      "my-project",
		Location:       "us-central1",
		ServiceAccount: "sa@my-project.iam.gserviceaccount.com",
		Network:        "net",
		Subnetwork:     "subnet",
		NFSServer:      "10.0.0.2",
		NFSExport:      "/export",
	}
	dir := t.TempDir()
	require.NoError(t, SaveVersionedSettings(dir, &VersionedSettings{
		SchemaVersion: "1",
		Runtimes: map[string]V1RuntimeConfig{
			"cr": {Type: "cloudrun", CloudRun: &want},
		},
	}))

	data, err := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "project_id: my-project", "saved file should use the snake_case key")
	errs, err := ValidateSettings(data, "1")
	require.NoError(t, err)
	assert.Empty(t, errs, "the saved file should validate against the schema")

	vs, err := loadVersionedSettingsFileOnly(dir)
	require.NoError(t, err)
	require.NotNil(t, vs.Runtimes["cr"].CloudRun)
	assert.Equal(t, want, *vs.Runtimes["cr"].CloudRun)
}

// TestLoadVersionedSettings_MultiWordAPIFields loads multi-word keys on
// pkg/api types through koanf. Before those types had koanf tags these
// fields were not loaded.
func TestLoadVersionedSettings_MultiWordAPIFields(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(`schema_version: "1"
shared_dirs:
  - name: cache
    read_only: true
    in_workspace: true
profiles:
  local:
    runtime: docker
    volumes:
      - source: /a
        target: /b
        read_only: true
        volume_name: vol
    secrets:
      - key: gcloud-adc
        type: file
        alternative_env_keys: [GOOGLE_APPLICATION_CREDENTIALS]
`), 0o644))

	vs, err := loadVersionedSettingsFileOnly(dir)
	require.NoError(t, err)
	assert.Equal(t, []api.SharedDir{{Name: "cache", ReadOnly: true, InWorkspace: true}}, vs.SharedDirs)
	p := vs.Profiles["local"]
	require.Len(t, p.Volumes, 1)
	assert.True(t, p.Volumes[0].ReadOnly, "volumes[].read_only must load")
	assert.Equal(t, "vol", p.Volumes[0].VolumeName)
	require.Len(t, p.Secrets, 1)
	assert.Equal(t, []string{"GOOGLE_APPLICATION_CREDENTIALS"}, p.Secrets[0].AlternativeEnvKeys)
}
