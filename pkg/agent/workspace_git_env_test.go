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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestEnforceWorkspaceGitEnv(t *testing.T) {
	in := []string{"A=1", "SCION_WORKSPACE_GIT=false", "B=2", "SCION_WORKSPACE_GIT=true", "SCION_WORKSPACE_GITX=y"}
	if got, want := enforceWorkspaceGitEnv(in, "", false), []string{"A=1", "B=2", "SCION_WORKSPACE_GITX=y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("absent caller value: got %v, want %v", got, want)
	}
	if got, want := enforceWorkspaceGitEnv(in, "false", true), []string{"A=1", "B=2", "SCION_WORKSPACE_GITX=y", "SCION_WORKSPACE_GIT=false"}; !reflect.DeepEqual(got, want) {
		t.Errorf("caller value false: got %v, want %v", got, want)
	}
	if got, want := enforceWorkspaceGitEnv(nil, "true", true), []string{"SCION_WORKSPACE_GIT=true"}; !reflect.DeepEqual(got, want) {
		t.Errorf("caller value true: got %v, want %v", got, want)
	}
}

// setupWorkspaceGitStartProject is setupEmptyPerAgentStartProject with
// SCION_WORKSPACE_GIT set by the project's own settings: in the settings
// harness_configs entry env (the auth env overlay fills absent keys from it)
// and in the harness-config directory's env (the config env layer). Each
// layer also sets a probe key, to show the layer reached the container env.
func setupWorkspaceGitStartProject(t *testing.T, parent, projectValue string) string {
	t.Helper()
	projectScionDir := filepath.Join(parent, ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
    env:
      SCION_WORKSPACE_GIT: "` + projectValue + `"
      WORKSPACE_GIT_PROBE_SETTINGS: "1"
profiles:
  local:
    runtime: docker
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	hcDir := filepath.Join(projectScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hc := "harness: gemini\nuser: scion\nimage: test-image:latest\nenv:\n  SCION_WORKSPACE_GIT: \"" + projectValue + "\"\n  WORKSPACE_GIT_PROBE_HC: \"1\"\n"
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hc), 0o644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(projectScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

// The container env carries SCION_WORKSPACE_GIT exactly as the caller (the
// runtime broker) computed it: a project's settings can neither add it when
// the broker left it absent nor change the broker's value.
func TestStart_WorkspaceGitEnvIsTheCallers(t *testing.T) {
	for _, tc := range []struct {
		name         string
		projectValue string
		callerEnv    map[string]string
		want         string // "" = absent
	}{
		{name: "absent from the caller, project sets false", projectValue: "false", want: ""},
		{name: "absent from the caller, project sets true", projectValue: "true", want: ""},
		{name: "caller false, project sets true", projectValue: "true", callerEnv: map[string]string{"SCION_WORKSPACE_GIT": "false"}, want: "false"},
		{name: "caller true, project sets false", projectValue: "false", callerEnv: map[string]string{"SCION_WORKSPACE_GIT": "true"}, want: "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)
			projectScionDir := setupWorkspaceGitStartProject(t, filepath.Join(tmpDir, "project"), tc.projectValue)

			env := map[string]string{"SCION_AGENT_ID": "agent-1", "SCION_PROJECT_ID": "proj-1"}
			for k, v := range tc.callerEnv {
				env[k] = v
			}
			var captured runtime.RunConfig
			var ran bool
			mgr := NewManager(emptyPerAgentStartRuntime(&captured, &ran))
			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name: "worker", ProjectPath: projectScionDir, NoAuth: true, HarnessConfig: "test-harness", Env: env,
			}); err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			if !ran {
				t.Fatal("runtime Run was not called")
			}
			var got []string
			probes := map[string]bool{}
			for _, kv := range captured.Env {
				if strings.HasPrefix(kv, "SCION_WORKSPACE_GIT=") {
					got = append(got, strings.TrimPrefix(kv, "SCION_WORKSPACE_GIT="))
				}
				if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "WORKSPACE_GIT_PROBE_") {
					probes[k] = true
				}
			}
			// Both layers must reach the container env, or the test would
			// not exercise that merge path.
			for _, k := range []string{"WORKSPACE_GIT_PROBE_SETTINGS", "WORKSPACE_GIT_PROBE_HC"} {
				if !probes[k] {
					t.Fatalf("probe %s did not reach the container env (env %v): its env layer is not exercised", k, captured.Env)
				}
			}
			switch {
			case tc.want == "" && len(got) != 0:
				t.Fatalf("SCION_WORKSPACE_GIT = %v, want absent", got)
			case tc.want != "" && (len(got) != 1 || got[0] != tc.want):
				t.Fatalf("SCION_WORKSPACE_GIT = %v, want exactly [%s]", got, tc.want)
			}
		})
	}
}
