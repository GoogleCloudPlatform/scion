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
	"syscall"
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

// structParityBase is a starting settings file for the struct-path parity
// test. file is the settings file name ("" means no file at all).
type structParityBase struct {
	name    string
	file    string
	content string
}

// structParityBases are the starting files TestUpdateVersionedSetting_MatchesStructPath
// runs every key against. A legacy hub project-key base lives with the
// legacy migration tests.
var structParityBases = []structParityBase{
	{name: "missing"},
	{name: "empty", file: "settings.yaml", content: "\n"},
	{name: "fixture", file: "settings.yaml", content: writebackFixture},
	{name: "populated", file: "settings.yaml", content: "schema_version: \"1\"\nactive_profile: a\ndefault_template: t\ndefault_harness_config: h\nworkspace_path: /w\nimage_registry: r\ncli:\n  autohelp: true\nhub:\n  enabled: true\n  linked: false\n  endpoint: https://e\n  local_only: true\n  project_id: p\nserver:\n  broker:\n    broker_id: b\n    broker_token: bt\n    broker_nickname: bn\n  auth:\n    display_name: d\n    email: e@x\n    username: u\n"},
	{name: "anchored-scalars", file: "settings.yaml", content: "schema_version: \"1\"\nimage_registry: &r ghcr.io/a\ndefault_template: *r\nhub:\n  endpoint: &ep https://e\n  enabled: &on true\n  local_only: *on\nserver:\n  auth:\n    display_name: *ep\n"},
	{name: "anchored-mapping", file: "settings.yaml", content: "schema_version: \"1\"\nserver:\n  broker: &b\n    broker_id: b\n  auth: &a\n    username: u\nhub: &h\n  endpoint: https://e\nx-copies:\n  broker: *b\n  hub: *h\n"},
	{name: "hub-null", file: "settings.yaml", content: "schema_version: \"1\"\nhub:\nactive_profile: a\n"},
	{name: "schema-version-missing", file: "settings.yaml", content: "# c\nactive_profile: a\nhub:\n  endpoint: https://e\n"},
	{name: "schema-version-null", file: "settings.yaml", content: "schema_version:\nactive_profile: a\n"},
	{name: "schema-version-int", file: "settings.yaml", content: "schema_version: 1\nactive_profile: a\n"},
	{name: "flow-parent", file: "settings.yaml", content: "schema_version: \"1\"\nhub: {endpoint: https://e, enabled: true}\nserver: {broker: {broker_id: b}}\n"},
	{name: "yml", file: "settings.yml", content: writebackFixture},
	{name: "merge-key", file: "settings.yaml", content: "schema_version: \"1\"\nx-base: &b\n  endpoint: https://base\n  enabled: true\nx-auth: &a\n  username: base-user\nhub:\n  <<: *b\n  linked: true\nserver:\n  auth:\n    <<: *a\n    email: own@x\n"},
	{name: "merge-key-explicit-override", file: "settings.yaml", content: "schema_version: \"1\"\nx-base: &b\n  endpoint: https://base\nhub:\n  <<: *b\n  endpoint: https://own\n"},
	{name: "merge-key-root", file: "settings.yaml", content: "x-top: &t\n  active_profile: merged\n  image_registry: merged-registry\nschema_version: \"1\"\n<<: *t\n"},
	{name: "tagged-map-parent", file: "settings.yaml", content: "schema_version: \"1\"\nhub: !!map\n  endpoint: https://e\n"},
	{name: "alias-key-nested", file: "settings.yaml", content: "schema_version: \"1\"\nk: &k endpoint\nhub:\n  *k : https://own\n"},
	{name: "alias-key-root", file: "settings.yaml", content: "schema_version: \"1\"\nk: &k hub\n*k :\n  endpoint: https://own\n"},
	{name: "complex-key", file: "settings.yaml", content: "schema_version: \"1\"\nhub:\n  ? [a, b]\n  : x\n  endpoint: https://own\n"},
}

// runStructParity checks, for every key and a spread of values, that the
// settings loaded after UpdateVersionedSetting equal those loaded after the
// pre-#1800 struct round-trip (updateVersionedSettingStruct).
func runStructParity(t *testing.T, bases []structParityBase) {
	t.Helper()
	values := []string{"new-value", "", "true", "false", "yes", "123"}
	for _, base := range bases {
		for _, key := range updateVersionedSettingKeys {
			for _, value := range values {
				t.Run(base.name+"/"+key+"="+value, func(t *testing.T) {
					newDir, oldDir := t.TempDir(), t.TempDir()
					if base.file != "" {
						for _, d := range []string{newDir, oldDir} {
							require.NoError(t, os.WriteFile(filepath.Join(d, base.file), []byte(base.content), 0644))
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

func TestUpdateVersionedSetting_MatchesStructPath(t *testing.T) {
	runStructParity(t, structParityBases)
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

// TestUpdateVersionedSetting_AnchorsNotShared is the review repro: editing
// an anchored value must not change the aliases that refer to it.
func TestUpdateVersionedSetting_AnchorsNotShared(t *testing.T) {
	t.Run("anchored scalar", func(t *testing.T) {
		dir := writeSettingsFixture(t, "schema_version: \"1\"\nimage_registry: &r ghcr.io/a\ndefault_template: *r\n")
		require.NoError(t, UpdateVersionedSetting(dir, "image_registry", "ghcr.io/b"))
		vs, err := LoadSingleFileVersioned(dir)
		require.NoError(t, err)
		assert.Equal(t, "ghcr.io/b", vs.ImageRegistry)
		assert.Equal(t, "ghcr.io/a", vs.DefaultTemplate)
	})
	t.Run("anchored nested scalar", func(t *testing.T) {
		dir := writeSettingsFixture(t, "schema_version: \"1\"\nhub:\n  endpoint: &ep https://a\nserver:\n  auth:\n    display_name: *ep\n")
		require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", "https://b"))
		vs, err := LoadSingleFileVersioned(dir)
		require.NoError(t, err)
		assert.Equal(t, "https://b", vs.Hub.Endpoint)
		assert.Equal(t, "https://a", vs.Server.Auth.DisplayName)
	})
	t.Run("delete inside anchored mapping", func(t *testing.T) {
		dir := writeSettingsFixture(t, "schema_version: \"1\"\nserver:\n  auth: &a\n    username: u\n    email: e@x\nx-copy: *a\n")
		require.NoError(t, UpdateVersionedSetting(dir, "server.auth.email", ""))
		vs, err := LoadSingleFileVersioned(dir)
		require.NoError(t, err)
		assert.Equal(t, "", vs.Server.Auth.Email)
		assert.Equal(t, "u", vs.Server.Auth.Username)
	})
	t.Run("helpers refuse anchored nodes", func(t *testing.T) {
		doc, err := parseYAMLMappingDocument([]byte("a: &x 1\nb:\n  c: 1\nd: &m\n  e: 1\n"))
		require.NoError(t, err)
		root := doc.Content[0]
		_, err = setYAMLPath(root, []string{"a"}, newYAMLStringScalar("2"))
		assert.ErrorIs(t, err, errYAMLEditThroughAlias)
		_, err = setYAMLPath(root, []string{"d", "f"}, newYAMLStringScalar("2"))
		assert.ErrorIs(t, err, errYAMLEditThroughAlias)
		_, err = deleteYAMLPath(root, []string{"d", "e"})
		assert.ErrorIs(t, err, errYAMLEditThroughAlias)
		_, err = deleteYAMLPath(root, []string{"a"})
		assert.ErrorIs(t, err, errYAMLEditThroughAlias)
		changed, err := setYAMLPath(root, []string{"b", "c"}, newYAMLStringScalar("2"))
		require.NoError(t, err)
		assert.True(t, changed)
	})
}

func TestUpdateVersionedSetting_StructFallbackKeepsYMLAndMode(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "settings.yml")
	require.NoError(t, os.WriteFile(yml, []byte("schema_version: \"1\"\nx-hub: &h\n  endpoint: https://h\nhub: *h\n"), 0600))
	require.NoError(t, UpdateVersionedSetting(dir, "hub.linked", "true"))
	_, err := os.Stat(filepath.Join(dir, "settings.yaml"))
	assert.True(t, os.IsNotExist(err), "struct fallback created settings.yaml next to settings.yml")
	info, err := os.Stat(yml)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	vs, err := LoadSingleFileVersioned(dir)
	require.NoError(t, err)
	require.NotNil(t, vs.Hub)
	require.NotNil(t, vs.Hub.Linked)
	assert.True(t, *vs.Hub.Linked)
	assert.Equal(t, "https://h", vs.Hub.Endpoint)
}

func TestUpdateVersionedSetting_CRLF(t *testing.T) {
	const src = "schema_version: \"1\"\r\nhub:\r\n  endpoint: a\r\n"
	dir := writeSettingsFixture(t, src)
	require.NoError(t, UpdateVersionedSetting(dir, "hub.linked", "true"))
	assert.Equal(t, src+"  linked: true\r\n", readSettingsFile(t, dir))
	require.NoError(t, UpdateVersionedSetting(dir, "server.auth.email", "e@x"))
	assert.Equal(t, src+"  linked: true\r\nserver:\r\n  auth:\r\n    email: e@x\r\n", readSettingsFile(t, dir))
}

func TestUpdateVersionedSetting_DeleteLastChildKeepsSplice(t *testing.T) {
	const src = "schema_version: \"1\"\n\nhub: # hub section\n  endpoint: https://h\n\nserver:\n  list:\n  - a\n"
	dir := writeSettingsFixture(t, src)
	require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", ""))
	assert.Equal(t, "schema_version: \"1\"\n\nhub: {} # hub section\n\nserver:\n  list:\n  - a\n", readSettingsFile(t, dir))
	vs, err := LoadSingleFileVersioned(dir)
	require.NoError(t, err)
	assert.Equal(t, "", vs.GetHubEndpoint())
}

func TestWriteSettingsFileAtomic_DirectoryRefusesNewFiles(t *testing.T) {
	t.Run("simulated EACCES falls back to in-place write", func(t *testing.T) {
		dir := writeSettingsFixture(t, writebackFixture)
		path := filepath.Join(dir, "settings.yaml")
		require.NoError(t, os.Chmod(path, 0600))
		orig := createTempFile
		t.Cleanup(func() { createTempFile = orig })
		createTempFile = func(string, string) (*os.File, error) {
			return nil, &os.PathError{Op: "open", Path: dir, Err: syscall.EACCES}
		}
		require.NoError(t, UpdateVersionedSetting(dir, "hub.brokerId", "new"))
		assert.Contains(t, readSettingsFile(t, dir), "broker_id: new")
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	})

	t.Run("simulated other error is returned", func(t *testing.T) {
		dir := writeSettingsFixture(t, writebackFixture)
		orig := createTempFile
		t.Cleanup(func() { createTempFile = orig })
		createTempFile = func(string, string) (*os.File, error) {
			return nil, &os.PathError{Op: "open", Path: dir, Err: syscall.ENOSPC}
		}
		require.Error(t, UpdateVersionedSetting(dir, "hub.brokerId", "new"))
		assert.Equal(t, writebackFixture, readSettingsFile(t, dir))
	})

	t.Run("real read-only directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := writeSettingsFixture(t, writebackFixture)
		require.NoError(t, os.Chmod(dir, 0555))
		t.Cleanup(func() { _ = os.Chmod(dir, 0755) })
		require.NoError(t, UpdateVersionedSetting(dir, "hub.brokerId", "new"))
		assert.Contains(t, readSettingsFile(t, dir), "broker_id: new")
		assertNoTempFiles(t, dir)
	})
}

func TestWriteSettingsFileAtomic_DanglingSymlink(t *testing.T) {
	t.Run("creates the link target and keeps the link", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Mkdir(filepath.Join(dir, "real"), 0755))
		link := filepath.Join(dir, "settings.yaml")
		require.NoError(t, os.Symlink(filepath.Join("real", "settings.yaml"), link))
		require.NoError(t, UpdateVersionedSetting(dir, "active_profile", "local"))
		fi, err := os.Lstat(link)
		require.NoError(t, err)
		assert.True(t, fi.Mode()&os.ModeSymlink != 0, "dangling symlink was replaced by a regular file")
		data, err := os.ReadFile(filepath.Join(dir, "real", "settings.yaml"))
		require.NoError(t, err)
		assert.Contains(t, string(data), "active_profile: local")
	})

	t.Run("target directory missing is a clear error", func(t *testing.T) {
		dir := t.TempDir()
		link := filepath.Join(dir, "settings.yaml")
		require.NoError(t, os.Symlink(filepath.Join(dir, "nope", "settings.yaml"), link))
		err := UpdateVersionedSetting(dir, "active_profile", "local")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dangling symlink")
		fi, lerr := os.Lstat(link)
		require.NoError(t, lerr)
		assert.True(t, fi.Mode()&os.ModeSymlink != 0)
	})
}

// spliceSet parses src and runs spliceSetYAMLPath with a 2-space indent.
func spliceSet(t *testing.T, src string, path []string, value *yaml.Node) (string, bool) {
	t.Helper()
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(src), &doc))
	out, ok := spliceSetYAMLPath([]byte(src), doc.Content[0], path, value, 2)
	return string(out), ok
}

func TestSpliceSetYAMLPath(t *testing.T) {
	str := newYAMLStringScalar
	tests := []struct {
		name   string
		src    string
		path   []string
		value  *yaml.Node
		want   string
		wantOK bool
	}{
		{"escaped double quotes", "a: \"x\\\"y\" # c\n", []string{"a"}, str("z"), "a: \"z\" # c\n", true},
		{"escaped backslash before closing quote", "a: \"x\\\\\" # c\n", []string{"a"}, str("z"), "a: \"z\" # c\n", true},
		{"doubled single quote", "a: 'it''s' # c\nb: 1\n", []string{"a"}, str("ok"), "a: 'ok' # c\nb: 1\n", true},
		{"single quoted value needing escape", "a: 'x'\n", []string{"a"}, str("it's"), "a: 'it''s'\n", true},
		{"plain to bool", "a: x\n", []string{"a"}, newYAMLBoolScalar(true), "a: true\n", true},
		{"multi-line plain scalar falls back", "a: one\n  two\nb: 1\n", []string{"a"}, str("z"), "", false},
		{"multi-line double quoted falls back", "a: \"one\n  two\"\nb: 1\n", []string{"a"}, str("z"), "", false},
		{"multi-line single quoted falls back", "a: 'one\n  two'\nb: 1\n", []string{"a"}, str("z"), "", false},
		{"block scalar falls back", "a: |\n  x\nb: 1\n", []string{"a"}, str("z"), "", false},
		{"bom on line one", "\ufeffa: x\nb: 1\n", []string{"a"}, str("z"), "\ufeffa: z\nb: 1\n", true},
		{"insert after deeper comment", "s:\n  a: 1\n    # deeper\nh: 2\n", []string{"s", "b"}, str("v"), "s:\n  a: 1\n    # deeper\n  b: v\nh: 2\n", true},
		{"insert before same-indent comment", "s:\n  a: 1\n  # about h\nh: 2\n", []string{"s", "b"}, str("v"), "s:\n  a: 1\n  b: v\n  # about h\nh: 2\n", true},
		{"insert before document marker", "a: 1\ns:\n  x: 1\n---\nother: 2\n", []string{"s", "y"}, str("v"), "a: 1\ns:\n  x: 1\n  y: v\n---\nother: 2\n", true},
		{"insert top-level before document end", "a: 1\n...\n", []string{"b", "c"}, str("v"), "a: 1\nb:\n  c: v\n...\n", true},
		{"flow parent falls back", "h: {a: 1}\n", []string{"h", "b"}, str("v"), "", false},
		{"flow root falls back", "{a: 1}\n", []string{"b"}, str("v"), "", false},
		{"multi-line value falls back", "a: 1\n", []string{"b"}, str("x\ny"), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := spliceSet(t, tt.src, tt.path, tt.value)
			require.Equal(t, tt.wantOK, ok, "splice result: %q", got)
			if ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestSpliceReplaceYAMLScalar_Rejects(t *testing.T) {
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte("a: !!str x\nb: [1]\n"), &doc))
	root := doc.Content[0]
	lines := splitYAMLLines([]byte("a: !!str x\nb: [1]\n"))
	_, ok := spliceReplaceYAMLScalar(lines, root.Content[1], newYAMLStringScalar("y"))
	assert.False(t, ok, "explicitly tagged scalar")
	_, ok = spliceReplaceYAMLScalar(lines, root.Content[3], newYAMLStringScalar("y"))
	assert.False(t, ok, "sequence value")
	_, ok = spliceReplaceYAMLScalar(lines, &yaml.Node{Kind: yaml.ScalarNode, Value: "x", Line: 9, Column: 1}, newYAMLStringScalar("y"))
	assert.False(t, ok, "line out of range")
}

func TestSpliceDeleteYAMLPath(t *testing.T) {
	tests := []struct {
		name   string
		src    string
		path   []string
		want   string
		wantOK bool
	}{
		{"delete one of several", "h:\n  a: 1 # c\n  b: 2\n", []string{"h", "a"}, "h:\n  b: 2\n", true},
		{"delete last child rewrites parent", "h: # keep\n  a: 1\nz: 2\n", []string{"h", "a"}, "h: {} # keep\nz: 2\n", true},
		{"delete last child crlf", "h:\r\n  a: 1\r\n", []string{"h", "a"}, "h: {}\r\n", true},
		{"delete top-level", "a: 1\nb: 2\n", []string{"a"}, "b: 2\n", true},
		{"quoted parent key falls back", "\"h\":\n  a: 1\n", []string{"h", "a"}, "", false},
		{"value on next line falls back", "h:\n  a:\n    x\n", []string{"h", "a"}, "", false},
		{"block scalar falls back", "a: |\n  x\nb: 1\n", []string{"a"}, "", false},
		{"missing key", "a: 1\n", []string{"b"}, "", false},
		{"flow parent falls back", "h: {a: 1}\n", []string{"h", "a"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var doc yaml.Node
			require.NoError(t, yaml.Unmarshal([]byte(tt.src), &doc))
			got, ok := spliceDeleteYAMLPath([]byte(tt.src), doc.Content[0], tt.path)
			require.Equal(t, tt.wantOK, ok, "splice result: %q", got)
			if ok {
				assert.Equal(t, tt.want, string(got))
			}
		})
	}
}

func TestDeleteYAMLPath_EmptiedParentWithKeyComment(t *testing.T) {
	doc, err := parseYAMLMappingDocument([]byte("a: 1\nhub: # c\n  endpoint: x\nz: 1\n"))
	require.NoError(t, err)
	changed, err := deleteYAMLPath(doc.Content[0], []string{"hub", "endpoint"})
	require.NoError(t, err)
	require.True(t, changed)
	out, err := encodeYAMLDocument(doc, 2)
	require.NoError(t, err)
	assert.Equal(t, "a: 1\nhub: {} # c\nz: 1\n", string(out))
}

func TestReplaceYAMLMapValue_NonScalar(t *testing.T) {
	for _, src := range []string{
		"x: &a 1\nk: *a # alias\n",
		"k: # mapping\n  inner: 1\n",
		"k: [1, 2] # seq\n",
	} {
		doc, err := parseYAMLMappingDocument([]byte(src))
		require.NoError(t, err)
		root := doc.Content[0]
		_, old := findMapKey(root, "k")
		lineComment := old.LineComment
		changed := replaceYAMLMapValue(root, "k", old, newYAMLStringScalar("v"))
		assert.True(t, changed, src)
		_, got := findMapKey(root, "k")
		assert.Equal(t, yaml.ScalarNode, got.Kind, src)
		assert.Equal(t, "v", got.Value, src)
		assert.Equal(t, lineComment, got.LineComment, src)
	}
}

// TestUpdateVersionedSetting_MergeKeys covers the review repros: a delete
// under a `<<` merge key must remove the merged value, as the struct path
// does, rather than silently do nothing or uncover it.
func TestUpdateVersionedSetting_MergeKeys(t *testing.T) {
	for name, src := range map[string]string{
		"merged only":       "schema_version: \"1\"\nx-base: &b {endpoint: https://base}\nhub: {<<: *b, linked: true}\n",
		"explicit override": "schema_version: \"1\"\nx-base: &b {endpoint: https://base}\nhub:\n  <<: *b\n  endpoint: https://own\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeSettingsFixture(t, src)
			require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", ""))
			vs, err := LoadSingleFileVersioned(dir)
			require.NoError(t, err)
			assert.Equal(t, "", vs.GetHubEndpoint())
		})
	}
	t.Run("helpers refuse mappings with merge keys", func(t *testing.T) {
		doc, err := parseYAMLMappingDocument([]byte("x: &b {k: 1}\nh:\n  <<: *b\n  j: 2\nt:\n  !!merge <<: *b\n"))
		require.NoError(t, err)
		root := doc.Content[0]
		_, err = setYAMLPath(root, []string{"h", "k"}, newYAMLStringScalar("v"))
		assert.ErrorIs(t, err, errYAMLEditThroughAlias)
		_, err = deleteYAMLPath(root, []string{"h", "j"})
		assert.ErrorIs(t, err, errYAMLEditThroughAlias)
		_, err = deleteYAMLPath(root, []string{"t", "k"})
		assert.ErrorIs(t, err, errYAMLEditThroughAlias)
		quoted, err := parseYAMLMappingDocument([]byte("h:\n  '<<': literal\n"))
		require.NoError(t, err)
		_, err = setYAMLPath(quoted.Content[0], []string{"h", "k"}, newYAMLStringScalar("v"))
		assert.NoError(t, err, "a quoted '<<' is an ordinary key")
	})
}

// TestWriteSettingsFileAtomic_SymlinkedDirectoryRelativeLink is the review's
// dotfiles layout: the settings directory is a symlink and the settings
// file is a relative link with `..`, which must resolve against the
// physical parent, as the kernel (and every read) does.
func TestWriteSettingsFileAtomic_SymlinkedDirectoryRelativeLink(t *testing.T) {
	setup := func(t *testing.T, realFile bool) (root, scionDir string) {
		root = t.TempDir()
		for _, d := range []string{"home/shared", "real/scion", "real/shared"} {
			require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0755))
		}
		require.NoError(t, os.Symlink(filepath.Join("..", "real", "scion"), filepath.Join(root, "home", ".scion")))
		require.NoError(t, os.Symlink(filepath.Join("..", "shared", "settings.yaml"), filepath.Join(root, "real", "scion", "settings.yaml")))
		if realFile {
			require.NoError(t, os.WriteFile(filepath.Join(root, "real", "shared", "settings.yaml"), []byte("schema_version: \"1\"\nimage_registry: a\n"), 0644))
		}
		return root, filepath.Join(root, "home", ".scion")
	}
	assertResult := func(t *testing.T, root, scionDir string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, "real", "shared", "settings.yaml"))
		require.NoError(t, err)
		assert.Contains(t, string(data), "image_registry: b")
		_, err = os.Stat(filepath.Join(root, "home", "shared", "settings.yaml"))
		assert.True(t, os.IsNotExist(err), "write landed at the lexical path")
		fi, err := os.Lstat(filepath.Join(root, "real", "scion", "settings.yaml"))
		require.NoError(t, err)
		assert.True(t, fi.Mode()&os.ModeSymlink != 0, "settings link was replaced")
		vs, err := LoadSingleFileVersioned(scionDir)
		require.NoError(t, err)
		assert.Equal(t, "b", vs.ImageRegistry)
	}

	t.Run("existing target", func(t *testing.T) {
		root, scionDir := setup(t, true)
		require.NoError(t, UpdateVersionedSetting(scionDir, "image_registry", "b"))
		assertResult(t, root, scionDir)
	})
	t.Run("dangling target", func(t *testing.T) {
		root, scionDir := setup(t, false)
		require.NoError(t, UpdateVersionedSetting(scionDir, "image_registry", "b"))
		assertResult(t, root, scionDir)
	})
	t.Run("struct save through the same layout", func(t *testing.T) {
		root, scionDir := setup(t, true)
		vs, err := LoadSingleFileVersioned(scionDir)
		require.NoError(t, err)
		vs.ImageRegistry = "b"
		require.NoError(t, SaveVersionedSettings(scionDir, vs))
		assertResult(t, root, scionDir)
	})
}

func TestUpdateVersionedSetting_DanglingYMLSymlink(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "real"), 0755))
	link := filepath.Join(dir, "settings.yml")
	require.NoError(t, os.Symlink(filepath.Join("real", "settings.yml"), link))
	require.NoError(t, UpdateVersionedSetting(dir, "active_profile", "local"))
	_, err := os.Lstat(filepath.Join(dir, "settings.yaml"))
	assert.True(t, os.IsNotExist(err), "settings.yaml created next to a dangling settings.yml link")
	fi, err := os.Lstat(link)
	require.NoError(t, err)
	assert.True(t, fi.Mode()&os.ModeSymlink != 0)
	vs, err := LoadSingleFileVersioned(dir)
	require.NoError(t, err)
	assert.Equal(t, "local", vs.ActiveProfile)
}

func TestUpdateVersionedSetting_RoundTripRefusal(t *testing.T) {
	t.Run("tagged mapping parent delete passes the guard", func(t *testing.T) {
		dir := writeSettingsFixture(t, "schema_version: \"1\"\nhub: !!map\n  endpoint: https://e\n  linked: true\n")
		require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", ""))
		vs, err := LoadSingleFileVersioned(dir)
		require.NoError(t, err)
		assert.Equal(t, "", vs.GetHubEndpoint())
		assert.True(t, vs.IsHubLinked())
	})
	t.Run("an encoding that does not round-trip is refused", func(t *testing.T) {
		const src = "schema_version: \"1\"\nhub: {endpoint: https://e}\n"
		dir := writeSettingsFixture(t, src)
		orig := encodeSettingsYAML
		t.Cleanup(func() { encodeSettingsYAML = orig })
		encodeSettingsYAML = func(*yaml.Node, int) ([]byte, error) {
			return []byte("hub: # c\n{}\n"), nil
		}
		err := UpdateVersionedSetting(dir, "hub.linked", "true")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "do not round-trip")
		assert.Equal(t, src, readSettingsFile(t, dir))
	})
}

// TestUpdateVersionedSetting_AliasKeys covers the review repros: a key
// written as an alias (`*k :`) expands to its anchor's value, which the
// node edit cannot match by name, so the edit must take the struct path
// rather than append a duplicate field (an unloadable file) or miss the
// delete.
func TestUpdateVersionedSetting_AliasKeys(t *testing.T) {
	bases := map[string]string{
		"nested": "schema_version: \"1\"\nk: &k endpoint\nhub:\n  *k : https://own\n",
		"root":   "schema_version: \"1\"\nk: &k hub\n*k :\n  endpoint: https://own\n",
	}
	for name, src := range bases {
		t.Run(name+"/set", func(t *testing.T) {
			dir := writeSettingsFixture(t, src)
			require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", "https://new"))
			vs, err := LoadSingleFileVersioned(dir)
			require.NoError(t, err, "settings file no longer loads")
			assert.Equal(t, "https://new", vs.GetHubEndpoint())
		})
		t.Run(name+"/delete", func(t *testing.T) {
			dir := writeSettingsFixture(t, src)
			require.NoError(t, UpdateVersionedSetting(dir, "hub.endpoint", ""))
			vs, err := LoadSingleFileVersioned(dir)
			require.NoError(t, err)
			assert.Equal(t, "", vs.GetHubEndpoint())
		})
	}
	t.Run("helpers refuse mappings with alias or non-scalar keys", func(t *testing.T) {
		doc, err := parseYAMLMappingDocument([]byte("k: &k e\nh:\n  *k : 1\nc:\n  ? [a]\n  : 1\n"))
		require.NoError(t, err)
		root := doc.Content[0]
		_, err = setYAMLPath(root, []string{"h", "e"}, newYAMLStringScalar("v"))
		assert.ErrorIs(t, err, errYAMLEditThroughAlias)
		_, err = deleteYAMLPath(root, []string{"c", "x"})
		assert.ErrorIs(t, err, errYAMLEditThroughAlias)
	})
}

// TestUpdateVersionedSetting_StructDecodeGuard shows the output struct
// decode alone blocks a write that is data-equivalent but unloadable: the
// hooked encoder emits `endpoint` twice (once through an alias key), which
// generic decoding accepts and VersionedSettings decoding rejects.
func TestUpdateVersionedSetting_StructDecodeGuard(t *testing.T) {
	const src = "schema_version: \"1\"\nhub: {linked: true}\n"
	dir := writeSettingsFixture(t, src)
	orig := encodeSettingsYAML
	t.Cleanup(func() { encodeSettingsYAML = orig })
	const bad = "schema_version: \"1\"\nhub:\n  linked: true\n  ? &k endpoint\n  : https://old\n  *k : https://new\n"
	var want interface{}
	require.NoError(t, yaml.Unmarshal([]byte("schema_version: \"1\"\nhub: {linked: true, endpoint: https://new}\n"), &want))
	require.True(t, yamlDecodesTo([]byte(bad), want), "test output must pass the generic round-trip check")
	encodeSettingsYAML = func(*yaml.Node, int) ([]byte, error) { return []byte(bad), nil }

	err := UpdateVersionedSetting(dir, "hub.endpoint", "https://new")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "would not load")
	assert.Equal(t, src, readSettingsFile(t, dir))
}

func TestSaveVersionedSettings_DanglingYMLSymlink(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "real"), 0755))
	link := filepath.Join(dir, "settings.yml")
	require.NoError(t, os.Symlink(filepath.Join("real", "settings.yml"), link))
	require.NoError(t, SaveVersionedSettings(dir, &VersionedSettings{SchemaVersion: "1", ActiveProfile: "local"}))
	_, err := os.Lstat(filepath.Join(dir, "settings.yaml"))
	assert.True(t, os.IsNotExist(err), "struct save created settings.yaml next to a dangling settings.yml link")
	data, err := os.ReadFile(filepath.Join(dir, "real", "settings.yml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "active_profile: local")

	// A regular settings.yaml still wins over a dangling settings.yml.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0644))
	assert.Equal(t, filepath.Join(dir, "settings.yaml"), newSettingsFilePath(dir))
}

func TestResolveSettingsWriteTarget_DanglingErrorWording(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "real"), 0755))
	link := filepath.Join(dir, "settings.yaml")
	require.NoError(t, os.Symlink("real/x.yaml/", link))
	_, err := resolveSettingsWriteTarget(link)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not name a file")
	assert.NotContains(t, err.Error(), "directory does not exist")

	missing := filepath.Join(dir, "missing.yaml")
	require.NoError(t, os.Symlink(filepath.Join(dir, "nope", "x.yaml"), missing))
	_, err = resolveSettingsWriteTarget(missing)
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestSplitLastPathElem(t *testing.T) {
	for in, want := range map[string][2]string{
		"x":      {".", "x"},
		"/x":     {"/", "x"},
		"a/b":    {"a", "b"},
		"a/../b": {"a/..", "b"},
		"a/b/":   {"a/b", ""},
		"/":      {"/", ""},
	} {
		dir, base := splitLastPathElem(in)
		assert.Equal(t, want, [2]string{dir, base}, in)
	}
}
