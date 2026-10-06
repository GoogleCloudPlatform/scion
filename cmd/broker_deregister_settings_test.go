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

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// deregisterTestSettings is a v1 global settings file with the keys a
// workstation broker typically has, plus a hub_connections entry for the
// connection being deregistered and one for another hub.
const deregisterTestSettings = `# global settings (this comment must survive)
schema_version: "1"
image_registry: ghcr.io/example/scion
default_harness_config: claude
active_profile: local
hub:
  endpoint: HUB_URL
hub_connections:
  myhub:
    endpoint: HUB_URL
  other:
    endpoint: https://other.example.com
server:
  broker:
    port: 19800
    broker_id: BROKER_ID
    broker_token: old-token
    instances:
      - name: flat-a
        runtime: docker
`

// flattenYAML returns the dotted leaf paths of a decoded YAML document.
func flattenYAML(prefix string, v interface{}, out map[string]interface{}) {
	switch m := v.(type) {
	case map[string]interface{}:
		for k, child := range m {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			flattenYAML(p, child, out)
		}
	case []interface{}:
		for i, child := range m {
			flattenYAML(prefix+"."+strconv.Itoa(i), child, out)
		}
	default:
		out[prefix] = v
	}
}

func readFlatYAML(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	out := map[string]interface{}{}
	flattenYAML("", doc, out)
	return out
}

// runDeregisterWithSettings writes content (with HUB_URL and BROKER_ID
// replaced) as the global settings file under fileName, registers one hub
// connection "myhub", and runs the real runBrokerDeregister against a fake
// hub. It returns the settings path and the flattened settings from before
// deregister.
func runDeregisterWithSettings(t *testing.T, fileName, content string) (settingsPath string, before map[string]interface{}) {
	t.Helper()
	const brokerID = "22222222-2222-2222-2222-222222222222"
	var deleted bool
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runtime-brokers/"+brokerID+"/projects":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/runtime-brokers/"+brokerID:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)

	home, globalDir := brokerTestHome(t)
	t.Setenv("SCION_HUB_ENDPOINT", hub.URL)

	settingsPath = filepath.Join(globalDir, fileName)
	content = strings.NewReplacer("HUB_URL", hub.URL, "BROKER_ID", brokerID).Replace(content)
	require.NoError(t, os.WriteFile(settingsPath, []byte(content), 0644))

	store := brokercredentials.NewMultiStore("")
	require.NoError(t, store.Save(&brokercredentials.BrokerCredentials{
		Name:         "myhub",
		BrokerID:     brokerID,
		SecretKey:    "c2VjcmV0",
		HubEndpoint:  hub.URL,
		RegisteredAt: time.Now(),
	}))

	savedProjectPath, savedAutoConfirm, savedName := projectPath, autoConfirm, brokerDeregisterName
	t.Cleanup(func() {
		projectPath, autoConfirm, brokerDeregisterName = savedProjectPath, savedAutoConfirm, savedName
	})
	projectPath = setupSecretProject(t, home, hub.URL)
	autoConfirm = true
	brokerDeregisterName = ""
	// A broker on a non-default port, found through the saved start args.
	brokerPort := newFakeBrokerServer(t, "healthy", nil)
	require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, globalDir, buildBrokerDaemonArgs(brokerPort, false, false)))

	before = readFlatYAML(t, settingsPath)
	out := captureStdout(t, func() {
		require.NoError(t, runBrokerDeregister(brokerDeregisterCmd, nil))
	})
	assert.Contains(t, out, "Broker server is running (status: healthy)", "deregister should probe the broker on port %d", brokerPort)
	require.True(t, deleted, "deregister should delete the broker on the hub")
	return settingsPath, before
}

// assertOnlyRemoved checks that every leaf of before survives in after
// except the removed keys, and that nothing was added.
func assertOnlyRemoved(t *testing.T, before, after map[string]interface{}, removed map[string]bool) {
	t.Helper()
	for key, want := range before {
		if removed[key] {
			assert.NotContains(t, after, key, "deregister should remove %s", key)
			continue
		}
		assert.Equal(t, want, after[key], "deregister must keep unrelated key %s", key)
	}
	for key := range after {
		_, existed := before[key]
		assert.True(t, existed, "deregister added unexpected key %s", key)
	}
}

// TestBrokerDeregister_KeepsUnrelatedSettings is the regression test for
// deregister rewriting the global settings file and dropping unrelated keys
// (image_registry, default_harness_config, server.broker, ...).
func TestBrokerDeregister_KeepsUnrelatedSettings(t *testing.T) {
	settingsPath, before := runDeregisterWithSettings(t, "settings.yaml", deregisterTestSettings)
	after := readFlatYAML(t, settingsPath)

	// Only the hub-connection keys deregister owns may go.
	assertOnlyRemoved(t, before, after, map[string]bool{
		"hub_connections.myhub.endpoint": true,
		"server.broker.broker_id":        true,
		"server.broker.broker_token":     true,
	})

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "# global settings (this comment must survive)")
}

// TestBrokerDeregister_LastConnectionDropsEmptyMap: deleting the only
// hub_connections entry removes the map instead of leaving
// hub_connections: {} behind.
func TestBrokerDeregister_LastConnectionDropsEmptyMap(t *testing.T) {
	content := `schema_version: "1"
image_registry: ghcr.io/example/scion
hub_connections:
  myhub:
    endpoint: HUB_URL
server:
  broker:
    instances:
      - name: flat-a
`
	settingsPath, _ := runDeregisterWithSettings(t, "settings.yaml", content)
	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	var doc map[string]interface{}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	assert.NotContains(t, doc, "hub_connections", "an emptied hub_connections map should be removed; file:\n%s", data)
	after := readFlatYAML(t, settingsPath)
	assert.Equal(t, "ghcr.io/example/scion", after["image_registry"])
	assert.Equal(t, "flat-a", after["server.broker.instances.0.name"])
}

// TestBrokerDeregister_LegacySettingsKeepMeaning: a legacy-format file
// (no schema_version, harnesses instead of harness_configs) must mean the
// same after deregister. It must not be stamped schema_version: "1" while
// still holding legacy keys, which would drop its harness configs.
func TestBrokerDeregister_LegacySettingsKeepMeaning(t *testing.T) {
	content := `active_profile: local
harnesses:
  claude:
    image: example.com/custom-claude:legacy
    user: scion
hub:
  endpoint: HUB_URL
  brokerId: BROKER_ID
hub_connections:
  myhub:
    endpoint: HUB_URL
  other:
    endpoint: https://other.example.com
`
	runDeregisterWithSettings(t, "settings.yaml", content)

	vs, _, err := config.LoadGlobalSettings()
	require.NoError(t, err)
	require.Contains(t, vs.HarnessConfigs, "claude")
	assert.Equal(t, "example.com/custom-claude:legacy", vs.HarnessConfigs["claude"].Image,
		"the legacy harness config must survive deregister")
}

// TestRemoveHubConnectionSetting_VersionedJSONRefused: a v1 JSON file
// cannot be edited in place, and the legacy writer would drop its v1 keys,
// so it is left untouched with a request to edit it by hand.
func TestRemoveHubConnectionSetting_VersionedJSONRefused(t *testing.T) {
	dir := t.TempDir()
	content := `{
  // comment
  "schema_version": "1",
  "image_registry": "ghcr.io/example",
  "hub_connections": {"myhub": {"endpoint": "https://hub.example.com"}}
}
`
	path := filepath.Join(dir, "settings.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	err := removeHubConnectionSetting(dir, "myhub")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "by hand")

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
	_, err = os.Stat(filepath.Join(dir, "settings.yaml"))
	assert.True(t, os.IsNotExist(err), "no YAML file should be written")
}

// TestRemoveHubConnectionSetting_NoEntryLeavesFileUntouched: a v1 file has
// no hub_connections entry (v1 ignores those writes at register), so
// deregister must not rewrite it at all.
func TestRemoveHubConnectionSetting_NoEntryLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	content := "schema_version: \"1\"\n# keep\nimage_registry:   ghcr.io/example\nserver:\n  broker:\n    instances:\n      - name: flat-a\n"
	path := filepath.Join(dir, "settings.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))

	require.NoError(t, removeHubConnectionSetting(dir, "myhub"))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))
}

// TestRemoveHubConnectionSetting_NoFile is a no-op without a settings file.
func TestRemoveHubConnectionSetting_NoFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, removeHubConnectionSetting(dir, "myhub"))
	_, err := os.Stat(filepath.Join(dir, "settings.yaml"))
	assert.True(t, os.IsNotExist(err), "no settings file should be created")
}

// TestRemoveHubConnectionSetting_ReadsUnderLock: the settings file is read
// under the settings lock, so the entry check and the last-entry decision
// see a change another writer made before the lock was released. Reading
// before the lock would see one entry, delete the whole hub_connections map
// and drop the entry added meanwhile.
func TestRemoveHubConnectionSetting_ReadsUnderLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nhub_connections:\n  myhub:\n    endpoint: https://a.example.com\n"), 0644))

	unlock := config.LockSettingsFile()
	done := make(chan error, 1)
	go func() { done <- removeHubConnectionSetting(dir, "myhub") }()
	// Give a pre-lock read time to happen before the file changes.
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, os.WriteFile(path, []byte("schema_version: \"1\"\nhub_connections:\n  myhub:\n    endpoint: https://a.example.com\n  other:\n    endpoint: https://b.example.com\n"), 0644))
	unlock()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("removeHubConnectionSetting did not finish")
	}
	after := readFlatYAML(t, path)
	assert.Equal(t, "https://b.example.com", after["hub_connections.other.endpoint"], "the entry added before the lock was released must survive")
	assert.NotContains(t, after, "hub_connections.myhub.endpoint")
}
