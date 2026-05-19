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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestResolveContainerID(t *testing.T) {
	agents := []api.AgentInfo{
		{
			ContainerID: "abc123def456789",
			Name:        "my-agent",
		},
		{
			ContainerID: "def456abc789012",
			Name:        "mygrove--other-agent",
		},
		{
			ContainerID: "fed987cba654321",
			Name:        "/slash-agent", // Docker sometimes returns names with leading /
		},
	}

	tests := []struct {
		name string
		id   string
		want string
	}{
		{
			name: "exact container ID",
			id:   "abc123def456789",
			want: "abc123def456789",
		},
		{
			name: "exact name match",
			id:   "my-agent",
			want: "abc123def456789",
		},
		{
			name: "agent name has leading slash, input does not",
			id:   "slash-agent",
			want: "fed987cba654321",
		},
		{
			name: "short container ID prefix (12 chars)",
			id:   "abc123def456",
			want: "abc123def456789",
		},
		{
			name: "grove-prefixed container name",
			id:   "mygrove--other-agent",
			want: "def456abc789012",
		},
		{
			name: "slug not matching container name (broker scenario)",
			id:   "other-agent",
			want: "other-agent", // no match — fallback to raw id
		},
		{
			name: "no match returns raw id",
			id:   "nonexistent",
			want: "nonexistent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveContainerID(agents, tt.id)
			if got != tt.want {
				t.Errorf("resolveContainerID(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestResolveContainerID_SlugMatchesAgentName(t *testing.T) {
	// Simulates the actual bug: container is named "grove--foo" but the
	// scion.name label (populated into AgentInfo.Name) is "foo".  The broker
	// passes the slug "foo" to Exec; the runtime must resolve it.
	agents := []api.AgentInfo{
		{
			ContainerID: "a1b2c3d4e5f60000",
			Name:        "foo", // scion.name label (slugified)
		},
	}

	got := resolveContainerID(agents, "foo")
	if got != "a1b2c3d4e5f60000" {
		t.Errorf("resolveContainerID(\"foo\") = %q, want %q", got, "a1b2c3d4e5f60000")
	}
}

func TestBuildCommonRunArgs(t *testing.T) {
	tmpHome := t.TempDir()
	tmpWorkspace := t.TempDir()

	// Set up test environment variable for volume expansion test
	t.Setenv("TEST_SCION_VOL_PATH", "/test/go")

	// Setup some dummy auth files
	tmpDir := t.TempDir()
	oauthFile := filepath.Join(tmpDir, "oauth.json")
	os.WriteFile(oauthFile, []byte("{}"), 0644)
	adcFile := filepath.Join(tmpDir, "adc.json")
	os.WriteFile(adcFile, []byte("{}"), 0644)

	tests := []struct {
		name    string
		config  RunConfig
		wantIn  []string
		wantOut []string
	}{
		{
			name: "basic config",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Task:         "hello",
			},
			wantIn: []string{"run", "-d", "-i", "--name", "test-agent", "scion-agent:latest", "sh", "-c", "tmux new-session -d -s scion -n agent"},
		},
		{
			name: "workspace and home",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				HomeDir:      tmpHome,
				Workspace:    tmpWorkspace,
				Task:         "hello",
			},
			wantIn: []string{
				"-v", fmt.Sprintf("%s:/home/scion", tmpHome),
				"-v", fmt.Sprintf("%s:/workspace", tmpWorkspace),
				"--workdir", "/workspace",
			},
		},
		{
			name: "gemini api key",
			config: RunConfig{
				Harness: &harness.GeminiCLI{},
				Name:    "test-agent",
				ResolvedAuth: &api.ResolvedAuth{
					Method: "api-key",
					EnvVars: map[string]string{
						"GEMINI_API_KEY":           "sk-123",
						"GEMINI_DEFAULT_AUTH_TYPE": "gemini-api-key",
					},
				},
				Image: "scion-agent:latest",
			},
			wantIn:  []string{"-e", "GEMINI_API_KEY=sk-123", "-e", "GEMINI_DEFAULT_AUTH_TYPE=gemini-api-key"},
			wantOut: []string{"--prompt-interactive"},
		},
		{
			name: "labels",
			config: RunConfig{
				Harness: &harness.GeminiCLI{},
				Name:    "test-agent",
				Labels: map[string]string{
					"foo": "bar",
				},
				Image: "scion-agent:latest",
				Task:  "hello",
			},
			wantIn: []string{
				"--label", "foo=bar",
				"tmux new-session -d -s scion -n agent",
			},
		},
		{
			name: "oauth propagation with home",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				HomeDir:      tmpHome,
				ResolvedAuth: &api.ResolvedAuth{
					Method: "oauth-personal",
					EnvVars: map[string]string{
						"GEMINI_DEFAULT_AUTH_TYPE": "oauth-personal",
					},
					Files: []api.FileMapping{
						{SourcePath: oauthFile, ContainerPath: "~/.gemini/oauth_creds.json"},
					},
				},
				Image: "scion-agent:latest",
			},
			wantIn: []string{"-e", "GEMINI_DEFAULT_AUTH_TYPE=oauth-personal"},
		},
		{
			name: "adc propagation without home",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				ResolvedAuth: &api.ResolvedAuth{
					Method: "compute-default-credentials",
					EnvVars: map[string]string{
						"GEMINI_DEFAULT_AUTH_TYPE": "compute-default-credentials",
					},
					Files: []api.FileMapping{
						{SourcePath: adcFile, ContainerPath: "~/.config/gcp/application_default_credentials.json"},
					},
				},
				Image: "scion-agent:latest",
			},
			wantIn: []string{
				"-v", fmt.Sprintf("%s:/home/scion/.config/gcp/application_default_credentials.json:ro", adcFile),
				"-e", "GEMINI_DEFAULT_AUTH_TYPE=compute-default-credentials",
			},
		},
		{
			name: "other auth and model",
			config: RunConfig{
				Harness: &harness.GeminiCLI{},
				Name:    "test-agent",
				ResolvedAuth: &api.ResolvedAuth{
					Method: "api-key",
					EnvVars: map[string]string{
						"GOOGLE_API_KEY":           "google-123",
						"GOOGLE_CLOUD_PROJECT":     "my-project",
						"GEMINI_DEFAULT_AUTH_TYPE": "gemini-api-key",
					},
				},
				Env:   []string{"GEMINI_MODEL=gemini-1.5-pro"},
				Image: "scion-agent:latest",
			},
			wantIn: []string{
				"-e GOOGLE_API_KEY=google-123",
				"-e GOOGLE_CLOUD_PROJECT=my-project",
				"-e GEMINI_MODEL=gemini-1.5-pro",
			},
		},
		{
			name: "resume and env",
			config: RunConfig{
				Harness: &harness.GeminiCLI{},
				Name:    "test-agent",
				Image:   "scion-agent:latest",
				Env:     []string{"FOO=BAR"},
				Task:    "hello",
				Resume:  true,
			},
			wantIn: []string{
				"-e FOO=BAR",
				"tmux new-session -d -s scion -n agent 'gemini' '--yolo' '--resume' '--prompt-interactive' 'hello'",
			},
		},
		{
			name: "resume with tmux",
			config: RunConfig{
				Harness: &harness.GeminiCLI{},
				Name:    "test-agent",
				Image:   "scion-agent:latest",
				Task:    "hello",
				Resume:  true,
			},
			wantIn: []string{
				"tmux new-session -d -s scion -n agent 'gemini' '--yolo' '--resume' '--prompt-interactive' 'hello'",
			},
		},
		{
			name: "template label",
			config: RunConfig{
				Harness:  &harness.GeminiCLI{},
				Name:     "test-agent",
				Image:    "scion-agent:latest",
				Template: "my-template",
			},
			wantIn: []string{
				"--label scion.template=my-template",
			},
		},
		{
			name: "oauth without home",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				ResolvedAuth: &api.ResolvedAuth{
					Method: "oauth-personal",
					EnvVars: map[string]string{
						"GEMINI_DEFAULT_AUTH_TYPE": "oauth-personal",
					},
					Files: []api.FileMapping{
						{SourcePath: oauthFile, ContainerPath: "~/.gemini/oauth_creds.json"},
					},
				},
				Image: "scion-agent:latest",
			},
			wantIn: []string{
				"-v " + oauthFile + ":/home/scion/.gemini/oauth_creds.json:ro",
				"-e GEMINI_DEFAULT_AUTH_TYPE=oauth-personal",
			},
		},
		{
			name: "git relative workspace",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				RepoRoot:     "/home/user/repo",
				Workspace:    "/home/user/repo/.scion/agents/test-agent/workspace",
				Image:        "scion-agent:latest",
			},
			wantIn: []string{
				"-v /home/user/repo/.git:/repo-root/.git",
				"-v /home/user/repo/.scion/agents/test-agent/workspace:/repo-root/.scion/agents/test-agent/workspace",
				"--workdir /repo-root/.scion/agents/test-agent/workspace",
			},
		},
		{
			name: "shared workspace (workspace equals repo root)",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				RepoRoot:     "/home/user/repo",
				Workspace:    "/home/user/repo",
				Image:        "scion-agent:latest",
			},
			wantIn: []string{
				"-v /home/user/repo:/workspace",
				"--workdir /workspace",
			},
			wantOut: []string{
				"/repo-root",
			},
		},
		{
			name: "generic volumes",
			config: RunConfig{
				Harness: &harness.GeminiCLI{},
				Volumes: []api.VolumeMount{
					{Source: "/host/path", Target: "/container/path", ReadOnly: true},
					{Source: "/host/data", Target: "/container/data", ReadOnly: false},
				},
				Image: "scion-agent:latest",
			},
			wantIn: []string{
				"-v /host/path:/container/path:ro",
				"-v /host/data:/container/data",
			},
		},
		{
			name: "volume expansion",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				UnixUsername: "scion",
				Volumes: []api.VolumeMount{
					{Source: "~/.config/gcloud", Target: "~/.config/gcloud", ReadOnly: true},
				},
				Image: "scion-agent:latest",
			},
			wantIn: []string{
				fmt.Sprintf("-v %s/.config/gcloud:/home/scion/.config/gcloud:ro", func() string {
					h, _ := os.UserHomeDir()
					return h
				}()),
			},
		},
		{
			name: "volume env var expansion",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				UnixUsername: "scion",
				Volumes: []api.VolumeMount{
					{Source: "${TEST_SCION_VOL_PATH}/pkg", Target: "/container/go/pkg", ReadOnly: false},
				},
				Image: "scion-agent:latest",
			},
			wantIn: []string{
				"-v /test/go/pkg:/container/go/pkg",
			},
		},
		{
			name: "attach without task",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Task:         "",
			},
			wantIn:  []string{"gemini", "--yolo"},
			wantOut: []string{"--prompt-interactive"},
		},
		{
			name: "workspace from volumes",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Volumes: []api.VolumeMount{
					{Source: "/host/project", Target: "/workspace", ReadOnly: false},
				},
			},
			wantIn: []string{
				"-v /host/project:/workspace",
				"--workdir /workspace",
			},
		},
		{
			name: "workspace precedence over volumes",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Workspace:    "/dedicated/workspace",
				Volumes: []api.VolumeMount{
					{Source: "/host/project", Target: "/workspace", ReadOnly: false},
				},
			},
			wantIn: []string{
				"-v /dedicated/workspace:/workspace",
				"--workdir /workspace",
			},
			wantOut: []string{
				"-v /host/project:/workspace",
			},
		},
		{
			name: "host uid and gid",
			config: RunConfig{
				Harness: &harness.GeminiCLI{},
				Image:   "scion-agent:latest",
			},
			wantIn: []string{
				"-e SCION_HOST_UID=",
				"-e SCION_HOST_GID=",
			},
		},
		{
			name: "git clone mode skips workspace mount",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				GitClone: &api.GitCloneConfig{
					URL:    "https://github.com/example/repo.git",
					Branch: "main",
					Depth:  1,
				},
			},
			wantIn: []string{
				"--workdir /workspace",
			},
			wantOut: []string{
				":/workspace",
			},
		},
		{
			name: "telemetry enabled injects harness telemetry env",
			config: RunConfig{
				Harness:          &harness.GeminiCLI{},
				Name:             "test-agent",
				UnixUsername:     "scion",
				Image:            "scion-agent:latest",
				TelemetryEnabled: true,
			},
			wantIn: []string{
				"-e GEMINI_TELEMETRY_ENABLED=true",
				"-e GEMINI_TELEMETRY_TARGET=local",
				"-e GEMINI_TELEMETRY_USE_COLLECTOR=true",
				"-e GEMINI_TELEMETRY_OTLP_ENDPOINT=http://localhost:4317",
				"-e GEMINI_TELEMETRY_OTLP_PROTOCOL=grpc",
				"-e GEMINI_TELEMETRY_LOG_PROMPTS=false",
			},
		},
		{
			name: "telemetry disabled omits harness telemetry env",
			config: RunConfig{
				Harness:          &harness.GeminiCLI{},
				Name:             "test-agent",
				UnixUsername:     "scion",
				Image:            "scion-agent:latest",
				TelemetryEnabled: false,
			},
			wantOut: []string{
				"GEMINI_TELEMETRY_ENABLED",
				"GEMINI_TELEMETRY_TARGET",
				"GEMINI_TELEMETRY_OTLP_ENDPOINT",
			},
		},
		{
			name: "git clone mode with home dir still mounts home",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				HomeDir:      tmpHome,
				GitClone: &api.GitCloneConfig{
					URL:    "https://github.com/example/repo.git",
					Branch: "dev",
				},
			},
			wantIn: []string{
				"--workdir /workspace",
				fmt.Sprintf("-v %s:/home/scion", tmpHome),
			},
			wantOut: []string{
				":/workspace:",
			},
		},
		{
			name: "gcs volume triggers fuse mount path",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Task:         "hello",
				Volumes: []api.VolumeMount{
					{Type: "gcs", Bucket: "my-bucket", Target: "/data"},
				},
			},
			wantIn: []string{
				"--cap-add SYS_ADMIN",
				"--device /dev/fuse",
				"-e SCION_START_CMD=",
				`exec sh -c "$SCION_START_CMD"`,
				"gcsfuse",
			},
		},
		{
			name: "gcs volume with prefix and mode",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Task:         "do stuff",
				Volumes: []api.VolumeMount{
					{Type: "gcs", Bucket: "b", Prefix: "subdir", Mode: "ro", Target: "/mnt"},
				},
			},
			wantIn: []string{
				"--only-dir",
				"-o",
				"--implicit-dirs",
				"-e SCION_START_CMD=",
			},
		},
	}

	for _, tt := range tests {

		t.Run(tt.name, func(t *testing.T) {

			args, err := buildCommonRunArgs(tt.config)

			if err != nil {

				t.Fatalf("buildCommonRunArgs failed: %v", err)

			}

			argStr := strings.Join(args, " ")

			for _, want := range tt.wantIn {

				if !strings.Contains(argStr, want) {

					t.Errorf("expected arg %q not found in %v", want, args)

				}

			}

			for _, notWant := range tt.wantOut {

				if strings.Contains(argStr, notWant) {

					t.Errorf("unexpected arg %q found in %v", notWant, args)

				}

			}

		})

	}

}

func TestRunSimpleCommand(t *testing.T) {

	out, err := runSimpleCommand(context.Background(), "echo", "hello")

	if err != nil {

		t.Fatalf("runSimpleCommand failed: %v", err)

	}

	if out != "hello" {

		t.Errorf("expected \"hello\", got %q", out)

	}

	_, err = runSimpleCommand(context.Background(), "false")

	if err == nil {

		t.Error("expected error from running 'false', got nil")

	}

}

func TestVolumeDeduplication(t *testing.T) {

	// Setup

	config := RunConfig{

		Harness: &harness.GeminiCLI{},

		Name: "test-agent",

		UnixUsername: "scion",

		Image: "scion-agent:latest",

		// Simulate duplicate volumes

		Volumes: []api.VolumeMount{

			{Source: "/host/path1", Target: "/container/target", ReadOnly: true},

			{Source: "/host/path2", Target: "/container/target", ReadOnly: false}, // Should override

			{Source: "/host/path3", Target: "/container/other", ReadOnly: false},
		},
	}

	args, err := buildCommonRunArgs(config)

	if err != nil {

		t.Fatalf("buildCommonRunArgs failed: %v", err)

	}

	argStr := strings.Join(args, " ")

	// Check that /container/target appears only once (ideally)

	count := strings.Count(argStr, ":/container/target")

	if count != 1 {

		t.Errorf("expected 1 mount for /container/target, got %d. Args: %v", count, args)

	}

	// Check that the last one won

	if !strings.Contains(argStr, "/host/path2:/container/target") {

		t.Errorf("expected /host/path2:/container/target to be present, got: %s", argStr)

	}

	if strings.Contains(argStr, "/host/path1:/container/target") {

		t.Errorf("expected /host/path1:/container/target to be ABSENT, got: %s", argStr)

	}

}

func TestDevBinariesMount(t *testing.T) {
	// When SCION_DEV_BINARIES is set to a valid directory, it should
	// be bind-mounted to /opt/scion/bin in the container.
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "sciontool"), []byte("fake"), 0755)

	t.Setenv("SCION_DEV_BINARIES", tmpDir)

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	argStr := strings.Join(args, " ")
	expected := fmt.Sprintf("-v %s:/opt/scion/bin:ro", tmpDir)
	if !strings.Contains(argStr, expected) {
		t.Errorf("expected dev binaries mount %q in args, got: %s", expected, argStr)
	}
}

func TestDevBinariesMountNotSetOrInvalid(t *testing.T) {
	// When SCION_DEV_BINARIES is not set, no mount should appear.
	t.Setenv("SCION_DEV_BINARIES", "")

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	argStr := strings.Join(args, " ")
	if strings.Contains(argStr, "/opt/scion/bin") {
		t.Errorf("expected no dev binaries mount when env is empty, got: %s", argStr)
	}

	// When set to a non-existent path, no mount should appear.
	t.Setenv("SCION_DEV_BINARIES", "/nonexistent/path")

	args, err = buildCommonRunArgs(RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	argStr = strings.Join(args, " ")
	if strings.Contains(argStr, "/opt/scion/bin") {
		t.Errorf("expected no dev binaries mount for missing path, got: %s", argStr)
	}

	// When set to a file (not a directory), no mount should appear.
	tmpFile := filepath.Join(t.TempDir(), "not-a-dir")
	os.WriteFile(tmpFile, []byte("x"), 0644)
	t.Setenv("SCION_DEV_BINARIES", tmpFile)

	args, err = buildCommonRunArgs(RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	argStr = strings.Join(args, " ")
	if strings.Contains(argStr, "/opt/scion/bin") {
		t.Errorf("expected no dev binaries mount for file path, got: %s", argStr)
	}
}

func TestGcloudMountPreCreatesDirectory(t *testing.T) {
	// The gcloud auto-mount in buildCommonRunArgs should pre-create the
	// mount-point directory inside the agent home so Docker does not create
	// it as root (which makes the agent dir undeletable by non-root users).
	home, _ := os.UserHomeDir()
	gcloudDir := filepath.Join(home, ".config", "gcloud")
	if _, err := os.Stat(gcloudDir); err != nil {
		t.Skip("host does not have ~/.config/gcloud; skipping")
	}

	agentHome := t.TempDir()

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		HomeDir:      agentHome,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	mountPoint := filepath.Join(agentHome, ".config", "gcloud")
	info, err := os.Stat(mountPoint)
	if err != nil {
		t.Fatalf("expected %s to exist after buildCommonRunArgs, got: %v", mountPoint, err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", mountPoint)
	}

	// Verify the gcloud mount is present in the args
	argStr := strings.Join(args, " ")
	if !strings.Contains(argStr, ".config/gcloud") {
		t.Errorf("expected gcloud mount in args, got: %s", argStr)
	}
}

func TestWriteRuntimeDebugFile(t *testing.T) {
	t.Run("writes file when debug is true", func(t *testing.T) {
		agentDir := t.TempDir()
		homeDir := filepath.Join(agentDir, "home")
		os.MkdirAll(homeDir, 0755)

		config := RunConfig{
			Debug:   true,
			HomeDir: homeDir,
		}

		WriteRuntimeDebugFile(config, "docker", []string{
			"run", "-t", "--name", "my-agent",
			"-e", "FOO=bar",
			"-v", "/host:/container",
			"my-image:latest",
			"tmux", "new-session", "-s", "scion",
		})

		debugPath := filepath.Join(agentDir, "runtime-exec-debug")
		content, err := os.ReadFile(debugPath)
		if err != nil {
			t.Fatalf("expected debug file to exist: %v", err)
		}

		text := string(content)

		// Should start with the command
		if !strings.HasPrefix(text, "docker") {
			t.Errorf("expected file to start with 'docker', got: %s", text)
		}

		// Should have continuation characters
		if !strings.Contains(text, " \\\n  ") {
			t.Errorf("expected backslash continuation characters, got: %s", text)
		}

		// Should contain each arg on its own line
		lines := strings.Split(text, "\n")
		// First line is "docker \", remaining are "  arg \" (last arg has no \)
		if len(lines) < 10 {
			t.Errorf("expected at least 10 lines (one per arg), got %d: %s", len(lines), text)
		}

		// Should contain specific args
		if !strings.Contains(text, "--name") {
			t.Errorf("expected --name in debug file, got: %s", text)
		}
		if !strings.Contains(text, "my-image:latest") {
			t.Errorf("expected image in debug file, got: %s", text)
		}
	})

	t.Run("no-op when debug is false", func(t *testing.T) {
		agentDir := t.TempDir()
		homeDir := filepath.Join(agentDir, "home")
		os.MkdirAll(homeDir, 0755)

		config := RunConfig{
			Debug:   false,
			HomeDir: homeDir,
		}

		WriteRuntimeDebugFile(config, "docker", []string{"run", "test"})

		debugPath := filepath.Join(agentDir, "runtime-exec-debug")
		if _, err := os.Stat(debugPath); err == nil {
			t.Error("expected no debug file when debug is false")
		}
	})

	t.Run("no-op when HomeDir is empty", func(t *testing.T) {
		config := RunConfig{
			Debug:   true,
			HomeDir: "",
		}

		// Should not panic
		WriteRuntimeDebugFile(config, "docker", []string{"run", "test"})
	})
}

func TestScionDirShadowedWhenFullRepoMounted(t *testing.T) {
	// When the full repo root is mounted (workspace outside repo root),
	// a tmpfs shadow mount should be added over /repo-root/.scion to
	// prevent agents from accessing other agents' secrets.
	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RepoRoot:     "/home/user/repo",
		Workspace:    "/some/external/workspace", // outside repo root
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	argStr := strings.Join(args, " ")

	// Should have the full repo root mount
	if !strings.Contains(argStr, "-v /home/user/repo:/repo-root") {
		t.Errorf("expected full repo root mount, got: %s", argStr)
	}

	// Should have the tmpfs shadow over .scion
	if !strings.Contains(argStr, "--mount type=tmpfs,destination=/repo-root/.scion") {
		t.Errorf("expected tmpfs shadow mount over .scion, got: %s", argStr)
	}
}

func TestScionDirNotShadowedWhenWorkspaceInsideRepo(t *testing.T) {
	// When the workspace is inside the repo root, .git and workspace are
	// mounted separately (no full repo mount), so no tmpfs shadow is needed.
	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RepoRoot:     "/home/user/repo",
		Workspace:    "/home/user/repo/.scion/agents/test/workspace",
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	argStr := strings.Join(args, " ")

	// Should NOT have the full repo root mount
	if strings.Contains(argStr, "-v /home/user/repo:/repo-root ") {
		t.Errorf("expected no full repo root mount, got: %s", argStr)
	}

	// Should NOT have the tmpfs shadow
	if strings.Contains(argStr, "tmpfs") {
		t.Errorf("expected no tmpfs shadow mount, got: %s", argStr)
	}
}

func TestGcloudMountSkippedInBrokerMode(t *testing.T) {
	// In broker mode, the gcloud auto-mount should be skipped to avoid
	// leaking the broker operator's GCP credentials into agent containers.
	home, _ := os.UserHomeDir()
	gcloudDir := filepath.Join(home, ".config", "gcloud")
	if _, err := os.Stat(gcloudDir); err != nil {
		t.Skip("host does not have ~/.config/gcloud; skipping")
	}

	agentHome := t.TempDir()

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		HomeDir:      agentHome,
		BrokerMode:   true,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	// The mount-point directory should NOT be pre-created in broker mode
	mountPoint := filepath.Join(agentHome, ".config", "gcloud")
	if _, err := os.Stat(mountPoint); err == nil {
		t.Errorf("expected %s to NOT exist in broker mode, but it does", mountPoint)
	}

	// Verify the gcloud mount is absent from the args
	argStr := strings.Join(args, " ")
	if strings.Contains(argStr, ".config/gcloud") {
		t.Errorf("expected no gcloud mount in broker mode args, got: %s", argStr)
	}
}

func TestResolveContainerWorkspace(t *testing.T) {
	tests := []struct {
		name      string
		repoRoot  string
		workspace string
		gitClone  *api.GitCloneConfig
		want      string
	}{
		{
			name: "empty workspace",
			want: "/workspace",
		},
		{
			name:      "git clone mode",
			workspace: "/some/path",
			gitClone:  &api.GitCloneConfig{URL: "https://example.com/repo.git"},
			want:      "/workspace",
		},
		{
			name:      "workspace under repo root (git worktree)",
			repoRoot:  "/home/user/myproject",
			workspace: "/home/user/myproject/.scion/agents/my-agent/workspace",
			want:      "/repo-root/.scion/agents/my-agent/workspace",
		},
		{
			name:      "workspace outside repo root",
			repoRoot:  "/home/user/myproject",
			workspace: "/tmp/worktrees/my-agent",
			want:      "/workspace",
		},
		{
			name:      "workspace without repo root",
			workspace: "/some/workspace",
			want:      "/workspace",
		},
		{
			name:      "workspace equals repo root (shared workspace)",
			repoRoot:  "/home/user/myproject",
			workspace: "/home/user/myproject",
			want:      "/workspace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveContainerWorkspace(tt.repoRoot, tt.workspace, tt.gitClone)
			if got != tt.want {
				t.Errorf("ResolveContainerWorkspace(%q, %q, %v) = %q, want %q",
					tt.repoRoot, tt.workspace, tt.gitClone, got, tt.want)
			}
		})
	}
}

func TestBridgeExtraHosts(t *testing.T) {
	tests := []struct {
		name        string
		runtimeName string
		env         []string
		want        []string
	}{
		{
			name:        "docker with host.docker.internal in env",
			runtimeName: "docker",
			env:         []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"},
			want:        []string{"host.docker.internal:host-gateway"},
		},
		{
			name:        "docker without bridge hostname",
			runtimeName: "docker",
			env:         []string{"SCION_HUB_ENDPOINT=http://example.com:8080"},
			want:        nil,
		},
		{
			name:        "docker with empty env",
			runtimeName: "docker",
			env:         nil,
			want:        nil,
		},
		{
			name:        "podman with host.containers.internal",
			runtimeName: "podman",
			env:         []string{"SCION_HUB_ENDPOINT=http://host.containers.internal:8080"},
			want:        nil,
		},
		{
			name:        "kubernetes runtime",
			runtimeName: "kubernetes",
			env:         []string{"SCION_HUB_ENDPOINT=http://host.docker.internal:8080"},
			want:        nil,
		},
		{
			name:        "docker with bridge hostname in SCION_HUB_URL",
			runtimeName: "docker",
			env:         []string{"FOO=bar", "SCION_HUB_URL=http://host.docker.internal:9090"},
			want:        []string{"host.docker.internal:host-gateway"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BridgeExtraHosts(tt.runtimeName, tt.env)
			if len(got) != len(tt.want) {
				t.Fatalf("BridgeExtraHosts(%q, %v) = %v, want %v", tt.runtimeName, tt.env, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("BridgeExtraHosts(%q, %v)[%d] = %q, want %q", tt.runtimeName, tt.env, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestResolveDockerNetworking(t *testing.T) {
	tests := []struct {
		name        string
		runtimeName string
		env         map[string]string
		wantMode    string
		wantEP      string // expected SCION_HUB_ENDPOINT after call (empty = unchanged/absent)
	}{
		{
			name:        "docker with bridge hostname rewrites to localhost",
			runtimeName: "docker",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "http://host.docker.internal:8080",
				"SCION_HUB_URL":      "http://host.docker.internal:8080",
			},
			wantMode: "host",
			wantEP:   "http://localhost:8080",
		},
		{
			name:        "docker with localhost endpoint",
			runtimeName: "docker",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "http://localhost:8080",
			},
			wantMode: "host",
			wantEP:   "http://localhost:8080",
		},
		{
			name:        "docker with 127.0.0.1 endpoint",
			runtimeName: "docker",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "http://127.0.0.1:9090",
			},
			wantMode: "host",
			wantEP:   "http://127.0.0.1:9090",
		},
		{
			name:        "docker with external endpoint",
			runtimeName: "docker",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "https://hub.example.com:443",
			},
			wantMode: "",
			wantEP:   "https://hub.example.com:443",
		},
		{
			name:        "docker with no hub endpoint",
			runtimeName: "docker",
			env:         map[string]string{},
			wantMode:    "",
		},
		{
			name:        "podman is not affected",
			runtimeName: "podman",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "http://localhost:8080",
			},
			wantMode: "",
			wantEP:   "http://localhost:8080",
		},
		{
			name:        "kubernetes is not affected",
			runtimeName: "kubernetes",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "http://localhost:8080",
			},
			wantMode: "",
			wantEP:   "http://localhost:8080",
		},
		{
			name:        "docker with bridge hostname in HUB_URL only",
			runtimeName: "docker",
			env: map[string]string{
				"SCION_HUB_URL": "http://host.docker.internal:9090",
			},
			wantMode: "host",
			wantEP:   "", // SCION_HUB_ENDPOINT not set
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Copy env to avoid mutation across subtests
			env := make(map[string]string)
			for k, v := range tt.env {
				env[k] = v
			}

			got := ResolveDockerNetworking(tt.runtimeName, env)
			if got != tt.wantMode {
				t.Errorf("ResolveDockerNetworking(%q) = %q, want %q", tt.runtimeName, got, tt.wantMode)
			}
			if tt.wantEP != "" {
				if ep := env["SCION_HUB_ENDPOINT"]; ep != tt.wantEP {
					t.Errorf("SCION_HUB_ENDPOINT = %q, want %q", ep, tt.wantEP)
				}
			}
			// Verify HUB_URL is also rewritten when bridge hostname was present
			if tt.runtimeName == "docker" && tt.wantMode == "host" {
				if hubURL, ok := env["SCION_HUB_URL"]; ok && strings.Contains(hubURL, "host.docker.internal") {
					t.Errorf("SCION_HUB_URL still contains bridge hostname: %q", hubURL)
				}
			}
		})
	}
}

func TestBuildCommonRunArgs_NetworkMode(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		NetworkMode:  "host",
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	found := false
	for i, arg := range args {
		if arg == "--network" && i+1 < len(args) && args[i+1] == "host" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --network host in args, got: %v", args)
	}
}

func TestBuildCommonRunArgs_ExtraHosts(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		ExtraHosts:   []string{"host.docker.internal:host-gateway"},
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	found := false
	for i, arg := range args {
		if arg == "--add-host" && i+1 < len(args) && args[i+1] == "host.docker.internal:host-gateway" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected --add-host host.docker.internal:host-gateway in args, got: %v", args)
	}
}

func TestWriteFileSecrets_DeduplicatesByTarget(t *testing.T) {
	homeDir := t.TempDir()
	containerHome := "/home/scion"

	secrets := []api.ResolvedSecret{
		{Name: "user-cert", Type: "file", Target: "/tmp/my-secret.json", Value: "user-data", Source: "user"},
		{Name: "grove-cert", Type: "file", Target: "/tmp/my-secret.json", Value: "grove-data", Source: "grove"},
		{Name: "other-file", Type: "file", Target: "/tmp/other.json", Value: "other-data", Source: "user"},
		{Name: "env-secret", Type: "environment", Target: "FOO", Value: "bar", Source: "user"},
	}

	mounts, err := writeFileSecrets(homeDir, containerHome, secrets)
	if err != nil {
		t.Fatalf("writeFileSecrets failed: %v", err)
	}

	// Should produce exactly 2 mounts: one for /tmp/my-secret.json (last wins) and one for /tmp/other.json
	if len(mounts) != 2 {
		t.Fatalf("expected 2 mount specs, got %d: %v", len(mounts), mounts)
	}

	// The /tmp/my-secret.json mount should use the grove-cert (last entry wins)
	var mySecretMount string
	for _, m := range mounts {
		if strings.Contains(m, "/tmp/my-secret.json") {
			mySecretMount = m
		}
	}
	if mySecretMount == "" {
		t.Fatal("expected mount for /tmp/my-secret.json")
	}
	if !strings.Contains(mySecretMount, "grove-cert") {
		t.Errorf("expected grove-cert to win for duplicate target, got: %s", mySecretMount)
	}
}

// TestSharedWorkspace_NoAgentStateInMounts asserts the structural invariant
// from .design/hub-shared-workspace-isolation.md: when an agent is launched
// in a shared-workspace grove (workspace == repo root), the assembled run
// args must not bind-mount any host path under <grove>/.scion/agents/ into
// the container. Per-agent state lives at the external project-configs path
// instead, so siblings cannot read it via /workspace.
func TestSharedWorkspace_NoAgentStateInMounts(t *testing.T) {
	tmpDir := t.TempDir()
	groveDir := filepath.Join(tmpDir, "grove")
	if err := os.MkdirAll(filepath.Join(groveDir, ".scion", "agents", "agent-a"), 0755); err != nil {
		t.Fatalf("mkdir in-grove agent dir: %v", err)
	}
	// External per-agent state for the agent under test (where prompt.md and
	// scion-agent.json are relocated to in shared-workspace mode).
	extAgentDir := filepath.Join(tmpDir, "external", "agents", "agent-a")
	homeDir := filepath.Join(extAgentDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatalf("mkdir homeDir: %v", err)
	}

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "agent-a",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		// Shared-workspace shape: workspace == repo root. buildCommonRunArgs
		// hits the relWorkspace == "." branch and mounts grove → /workspace.
		RepoRoot:  groveDir,
		Workspace: groveDir,
		HomeDir:   homeDir,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	forbidden := filepath.Join(groveDir, ".scion", "agents")
	for i, a := range args {
		if strings.Contains(a, forbidden) {
			t.Errorf("arg[%d] = %q references forbidden host path %s; per-agent state must live external (.design/hub-shared-workspace-isolation.md)", i, a, forbidden)
		}
	}
	// Sanity: the grove itself should still be mounted at /workspace.
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, fmt.Sprintf("%s:/workspace", groveDir)) {
		t.Errorf("expected grove %s to be mounted at /workspace, args: %s", groveDir, joined)
	}
}

func TestBuildCommonRunArgs_FuseMountArgOrdering(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.ClaudeCode{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello world",
		Volumes: []api.VolumeMount{
			{Type: "gcs", Bucket: "my-bucket", Target: "/data"},
		},
	}
	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	imageIdx := -1
	envIdx := -1
	for i, a := range args {
		if a == config.Image {
			imageIdx = i
		}
		if a == "-e" && i+1 < len(args) && strings.HasPrefix(args[i+1], "SCION_START_CMD=") {
			envIdx = i
		}
	}
	if envIdx == -1 {
		t.Fatalf("SCION_START_CMD env var not found in args: %v", args)
	}
	if imageIdx == -1 {
		t.Fatalf("image %q not found in args: %v", config.Image, args)
	}
	if envIdx >= imageIdx {
		t.Errorf("-e SCION_START_CMD at index %d must come before image %q at index %d; args: %v",
			envIdx, config.Image, imageIdx, args)
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "hello", "'hello'"},
		{"empty", "", "''"},
		{"spaces", "hello world", "'hello world'"},
		{"backticks", "use `command` here", "'use `command` here'"},
		{"dollar sign", "value is $HOME", "'value is $HOME'"},
		{"command substitution", "$(rm -rf /)", "'$(rm -rf /)'"},
		{"double quotes", `say "hello"`, `'say "hello"'`},
		{"single quotes", "it's", "'it'\\''s'"},
		{"mixed metacharacters", "run `cmd` with $VAR and 'quotes'", "'run `cmd` with $VAR and '\\''quotes'\\'''"},
		{"newlines", "line1\nline2", "'line1\nline2'"},
		{"backslashes", `path\to\file`, `'path\to\file'`},
		{"semicolons", "cmd1; cmd2", "'cmd1; cmd2'"},
		{"pipes", "cmd1 | cmd2", "'cmd1 | cmd2'"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildCommonRunArgs_ShellMetacharsInPrompt(t *testing.T) {
	prompts := []struct {
		name string
		task string
	}{
		{"backticks", "Fix the bug in `main.go` using ```go\nfmt.Println()\n```"},
		{"dollar signs", "Set $HOME and $(whoami) correctly"},
		{"single quotes", "Don't use 'unsafe' code"},
		{"mixed", "Run `cmd` with $VAR and 'quotes' in $(subshell)"},
	}

	for _, tt := range prompts {
		t.Run(tt.name, func(t *testing.T) {
			config := RunConfig{
				Harness:      &harness.ClaudeCode{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Task:         tt.task,
			}
			args, err := buildCommonRunArgs(config)
			if err != nil {
				t.Fatalf("buildCommonRunArgs failed: %v", err)
			}

			// The last arg is the tmux command passed to "sh -c".
			// Verify the prompt is single-quoted (not double-quoted).
			shCmd := args[len(args)-1]
			quoted := shellQuote(tt.task)
			if !strings.Contains(shCmd, quoted) {
				t.Errorf("expected single-quoted prompt %q in sh -c arg, got: %s", quoted, shCmd)
			}
		})
	}
}
