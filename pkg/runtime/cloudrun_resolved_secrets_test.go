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

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/run/apiv2/runpb"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// Placeholder values only; none of these are real credentials.
const (
	crTestEnvSecretValue  = "placeholder-env-secret-value"
	crTestFileSecretValue = "placeholder-file-secret-content"
	crTestFileTarget      = "/home/scion/.config/example/creds.json"
)

func crTestResolvedSecrets() []api.ResolvedSecret {
	return []api.ResolvedSecret{
		{Name: "api-key", Type: "environment", Target: "EXAMPLE_API_KEY", Value: crTestEnvSecretValue},
		{Name: "creds", Type: "file", Target: crTestFileTarget, Value: crTestFileSecretValue},
		// Collides with a key already in cfg.Env: cfg.Env must win.
		{Name: "dup", Type: "environment", Target: "PRESET_KEY", Value: "from-secret"},
	}
}

// assertResolvedSecretsInEnv checks that env carries the env-type secret as a
// plain variable, the file-type secret inside the staged-secrets blob, and
// that a secret never overrides a key already present in cfg.Env.
func assertResolvedSecretsInEnv(t *testing.T, env map[string]string) {
	t.Helper()
	if got := env["EXAMPLE_API_KEY"]; got != crTestEnvSecretValue {
		t.Errorf("env-type secret EXAMPLE_API_KEY not delivered (got %q)", got)
	}
	if got := env["PRESET_KEY"]; got != "from-env" {
		t.Errorf("PRESET_KEY = %q, want cfg.Env value %q", got, "from-env")
	}
	blob, ok := env[stagedsecrets.EnvVar]
	if !ok {
		t.Fatalf("%s not set; file-type secret not delivered", stagedsecrets.EnvVar)
	}
	staged, err := stagedsecrets.Decode(blob)
	if err != nil {
		t.Fatalf("decode %s: %v", stagedsecrets.EnvVar, err)
	}
	if len(staged.FileSecrets) != 1 || staged.FileSecrets[0].Target != crTestFileTarget {
		t.Fatalf("staged file secrets = %+v, want one entry for %s", staged.FileSecrets, crTestFileTarget)
	}
}

func TestCloudRunRun_DeliversResolvedSecrets(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr()}
	rt := newFakeCloudRunRuntime(t, fake)

	cfg := runConfigForTest()
	cfg.UnixUsername = "scion"
	cfg.Env = []string{"PRESET_KEY=from-env"}
	cfg.ResolvedSecrets = crTestResolvedSecrets()

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.createReqs) != 1 {
		t.Fatalf("CreateInstance called %d times, want 1", len(fake.createReqs))
	}

	env := make(map[string]string)
	count := make(map[string]int)
	for _, ev := range fake.createReqs[0].Instance.Containers[0].Env {
		count[ev.Name]++
		if v, ok := ev.GetValues().(*runpb.EnvVar_Value); ok {
			env[ev.Name] = v.Value
		}
	}
	for name, n := range count {
		if n > 1 {
			t.Errorf("env var %s appears %d times in the instance spec", name, n)
		}
	}
	assertResolvedSecretsInEnv(t, env)
}

func TestCloudRunSandboxRun_DeliversResolvedSecrets(t *testing.T) {
	tmpDir := t.TempDir()
	argsFile := filepath.Join(tmpDir, "sandbox-args")
	mockBin := filepath.Join(tmpDir, "sandbox")
	script := "#!/bin/sh\nif [ \"$1\" = \"run\" ]; then\n  printf '%s\\n' \"$@\" > " + argsFile + "\nfi\necho sandbox-ok\n"
	if err := os.WriteFile(mockBin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(tmpDir, "agent-home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatal(err)
	}

	rt := &CloudRunSandboxRuntime{
		bin:          mockBin,
		state:        newSandboxStateStore(filepath.Join(tmpDir, "state.json")),
		rootDir:      filepath.Join(tmpDir, "scion"),
		watchCancels: make(map[string]context.CancelFunc),
	}
	t.Cleanup(func() {
		rt.watchMu.Lock()
		for _, cancel := range rt.watchCancels {
			cancel()
		}
		rt.watchMu.Unlock()
	})

	cfg := RunConfig{
		Name:            "secrets-agent",
		HomeDir:         homeDir,
		Workspace:       filepath.Join(tmpDir, "workspace"),
		Image:           "omni-image",
		UnixUsername:    "scion",
		Harness:         &mockHarness{command: []string{"gemini"}, env: map[string]string{}},
		Env:             []string{"PRESET_KEY=from-env"},
		ResolvedSecrets: crTestResolvedSecrets(),
	}
	_ = os.MkdirAll(cfg.Workspace, 0755)

	if _, err := rt.Run(context.Background(), cfg); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	data, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read mock binary args: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	env := make(map[string]string)
	for i := 0; i+1 < len(lines); i++ {
		if lines[i] == "--" {
			break
		}
		if lines[i] == "--env" {
			if k, v, ok := strings.Cut(lines[i+1], "="); ok {
				env[k] = v
			}
			i++
		}
	}
	assertResolvedSecretsInEnv(t, env)
}
