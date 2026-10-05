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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/google/uuid"
)

// Start labels the new runtime entry with the run ID the broker passes
// (ptone/scion#2550 P1), and mints a UUID when none is given (a local CLI
// start), so every entry carries a run label.
func TestStart_LabelsRunID(t *testing.T) {
	for _, tc := range []struct{ name, runID string }{
		{"hub run ID", "run-from-hub"},
		{"minted when absent", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectScionDir, _ := startTZFixture(t, "", `""`)
			var labels map[string]string
			rt := &runtime.MockRuntime{
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
				RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
					labels = cfg.Labels
					return "mock-id", nil
				},
			}
			info, err := NewManager(rt).Start(context.Background(), api.StartOptions{
				Name:        "tz-agent",
				ProjectPath: projectScionDir,
				BrokerMode:  true,
				NoAuth:      true,
				RunID:       tc.runID,
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
			if info.RunID != got {
				t.Errorf("returned RunID = %q, want the labelled %q", info.RunID, got)
			}
		})
	}
}
