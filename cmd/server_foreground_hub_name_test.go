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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// A hub_name the schema rejects is warned about at startup, not fatal
// (ptone/scion#2073).
func TestWarnNonConformingHubName(t *testing.T) {
	cases := map[string]bool{
		"":         false, // unset: falls back to the hostname
		"prod-hub": false,
		"Prod.Hub": true,
	}
	for name, want := range cases {
		if got := warnNonConformingHubName(name); got != want {
			t.Errorf("warnNonConformingHubName(%q) = %v, want %v", name, got, want)
		}
	}
}

// logHubLabel logs one record through a GCPHandler built with startup, as
// initServerLogging builds it before settings load, and returns the record's
// "hub" label.
func logHubLabel(t *testing.T, startup string) any {
	t.Helper()
	var buf bytes.Buffer
	h := logging.NewGCPHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}, "scion-hub", startup)
	slog.New(h).Info("m")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("decode log record: %v", err)
	}
	labels, _ := rec[logging.GCPKeyLabels].(map[string]any)
	return labels["hub"]
}

// TestApplySnapshot_RenameUpdatesLogHubLabel checks a hub_name applied after
// startup (admin save or cross-replica propagation, both through
// ApplySnapshot) reaches log records, since the hub's own log queries filter
// on the applied name (ptone/scion#4903).
func TestApplySnapshot_RenameUpdatesLogHubLabel(t *testing.T) {
	t.Cleanup(func() { logging.SetHubName("") })
	srv := &hub.Server{}

	hub.ApplySnapshot(srv, hub.Layer1Snapshot{HubName: "first-hub"})
	if got := logHubLabel(t, "startup-hub"); got != "first-hub" {
		t.Fatalf("after first apply: hub label = %v, want first-hub", got)
	}

	hub.ApplySnapshot(srv, hub.Layer1Snapshot{HubName: "renamed-hub"})
	if got := srv.HubName(); got != "renamed-hub" {
		t.Fatalf("HubName() = %q, want renamed-hub", got)
	}
	if got := logHubLabel(t, "startup-hub"); got != "renamed-hub" {
		t.Errorf("after rename: hub label = %v, want renamed-hub", got)
	}
}
