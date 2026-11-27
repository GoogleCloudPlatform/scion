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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// mockPodmanID is a well-formed full container ID, as "podman run -d"
// prints it.
const mockPodmanID = "a3f1c2d4e5b60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

// runPodmanRecorded runs a PodmanRuntime against a mock podman that records
// its argv and prints mockPodmanID, and returns the recorded run argv as
// one space-joined line.
func runPodmanRecorded(t *testing.T, rootless bool, config RunConfig) string {
	t.Helper()
	dir := t.TempDir()
	mockPodman := filepath.Join(dir, "mock-podman")
	argvFile := filepath.Join(dir, "argv")
	script := "#!/bin/sh\ncase \"$*\" in\n  *\"netns-setup\"*) exit 0 ;;\nesac\necho \"$@\" > '" + argvFile + "'\necho " + mockPodmanID + "\n"
	if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}
	rt := &PodmanRuntime{Command: mockPodman, Rootless: rootless}
	id, err := rt.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("runtime.Run failed: %v", err)
	}
	if id != mockPodmanID {
		t.Fatalf("runtime.Run returned %q, want %q", id, mockPodmanID)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("failed to read recorded argv: %v", err)
	}
	return strings.TrimSpace(string(argv))
}

func TestPodmanRuntime_Run_NoInitFlag(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	}

	out := runPodmanRecorded(t, false, config)

	// sciontool handles PID 1 responsibilities, so --init should NOT be present
	if strings.Contains(out, "--init") {
		t.Errorf("expected '--init' to be absent in output, got %q", out)
	}

	if !strings.Contains(out, "run -t") {
		t.Errorf("expected 'run -t' in output, got %q", out)
	}
}

func TestPodmanRuntime_Exec_UserFlag(t *testing.T) {
	// Create a temporary script to act as a mock podman
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	t.Run("rootful uses scion user", func(t *testing.T) {
		rt := &PodmanRuntime{
			Command:  mockPodman,
			Rootless: false,
		}

		out, err := rt.Exec(context.Background(), "test-container", []string{"whoami"})
		if err != nil {
			t.Fatalf("runtime.Exec failed: %v", err)
		}

		if !strings.Contains(out, "--user scion") {
			t.Errorf("expected '--user scion' in exec output, got %q", out)
		}
	})

	t.Run("rootless uses scion user", func(t *testing.T) {
		rt := &PodmanRuntime{
			Command:  mockPodman,
			Rootless: true,
		}

		out, err := rt.Exec(context.Background(), "test-container", []string{"whoami"})
		if err != nil {
			t.Fatalf("runtime.Exec failed: %v", err)
		}

		if !strings.Contains(out, "--user scion") {
			t.Errorf("expected '--user scion' in exec output, got %q", out)
		}
	})

	t.Run("rootless single-UID uses root user and /home/scion HOME", func(t *testing.T) {
		rt := &PodmanRuntime{
			Command:   mockPodman,
			Rootless:  true,
			SingleUID: true,
		}

		out, err := rt.Exec(context.Background(), "test-container", []string{"whoami"})
		if err != nil {
			t.Fatalf("runtime.Exec failed: %v", err)
		}

		if !strings.Contains(out, "-e HOME=/home/scion --user root") {
			t.Errorf("expected '-e HOME=/home/scion --user root' in exec output, got %q", out)
		}
	})
}

func TestPodmanRuntime_ExecUserMethod(t *testing.T) {
	t.Run("rootful returns scion", func(t *testing.T) {
		rt := &PodmanRuntime{Rootless: false}
		if got := rt.ExecUser(); got != "scion" {
			t.Errorf("expected 'scion', got %q", got)
		}
	})

	t.Run("rootless returns scion", func(t *testing.T) {
		rt := &PodmanRuntime{Rootless: true}
		if got := rt.ExecUser(); got != "scion" {
			t.Errorf("expected 'scion', got %q", got)
		}
	})

	t.Run("rootless single-UID returns root", func(t *testing.T) {
		rt := &PodmanRuntime{Rootless: true, SingleUID: true}
		if got := rt.ExecUser(); got != "root" {
			t.Errorf("expected 'root', got %q", got)
		}
	})
}

// TestPodmanRuntime_Run_MetadataInterception checks that rootless Podman,
// whose init drops to the keep-id user before the metadata server starts,
// never gets a required interception marker, while rootful Podman gets the
// required and runtime-source markers without granting NET_ADMIN to the
// agent container.
func TestPodmanRuntime_Run_MetadataInterception(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rootless   bool
		wantMarker string
		wantSource bool
	}{
		{name: "rootless", rootless: true, wantMarker: "SCION_METADATA_INTERCEPTION=off", wantSource: false},
		{name: "rootful", rootless: false, wantMarker: "SCION_METADATA_INTERCEPTION=required", wantSource: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := runPodmanRecorded(t, tc.rootless, RunConfig{
				Harness:              &harness.Generic{},
				Name:                 "test-agent",
				UnixUsername:         "scion",
				Image:                "scion-agent:latest",
				Task:                 "hello",
				Env:                  []string{"SCION_METADATA_MODE=block"},
				MetadataInterception: true,
			})
			if !strings.Contains(out, tc.wantMarker) {
				t.Errorf("expected %q in args, got %q", tc.wantMarker, out)
			}
			if got := strings.Contains(out, "SCION_METADATA_INTERCEPTION_SOURCE=runtime"); got != tc.wantSource {
				t.Errorf("SCION_METADATA_INTERCEPTION_SOURCE=runtime present = %v, want %v (args %q)", got, tc.wantSource, out)
			}
			if strings.Contains(out, "--cap-add NET_ADMIN") {
				t.Errorf("agent container must not receive --cap-add NET_ADMIN (args %q)", out)
			}
		})
	}
}

func TestPodmanRuntime_Run_RootlessKeepID(t *testing.T) {
	t.Run("rootless adds --userns=keep-id with uid/gid mapping and env var", func(t *testing.T) {
		config := RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			Task:         "hello",
		}
		out := runPodmanRecorded(t, true, config)
		if !strings.Contains(out, "--userns=keep-id:uid=1000,gid=1000") {
			t.Errorf("expected '--userns=keep-id:uid=1000,gid=1000' in output, got %q", out)
		}
		if !strings.Contains(out, "SCION_KEEPID_UID=1000") {
			t.Errorf("expected 'SCION_KEEPID_UID=1000' env var in output, got %q", out)
		}
	})

	t.Run("rootful omits --userns=keep-id", func(t *testing.T) {
		config := RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			Task:         "hello",
		}
		out := runPodmanRecorded(t, false, config)
		if strings.Contains(out, "--userns=keep-id") {
			t.Errorf("did not expect '--userns=keep-id' in output, got %q", out)
		}
	})

	t.Run("rootless single-UID omits --userns=keep-id and SCION_KEEPID_UID", func(t *testing.T) {
		dir := t.TempDir()
		mockPodman := filepath.Join(dir, "mock-podman")
		argvFile := filepath.Join(dir, "argv")
		script := "#!/bin/sh\necho \"$@\" > '" + argvFile + "'\necho " + mockPodmanID + "\n"
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatalf("failed to write mock podman: %v", err)
		}
		rt := &PodmanRuntime{Command: mockPodman, Rootless: true, SingleUID: true}
		cfg := RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			Task:         "hello",
		}
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("runtime.Run failed: %v", err)
		}
		argvBytes, err := os.ReadFile(argvFile)
		if err != nil {
			t.Fatalf("read argv: %v", err)
		}
		out := string(argvBytes)
		if strings.Contains(out, "--userns=keep-id") {
			t.Errorf("did not expect '--userns=keep-id' in single-UID output, got %q", out)
		}
		if strings.Contains(out, "SCION_KEEPID_UID") {
			t.Errorf("did not expect 'SCION_KEEPID_UID' in single-UID output, got %q", out)
		}
	})
}

func TestPodmanRuntime_List_JSONArray(t *testing.T) {
	// Create a mock podman that returns Podman-style JSON array output
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	jsonOutput := `[{"Id":"abc123def456","Names":["test-agent"],"Status":"Up 2 hours","Image":"scion-agent:latest","Labels":{"scion.project":"myproject","scion.template":"default"}}]`

	script := `#!/bin/sh
echo '` + jsonOutput + `'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}

	a := agents[0]
	if a.ContainerID != "abc123def456" {
		t.Errorf("expected ContainerID 'abc123def456', got %q", a.ContainerID)
	}
	if a.Name != "test-agent" {
		t.Errorf("expected Name 'test-agent', got %q", a.Name)
	}
	if a.ContainerStatus != "Up 2 hours" {
		t.Errorf("expected ContainerStatus 'Up 2 hours', got %q", a.ContainerStatus)
	}
	if a.Image != "scion-agent:latest" {
		t.Errorf("expected Image 'scion-agent:latest', got %q", a.Image)
	}
	if a.Labels["scion.project"] != "myproject" {
		t.Errorf("expected label scion.project='myproject', got %q", a.Labels["scion.project"])
	}
	if a.Template != "default" {
		t.Errorf("expected Template 'default', got %q", a.Template)
	}
	if a.Runtime != "podman" {
		t.Errorf("expected Runtime 'podman', got %q", a.Runtime)
	}
}

func TestPodmanRuntime_List_EmptyArray(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	script := `#!/bin/sh
echo '[]'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 0 {
		t.Errorf("expected 0 agents for empty array, got %d", len(agents))
	}
}

func TestPodmanRuntime_List_LabelFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	jsonOutput := `[{"Id":"aaa","Names":["agent-a"],"Status":"Up","Image":"img","Labels":{"scion.project":"project1","scion.template":"default"}},{"Id":"bbb","Names":["agent-b"],"Status":"Up","Image":"img","Labels":{"scion.project":"project2","scion.template":"custom"}}]`

	script := `#!/bin/sh
echo '` + jsonOutput + `'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	// Filter for project1 only
	agents, err := rt.List(context.Background(), map[string]string{"scion.project": "project1"})
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent after filtering, got %d", len(agents))
	}
	if agents[0].Name != "agent-a" {
		t.Errorf("expected filtered agent 'agent-a', got %q", agents[0].Name)
	}
}

// noopGlobalLayoutReporter discards every config.MigrateLegacyGlobalLayout
// report; these tests only care about the resulting filesystem state.
type noopGlobalLayoutReporter struct{}

func (noopGlobalLayoutReporter) Migrated(old, new string, tracked bool)      {}
func (noopGlobalLayoutReporter) Conflict(old, new, detail string)            {}
func (noopGlobalLayoutReporter) Skipped(old, reason, manual string)          {}
func (noopGlobalLayoutReporter) EnvIgnored(name, replacement string)         {}
func (noopGlobalLayoutReporter) PrecedenceChanged(path, value, other string) {}

// TestPodmanRuntime_List_LabelFiltering_ProjectPathThroughMigratedSymlink
// proves that, for the podman runtime, an agent whose scion.project_path
// label still holds a project's pre-rename path (recorded
// on the container before config.MigrateLegacyGlobalLayout moved that
// project) is still found when listing filters by the project's current,
// canonical path. The legacy layout is built with the real migrator, which
// symlinks each entry it moves individually — the legacy root itself stays a
// real directory — not with a hand-made root-level symlink.
func TestPodmanRuntime_List_LabelFiltering_ProjectPathThroughMigratedSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	legacyProjectPath := filepath.Join(home, ".scion", "groves", "proj")
	if err := os.MkdirAll(legacyProjectPath, 0755); err != nil {
		t.Fatal(err)
	}
	config.MigrateLegacyGlobalLayout(filepath.Join(home, ".scion"), noopGlobalLayoutReporter{})

	canonicalProjectPath := filepath.Join(home, ".scion", "projects", "proj")
	if _, err := os.Stat(canonicalProjectPath); err != nil {
		t.Fatalf("migration did not create the canonical project dir: %v", err)
	}
	siblingPath := filepath.Join(home, ".scion", "projects", "proj-2")
	if err := os.MkdirAll(siblingPath, 0755); err != nil {
		t.Fatal(err)
	}

	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")
	jsonOutput := `[{"Id":"aaa","Names":["agent-a"],"Status":"Up","Image":"img","Labels":{"scion.project_path":"` +
		legacyProjectPath + `"}}]`
	script := "#!/bin/sh\necho '" + jsonOutput + "'\n"
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}
	rt := &PodmanRuntime{Command: mockPodman}

	agents, err := rt.List(context.Background(), map[string]string{"scion.project_path": canonicalProjectPath})
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "agent-a" {
		t.Fatalf("expected agent-a to match through the migrated symlink, got %+v", agents)
	}

	agents, err = rt.List(context.Background(), map[string]string{"scion.project_path": siblingPath})
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("expected no match against a sibling project path, got %+v", agents)
	}
}

func TestPodmanRuntime_List_NoLabels(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	jsonOutput := `[{"Id":"nolabel","Names":["no-label-agent"],"Status":"Up","Image":"img","Labels":null}]`

	script := `#!/bin/sh
echo '` + jsonOutput + `'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Labels == nil {
		t.Errorf("expected non-nil Labels map, got nil")
	}
}

func TestPodmanRuntime_List_MultipleNames(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	jsonOutput := `[{"Id":"multi","Names":["primary-name","alias-name"],"Status":"Up","Image":"img","Labels":{}}]`

	script := `#!/bin/sh
echo '` + jsonOutput + `'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	// Should use the first name
	if agents[0].Name != "primary-name" {
		t.Errorf("expected first name 'primary-name', got %q", agents[0].Name)
	}
}

func TestPodmanRuntime_Name(t *testing.T) {
	rt := &PodmanRuntime{Command: "podman"}
	if rt.Name() != "podman" {
		t.Errorf("expected Name() = 'podman', got %q", rt.Name())
	}
}

func TestParsePodmanVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"podman version 4.9.3", "4.9.3"},
		{"podman version 5.0.0", "5.0.0"},
		{"podman version 3.4.1", "3.4.1"},
		{"4.9.3", "4.9.3"},
	}

	for _, tc := range tests {
		result := parsePodmanVersion(tc.input)
		if result != tc.expected {
			t.Errorf("parsePodmanVersion(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestParseMajorVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected int
		wantErr  bool
	}{
		{"4.9.3", 4, false},
		{"5.0.0", 5, false},
		{"3.4.1", 3, false},
		{"", 0, true},
	}

	for _, tc := range tests {
		result, err := parseMajorVersion(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseMajorVersion(%q) expected error, got nil", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMajorVersion(%q) unexpected error: %v", tc.input, err)
			continue
		}
		if result != tc.expected {
			t.Errorf("parseMajorVersion(%q) = %d, want %d", tc.input, result, tc.expected)
		}
	}
}

func TestParsePodmanMajorMinorVersion(t *testing.T) {
	tests := []struct {
		in        string
		wantMajor int
		wantMinor int
		wantOK    bool
	}{
		{"podman version 4.3.1", 4, 3, true},
		{"podman version 4.7.0", 4, 7, true},
		{"podman version 4.9.3", 4, 9, true},
		{"podman version 5.0.0", 5, 0, true},
		{"podman version 4.7.0-rc1", 4, 7, true},
		{"podman version 4.7-dev", 4, 7, true},
		{"podman-remote version 4.6.2", 4, 6, true},
		{"v4.7.1", 4, 7, true},
		{"WARNING: something\npodman version 4.3.1", 4, 3, true},
		{"", 0, 0, false},
		{"garbage", 0, 0, false},
		{"podman version 4", 0, 0, false},
		{"podman version x.y.z", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			major, minor, ok := parsePodmanMajorMinorVersion(tc.in)
			if ok != tc.wantOK || major != tc.wantMajor || minor != tc.wantMinor {
				t.Errorf("parsePodmanMajorMinorVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tc.in, major, minor, ok, tc.wantMajor, tc.wantMinor, tc.wantOK)
			}
		})
	}
}

func TestParsePodmanVersionJSON(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantMajor int
		wantMinor int
		wantOK    bool
	}{
		{
			name:      "prefers server version over newer client",
			in:        `{"Client":{"Version":"5.3.0"},"Server":{"Version":"4.3.1"}}`,
			wantMajor: 4, wantMinor: 3, wantOK: true,
		},
		{
			name:      "prefers server version over older client",
			in:        `{"Client":{"Version":"4.3.1"},"Server":{"Version":"4.7.0"}}`,
			wantMajor: 4, wantMinor: 7, wantOK: true,
		},
		{
			name:      "falls back to client when no server present",
			in:        `{"Client":{"Version":"4.3.1"}}`,
			wantMajor: 4, wantMinor: 3, wantOK: true,
		},
		{
			name:      "falls back to client when server version is empty",
			in:        `{"Client":{"Version":"4.9.3"},"Server":{"Version":""}}`,
			wantMajor: 4, wantMinor: 9, wantOK: true,
		},
		{
			name:      "tolerates leading warning lines",
			in:        "WARNING: something\n{\"Client\":{\"Version\":\"5.0.0\"},\"Server\":{\"Version\":\"4.6.2\"}}",
			wantMajor: 4, wantMinor: 6, wantOK: true,
		},
		{
			name:   "invalid json",
			in:     "not json",
			wantOK: false,
		},
		{
			name:   "unparseable versions",
			in:     `{"Client":{"Version":"4"},"Server":{"Version":"bad"}}`,
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, ok := parsePodmanVersionJSON(tc.in)
			if ok != tc.wantOK || major != tc.wantMajor || minor != tc.wantMinor {
				t.Errorf("parsePodmanVersionJSON(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tc.in, major, minor, ok, tc.wantMajor, tc.wantMinor, tc.wantOK)
			}
		})
	}
}

func TestPodmanSupportsHostGateway(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   bool
	}{
		{
			name: "remote server 4.3.1 with client 5.3.0 unsupported",
			script: "#!/bin/sh\n" +
				"if [ \"$1\" = \"version\" ]; then echo '{\"Client\":{\"Version\":\"5.3.0\"},\"Server\":{\"Version\":\"4.3.1\"}}'; exit 0; fi\n" +
				"echo 'podman version 5.3.0'\n",
			want: false,
		},
		{
			name: "remote server 4.7.0 with client 4.3.1 supported",
			script: "#!/bin/sh\n" +
				"if [ \"$1\" = \"version\" ]; then echo '{\"Client\":{\"Version\":\"4.3.1\"},\"Server\":{\"Version\":\"4.7.0\"}}'; exit 0; fi\n" +
				"echo 'podman version 4.3.1'\n",
			want: true,
		},
		{
			name: "local daemonless client-only json 4.3.1 unsupported",
			script: "#!/bin/sh\n" +
				"if [ \"$1\" = \"version\" ]; then echo '{\"Client\":{\"Version\":\"4.3.1\"}}'; exit 0; fi\n" +
				"exit 1\n",
			want: false,
		},
		{
			name: "local daemonless client-only json 4.7.0 supported",
			script: "#!/bin/sh\n" +
				"if [ \"$1\" = \"version\" ]; then echo '{\"Client\":{\"Version\":\"4.7.0\"}}'; exit 0; fi\n" +
				"exit 1\n",
			want: true,
		},
		{
			name: "fallback to --version when json version fails (4.3.1 unsupported)",
			script: "#!/bin/sh\n" +
				"if [ \"$1\" = \"version\" ]; then exit 1; fi\n" +
				"echo 'podman version 4.3.1'\n",
			want: false,
		},
		{
			name: "fallback to --version when json version fails (4.6.2 unsupported)",
			script: "#!/bin/sh\n" +
				"if [ \"$1\" = \"version\" ]; then exit 1; fi\n" +
				"echo 'podman version 4.6.2'\n",
			want: false,
		},
		{
			name: "fallback to --version when json version fails (4.7.0 supported)",
			script: "#!/bin/sh\n" +
				"if [ \"$1\" = \"version\" ]; then exit 1; fi\n" +
				"echo 'podman version 4.7.0'\n",
			want: true,
		},
		{
			name: "fallback to --version when json version fails (5.3.0 supported)",
			script: "#!/bin/sh\n" +
				"if [ \"$1\" = \"version\" ]; then exit 1; fi\n" +
				"echo 'podman version 5.3.0'\n",
			want: true,
		},
		{
			name:   "probe command failure defaults to supported",
			script: "#!/bin/sh\nexit 1\n",
			want:   true,
		},
		{
			name:   "unparseable output defaults to supported",
			script: "#!/bin/sh\necho 'unexpected output'\n",
			want:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockPodman := filepath.Join(t.TempDir(), "mock-podman")
			if err := os.WriteFile(mockPodman, []byte(tc.script), 0o755); err != nil {
				t.Fatalf("failed to write mock podman: %v", err)
			}
			got := PodmanSupportsHostGateway(context.Background(), mockPodman)
			if got != tc.want {
				t.Errorf("PodmanSupportsHostGateway() = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("json probe timeout still allows --version fallback with fresh timeout", func(t *testing.T) {
		mockPodman := filepath.Join(t.TempDir(), "mock-podman")
		script := "#!/bin/sh\n" +
			"if [ \"$1\" = \"version\" ]; then sleep 2; exit 0; fi\n" +
			"echo 'podman version 4.3.1'\n"
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatalf("failed to write mock podman: %v", err)
		}
		got := probePodmanHostGatewayWithTimeout(context.Background(), mockPodman, "", 100*time.Millisecond)
		if got {
			t.Errorf("probePodmanHostGatewayWithTimeout() = true, want false (fallback --version should run with fresh timeout after json probe times out)")
		}
	})
}

func TestPodmanRuntime_Run_HostGatewayProbe(t *testing.T) {
	writeMockPodman := func(t *testing.T, versionLine string) (string, string) {
		t.Helper()
		dir := t.TempDir()
		mockPodman := filepath.Join(dir, "mock-podman")
		argvFile := filepath.Join(dir, "argv")
		script := "#!/bin/sh\n" +
			"if [ \"$1\" = \"version\" ]; then\n" +
			"  exit 1\n" +
			"fi\n" +
			"if [ \"$1\" = \"--version\" ]; then\n" +
			"  echo '" + versionLine + "'\n" +
			"  exit 0\n" +
			"fi\n" +
			"if [ \"$1\" = \"info\" ]; then\n" +
			"  echo 'false'\n" +
			"  exit 0\n" +
			"fi\n" +
			"echo \"$@\" > '" + argvFile + "'\n" +
			"echo " + mockPodmanID + "\n"
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatalf("failed to write mock podman: %v", err)
		}
		return mockPodman, argvFile
	}

	baseCfg := func(extraHosts []string) RunConfig {
		return RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			Task:         "hello",
			ExtraHosts:   extraHosts,
		}
	}

	t.Run("podman 4.3.1 omits host.containers.internal:host-gateway", func(t *testing.T) {
		mockPodman, argvFile := writeMockPodman(t, "podman version 4.3.1")
		rt := &PodmanRuntime{Command: mockPodman}
		id, err := rt.Run(context.Background(), baseCfg([]string{"host.containers.internal:host-gateway"}))
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		if id != mockPodmanID {
			t.Fatalf("Run() id = %q, want %q", id, mockPodmanID)
		}
		argv, err := os.ReadFile(argvFile)
		if err != nil {
			t.Fatalf("read argv: %v", err)
		}
		if strings.Contains(string(argv), "host-gateway") {
			t.Errorf("expected host-gateway to be omitted on Podman 4.3.1, got args: %s", string(argv))
		}
	})

	t.Run("podman 4.3.1 fails with typed launch failure for non-native host-gateway", func(t *testing.T) {
		mockPodman, argvFile := writeMockPodman(t, "podman version 4.3.1")
		rt := &PodmanRuntime{Command: mockPodman}
		_, err := rt.Run(context.Background(), baseCfg([]string{"hub.example.com:host-gateway"}))
		if err == nil {
			t.Fatal("Run() error = nil, want typed launch failure")
		}
		if !errors.Is(err, ErrPodmanHostGatewayUnsupported) {
			t.Errorf("Run() error = %v, want errors.Is(_, ErrPodmanHostGatewayUnsupported)", err)
		}
		var lf *LaunchFailure
		if !errors.As(err, &lf) {
			t.Fatalf("Run() error = %T (%v), want *LaunchFailure", err, err)
		}
		if lf.Code != api.LaunchCodePodmanHostGatewayUnsupported {
			t.Errorf("LaunchFailure.Code = %q, want %q", lf.Code, api.LaunchCodePodmanHostGatewayUnsupported)
		}
		if lf.Step != api.LaunchStepLaunching {
			t.Errorf("LaunchFailure.Step = %q, want %q", lf.Step, api.LaunchStepLaunching)
		}
		if !strings.Contains(err.Error(), "4.7") {
			t.Errorf("Run() error = %q, want minimum version 4.7 mentioned", err.Error())
		}
		if _, statErr := os.Stat(argvFile); !os.IsNotExist(statErr) {
			t.Errorf("expected podman run not to be invoked when host-gateway is unsupported")
		}
	})

	t.Run("podman 4.3.1 allows explicit IP in --add-host", func(t *testing.T) {
		mockPodman, argvFile := writeMockPodman(t, "podman version 4.3.1")
		rt := &PodmanRuntime{Command: mockPodman}
		if _, err := rt.Run(context.Background(), baseCfg([]string{"hub.example.com:10.0.0.1"})); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		argv, err := os.ReadFile(argvFile)
		if err != nil {
			t.Fatalf("read argv: %v", err)
		}
		if !strings.Contains(string(argv), "--add-host hub.example.com:10.0.0.1") {
			t.Errorf("expected --add-host hub.example.com:10.0.0.1, got: %s", string(argv))
		}
	})

	t.Run("podman 4.7.0 passes host-gateway", func(t *testing.T) {
		mockPodman, argvFile := writeMockPodman(t, "podman version 4.7.0")
		rt := &PodmanRuntime{Command: mockPodman}
		if _, err := rt.Run(context.Background(), baseCfg([]string{"hub.example.com:host-gateway"})); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		argv, err := os.ReadFile(argvFile)
		if err != nil {
			t.Fatalf("read argv: %v", err)
		}
		if !strings.Contains(string(argv), "--add-host hub.example.com:host-gateway") {
			t.Errorf("expected --add-host hub.example.com:host-gateway, got: %s", string(argv))
		}
	})

	t.Run("NewPodmanRuntime caches version capability from construction", func(t *testing.T) {
		mockPodman, _ := writeMockPodman(t, "podman version 4.3.1")
		rt := NewPodmanRuntime(mockPodman)
		pr, ok := rt.(*PodmanRuntime)
		if !ok {
			t.Fatalf("NewPodmanRuntime() = %T, want *PodmanRuntime", rt)
		}
		if pr.hostGatewaySupported {
			t.Fatalf("expected cached hostGatewaySupported = false for Podman 4.3.1")
		}
		_, err := pr.Run(context.Background(), baseCfg([]string{"hub.example.com:host-gateway"}))
		if !errors.Is(err, ErrPodmanHostGatewayUnsupported) {
			t.Errorf("Run() error = %v, want ErrPodmanHostGatewayUnsupported", err)
		}
	})

	t.Run("NewPodmanRuntime prefers remote server version over newer client", func(t *testing.T) {
		dir := t.TempDir()
		mockPodman := filepath.Join(dir, "mock-podman")
		script := "#!/bin/sh\n" +
			"if [ \"$1\" = \"version\" ]; then echo '{\"Client\":{\"Version\":\"5.3.0\"},\"Server\":{\"Version\":\"4.3.1\"}}'; exit 0; fi\n" +
			"if [ \"$1\" = \"--version\" ]; then echo 'podman version 5.3.0'; exit 0; fi\n" +
			"if [ \"$1\" = \"info\" ]; then echo 'false'; exit 0; fi\n" +
			"echo " + mockPodmanID + "\n"
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatalf("failed to write mock podman: %v", err)
		}
		rt := NewPodmanRuntime(mockPodman)
		pr, ok := rt.(*PodmanRuntime)
		if !ok {
			t.Fatalf("NewPodmanRuntime() = %T, want *PodmanRuntime", rt)
		}
		_, err := pr.Run(context.Background(), baseCfg([]string{"hub.example.com:host-gateway"}))
		if !errors.Is(err, ErrPodmanHostGatewayUnsupported) {
			t.Errorf("Run() error = %v, want ErrPodmanHostGatewayUnsupported when server is 4.3.1", err)
		}
	})

	t.Run("caches unparseable probe failure once per runtime across multiple Run calls", func(t *testing.T) {
		dir := t.TempDir()
		mockPodman := filepath.Join(dir, "mock-podman")
		probeCountFile := filepath.Join(dir, "probes")
		script := "#!/bin/sh\n" +
			"if [ \"$1\" = \"version\" ] || [ \"$1\" = \"--version\" ]; then\n" +
			"  echo x >> '" + probeCountFile + "'\n" +
			"  echo 'unparseable'\n" +
			"  exit 0\n" +
			"fi\n" +
			"echo " + mockPodmanID + "\n"
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatalf("failed to write mock podman: %v", err)
		}
		rt := &PodmanRuntime{Command: mockPodman}
		for i := 0; i < 3; i++ {
			if _, err := rt.Run(context.Background(), baseCfg([]string{"hub.example.com:host-gateway"})); err != nil {
				t.Fatalf("Run() #%d error = %v, want nil (conservative default)", i+1, err)
			}
		}
		data, err := os.ReadFile(probeCountFile)
		if err != nil {
			t.Fatalf("read probe count: %v", err)
		}
		// One probe consists of `version --format json` followed by `--version` fallback = 2 lines.
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != 2 {
			t.Errorf("expected exactly 1 probe (2 version invocations: json + --version fallback) across 3 Runs, got %d invocations", len(lines))
		}
	})

	t.Run("NewPodmanRuntime caches unparseable minor version without re-probing on Run", func(t *testing.T) {
		dir := t.TempDir()
		mockPodman := filepath.Join(dir, "mock-podman")
		probeCountFile := filepath.Join(dir, "probes")
		script := "#!/bin/sh\n" +
			"if [ \"$1\" = \"version\" ]; then echo json >> '" + probeCountFile + "'; exit 1; fi\n" +
			"if [ \"$1\" = \"--version\" ]; then echo ver >> '" + probeCountFile + "'; echo 'podman version 4'; exit 0; fi\n" +
			"if [ \"$1\" = \"info\" ]; then echo 'false'; exit 0; fi\n" +
			"echo " + mockPodmanID + "\n"
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatalf("failed to write mock podman: %v", err)
		}
		rt := NewPodmanRuntime(mockPodman)
		pr, ok := rt.(*PodmanRuntime)
		if !ok {
			t.Fatalf("NewPodmanRuntime() = %T, want *PodmanRuntime", rt)
		}
		beforeData, err := os.ReadFile(probeCountFile)
		if err != nil {
			t.Fatalf("read probe count before Run: %v", err)
		}
		if _, err := pr.Run(context.Background(), baseCfg([]string{"hub.example.com:host-gateway"})); err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
		afterData, err := os.ReadFile(probeCountFile)
		if err != nil {
			t.Fatalf("read probe count after Run: %v", err)
		}
		if string(beforeData) != string(afterData) {
			t.Errorf("expected no re-probe on Run after NewPodmanRuntime, before=%q after=%q", string(beforeData), string(afterData))
		}
	})
}

func TestParsePodmanInfoJSON(t *testing.T) {
	tests := []struct {
		name          string
		output        string
		fallbackMajor int
		want          podmanHostInfo
	}{
		{
			name:          "rootful podman 5",
			output:        `{"version":{"Version":"5.8.6"},"host":{"security":{"rootless":false},"pasta":{"executable":"/usr/bin/pasta"}}}`,
			fallbackMajor: 5,
			want:          podmanHostInfo{Rootless: false, SingleUID: false, RootlessNetwork: ""},
		},
		{
			name: "rootless multi-UID podman 5 prefers pasta",
			output: `{"version":{"Version":"5.8.6"},"host":{"security":{"rootless":true},` +
				`"idMappings":{"uidmap":[{"container_id":0,"host_id":1000,"size":1},{"container_id":1,"host_id":100000,"size":65536}],` +
				`"gidmap":[{"container_id":0,"host_id":1000,"size":1},{"container_id":1,"host_id":100000,"size":65536}]},` +
				`"pasta":{"executable":"/usr/bin/pasta"},"slirp4netns":{"executable":"/usr/bin/slirp4netns"}}}`,
			fallbackMajor: 5,
			want:          podmanHostInfo{Rootless: true, SingleUID: false, RootlessNetwork: podmanRootlessNetPasta},
		},
		{
			name: "rootless single-UID podman 5 with pasta",
			output: `{"version":{"Version":"5.8.6"},"host":{"security":{"rootless":true},` +
				`"idMappings":{"uidmap":[{"container_id":0,"host_id":100001,"size":1}],"gidmap":[{"container_id":0,"host_id":100001,"size":1}]},` +
				`"pasta":{"executable":"/usr/bin/pasta"},"slirp4netns":{"executable":"/usr/bin/slirp4netns"}}}`,
			fallbackMajor: 5,
			want:          podmanHostInfo{Rootless: true, SingleUID: true, RootlessNetwork: podmanRootlessNetPasta},
		},
		{
			name: "rootless asymmetric single-GID triggers SingleUID",
			output: `{"version":{"Version":"5.3.0"},"host":{"security":{"rootless":true},` +
				`"idMappings":{"uidmap":[{"container_id":0,"host_id":1000,"size":1},{"container_id":1,"host_id":100000,"size":65536}],` +
				`"gidmap":[{"container_id":0,"host_id":1000,"size":1}]},` +
				`"pasta":{"executable":"/usr/bin/pasta"}}}`,
			fallbackMajor: 5,
			want:          podmanHostInfo{Rootless: true, SingleUID: true, RootlessNetwork: podmanRootlessNetPasta},
		},
		{
			name: "rootless podman 4 selects slirp4netns",
			output: `{"version":{"Version":"4.9.3"},"host":{"security":{"rootless":true},` +
				`"idMappings":{"uidmap":[{"container_id":0,"host_id":1000,"size":1},{"container_id":1,"host_id":100000,"size":65536}],` +
				`"gidmap":[{"container_id":0,"host_id":1000,"size":1},{"container_id":1,"host_id":100000,"size":65536}]},` +
				`"pasta":{"executable":"/usr/bin/pasta"},"slirp4netns":{"executable":"/usr/bin/slirp4netns"}}}`,
			fallbackMajor: 4,
			want:          podmanHostInfo{Rootless: true, SingleUID: false, RootlessNetwork: podmanRootlessNetSlirp4netns},
		},
		{
			name: "rootless podman 5 without pasta falls back to slirp4netns",
			output: `{"version":{"Version":"5.2.0"},"host":{"security":{"rootless":true},` +
				`"slirp4netns":{"executable":"/usr/bin/slirp4netns"}}}`,
			fallbackMajor: 5,
			want:          podmanHostInfo{Rootless: true, SingleUID: false, RootlessNetwork: podmanRootlessNetSlirp4netns},
		},
		{
			name:          "leading warning line before JSON is tolerated",
			output:        "WARN[0000] some warning\n" + `{"host":{"security":{"rootless":true},"idMappings":{"uidmap":[{"container_id":0,"host_id":501,"size":1}]},"pasta":{"executable":"/usr/bin/pasta"}}}`,
			fallbackMajor: 5,
			want:          podmanHostInfo{Rootless: true, SingleUID: true, RootlessNetwork: podmanRootlessNetPasta},
		},
		{
			name:          "legacy plain true mock fallback",
			output:        "true\n",
			fallbackMajor: 5,
			want:          podmanHostInfo{Rootless: true, SingleUID: false, RootlessNetwork: ""},
		},
		{
			name:          "legacy plain false mock fallback",
			output:        "false\n",
			fallbackMajor: 5,
			want:          podmanHostInfo{},
		},
		{
			name:          "malformed JSON fails safe",
			output:        "{not-json",
			fallbackMajor: 5,
			want:          podmanHostInfo{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePodmanInfoJSON(tc.output, tc.fallbackMajor)
			if got != tc.want {
				t.Errorf("parsePodmanInfoJSON() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPodmanRuntime_Run_RootlessLoopback(t *testing.T) {
	writeMock := func(t *testing.T, versionLine string) (string, string) {
		t.Helper()
		dir := t.TempDir()
		mockPodman := filepath.Join(dir, "mock-podman")
		argvFile := filepath.Join(dir, "argv")
		script := "#!/bin/sh\n" +
			"if [ \"$1\" = \"version\" ]; then exit 1; fi\n" +
			"if [ \"$1\" = \"--version\" ]; then echo '" + versionLine + "'; exit 0; fi\n" +
			"echo \"$@\" > '" + argvFile + "'\n" +
			"echo " + mockPodmanID + "\n"
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatalf("failed to write mock podman: %v", err)
		}
		return mockPodman, argvFile
	}

	baseCfg := func() RunConfig {
		return RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			Task:         "hello",
		}
	}

	t.Run("pasta enables map-host-loopback and pins host.containers.internal when TrustedHubEndpoint targets it", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 5.8.6")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetPasta,
		}
		cfg := baseCfg()
		cfg.BrokerMode = true
		cfg.TrustedHubEndpoint = "http://host.containers.internal:19810"
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		out := string(argv)
		if !strings.Contains(out, "--network pasta:--map-host-loopback,169.254.1.2") {
			t.Errorf("expected pasta loopback network flag, got: %s", out)
		}
		if !strings.Contains(out, "--add-host host.containers.internal:169.254.1.2") {
			t.Errorf("expected pinned host.containers.internal:169.254.1.2 add-host, got: %s", out)
		}
	})

	t.Run("slirp4netns enables allow_host_loopback and maps host.containers.internal to 10.0.2.2", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 4.9.0")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetSlirp4netns,
		}
		cfg := baseCfg()
		cfg.BrokerMode = true
		cfg.TrustedHubEndpoint = "http://host.containers.internal:8080"
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		out := string(argv)
		if !strings.Contains(out, "--network slirp4netns:allow_host_loopback=true") {
			t.Errorf("expected slirp4netns loopback network flag, got: %s", out)
		}
		if !strings.Contains(out, "--add-host host.containers.internal:10.0.2.2") {
			t.Errorf("expected host.containers.internal:10.0.2.2 add-host, got: %s", out)
		}
	})

	t.Run("slirp4netns on podman 4.3.1 preserves loopback intent when host-gateway is omitted", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 4.3.1")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetSlirp4netns,
		}
		cfg := baseCfg()
		cfg.ExtraHosts = []string{"host.containers.internal:host-gateway"}
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		out := string(argv)
		if strings.Contains(out, "host-gateway") {
			t.Errorf("expected host-gateway to be stripped on Podman 4.3.1, got: %s", out)
		}
		if !strings.Contains(out, "--network slirp4netns:allow_host_loopback=true") {
			t.Errorf("expected slirp4netns loopback network flag, got: %s", out)
		}
		if !strings.Contains(out, "--add-host host.containers.internal:10.0.2.2") {
			t.Errorf("expected host.containers.internal:10.0.2.2 add-host, got: %s", out)
		}
	})

	t.Run("slirp4netns on podman 4.8.0 replaces host.containers.internal:host-gateway with 10.0.2.2", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 4.8.0")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetSlirp4netns,
		}
		cfg := baseCfg()
		cfg.ExtraHosts = []string{"host.containers.internal:host-gateway"}
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		out := string(argv)
		if strings.Contains(out, "host-gateway") {
			t.Errorf("expected host-gateway placeholder to be replaced by 10.0.2.2 under slirp4netns, got: %s", out)
		}
		if strings.Count(out, "host.containers.internal:") != 1 || !strings.Contains(out, "--add-host host.containers.internal:10.0.2.2") {
			t.Errorf("expected single host.containers.internal:10.0.2.2 entry, got: %s", out)
		}
	})

	t.Run("slirp4netns preserves explicit IP mapping for host.containers.internal", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 4.9.0")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetSlirp4netns,
		}
		cfg := baseCfg()
		cfg.ExtraHosts = []string{"host.containers.internal:10.9.8.7"}
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		out := string(argv)
		if !strings.Contains(out, "--add-host host.containers.internal:10.9.8.7") || strings.Contains(out, "10.0.2.2") {
			t.Errorf("expected explicit IP 10.9.8.7 to be preserved without adding 10.0.2.2, got: %s", out)
		}
	})

	t.Run("broker mode ignores untrusted Env host.containers.internal (least privilege)", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 5.8.6")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetPasta,
		}
		cfg := baseCfg()
		cfg.BrokerMode = true
		cfg.TrustedHubEndpoint = "https://hub.example.com"
		cfg.Env = []string{"SCION_HUB_ENDPOINT=http://host.containers.internal:8080"}
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		if strings.Contains(string(argv), "--network") {
			t.Errorf("broker mode must not open host loopback for untrusted Env, got: %s", string(argv))
		}
	})

	t.Run("rejects lookalike hostname suffix spoofing", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 5.8.6")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetPasta,
		}
		cfg := baseCfg()
		cfg.BrokerMode = true
		cfg.TrustedHubEndpoint = "https://host.containers.internal.attacker.example"
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		if strings.Contains(string(argv), "--network") {
			t.Errorf("lookalike hostname must not trigger loopback mapping, got: %s", string(argv))
		}
	})

	t.Run("non-broker mode enables loopback when Env references host.containers.internal", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 5.8.6")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetPasta,
		}
		cfg := baseCfg()
		cfg.Env = []string{"SCION_HUB_ENDPOINT=http://host.containers.internal:8080"}
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		if !strings.Contains(string(argv), "--network pasta:--map-host-loopback,169.254.1.2") {
			t.Errorf("expected pasta loopback network flag in non-broker local mode, got: %s", string(argv))
		}
	})

	t.Run("non-broker mode enables loopback when ResolvedAuth.EnvVars references host.containers.internal", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 5.8.6")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetPasta,
		}
		cfg := baseCfg()
		cfg.ResolvedAuth = &api.ResolvedAuth{
			EnvVars: map[string]string{"GOOGLE_CLOUD_ENDPOINT": "http://host.containers.internal:18420"},
		}
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		if !strings.Contains(string(argv), "--network pasta:--map-host-loopback,169.254.1.2") {
			t.Errorf("expected pasta loopback network flag for ResolvedAuth.EnvVars, got: %s", string(argv))
		}
	})

	t.Run("explicit NetworkMode is never overridden", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 5.8.6")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        true,
			RootlessNetwork: podmanRootlessNetPasta,
		}
		cfg := baseCfg()
		cfg.NetworkMode = "custom-net"
		cfg.TrustedHubEndpoint = "http://host.containers.internal:8080"
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		out := string(argv)
		if !strings.Contains(out, "--network custom-net") || strings.Contains(out, "pasta:") {
			t.Errorf("expected explicit NetworkMode to be preserved, got: %s", out)
		}
	})

	t.Run("rootful mode does not inject rootless loopback flags", func(t *testing.T) {
		mockPodman, argvFile := writeMock(t, "podman version 5.8.6")
		rt := &PodmanRuntime{
			Command:         mockPodman,
			Rootless:        false,
			RootlessNetwork: podmanRootlessNetPasta,
		}
		cfg := baseCfg()
		cfg.TrustedHubEndpoint = "http://host.containers.internal:8080"
		if _, err := rt.Run(context.Background(), cfg); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		argv, _ := os.ReadFile(argvFile)
		if strings.Contains(string(argv), "--network") {
			t.Errorf("did not expect --network flag in rootful mode, got: %s", string(argv))
		}
	})
}

func TestNewPodmanRuntime_ProbesSingleUIDAndPasta(t *testing.T) {
	dir := t.TempDir()
	mockPodman := filepath.Join(dir, "mock-podman")
	argvFile := filepath.Join(dir, "argv")
	infoJSON := `{"version":{"Version":"5.8.6"},"host":{"security":{"rootless":true},"idMappings":{"uidmap":[{"container_id":0,"host_id":100001,"size":1}],"gidmap":[{"container_id":0,"host_id":100001,"size":1}]},"pasta":{"executable":"/usr/bin/pasta"}}}`
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'podman version 5.8.6'; exit 0; fi\n" +
		"if [ \"$1\" = \"version\" ]; then echo '{\"Client\":{\"Version\":\"5.8.6\"}}'; exit 0; fi\n" +
		"if [ \"$1\" = \"info\" ]; then echo '" + infoJSON + "'; exit 0; fi\n" +
		"echo \"$@\" > '" + argvFile + "'\n" +
		"echo " + mockPodmanID + "\n"
	if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := NewPodmanRuntime(mockPodman)
	pr, ok := rt.(*PodmanRuntime)
	if !ok {
		t.Fatalf("NewPodmanRuntime() = %T, want *PodmanRuntime", rt)
	}
	if !pr.Rootless || !pr.SingleUID || pr.RootlessNetwork != podmanRootlessNetPasta {
		t.Fatalf("unexpected probed state: Rootless=%v SingleUID=%v RootlessNetwork=%q", pr.Rootless, pr.SingleUID, pr.RootlessNetwork)
	}
	if got := pr.ExecUser(); got != "root" {
		t.Errorf("ExecUser() = %q, want %q", got, "root")
	}
	_, err := pr.Run(context.Background(), RunConfig{
		Harness:            &harness.Generic{},
		Name:               "test-agent",
		UnixUsername:       "scion",
		Image:              "scion-agent:latest",
		Task:               "hello",
		BrokerMode:         true,
		TrustedHubEndpoint: "http://host.containers.internal:19810",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	out := string(argv)
	if strings.Contains(out, "--userns=keep-id") || strings.Contains(out, "SCION_KEEPID_UID") {
		t.Errorf("single-UID runtime must omit keep-id flags, got: %s", out)
	}
	if !strings.Contains(out, "--network pasta:--map-host-loopback,169.254.1.2") {
		t.Errorf("expected pasta loopback network flag, got: %s", out)
	}
}
