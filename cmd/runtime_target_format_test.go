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
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// flatInfoFixture is a flat Runtime Broker as the Hub returns it, with a
// presentation that sets every FC4-a field.
func flatInfoFixture() *hubclient.RuntimeBroker {
	evict := false
	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	observed := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	return &hubclient.RuntimeBroker{
		ID: "b-1", Name: "flat-k8s", Slug: "flat-k8s", Version: "1.2.3",
		Status: "online", ConnectionState: "connected", Created: created, Updated: created,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: "rt-1234", Type: "kubernetes", DisplayName: "Cluster A"},
		RuntimeTargetPresentation: &api.RuntimeTargetPresentation{
			SchemaVersion: 1, Key: "k8s-nfs-1", Name: "k8s nfs one",
			Target: &api.PresentedTarget{
				Type: "kubernetes", DisplayName: "Cluster A", Namespace: "agents", GKE: true,
				PriorityClassName: "agents-high", SharedDirStorageClass: "standard-rwx", SharedDirSize: "40Gi",
				SafeToEvict: &evict, SharedDirStorageBackend: "nfs",
				SharedDirStorageBackends: map[string]string{"data": "local", "cache": "nfs"},
				HomeStorageBackend:       "nfs",
			},
			AgentDefaults: &api.PresentedAgentDefaults{
				DefaultHarnessConfig: "claude", ImageRegistry: "registry.example/team", CloneDepth: "full",
				Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "2Gi"},
					Limits: api.ResourceList{CPU: "2", Memory: "4Gi"}, Disk: "20Gi"},
				Volumes: []api.PresentedVolume{{Target: "/mnt/a", ReadOnly: true, Type: "nfs"}},
				Secrets: []api.PresentedSecretDecl{{Key: "API_KEY", Type: "file", Description: "the API key"}},
				HarnessOverrides: map[string]api.PresentedHarnessOverride{
					"gemini": {EnvKeys: []string{"OTHER"}},
					"claude": {
						Image: "img:1", ImagePullPolicy: "Never", AuthSelectedType: "vertex-ai", User: "agent",
						EnvKeys:   []string{"EMPTY", "TOKEN"},
						Volumes:   []api.PresentedVolume{{Target: "/mnt/b", ReadOnly: true, Type: "nfs"}},
						Resources: &api.ResourceSpec{Limits: api.ResourceList{Memory: "8Gi"}},
					},
				},
			},
		},
		RuntimeTargetPresentationObservedAt: &observed,
	}
}

// compareGolden compares got with testdata/<name>, or writes it when
// SCION_TEST_UPDATE_GOLDEN=1 (projectListGoldenEnv).
func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv(projectListGoldenEnv) == "1" {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

// TestHubBrokersInfo_RuntimeTargetSections_Golden pins the flat "Runtime
// target" and "Reported defaults" sections of `hub brokers info`, byte for
// byte: safe_to_evict is shown as unset/true/false and env as keys only.
func TestHubBrokersInfo_RuntimeTargetSections_Golden(t *testing.T) {
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(nil) })
	b := flatInfoFixture()
	var buf bytes.Buffer
	writeRuntimeTargetSections(&buf, b.RuntimeTarget, b.RuntimeTargetPresentation, b.RuntimeTargetPresentationObservedAt)
	compareGolden(t, "hub-brokers-info-runtime-target.txt", buf.String())

	buf.Reset()
	writeRuntimeTargetSections(&buf, b.RuntimeTarget, nil, nil)
	compareGolden(t, "hub-brokers-info-runtime-target-none.txt", buf.String())

	buf.Reset()
	writeRuntimeTargetSections(&buf, nil, b.RuntimeTargetPresentation, nil)
	assert.Empty(t, buf.String(), "a legacy row has no runtime target section")
}

// safe_to_evict renders as unset, true or false.
func TestHubBrokersInfo_SafeToEvictTriState(t *testing.T) {
	tr, fa := true, false
	for want, v := range map[string]*bool{"unset": nil, "true": &tr, "false": &fa} {
		b := flatInfoFixture()
		b.RuntimeTargetPresentation.Target.SafeToEvict = v
		var buf bytes.Buffer
		writeRuntimeTargetSections(&buf, b.RuntimeTarget, b.RuntimeTargetPresentation, nil)
		assert.Contains(t, buf.String(), "Safe to evict:              "+want+"\n")
	}
}

// TestHubBrokersInfo_JSON_Golden pins `hub brokers info --json` for a flat
// Runtime Broker: the presentation and its observation time are included
// as the Hub returns them.
func TestHubBrokersInfo_JSON_Golden(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(brokerInfoJSON(flatInfoFixture())))
	compareGolden(t, "hub-brokers-info-flat.json", buf.String())
}
