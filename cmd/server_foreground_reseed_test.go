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
	"context"
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// ptone/scion#2073: settings.yaml default_runtime_broker
// and default_timezone reach the hub through the boot reseed. Before
// extractAgentDefaults listed them, the seeded agent_defaults row dropped
// both and the hub applied "". The hub side (hubAgentDefaults after
// ApplySnapshot) is pinned by
// TestHubAgentDefaults_RuntimeBrokerAndTimezoneSurviveReseed.
func TestBootReseed_FileRuntimeBrokerAndTimezoneSurvive(t *testing.T) {
	fs := newFakeHubSettingStore()
	fileK := koanf.New(".")
	_ = fileK.Load(confmap.Provider(map[string]interface{}{
		"default_runtime_broker": "broker-1",
		"default_timezone":       "America/Los_Angeles",
		"server.hub.hub_name":    "prod-hub",
	}, "."), nil)

	ctx := context.Background()
	if err := syncHubSettings(ctx, fs, fileK); err != nil {
		t.Fatalf("syncHubSettings: %v", err)
	}
	ops := hub.NewOperationalSettings(fs, fileK, koanf.New("."))
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snap := ops.Snapshot()
	if snap.DefaultRuntimeBroker != "broker-1" {
		t.Errorf("Snapshot DefaultRuntimeBroker = %q, want broker-1", snap.DefaultRuntimeBroker)
	}
	if snap.DefaultTimezone != "America/Los_Angeles" {
		t.Errorf("Snapshot DefaultTimezone = %q, want America/Los_Angeles", snap.DefaultTimezone)
	}
	srv := &hub.Server{}
	hub.ApplySnapshot(srv, snap)
	if got := srv.HubName(); got != "prod-hub" {
		t.Errorf("HubName() = %q, want prod-hub", got)
	}

	fs.mu.Lock()
	var row map[string]interface{}
	_ = json.Unmarshal(fs.settings["agent_defaults"].Value, &row)
	fs.mu.Unlock()
	if row["default_runtime_broker"] != "broker-1" || row["default_timezone"] != "America/Los_Angeles" {
		t.Errorf("seeded agent_defaults row = %v, want default_runtime_broker and default_timezone", row)
	}
}
