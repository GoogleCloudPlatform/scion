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

package runtimebroker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Flat Runtime Brokers phase 4 (design §2.1): a flat instance sends its
// FC4-a presentation on every heartbeat, as a full snapshot, with its
// runtime target ID, the process epoch and a per-process sequence.

// TestFlatHeartbeat_SendsPresentationWithFence: every heartbeat of a flat
// instance carries PresentInstance(entry).API() for its own entry, its
// runtime target ID, the server's start time as the epoch, and a sequence
// that grows by one per heartbeat.
func TestFlatHeartbeat_SendsPresentationWithFence(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	client := &mockRuntimeBrokerService{}
	svc := f.srv.newHeartbeatService(client, f.identity.RuntimeBrokerID, f.srv.config.HubEndpoint, time.Hour)

	first := lastHeartbeat(t, svc, client)
	second := lastHeartbeat(t, svc, client)

	want := config.PresentInstance(f.instances[0]).API()
	if first.RuntimeTargetPresentation == nil || second.RuntimeTargetPresentation == nil {
		t.Fatal("a flat heartbeat carries no presentation")
	}
	if !reflect.DeepEqual(*first.RuntimeTargetPresentation, want) || !reflect.DeepEqual(*second.RuntimeTargetPresentation, want) {
		t.Fatalf("presentation = %+v, want PresentInstance(entry).API() = %+v", *first.RuntimeTargetPresentation, want)
	}
	if want.Key != flatInstanceKey || want.Target == nil || want.Target.Type != "docker" {
		t.Fatalf("the instance entry was not the fixture's: %+v", want)
	}
	if first.PresentationTargetID != f.identity.RuntimeTarget.ID || first.PresentationTargetID == "" {
		t.Errorf("presentation target ID = %q, want the instance's %q", first.PresentationTargetID, f.identity.RuntimeTarget.ID)
	}
	f.srv.mu.RLock()
	wantEpoch := f.srv.startTime.UnixNano()
	f.srv.mu.RUnlock()
	if first.PresentationEpoch != wantEpoch || second.PresentationEpoch != wantEpoch || wantEpoch == 0 {
		t.Errorf("epochs = %d, %d, want the server start time %d on both", first.PresentationEpoch, second.PresentationEpoch, wantEpoch)
	}
	if first.PresentationSeq != 1 || second.PresentationSeq != 2 {
		t.Errorf("sequences = %d, %d, want 1, 2", first.PresentationSeq, second.PresentationSeq)
	}
}

// TestFlatHeartbeat_PresentationNeverFieldsOffTheWire: with every field of
// the instance entry set, the heartbeat's JSON holds no value FC4-a never
// publishes (kubeconfig context and path, service-account policy, home
// storage leaf, volume source layout, secret targets, env values).
func TestFlatHeartbeat_PresentationNeverFieldsOffTheWire(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	evict := true
	vol := func(tag string) api.VolumeMount {
		return api.VolumeMount{Source: "NEVER-source-" + tag, Target: "/mnt/" + tag, Type: "nfs",
			Bucket: "NEVER-bucket-" + tag, Prefix: "NEVER-prefix-" + tag, Mode: "NEVER-mode-" + tag,
			Server: "NEVER-server-" + tag, VolumeName: "NEVER-volume-name-" + tag}
	}
	f.srv.config.FlatInstance.Instance = config.V1RuntimeBrokerInstanceConfig{
		Key: flatInstanceKey, Name: flatInstanceName,
		RuntimeTarget: &config.V1RuntimeTargetConfig{
			Type: "kubernetes", DisplayName: "Cluster A", Namespace: "agents",
			Context: "NEVER-context", Kubeconfig: "/NEVER-kubeconfig",
			KubernetesBlockServiceAccount:    "NEVER-block-sa",
			KubernetesServiceAccountMappings: map[string]string{"NEVER-gsa@example.iam.gserviceaccount.com": "NEVER-ksa"},
			SafeToEvict:                      &evict, HomeStorageBackend: "nfs", HomeStorageLeaf: "NEVER-leaf",
		},
		AgentDefaults: &config.V1InstanceAgentDefaults{
			Volumes: []api.VolumeMount{vol("a")},
			Secrets: []api.RequiredSecret{{Key: "API_KEY", Type: "file", Target: "NEVER-target", AlternativeEnvKeys: []string{"NEVER-ALT"}}},
			HarnessOverrides: map[string]config.V1HarnessOverride{"claude": {
				Image: "img:1", Env: map[string]string{"TOKEN": "NEVER-env-value"}, Volumes: []api.VolumeMount{vol("b")},
			}},
		},
	}
	client := &mockRuntimeBrokerService{}
	svc := f.srv.newHeartbeatService(client, f.identity.RuntimeBrokerID, f.srv.config.HubEndpoint, time.Hour)
	hb := lastHeartbeat(t, svc, client)
	raw, err := json.Marshal(hb)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"runtimeTargetPresentation"`) || !strings.Contains(string(raw), `"envKeys":["TOKEN"]`) {
		t.Fatalf("the heartbeat does not carry the populated presentation: %s", raw)
	}
	if strings.Contains(string(raw), "NEVER") {
		t.Fatalf("a never-published value is on the wire: %s", raw)
	}
	if got := hb.RuntimeTargetPresentation.Target.SafeToEvict; got == nil || !*got {
		t.Errorf("safeToEvict = %v, want true", got)
	}
}

// TestLegacyHeartbeat_NoPresentation: a legacy Runtime Broker's heartbeat
// carries no presentation and no fence fields.
func TestLegacyHeartbeat_NoPresentation(t *testing.T) {
	legacy := newTestServerWithManager(t, &mockManager{})
	client := &mockRuntimeBrokerService{}
	svc := legacy.newHeartbeatService(client, "legacy", "", time.Hour)
	hb := lastHeartbeat(t, svc, client)
	if hb.RuntimeTargetPresentation != nil || hb.PresentationTargetID != "" || hb.PresentationEpoch != 0 || hb.PresentationSeq != 0 {
		t.Fatalf("legacy heartbeat carries presentation fields: %+v", hb)
	}
	raw, _ := json.Marshal(hb)
	for _, k := range []string{"runtimeTargetPresentation", "presentationTargetId", "presentationEpoch", "presentationSeq"} {
		if strings.Contains(string(raw), k) {
			t.Errorf("legacy heartbeat JSON has %q: %s", k, raw)
		}
	}
}
