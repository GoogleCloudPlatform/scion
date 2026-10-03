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

//go:build !no_sqlite

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestInitOperationalSettings_ProfileTimezoneRetire_TwoBoots runs the real
// boot path (every-boot seed sync, Refresh, retirement) twice on a DB-tier
// hub whose profiles row is seeded from settings.yaml and still carries the
// legacy key, as an older hub's sync wrote it. The first boot strips the row
// and copies the single zone into agent_defaults (now managed); the second
// boot writes nothing.
func TestInitOperationalSettings_ProfileTimezoneRetire_TwoBoots(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const settingsBody = "schema_version: \"1\"\n" +
		"profiles:\n" +
		"  a:\n    runtime: docker\n    timezone: Asia/Kathmandu\n" +
		"  team.west:\n    runtime: docker\n    timezone: Asia/Kathmandu\n"
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	if err := os.WriteFile(settingsPath, []byte(settingsBody), 0o600); err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	hs := st.(store.HubSettingStore)
	legacyRow := json.RawMessage(`{"a":{"runtime":"docker","timezone":"Asia/Kathmandu"},"team":{"west":{"runtime":"docker","timezone":"Asia/Kathmandu"}}}`)
	if _, err := hs.UpsertHubSetting(ctx, "profiles", legacyRow, "seed", 0, "seeded"); err != nil {
		t.Fatalf("seeding legacy profiles row: %v", err)
	}

	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "postgres"

	boot := func() (hub.Layer1Snapshot, string) {
		t.Helper()
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer slog.SetDefault(prev)

		srv, err := hub.New(hub.ServerConfig{}, st)
		if err != nil {
			t.Fatalf("hub.New: %v", err)
		}
		srv.SetIntegrationHA("postgres", nil, "")
		if err := initOperationalSettings(ctx, cfg, srv, st, globalDir); err != nil {
			t.Fatalf("initOperationalSettings: %v", err)
		}
		return srv.GetOperationalSettings().Snapshot(), buf.String()
	}
	rows := func() map[string]store.HubSetting {
		t.Helper()
		list, err := hs.ListHubSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]store.HubSetting{}
		for _, r := range list {
			out[r.Section] = r
		}
		return out
	}

	// Boot 1.
	snap, out := boot()
	if snap.DefaultTimezone != "Asia/Kathmandu" {
		t.Errorf("boot 1: DefaultTimezone = %q, want Asia/Kathmandu", snap.DefaultTimezone)
	}
	after1 := rows()
	prof := after1["profiles"]
	if strings.Contains(string(prof.Value), "timezone") || prof.Origin != "seeded" {
		t.Errorf("boot 1: profiles row %s origin %q; want no timezone, still seeded", prof.Value, prof.Origin)
	}
	if !strings.Contains(string(prof.Value), "docker") {
		t.Errorf("boot 1: profiles row lost other keys: %s", prof.Value)
	}
	ad := after1["agent_defaults"]
	var adDoc map[string]any
	_ = json.Unmarshal(ad.Value, &adDoc)
	if adDoc["default_timezone"] != "Asia/Kathmandu" || ad.Origin != "managed" || ad.UpdatedBy != "system:profile-timezone-retire" {
		t.Errorf("boot 1: agent_defaults %s origin %q by %q; want the zone, managed, system:profile-timezone-retire", ad.Value, ad.Origin, ad.UpdatedBy)
	}
	if n := strings.Count(out, "agent_defaults is now admin-managed"); n != 1 {
		t.Errorf("boot 1: admin-managed lines = %d, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "remove it from the file"); n != 2 {
		t.Errorf("boot 1: remove warnings = %d, want 2:\n%s", n, out)
	}
	if !strings.Contains(out, "profile=team.west") {
		t.Errorf("boot 1: warning lacks the dotted profile name:\n%s", out)
	}

	// Boot 2: nothing to do.
	snap, out = boot()
	if snap.DefaultTimezone != "Asia/Kathmandu" {
		t.Errorf("boot 2: DefaultTimezone = %q, want Asia/Kathmandu", snap.DefaultTimezone)
	}
	after2 := rows()
	for section, r1 := range after1 {
		if section == "_meta" {
			continue // the seed sync's own sentinel is rewritten every boot
		}
		if r2 := after2[section]; r2.Revision != r1.Revision {
			t.Errorf("boot 2 wrote %s: revision %d -> %d", section, r1.Revision, r2.Revision)
		}
	}
	if strings.Contains(out, "admin-managed") || strings.Contains(out, "copied into") {
		t.Errorf("boot 2 copied again:\n%s", out)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil || string(data) != settingsBody {
		t.Errorf("settings.yaml changed (err %v):\n%s", err, data)
	}
}
