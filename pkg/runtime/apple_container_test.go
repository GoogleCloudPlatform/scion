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

func TestStripUnsupportedAppleFlags(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "strips cap-add",
			in:   []string{"-e", "FOO=bar", "--cap-add", "NET_ADMIN", "--name", "x"},
			want: []string{"-e", "FOO=bar", "--name", "x"},
		},
		{
			name: "strips multiple unsupported flags",
			in:   []string{"--cap-add", "NET_ADMIN", "--device", "/dev/fuse", "--cap-add", "SYS_ADMIN", "--name", "x"},
			want: []string{"--name", "x"},
		},
		{
			name: "strips mount add-host network",
			in:   []string{"--mount", "type=tmpfs,dst=/x", "--add-host", "h:1.2.3.4", "--network", "host", "-e", "A=B"},
			want: []string{"-e", "A=B"},
		},
		{
			name: "no-op when nothing unsupported",
			in:   []string{"-e", "X=1", "--name", "y", "-v", "/a:/b"},
			want: []string{"-e", "X=1", "--name", "y", "-v", "/a:/b"},
		},
		{
			name: "empty input",
			in:   []string{},
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripUnsupportedAppleFlags(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("index %d: got %q, want %q (full: %v vs %v)", i, got[i], tt.want[i], got, tt.want)
				}
			}
		})
	}
}

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

func TestAppleContainerRuntime_Run_StripsCapAdd(t *testing.T) {
	tmpDir := t.TempDir()
	mockContainer := filepath.Join(tmpDir, "mock-container")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockContainer, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock container: %v", err)
	}

	runtime := &AppleContainerRuntime{Command: mockContainer}

	config := RunConfig{
		Harness:              &harness.GeminiCLI{},
		Name:                 "test-agent",
		UnixUsername:         "scion",
		Image:                "scion-agent:latest",
		Task:                 "hello",
		MetadataInterception: true,
	}

	out, err := runtime.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("runtime.Run failed: %v", err)
	}

	if strings.Contains(out, "--cap-add") {
		t.Errorf("Apple container args should not contain --cap-add, got %q", out)
	}
}
