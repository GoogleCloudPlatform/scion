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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// bootWithBootstrap simulates a hub boot: syncHubSettings reseeds from the
// bootstrap koanf, then OperationalSettings refreshes from the store and the
// snapshot is applied to a server, exactly as server_foreground does.
func bootWithBootstrap(t *testing.T, fs *fakeHubSettingStore, bootstrapK *koanf.Koanf) *hub.Server {
	t.Helper()
	ctx := context.Background()
	if err := syncHubSettings(ctx, fs, bootstrapK); err != nil {
		t.Fatalf("syncHubSettings: %v", err)
	}
	ops := hub.NewOperationalSettings(fs, bootstrapK, koanf.New("."))
	if _, err := ops.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv := &hub.Server{}
	hub.ApplySnapshot(srv, ops.Snapshot())
	return srv
}

// Design §5.A item 7 / AC5: a file (settings.yaml) default_user_role survives
// the boot reseed in SQLite/DB-backed mode. Before the opsettings map fix the
// reseeded access row dropped the key and the hub reverted to member.
func TestBootReseed_FileDefaultUserRoleSurvivesRestart(t *testing.T) {
	fs := newFakeHubSettingStore()
	fileK := koanf.New(".")
	_ = fileK.Load(confmap.Provider(map[string]interface{}{
		"server.auth.default_user_role": "viewer",
	}, "."), nil)

	// First boot: empty table → access seeded.
	if got := bootWithBootstrap(t, fs, fileK).DefaultUserRole(); got != "viewer" {
		t.Errorf("first boot DefaultUserRole() = %q, want viewer", got)
	}
	fs.mu.Lock()
	var access struct {
		DefaultUserRole string `json:"default_user_role"`
	}
	_ = json.Unmarshal(fs.settings["access"].Value, &access)
	fs.mu.Unlock()
	if access.DefaultUserRole != "viewer" {
		t.Errorf("seeded access row default_user_role = %q, want viewer", access.DefaultUserRole)
	}

	// Restart: a seeded row written before the fix (key missing) is updated.
	fs.mu.Lock()
	fs.settings["access"].Value = json.RawMessage(`{}`)
	fs.mu.Unlock()
	if got := bootWithBootstrap(t, fs, fileK).DefaultUserRole(); got != "viewer" {
		t.Errorf("restart DefaultUserRole() = %q, want viewer", got)
	}
}

// AC6: SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE seeds the shared value.
func TestBootReseed_SeedEnvDefaultUserRole(t *testing.T) {
	t.Setenv("SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE", "viewer")
	fs := newFakeHubSettingStore()
	if got := bootWithBootstrap(t, fs, config.LoadSeedEnvKoanf()).DefaultUserRole(); got != "viewer" {
		t.Errorf("DefaultUserRole() = %q, want viewer from SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE", got)
	}
}
