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

package hub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// TestApplySnapshot_LogHubLabelFollowsAppliedName checks every applied
// hub_name, including a later rename and a cleared name, reaches the "hub"
// label of records from a log handler built with a startup name
// (ptone/scion#4903).
func TestApplySnapshot_LogHubLabelFollowsAppliedName(t *testing.T) {
	t.Cleanup(func() { logging.SetHubName("") })
	hubLabel := func() any {
		t.Helper()
		var buf bytes.Buffer
		h := logging.NewGCPHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}, "scion-hub", "startup-hub")
		slog.New(h).Info("m")
		var rec map[string]any
		if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		labels, _ := rec[logging.GCPKeyLabels].(map[string]any)
		return labels["hub"]
	}

	running := &Server{}
	ApplySnapshot(running, Layer1Snapshot{HubName: "first-hub"})
	if got := hubLabel(); got != "first-hub" {
		t.Fatalf("after first apply: hub label = %v, want first-hub", got)
	}
	ApplySnapshot(running, Layer1Snapshot{HubName: "renamed-hub"})
	if got := hubLabel(); got != "renamed-hub" {
		t.Fatalf("after rename: hub label = %v, want renamed-hub", got)
	}
	// A cleared hub_name returns to the startup default, in logs too.
	ApplySnapshot(running, Layer1Snapshot{})
	if got, want := hubLabel(), config.ResolveHubNameOrDefault(""); got != want {
		t.Fatalf("after clearing: hub label = %v, want the startup default %q", got, want)
	}
}
