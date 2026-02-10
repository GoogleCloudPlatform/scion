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

	"github.com/ptone/scion-agent/pkg/harness"
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
		Harness:      &harness.GeminiCLI{},
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
