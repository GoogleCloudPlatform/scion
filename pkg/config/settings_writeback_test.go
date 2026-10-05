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
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// writebackFixture is a hand-maintained settings file: comments, blank
// lines, a zero-indented sequence and unknown keys at the top level, under
// profiles.<name> and under server.
const writebackFixture = `# Global scion settings, managed by hand.

schema_version: "1"
active_profile: local # the default profile

# Server section.
server:
  broker:
    broker_id: old-id # set by registration
  unknown_server_key: keep-me
  list:
  - a
  - b

profiles:
  docker:
    runtime: docker
    unknown_profile_key: x # profile comment
  empty: {}

unknown_top_key: [1, 2]
# trailing comment
`

func writeSettingsFixture(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0644))
	return dir
}

func readSettingsFile(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	require.NoError(t, err)
	return string(data)
}

// TestUpdateVersionedSetting_PreservesFile checks that one-key updates
// change exactly that key: comments, blank lines, key order and unknown
// keys at the top level, under profiles.<name> and under server survive.
func TestUpdateVersionedSetting_PreservesFile(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{
			name:  "replace existing nested scalar",
			key:   "hub.brokerId",
			value: "b-123",
			want:  strings.Replace(writebackFixture, "broker_id: old-id #", "broker_id: b-123 #", 1),
		},
		{
			name:  "insert into existing nested mapping",
			key:   "hub.brokerToken",
			value: "tok",
			want: strings.Replace(writebackFixture,
				"    broker_id: old-id # set by registration\n",
				"    broker_id: old-id # set by registration\n    broker_token: tok\n", 1),
		},
		{
			name:  "insert missing parents into existing mapping",
			key:   "server.auth.email",
			value: "a@b.c",
			want: strings.Replace(writebackFixture,
				"  - b\n",
				"  - b\n  auth:\n    email: a@b.c\n", 1),
		},
		{
			name:  "insert new top-level section",
			key:   "hub.endpoint",
			value: "https://hub.example.com",
			want: strings.Replace(writebackFixture,
				"unknown_top_key: [1, 2]\n",
				"unknown_top_key: [1, 2]\nhub:\n  endpoint: https://hub.example.com\n", 1),
		},
		{
			name:  "insert bool",
			key:   "hub.enabled",
			value: "true",
			want: strings.Replace(writebackFixture,
				"unknown_top_key: [1, 2]\n",
				"unknown_top_key: [1, 2]\nhub:\n  enabled: true\n", 1),
		},
		{
			name:  "string that looks like a bool is quoted",
			key:   "image_registry",
			value: "true",
			want: strings.Replace(writebackFixture,
				"unknown_top_key: [1, 2]\n",
				"unknown_top_key: [1, 2]\nimage_registry: \"true\"\n", 1),
		},
		{
			name:  "replace top-level scalar keeps inline comment",
			key:   "active_profile",
			value: "docker",
			want:  strings.Replace(writebackFixture, "active_profile: local #", "active_profile: docker #", 1),
		},
		{
			name:  "empty value deletes the key",
			key:   "active_profile",
			value: "",
			want:  strings.Replace(writebackFixture, "active_profile: local # the default profile\n", "", 1),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeSettingsFixture(t, writebackFixture)
			require.NoError(t, UpdateVersionedSetting(dir, tt.key, tt.value))
			assert.Equal(t, tt.want, readSettingsFile(t, dir))
		})
	}
}

// TestUpdateVersionedSetting_FallbackEncodePreserves covers edits the byte
// splice cannot make (here: a flow-style parent). The re-encoded file still
// keeps comments, key order and unknown keys, with the file's indentation.
func TestUpdateVersionedSetting_FallbackEncodePreserves(t *testing.T) {
	const src = `# head
schema_version: "1"
hub: {endpoint: "https://old"} # flow
server:
    unknown_server_key: 1
    broker:
        broker_id: x
profiles:
    docker:
        runtime: docker
        unknown_profile_key: x # keep
unknown_top_key: y
`
	dir := writeSettingsFixture(t, src)
	require.NoError(t, UpdateVersionedSetting(dir, "hub.linked", "true"))
	got := readSettingsFile(t, dir)
	assert.Equal(t, `# head
schema_version: "1"
hub: {endpoint: "https://old", linked: true} # flow
server:
    unknown_server_key: 1
    broker:
        broker_id: x
profiles:
    docker:
        runtime: docker
        unknown_profile_key: x # keep
unknown_top_key: y
`, got)
}

func TestUpdateVersionedSetting_NoOpLeavesFileUntouched(t *testing.T) {
	dir := writeSettingsFixture(t, writebackFixture)
	path := filepath.Join(dir, "settings.yaml")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, past, past))

	for _, kv := range [][2]string{
		{"active_profile", "local"},
		{"hub.brokerId", "old-id"},
		{"image_registry", ""}, // deleting an absent key
		{"hub.token", "ignored"},
	} {
		require.NoError(t, UpdateVersionedSetting(dir, kv[0], kv[1]), kv[0])
	}
	assert.Equal(t, writebackFixture, readSettingsFile(t, dir))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(past), "mtime changed on a no-op update: %v", info.ModTime())

	// A repeat of a real change is also a no-op.
	require.NoError(t, UpdateVersionedSetting(dir, "hub.enabled", "true"))
	require.NoError(t, os.Chtimes(path, past, past))
	before := readSettingsFile(t, dir)
	require.NoError(t, UpdateVersionedSetting(dir, "hub.enabled", "true"))
	assert.Equal(t, before, readSettingsFile(t, dir))
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(past))
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp-", "temporary file left behind")
	}
}

func TestUpdateVersionedSetting_AtomicWrite(t *testing.T) {
	t.Run("mode preserved and no temp files", func(t *testing.T) {
		dir := writeSettingsFixture(t, writebackFixture)
		path := filepath.Join(dir, "settings.yaml")
		require.NoError(t, os.Chmod(path, 0600))
		require.NoError(t, UpdateVersionedSetting(dir, "hub.brokerId", "new"))
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
		assertNoTempFiles(t, dir)
	})

	t.Run("replaces the file rather than writing through it", func(t *testing.T) {
		// A reader holding the old file open must keep seeing the complete
		// old content: the new content arrives by rename, never by an
		// in-place truncate and rewrite.
		dir := writeSettingsFixture(t, writebackFixture)
		path := filepath.Join(dir, "settings.yaml")
		f, err := os.Open(path)
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		require.NoError(t, UpdateVersionedSetting(dir, "hub.brokerId", "new"))
		old, err := os.ReadFile("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
		if err != nil {
			t.Skip("no /proc/self/fd")
		}
		assert.Equal(t, writebackFixture, string(old))
		assert.Contains(t, readSettingsFile(t, dir), "broker_id: new")
	})

	t.Run("symlinked settings file keeps its link", func(t *testing.T) {
		dir := t.TempDir()
		real := filepath.Join(t.TempDir(), "real-settings.yaml")
		require.NoError(t, os.WriteFile(real, []byte(writebackFixture), 0640))
		link := filepath.Join(dir, "settings.yaml")
		require.NoError(t, os.Symlink(real, link))
		require.NoError(t, UpdateVersionedSetting(dir, "hub.brokerId", "new"))
		fi, err := os.Lstat(link)
		require.NoError(t, err)
		assert.True(t, fi.Mode()&os.ModeSymlink != 0, "symlink was replaced by a regular file")
		data, err := os.ReadFile(real)
		require.NoError(t, err)
		assert.Contains(t, string(data), "broker_id: new")
		info, err := os.Stat(real)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0640), info.Mode().Perm())
	})
}

func TestUpdateVersionedSetting_EdgeCases(t *testing.T) {
	t.Run("missing directory and file", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "new", "dir")
		require.NoError(t, UpdateVersionedSetting(dir, "hub.brokerId", "b1"))
		assert.Equal(t, "schema_version: \"1\"\nserver:\n  broker:\n    broker_id: b1\n", readSettingsFile(t, dir))
	})

	t.Run("empty file", func(t *testing.T) {
		dir := writeSettingsFixture(t, "")
		require.NoError(t, UpdateVersionedSetting(dir, "hub.enabled", "false"))
		assert.Equal(t, "schema_version: \"1\"\nhub:\n  enabled: false\n", readSettingsFile(t, dir))
	})

	t.Run("comment-only file", func(t *testing.T) {
		dir := writeSettingsFixture(t, "# nothing here yet\n")
		require.NoError(t, UpdateVersionedSetting(dir, "active_profile", "local"))
		vs, err := LoadSingleFileVersioned(dir)
		require.NoError(t, err)
		assert.Equal(t, "local", vs.ActiveProfile)
		assert.Equal(t, "1", vs.SchemaVersion)
	})

	t.Run("non-mapping document is rejected and left alone", func(t *testing.T) {
		dir := writeSettingsFixture(t, "- a\n- b\n")
		err := UpdateVersionedSetting(dir, "active_profile", "local")
		require.Error(t, err)
		assert.Equal(t, "- a\n- b\n", readSettingsFile(t, dir))
	})

	t.Run("type error elsewhere in file is rejected as before", func(t *testing.T) {
		const src = "schema_version: \"1\"\nhub: 5\n"
		dir := writeSettingsFixture(t, src)
		require.Error(t, UpdateVersionedSetting(dir, "active_profile", "local"))
		assert.Equal(t, src, readSettingsFile(t, dir))
	})

	t.Run("null parent becomes a mapping", func(t *testing.T) {
		dir := writeSettingsFixture(t, "schema_version: \"1\"\nhub: # later\nactive_profile: local\n")
		require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", "https://h"))
		vs, err := LoadSingleFileVersioned(dir)
		require.NoError(t, err)
		require.NotNil(t, vs.Hub)
		assert.Equal(t, "https://h", vs.Hub.Endpoint)
		assert.Equal(t, "local", vs.ActiveProfile)
		assert.Contains(t, readSettingsFile(t, dir), "# later")
	})

	t.Run("missing schema_version is added first", func(t *testing.T) {
		dir := writeSettingsFixture(t, "# c\nactive_profile: local\n")
		require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", "https://h"))
		got := readSettingsFile(t, dir)
		assert.Equal(t, "# c\nschema_version: \"1\"\nactive_profile: local\nhub:\n  endpoint: https://h\n", got)
	})

	t.Run("alias parent falls back to struct path", func(t *testing.T) {
		const src = "schema_version: \"1\"\nx-hub: &h\n  endpoint: https://h\nhub: *h\n"
		dir := writeSettingsFixture(t, src)
		require.NoError(t, UpdateVersionedSetting(dir, "hub.linked", "true"))
		vs, err := LoadSingleFileVersioned(dir)
		require.NoError(t, err)
		require.NotNil(t, vs.Hub)
		assert.Equal(t, "https://h", vs.Hub.Endpoint)
		require.NotNil(t, vs.Hub.Linked)
		assert.True(t, *vs.Hub.Linked)
	})

	t.Run("quoted value keeps its quoting", func(t *testing.T) {
		dir := writeSettingsFixture(t, "schema_version: \"1\"\nhub:\n  endpoint: 'https://old' # c\n")
		require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", "https://new"))
		assert.Equal(t, "schema_version: \"1\"\nhub:\n  endpoint: 'https://new' # c\n", readSettingsFile(t, dir))
	})

	t.Run("insert after a trailing block scalar", func(t *testing.T) {
		const src = "schema_version: \"1\"\nserver:\n  note: |\n    line one\n    line two\n"
		dir := writeSettingsFixture(t, src)
		require.NoError(t, UpdateVersionedSetting(dir, "server.auth.username", "u"))
		assert.Equal(t, src+"  auth:\n    username: u\n", readSettingsFile(t, dir))
	})

	t.Run("file without trailing newline", func(t *testing.T) {
		dir := writeSettingsFixture(t, "schema_version: \"1\"\nactive_profile: local")
		require.NoError(t, UpdateVersionedSetting(dir, "image_registry", "ghcr.io/x"))
		assert.Equal(t, "schema_version: \"1\"\nactive_profile: local\nimage_registry: ghcr.io/x\n", readSettingsFile(t, dir))
	})

	t.Run("settings.yml is edited in place", func(t *testing.T) {
		dir := t.TempDir()
		yml := filepath.Join(dir, "settings.yml")
		require.NoError(t, os.WriteFile(yml, []byte("schema_version: \"1\"\n# keep\n"), 0644))
		require.NoError(t, UpdateVersionedSetting(dir, "active_profile", "local"))
		data, err := os.ReadFile(yml)
		require.NoError(t, err)
		assert.Contains(t, string(data), "active_profile: local")
		assert.Contains(t, string(data), "# keep")
		_, err = os.Stat(filepath.Join(dir, "settings.yaml"))
		assert.True(t, os.IsNotExist(err), "settings.yaml should not be created next to settings.yml")
	})

	t.Run("json keeps the struct path", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.json"),
			[]byte(`{"schema_version":"1","active_profile":"local"}`), 0644))
		require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", "https://h"))
		vs, err := LoadSingleFileVersioned(dir)
		require.NoError(t, err)
		assert.Equal(t, "local", vs.ActiveProfile)
		require.NotNil(t, vs.Hub)
		assert.Equal(t, "https://h", vs.Hub.Endpoint)
	})

	t.Run("unknown key is rejected", func(t *testing.T) {
		dir := writeSettingsFixture(t, writebackFixture)
		require.Error(t, UpdateVersionedSetting(dir, "registries.foo", "x"))
		assert.Equal(t, writebackFixture, readSettingsFile(t, dir))
	})
}

// updateVersionedSettingKeys lists every key handled by the switch in
// updateVersionedSettingStruct (the pre-#1800 behaviour), plus the project
// ID aliases and the ignored keys.
var updateVersionedSettingKeys = []string{
	"active_profile", "default_template", "default_harness_config", "workspace_path",
	"image_registry", "cli.autohelp", "hub.enabled", "hub.linked", "hub.endpoint",
	"hub.local_only", "hub.brokerId", "hub.brokerToken", "hub.brokerNickname",
	"server.auth.display_name", "server.auth.email", "server.auth.username",
	"project_id", "hub.project_id",
	"hub.token", "hub.apiKey", "hub.lastSyncedAt",
	"bucket.provider", "bucket.name", "bucket.prefix", "hub_connections.foo.endpoint",
}

func TestVersionedSettingKeys_MatchStructSwitch(t *testing.T) {
	var want []string
	for _, k := range updateVersionedSettingKeys {
		edit, err := versionedSettingEditFor(k, "v")
		require.NoError(t, err, k)
		if edit.noop || k == "project_id" || k == "hub.project_id" {
			continue
		}
		want = append(want, k)
	}
	var got []string
	for k := range versionedSettingKeys {
		got = append(got, k)
	}
	sort.Strings(want)
	sort.Strings(got)
	assert.Equal(t, want, got)
}

// normalizedSettings renders vs as generic data with empty mappings
// pruned, so a nil pointer and an empty struct (`hub: {}`) compare equal.
func normalizedSettings(t *testing.T, vs *VersionedSettings) interface{} {
	t.Helper()
	data, err := yaml.Marshal(vs)
	require.NoError(t, err)
	var v interface{}
	require.NoError(t, yaml.Unmarshal(data, &v))
	return pruneEmptyMaps(v)
}

func pruneEmptyMaps(v interface{}) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return v
	}
	out := map[string]interface{}{}
	for k, child := range m {
		child = pruneEmptyMaps(child)
		if cm, ok := child.(map[string]interface{}); ok && len(cm) == 0 {
			continue
		}
		out[k] = child
	}
	return out
}

// TestUpdateVersionedSetting_MatchesStructPath compares, for every key and a
// spread of values, the settings loaded after the in-place edit with those
// loaded after the pre-#1800 struct round-trip (updateVersionedSettingStruct).
func TestUpdateVersionedSetting_MatchesStructPath(t *testing.T) {
	bases := map[string]string{
		"missing":   "",
		"empty":     "\n",
		"fixture":   writebackFixture,
		"populated": "schema_version: \"1\"\nactive_profile: a\ndefault_template: t\ndefault_harness_config: h\nworkspace_path: /w\nimage_registry: r\ncli:\n  autohelp: true\nhub:\n  enabled: true\n  linked: false\n  endpoint: https://e\n  local_only: true\n  project_id: p\nserver:\n  broker:\n    broker_id: b\n    broker_token: bt\n    broker_nickname: bn\n  auth:\n    display_name: d\n    email: e@x\n    username: u\n",
	}
	values := []string{"new-value", "", "true", "false", "yes", "123"}
	for baseName, base := range bases {
		for _, key := range updateVersionedSettingKeys {
			for _, value := range values {
				t.Run(baseName+"/"+key+"="+value, func(t *testing.T) {
					newDir, oldDir := t.TempDir(), t.TempDir()
					if baseName != "missing" {
						for _, d := range []string{newDir, oldDir} {
							require.NoError(t, os.WriteFile(filepath.Join(d, "settings.yaml"), []byte(base), 0644))
						}
					}
					errNew := UpdateVersionedSetting(newDir, key, value)
					errOld := updateVersionedSettingStruct(oldDir, key, value)
					require.Equal(t, errOld == nil, errNew == nil, "new err=%v old err=%v", errNew, errOld)
					if errOld != nil {
						return
					}
					gotNew, err := LoadSingleFileVersioned(newDir)
					require.NoError(t, err)
					gotOld, err := LoadSingleFileVersioned(oldDir)
					require.NoError(t, err)
					assert.Equal(t, normalizedSettings(t, gotOld), normalizedSettings(t, gotNew))
					vNew, errN := GetVersionedSettingValue(gotNew, key)
					vOld, errO := GetVersionedSettingValue(gotOld, key)
					assert.Equal(t, errO == nil, errN == nil)
					assert.Equal(t, vOld, vNew)
				})
			}
		}
	}
}

func TestSaveVersionedSettings_AtomicAndSkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	vs := &VersionedSettings{SchemaVersion: "1", ActiveProfile: "local"}
	require.NoError(t, SaveVersionedSettings(dir, vs))
	path := filepath.Join(dir, "settings.yaml")
	require.NoError(t, os.Chmod(path, 0600))
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(path, past, past))

	require.NoError(t, SaveVersionedSettings(dir, vs))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(past), "unchanged save rewrote the file")

	vs.ActiveProfile = "docker"
	require.NoError(t, SaveVersionedSettings(dir, vs))
	info, err = os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	assert.False(t, info.ModTime().Equal(past))
	assertNoTempFiles(t, dir)
	got, err := LoadSingleFileVersioned(dir)
	require.NoError(t, err)
	assert.Equal(t, "docker", got.ActiveProfile)
}

func TestSetAndDeleteYAMLPath(t *testing.T) {
	doc, err := parseYAMLMappingDocument([]byte("a: 1\nb:\n  c: x\nd: scalar\n"))
	require.NoError(t, err)
	root := doc.Content[0]

	changed, err := setYAMLPath(root, []string{"b", "e", "f"}, newYAMLStringScalar("v"))
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = setYAMLPath(root, []string{"b", "e", "f"}, newYAMLStringScalar("v"))
	require.NoError(t, err)
	assert.False(t, changed, "setting the same value again must report no change")

	_, err = setYAMLPath(root, []string{"d", "x"}, newYAMLStringScalar("v"))
	assert.Error(t, err, "a scalar intermediate cannot gain children")

	changed, err = deleteYAMLPath(root, []string{"b", "c"})
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = deleteYAMLPath(root, []string{"b", "missing"})
	require.NoError(t, err)
	assert.False(t, changed)
	changed, err = deleteYAMLPath(root, []string{"d", "x"})
	require.NoError(t, err)
	assert.False(t, changed)

	out, err := encodeYAMLDocument(doc, 2)
	require.NoError(t, err)
	assert.Equal(t, "a: 1\nb:\n  e:\n    f: v\nd: scalar\n", string(out))

	alias, err := parseYAMLMappingDocument([]byte("x: &h\n  k: 1\nhub: *h\n"))
	require.NoError(t, err)
	_, err = setYAMLPath(alias.Content[0], []string{"hub", "k"}, newYAMLStringScalar("2"))
	assert.ErrorIs(t, err, errYAMLEditThroughAlias)
}

func TestDetectYAMLIndent(t *testing.T) {
	for src, want := range map[string]int{
		"a: 1\n":                    2,
		"a:\n  b: 1\n":              2,
		"a:\n    b: 1\n":            4,
		"a:\n- x\nb:\n   c: 1\n":    3,
		"a: {b: 1}\nc:\n    d: 1\n": 4,
	} {
		doc, err := parseYAMLMappingDocument([]byte(src))
		require.NoError(t, err)
		assert.Equal(t, want, detectYAMLIndent(doc.Content[0]), src)
	}
}
