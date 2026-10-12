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
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Some runtimes do not let the host write files in the container, so Start
// writes the running phase, runtime and profile to agent-info.json before
// the container starts, not after it (ptone/scion#4766). This covers the
// path where the pre-start runtime List succeeds; when it fails, Start still
// records the run ID after the create (recordAfterCreate), which moves out
// of agent-info.json in ptone/scion#4771. The fake runtime snapshots the
// file when the container starts; any later host write replaces it
// (writeAgentInfoFile renames a temp file over it), which the comparison
// after Start detects.
func TestStart_ListOK_NoAgentInfoWriteWhileContainerRuns(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		name := "provisioned"
		if fresh {
			name = "fresh provision"
		}
		t.Run(name, func(t *testing.T) {
			projectScionDir, agentDir := startTZFixture(t, "", `""`)
			if fresh {
				if err := os.RemoveAll(agentDir); err != nil {
					t.Fatal(err)
				}
			} else {
				writeRun1AgentInfo(t, agentDir)
			}
			infoPath := filepath.Join(agentDir, "home", "agent-info.json")

			var atStart os.FileInfo
			var atStartData []byte
			started := false
			rt := &runtime.MockRuntime{
				NameFunc: func() string { return "fake" },
				ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
					if !started {
						return nil, nil
					}
					return []api.AgentInfo{{
						Name: "tz-agent", ContainerID: "cid-run-2", RunID: "run-2", Phase: "running",
						Labels: map[string]string{"scion.name": "tz-agent", api.LabelRunID: "run-2"},
					}}, nil
				},
				RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
					fi, err := os.Stat(infoPath)
					if err != nil {
						t.Fatalf("agent-info.json missing when the container starts: %v", err)
					}
					data, err := os.ReadFile(infoPath)
					if err != nil {
						t.Fatal(err)
					}
					atStart, atStartData = fi, data
					started = true
					return "cid-run-2", nil
				},
			}
			if _, err := NewManager(rt).Start(context.Background(), api.StartOptions{
				Name: "tz-agent", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
				Template: "default",
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if !started {
				t.Fatal("Start never started the container")
			}

			// Written before the container started.
			var info api.AgentInfo
			if err := json.Unmarshal(atStartData, &info); err != nil {
				t.Fatalf("agent-info.json at container start: %v", err)
			}
			if info.Phase != "running" || info.Runtime != "fake" {
				t.Errorf("agent-info.json at container start: phase %q runtime %q, want running/fake written before the start", info.Phase, info.Runtime)
			}

			// Not written while the container runs.
			after, err := os.Stat(infoPath)
			if err != nil {
				t.Fatal(err)
			}
			afterData, err := os.ReadFile(infoPath)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(atStart, after) || !after.ModTime().Equal(atStart.ModTime()) || !bytes.Equal(afterData, atStartData) {
				t.Errorf("the host wrote agent-info.json while the container was running:\nat start: %s\nafter:    %s", atStartData, afterData)
			}
		})
	}
}
