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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// TestInitOperationalSettings_TelemetryAndLogsUseAppliedHubName checks the
// hub name in the metrics identity and in log records is the one applied
// from the stored operational settings, not the startup configuration's
// (ptone/scion#4903). When the two match nothing changes.
func TestInitOperationalSettings_TelemetryAndLogsUseAppliedHubName(t *testing.T) {
	for _, tc := range []struct {
		name, startup, stored, want string
	}{
		{name: "stored differs", startup: "startup-hub", stored: "applied-hub", want: "applied-hub"},
		{name: "stored matches", startup: "startup-hub", stored: "startup-hub", want: "startup-hub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { logging.SetHubName("") })
			ctx := context.Background()
			home := t.TempDir()
			t.Setenv("HOME", home)
			globalDir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(globalDir, 0o755); err != nil {
				t.Fatal(err)
			}
			// A node-level override would win over the stored value.
			// t.Setenv registers the restore of any inherited value;
			// os.Unsetenv then removes it for this test.
			t.Setenv("SCION_SERVER_HUB_HUBNAME", "")
			if err := os.Unsetenv("SCION_SERVER_HUB_HUBNAME"); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SCION_SEED_SERVER_HUB_HUBNAME", tc.stored)

			cfg := &config.GlobalConfig{}
			st := newTestStore(t)
			srv, err := hub.New(hub.ServerConfig{HubName: tc.startup}, st)
			if err != nil {
				t.Fatalf("hub.New: %v", err)
			}
			if err := initOperationalSettings(ctx, cfg, srv, st, globalDir); err != nil {
				t.Fatalf("initOperationalSettings: %v", err)
			}
			if got := srv.HubName(); got != tc.want {
				t.Fatalf("applied HubName() = %q, want %q", got, tc.want)
			}

			// Metrics identity, as runServer builds it after initHubServer.
			id := newHubTelemetryIdentity(srv)
			res, err := hubmetrics.NewResource(ctx, id.metricsOptions()...)
			if err != nil {
				t.Fatalf("metrics resource: %v", err)
			}
			if got := resourceAttr(res, hubmetrics.HubNameAttribute); got != tc.want {
				t.Errorf("metrics %s = %q, want %q", hubmetrics.HubNameAttribute, got, tc.want)
			}

			// Log records from a handler built with the startup name.
			if got := logHubLabel(t, tc.startup); got != tc.want {
				t.Errorf("log record hub label = %v, want %q", got, tc.want)
			}
		})
	}
}
