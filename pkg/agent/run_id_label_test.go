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

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/google/uuid"
)

// Start labels the new runtime entry with the run ID the broker passes
// (ptone/scion#2550 P1), and mints a UUID when none is given (a local CLI
// start), so every entry carries a run label. SCION_LAUNCH_ID in the
// container env is the same value, whatever the request env carried.
func TestStart_LabelsRunID(t *testing.T) {
	for _, tc := range []struct {
		name, runID string
		env         map[string]string
	}{
		{name: "hub run ID", runID: "run-from-hub"},
		{name: "minted when absent"},
		{name: "request env value replaced", runID: "run-from-hub", env: map[string]string{"SCION_LAUNCH_ID": "forged"}},
		{name: "request env value replaced when minted", env: map[string]string{"SCION_LAUNCH_ID": "forged"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir, _ := startTZFixture(t, "", `""`)
			var labels map[string]string
			var env []string
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
					labels = cfg.Labels
					env = cfg.Env
					return "mock-id", nil
				},
			}
			info, err := NewManager(rt).Start(context.Background(), api.StartOptions{
				Name:        "tz-agent",
				ProjectPath: projectScionDir,
				BrokerMode:  true,
				NoAuth:      true,
				RunID:       tc.runID,
				Env:         tc.env,
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			got := labels[api.LabelRunID]
			if tc.runID != "" {
				if got != tc.runID {
					t.Errorf("label %s = %q, want %q", api.LabelRunID, got, tc.runID)
				}
			} else if _, err := uuid.Parse(got); err != nil {
				t.Errorf("label %s = %q, want a minted UUID", api.LabelRunID, got)
			}
			var launchIDs []string
			for _, kv := range env {
				if v, ok := strings.CutPrefix(kv, "SCION_LAUNCH_ID="); ok {
					launchIDs = append(launchIDs, v)
				}
			}
			if len(launchIDs) != 1 || launchIDs[0] != got {
				t.Errorf("env SCION_LAUNCH_ID = %q, want exactly the labelled %q", launchIDs, got)
			}
			if info.RunID != got {
				t.Errorf("returned RunID = %q, want the labelled %q", info.RunID, got)
			}
		})
	}
}
