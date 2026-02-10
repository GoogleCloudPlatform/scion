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

func TestAppleContainerRuntime_Run_MemoryFlag(t *testing.T) {
	// Create a temporary script to act as a mock container command
	tmpDir := t.TempDir()
	mockContainer := filepath.Join(tmpDir, "mock-container")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockContainer, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock container: %v", err)
	}

	runtime := &AppleContainerRuntime{
		Command: mockContainer,
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

	if !strings.Contains(out, "run -d -t -m 2G") {
		t.Errorf("expected 'run -d -t -m 2G' in output, got %q", out)
	}
}