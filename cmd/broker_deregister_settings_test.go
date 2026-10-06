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

// TestBrokerDeregister_KeepsUnrelatedSettings is the regression test for
// deregister rewriting the global settings file and dropping unrelated keys
// (image_registry, default_harness_config, server.broker, ...).
func TestBrokerDeregister_KeepsUnrelatedSettings(t *testing.T) {
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

	settingsPath := filepath.Join(globalDir, "settings.yaml")
	content := strings.NewReplacer("HUB_URL", hub.URL, "BROKER_ID", brokerID).Replace(deregisterTestSettings)
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
	setBrokerFlagForTest(t, brokerDeregisterCmd, "port", strconv.Itoa(unusedPort(t)))

	before := readFlatYAML(t, settingsPath)

	captureStdout(t, func() {
		require.NoError(t, runBrokerDeregister(brokerDeregisterCmd, nil))
	})
	require.True(t, deleted, "deregister should delete the broker on the hub")

	after := readFlatYAML(t, settingsPath)

	// Only the hub-connection keys deregister owns may go.
	removed := map[string]bool{
		"hub_connections.myhub.endpoint": true,
		"server.broker.broker_id":        true,
		"server.broker.broker_token":     true,
	}
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

	data, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "# global settings (this comment must survive)")
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
