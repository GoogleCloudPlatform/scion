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

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestDockerRuntime_Run_NoInitFlag(t *testing.T) {
	// Create a temporary script to act as a mock docker
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	runtime := &DockerRuntime{
		Command: mockDocker,
	}

	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	}

	out, err := runtime.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("runtime.Run failed: %v", err)
	}

	// sciontool handles PID 1 responsibilities, so --init should NOT be present
	if strings.Contains(out, "--init") {
		t.Errorf("expected '--init' to be absent in output, got %q", out)
	}

	if !strings.Contains(out, "run -t") {
		t.Errorf("expected 'run -t' in output, got %q", out)
	}
}

func TestDockerRuntime_Exec_UserFlag(t *testing.T) {
	// Create a temporary script to act as a mock docker
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	runtime := &DockerRuntime{
		Command: mockDocker,
	}

	out, err := runtime.Exec(context.Background(), "test-container", []string{"whoami"})
	if err != nil {
		t.Fatalf("runtime.Exec failed: %v", err)
	}

	if !strings.Contains(out, "--user scion") {
		t.Errorf("expected '--user scion' in exec output, got %q", out)
	}
}

// TestDockerRuntime_List_FormatAvoidsSize guards ptone/scion#1867: the ps
// format must not reference .Size (directly or via "{{json .}}"), because that
// makes the daemon compute container sizes, which fails intermittently on
// hosts with high overlay churn.
func TestDockerRuntime_List_FormatAvoidsSize(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")
	argsFile := filepath.Join(tmpDir, "args")

	// Record the args, then emit one container line in the shape the real
	// format produces.
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsFile + `"
echo '{"ID":"abc123","Names":"proj--agent1","Status":"Up 5 minutes","Image":"scion-claude:latest","Labels":"scion.name=agent1,scion.template=developer"}'
`
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock docker: %v", err)
	}

	rt := &DockerRuntime{Command: mockDocker}
	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("failed to read recorded args: %v", err)
	}
	args := string(raw)
	if strings.Contains(args, "{{json .}}") {
		t.Errorf("ps format must not use {{json .}} (triggers size calculation); args: %q", args)
	}
	if strings.Contains(args, ".Size") {
		t.Errorf("ps format must not reference .Size; args: %q", args)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	a := agents[0]
	if a.ContainerID != "abc123" || a.Name != "agent1" || a.Image != "scion-claude:latest" ||
		a.ContainerStatus != "Up 5 minutes" || a.Template != "developer" {
		t.Errorf("unexpected parsed agent: %+v", a)
	}
}
