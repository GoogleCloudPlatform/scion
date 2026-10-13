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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestRunAgent_SetsHarnessReadyEnv starts an agent from a declarative
// harness-config (it has a command, so it resolves to the config's own
// harness rather than Generic) and checks the container gets SCION_HARNESS_READY with the config's resolved
// ready spec, overwriting a value the caller (a template) supplied, on the
// first start and on a resume.
func TestRunAgent_SetsHarnessReadyEnv(t *testing.T) {
	for _, tt := range []struct {
		name  string
		ready string
		want  api.ReadySpec
	}{
		{
			name:  "hook block",
			ready: "ready:\n  strategy: hook\n  settle: 2s\n  timeout: 30s\n",
			want:  api.ReadySpec{Strategy: state.ReadyStrategyHook, Settle: 2 * time.Second, Timeout: 30 * time.Second},
		},
		{
			name:  "no block uses the delay defaults",
			ready: "",
			want:  state.DefaultReadySpec(state.ReadyStrategyDelay),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			oldWd, _ := os.Getwd()
			_ = os.Chdir(tmpDir)
			t.Cleanup(func() { _ = os.Chdir(oldWd) })
			t.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")
			hcDir := filepath.Join(globalScionDir, "harness-configs", "ready-hc")
			_ = os.MkdirAll(filepath.Join(hcDir, "home"), 0o755)
			_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"),
				[]byte("harness: claude\nuser: scion\nimage: scion-claude:latest\ncommand:\n  base: [claude]\n"+tt.ready), 0o644)
			tplDir := filepath.Join(globalScionDir, "templates", "default")
			_ = os.MkdirAll(tplDir, 0o755)
			_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config":"ready-hc"}`), 0o644)
			_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0o644)
			projectScionDir := filepath.Join(tmpDir, "project", ".scion")
			_ = os.MkdirAll(projectScionDir, 0o755)

			var captured runtime.RunConfig
			mgr := NewManager(&runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					captured = config
					return "mock-id", nil
				},
			})
			check := func(step string) {
				t.Helper()
				var values []string
				for _, e := range captured.Env {
					if v, ok := strings.CutPrefix(e, state.HarnessReadyEnv+"="); ok {
						values = append(values, v)
					}
				}
				if len(values) != 1 {
					t.Fatalf("%s: %s values = %q, want exactly one", step, state.HarnessReadyEnv, values)
				}
				var got api.ReadySpec
				if err := json.Unmarshal([]byte(values[0]), &got); err != nil {
					t.Fatalf("%s: %s = %q is not JSON: %v", step, state.HarnessReadyEnv, values[0], err)
				}
				if got != tt.want {
					t.Errorf("%s: %s = %+v, want %+v", step, state.HarnessReadyEnv, got, tt.want)
				}
			}

			templateValue := map[string]string{state.HarnessReadyEnv: `{"Strategy":"delay","Settle":1}`}
			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name: "ready-agent", Template: tplDir, ProjectPath: projectScionDir,
				NoAuth: true, Env: templateValue,
			}); err != nil {
				t.Fatalf("create Start: %v", err)
			}
			check("create")

			captured = runtime.RunConfig{}
			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name: "ready-agent", ProjectPath: projectScionDir, Resume: true,
				NoAuth: true, Env: map[string]string{state.HarnessReadyEnv: `{"Strategy":"delay","Settle":1}`},
			}); err != nil {
				t.Fatalf("resume Start: %v", err)
			}
			check("resume")
		})
	}
}
