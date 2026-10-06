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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

func TestResolveContainerID(t *testing.T) {
	agents := []api.AgentInfo{
		{
			ContainerID: "abc123def456789",
			Name:        "my-agent",
		},
		{
			ContainerID: "def456abc789012",
			Name:        "myproject--other-agent",
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
			name: "project-prefixed container name",
			id:   "myproject--other-agent",
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
	// Simulates the actual bug: container is named "project--foo" but the
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

// homeDirForTest pins $HOME to a deterministic, test-isolated directory and
// returns it, for test cases that need a value ValidateWorkspaceSource's
// floors refuse regardless of the host running the test.
func homeDirForTest(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestSyncGCSVolumesValidation(t *testing.T) {
	encode := func(t *testing.T, volumes []gcsVolumeInfo) string {
		t.Helper()
		data, err := json.Marshal(volumes)
		if err != nil {
			t.Fatal(err)
		}
		return base64.StdEncoding.EncodeToString(data)
	}

	tests := []struct {
		name      string
		encoded   string
		direction SyncDirection
		wantError string
	}{
		{
			name:      "invalid base64",
			encoded:   "%%%",
			direction: SyncTo,
			wantError: "failed to decode gcs volume info",
		},
		{
			name:      "invalid json",
			encoded:   base64.StdEncoding.EncodeToString([]byte("not json")),
			direction: SyncTo,
			wantError: "failed to parse gcs volume info",
		},
		{
			name:      "direction required for sourced volume",
			encoded:   encode(t, []gcsVolumeInfo{{Source: "/workspace", Bucket: "bucket"}}),
			direction: SyncUnspecified,
			wantError: "sync direction must be specified for GCS volumes",
		},
		{
			name:      "empty source skipped",
			encoded:   encode(t, []gcsVolumeInfo{{Bucket: "bucket"}}),
			direction: SyncUnspecified,
		},
		{
			name:      "empty list",
			encoded:   encode(t, nil),
			direction: SyncUnspecified,
		},
		{
			name:      "root source rejected",
			encoded:   encode(t, []gcsVolumeInfo{{Source: "/", Bucket: "bucket"}}),
			direction: SyncTo,
			wantError: "invalid GCS volume source",
		},
		{
			name:      "home directory source rejected",
			encoded:   encode(t, []gcsVolumeInfo{{Source: homeDirForTest(t), Bucket: "bucket"}}),
			direction: SyncTo,
			wantError: "invalid GCS volume source",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := syncGCSVolumes(context.Background(), test.encoded, test.direction)
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("syncGCSVolumes() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("syncGCSVolumes() error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestAppendContainerResourceArgs(t *testing.T) {
	tests := []struct {
		name      string
		resources *api.ResourceSpec
		want      []string
		wantError string
	}{
		{
			name: "nil resources",
			want: []string{"run", "-t"},
		},
		{
			name: "all supported resources",
			resources: &api.ResourceSpec{
				Limits:   api.ResourceList{Memory: "1Gi", CPU: "1500m"},
				Requests: api.ResourceList{Memory: "512Mi", CPU: "250m"},
			},
			want: []string{"run", "-t", "--memory", "1g", "--memory-reservation", "512m", "--cpus", "1.5"},
		},
		{
			// The built-in default spec (limits.cpu only) must produce only
			// --cpus: the Kubernetes request defaults never reach Docker/Podman.
			name:      "builtin default cpu limit only",
			resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}},
			want:      []string{"run", "-t", "--cpus", "2"},
		},
		{
			name:      "disk only is not mapped",
			resources: &api.ResourceSpec{Disk: "20Gi"},
			want:      []string{"run", "-t"},
		},
		{
			name:      "invalid memory limit",
			resources: &api.ResourceSpec{Limits: api.ResourceList{Memory: "bad"}},
			wantError: `invalid memory limit "bad"`,
		},
		{
			name:      "invalid memory request",
			resources: &api.ResourceSpec{Requests: api.ResourceList{Memory: "bad"}},
			wantError: `invalid memory request "bad"`,
		},
		{
			name:      "invalid cpu limit",
			resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "bad"}},
			wantError: `invalid cpu limit "bad"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := appendContainerResourceArgs([]string{"run", "-t"}, test.resources)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("appendContainerResourceArgs() error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("appendContainerResourceArgs() error = %v", err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("appendContainerResourceArgs() = %v, want %v", got, test.want)
			}
		})
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
	_ = os.WriteFile(oauthFile, []byte("{}"), 0644)
	adcFile := filepath.Join(tmpDir, "adc.json")
	_ = os.WriteFile(adcFile, []byte("{}"), 0644)

	// A real, existing repo root for the RepoRoot-resolution cases below:
	// buildCommonRunArgs resolves RepoRoot through filepath.EvalSymlinks, so
	// the path must actually exist.
	repoRoot := t.TempDir()
	repoWorktreeWorkspace := filepath.Join(repoRoot, ".scion", "agents", "test-agent", "workspace")
	if err := os.MkdirAll(repoWorktreeWorkspace, 0755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		config  RunConfig
		wantIn  []string
		wantOut []string
	}{
		{
			name: "basic config",
			config: RunConfig{
				Harness:      &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
				Harness: &harness.Generic{},
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
				Harness: &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
				Harness: &harness.Generic{},
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
				Harness: &harness.Generic{},
				Name:    "test-agent",
				Image:   "scion-agent:latest",
				Env:     []string{"FOO=BAR"},
				Task:    "hello",
				Resume:  true,
			},
			wantIn: []string{
				"-e FOO=BAR",
				"tmux new-session -d -s scion -n agent sh -c ",
				`'\''hello'\''`,
				"; echo $? > /tmp/scion-harness-exit-code",
			},
		},
		{
			name: "resume with tmux",
			config: RunConfig{
				Harness: &harness.Generic{},
				Name:    "test-agent",
				Image:   "scion-agent:latest",
				Task:    "hello",
				Resume:  true,
			},
			wantIn: []string{
				"tmux new-session -d -s scion -n agent sh -c ",
				`'\''hello'\''`,
				"; echo $? > /tmp/scion-harness-exit-code",
			},
		},
		{
			name: "template label",
			config: RunConfig{
				Harness:  &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				RepoRoot:     repoRoot,
				Workspace:    repoWorktreeWorkspace,
				Image:        "scion-agent:latest",
			},
			wantIn: []string{
				"-v " + filepath.Join(repoRoot, ".git") + ":/repo-root/.git",
				"-v " + repoWorktreeWorkspace + ":/repo-root/.scion/agents/test-agent/workspace",
				"--workdir /repo-root/.scion/agents/test-agent/workspace",
			},
		},
		{
			name: "shared workspace (workspace equals repo root)",
			config: RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				RepoRoot:     repoRoot,
				Workspace:    repoRoot,
				Image:        "scion-agent:latest",
			},
			wantIn: []string{
				"-v " + repoRoot + ":/workspace",
				"--workdir /workspace",
			},
			wantOut: []string{
				"/repo-root",
			},
		},
		{
			name: "generic volumes",
			config: RunConfig{
				Harness: &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Task:         "",
			},
			wantIn:  []string{"tmux new-session -d -s scion -n agent"},
			wantOut: []string{"--prompt-interactive"},
		},
		{
			name: "workspace from volumes",
			config: RunConfig{
				Harness:      &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
				Harness: &harness.Generic{},
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
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				GitClone: &api.GitCloneConfig{
					URL:    "https://github.com/example/repo.git",
					Branch: "main",
					Depth:  intPtr(1),
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
			name: "telemetry enabled with generic harness has no telemetry env",
			config: RunConfig{
				Harness:          &harness.Generic{},
				Name:             "test-agent",
				UnixUsername:     "scion",
				Image:            "scion-agent:latest",
				TelemetryEnabled: true,
			},
			wantOut: []string{
				"GEMINI_TELEMETRY_ENABLED",
				"GEMINI_TELEMETRY_TARGET",
				"GEMINI_TELEMETRY_OTLP_ENDPOINT",
			},
		},
		{
			name: "telemetry disabled omits harness telemetry env",
			config: RunConfig{
				Harness:          &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
		{
			name: "project identity",
			config: RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Task:         "hello",
				Project:      "p",
				ProjectID:    "pid",
			},
			wantIn: []string{
				"-e SCION_PROJECT=p",
				"-e SCION_PROJECT_ID=pid",
			},
			wantOut: []string{
				"SCION_GROVE=",
				"SCION_GROVE_ID=",
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

// TestBuildCommonRunArgs_HubNativeWorktreeWorkspace covers the hub-native
// worktree-per-agent shared-base layout end to end through buildCommonRunArgs:
// RepoRoot is the project's own ~/.scion/projects/<slug> directory (the
// shared git checkout itself, not a "workspace" subdirectory of it), and
// Workspace is a direct child of its "worktrees" subdirectory. Both must be
// accepted by the workspace-source validation this function runs before any
// mount is built, restoring the shape's pre-existing support.
func TestBuildCommonRunArgs_HubNativeWorktreeWorkspace(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	repoRoot := filepath.Join(tmpHome, ".scion", "projects", "my-project")
	workspace := filepath.Join(repoRoot, "worktrees", "agent-1")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	_, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RepoRoot:     repoRoot,
		Workspace:    workspace,
	})
	if err != nil {
		t.Fatalf("expected the hub-native worktree-per-agent workspace to be accepted, got: %v", err)
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

		Harness: &harness.Generic{},

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
	_ = os.WriteFile(filepath.Join(tmpDir, "sciontool"), []byte("fake"), 0755)

	t.Setenv("SCION_DEV_BINARIES", tmpDir)

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
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
		Harness:      &harness.Generic{},
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
		Harness:      &harness.Generic{},
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
	_ = os.WriteFile(tmpFile, []byte("x"), 0644)
	t.Setenv("SCION_DEV_BINARIES", tmpFile)

	args, err = buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
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
		Harness:      &harness.Generic{},
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
	t.Run("writes redacted file when debug is true", func(t *testing.T) {
		agentDir := t.TempDir()
		homeDir := filepath.Join(agentDir, "home")
		_ = os.MkdirAll(homeDir, 0755)

		config := RunConfig{
			Debug:   true,
			HomeDir: homeDir,
		}

		args := []string{
			"run", "-t", "--name", "my-agent",
			"--env", "FOO=bar",
			"-v", "/host:/container",
			"my-image:latest",
			"tmux", "new-session", "-s", "scion",
		}
		WriteRuntimeDebugFile(config, "docker", args)

		debugPath := filepath.Join(agentDir, "runtime-exec-debug")
		content, err := os.ReadFile(debugPath)
		if err != nil {
			t.Fatalf("expected debug file to exist: %v", err)
		}

		text := string(content)

		// Should start with the command name.
		if !strings.HasPrefix(text, "docker") {
			t.Errorf("expected file to start with 'docker', got: %s", text)
		}

		// Should contain the arg count.
		if !strings.Contains(text, fmt.Sprintf("%d args", len(args))) {
			t.Errorf("expected file to mention arg count %d, got: %s", len(args), text)
		}

		// Must NOT contain actual argument values — they may carry secrets.
		if strings.Contains(text, "--name") {
			t.Errorf("file contains arg values that should be redacted: %s", text)
		}
		if strings.Contains(text, "my-image:latest") {
			t.Errorf("file contains arg values that should be redacted: %s", text)
		}
		if strings.Contains(text, "FOO=bar") {
			t.Errorf("file contains env value that should be redacted: %s", text)
		}
	})

	t.Run("no-op when debug is false", func(t *testing.T) {
		agentDir := t.TempDir()
		homeDir := filepath.Join(agentDir, "home")
		_ = os.MkdirAll(homeDir, 0755)

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

// TestRunSimpleCommand_NoSecretsInDebugLog is the mutation test for Site 1
// of #127/P5. It verifies that secret values carried in command arguments do
// NOT appear in debug log output.
//
// Mutation proof: reverting the fix in runSimpleCommand (logging the full
// cmdStr instead of command+argc) causes this test to fail, because the
// sentinel value then appears in the captured log output.
func TestRunSimpleCommand_NoSecretsInDebugLog(t *testing.T) {
	const sentinel = "FAKE-KEY-SENTINEL-not-a-real-credential"

	// runtimeLog captured slog.Default() at package init, before any custom
	// handler was installed (#1339). Its handler bridges to the stdlib log
	// package. We capture that output here and lower the level gate so Debug
	// lines reach the buffer.
	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0) // drop date/time prefix for simpler assertions
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})
	origLevel := slog.SetLogLoggerLevel(slog.LevelDebug)
	t.Cleanup(func() { slog.SetLogLoggerLevel(origLevel) })

	// --- Success path ---
	// "echo" succeeds; its args include the sentinel via --env.
	buf.Reset()
	_, _ = runSimpleCommand(context.Background(), "echo", "--env", "SECRET_KEY="+sentinel)

	logOutput := buf.String()
	if strings.Contains(logOutput, sentinel) {
		t.Errorf("SECURITY: debug log (success path) contains secret sentinel %q\nLog: %s",
			sentinel, logOutput)
	}
	// The command name must still be logged — we redacted args, not everything.
	if !strings.Contains(logOutput, "echo") {
		t.Errorf("debug log should contain the command name 'echo'\nLog: %s", logOutput)
	}

	// --- Failure path ---
	// Use "false" so the command fails with no output (the sentinel can only
	// appear if it leaks from the argument list, not from command stdout).
	buf.Reset()
	_, err := runSimpleCommand(context.Background(), "false", "--env", "SECRET_KEY="+sentinel)
	if err == nil {
		t.Fatal("expected error from 'false', got nil")
	}

	logOutput = buf.String()
	if strings.Contains(logOutput, sentinel) {
		t.Errorf("SECURITY: debug log (failure path) contains secret sentinel %q\nLog: %s",
			sentinel, logOutput)
	}

	// The error message returned to callers must also not contain the sentinel,
	// because callers may log it at non-debug levels.
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("SECURITY: error message contains secret sentinel %q\nError: %s",
			sentinel, err.Error())
	}
}

// TestWriteRuntimeDebugFile_NoSecretsInFile is the mutation test for Site 2
// of #127/P5. It verifies that secret values carried in command arguments do
// NOT appear in the debug file written by WriteRuntimeDebugFile.
//
// Mutation proof: reverting the fix in WriteRuntimeDebugFile (writing the full
// args instead of the redacted summary) causes this test to fail, because the
// sentinel value then appears in the file contents.
func TestWriteRuntimeDebugFile_NoSecretsInFile(t *testing.T) {
	const sentinel = "FAKE-AUTH-SENTINEL-not-a-real-credential"

	agentDir := t.TempDir()
	homeDir := filepath.Join(agentDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatalf("failed to create home dir: %v", err)
	}

	config := RunConfig{
		Debug:   true,
		HomeDir: homeDir,
	}

	// Build an arg list that mimics a sandbox start command with secrets in
	// --env flags — the exact shape that leaks on this tier.
	args := []string{
		"run", "--detach", "--rootfs", "/",
		"--env", "SAFE_VAR=safe-value",
		"--env", "GEMINI_API_KEY=" + sentinel,
		"--env", "SCION_AUTH_TOKEN=" + sentinel,
		"--", "/usr/bin/sciontool", "init",
	}
	WriteRuntimeDebugFile(config, "/usr/local/gcp/bin/sandbox", args)

	debugPath := filepath.Join(agentDir, "runtime-exec-debug")
	content, err := os.ReadFile(debugPath)
	if err != nil {
		t.Fatalf("expected debug file to exist: %v", err)
	}

	text := string(content)

	// The sentinel must NOT appear in the file.
	if strings.Contains(text, sentinel) {
		t.Errorf("SECURITY: debug file contains secret sentinel %q\nFile contents:\n%s",
			sentinel, text)
	}

	// The command name must still appear — we redacted args, not the command.
	if !strings.Contains(text, "sandbox") {
		t.Errorf("debug file should contain the command name\nFile contents:\n%s", text)
	}

	// The arg count must appear for diagnostic value.
	if !strings.Contains(text, fmt.Sprintf("%d args", len(args))) {
		t.Errorf("debug file should mention arg count %d\nFile contents:\n%s",
			len(args), text)
	}

	// Env values must not appear even for "safe" values — we redact all args,
	// because classification is exactly what #127 showed we get wrong.
	if strings.Contains(text, "safe-value") {
		t.Errorf("debug file contains non-secret arg value — all args should be redacted\nFile contents:\n%s", text)
	}
}

func TestScionDirShadowedWhenFullRepoMounted(t *testing.T) {
	// When the full repo root is mounted (workspace outside repo root),
	// a tmpfs shadow mount should be added over /repo-root/.scion to
	// prevent agents from accessing other agents' secrets.
	repoRoot := t.TempDir()
	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RepoRoot:     repoRoot,
		Workspace:    "/some/external/workspace", // outside repo root
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	argStr := strings.Join(args, " ")

	// Should have the full repo root mount
	if !strings.Contains(argStr, "-v "+repoRoot+":/repo-root") {
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
	repoRoot := t.TempDir()
	workspace := filepath.Join(repoRoot, ".scion", "agents", "test", "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RepoRoot:     repoRoot,
		Workspace:    workspace,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	argStr := strings.Join(args, " ")

	// Should NOT have the full repo root mount
	if strings.Contains(argStr, "-v "+repoRoot+":/repo-root ") {
		t.Errorf("expected no full repo root mount, got: %s", argStr)
	}

	// Should NOT have the tmpfs shadow
	if strings.Contains(argStr, "tmpfs") {
		t.Errorf("expected no tmpfs shadow mount, got: %s", argStr)
	}
}

// setupHubManagedBaseRepo creates a fake shared base repo under
// <home>/.scion/projects/<slug> (the hub-native worktree-per-agent
// convention) with a .git admin dir (config, hooks/, info/) and an
// agent worktree directory, and points $HOME at home so
// isHubManagedWorktreeBase resolves it as hub-managed. Returns the repo
// root and the worktree path.
func setupHubManagedBaseRepo(t *testing.T, agentName string) (repoRoot, workspace string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoRoot = filepath.Join(home, config.GlobalDir, config.ProjectsDir, "myproject")
	gitDir := filepath.Join(repoRoot, ".git")
	for _, d := range []string{filepath.Join(gitDir, "hooks"), filepath.Join(gitDir, "info")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("failed to create %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n"), 0644); err != nil {
		t.Fatalf("failed to write .git/config: %v", err)
	}

	workspace = filepath.Join(repoRoot, "worktrees", agentName)
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("failed to create workspace: %v", err)
	}
	return repoRoot, workspace
}

func TestNarrowGitAdminMounts_HubNativeDocker(t *testing.T) {
	// Part A: for a hub-native worktree-per-agent base repo, Docker runs must
	// layer read-only mounts over the shared base's .git admin surface
	// (config, hooks/, info/) on top of the rw .git mount, so the container
	// can no longer write a hook/filter/config key the broker will later
	// honor host-side.
	repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  "docker",
		RepoRoot:     repoRoot,
		Workspace:    workspace,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	argStr := strings.Join(args, " ")

	// The rw base .git mount must remain (agents still need to commit).
	wantRW := fmt.Sprintf("-v %s:/repo-root/.git ", filepath.Join(repoRoot, ".git"))
	if !strings.Contains(argStr, wantRW) {
		t.Errorf("expected rw base .git mount %q, got: %s", wantRW, argStr)
	}

	for _, sub := range []string{"config", "hooks", "info"} {
		want := fmt.Sprintf("-v %s:/repo-root/.git/%s:ro", filepath.Join(repoRoot, ".git", sub), sub)
		if !strings.Contains(argStr, want) {
			t.Errorf("expected narrowed read-only mount %q, got: %s", want, argStr)
		}
	}

	// config.worktree was never created for this agent, so it must not be
	// mounted (mounting a nonexistent source would make Docker create an
	// empty file on the host).
	if strings.Contains(argStr, "config.worktree") {
		t.Errorf("did not expect a config.worktree mount when the file does not exist, got: %s", argStr)
	}
}

func TestNarrowGitAdminMounts_SkippedForLinkedProject(t *testing.T) {
	// Part A must NOT engage for a linked project — its base is the user's
	// own checkout (outside ~/.scion/projects), and their own hooks
	// legitimately run today.
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoRoot := t.TempDir() // an unrelated path outside ~/.scion/projects
	gitDir := filepath.Join(repoRoot, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "hooks"), 0755); err != nil {
		t.Fatalf("failed to create hooks dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n"), 0644); err != nil {
		t.Fatalf("failed to write .git/config: %v", err)
	}
	workspace := filepath.Join(repoRoot, "worktrees", "agent-1")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatalf("failed to create workspace: %v", err)
	}

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  "docker",
		RepoRoot:     repoRoot,
		Workspace:    workspace,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	argStr := strings.Join(args, " ")

	if strings.Contains(argStr, ":/repo-root/.git/config:ro") ||
		strings.Contains(argStr, ":/repo-root/.git/hooks:ro") ||
		strings.Contains(argStr, ":/repo-root/.git/info:ro") {
		t.Errorf("linked project must not get the narrowed admin-dir mount, got: %s", argStr)
	}
}

func TestNarrowGitAdminMounts_SkippedForNonDockerRuntime(t *testing.T) {
	// The mount narrowing is gated to Docker only; podman/apple (and any
	// RuntimeName not yet extended) must fall through unchanged until their
	// own support lands.
	repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")

	for _, runtimeName := range []string{"", "podman", "apple"} {
		args, err := buildCommonRunArgs(RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			RuntimeName:  runtimeName,
			RepoRoot:     repoRoot,
			Workspace:    workspace,
		})
		if err != nil {
			t.Fatalf("buildCommonRunArgs failed for runtime %q: %v", runtimeName, err)
		}
		argStr := strings.Join(args, " ")
		if strings.Contains(argStr, ":/repo-root/.git/config:ro") {
			t.Errorf("runtime %q must not get the narrowed admin-dir mount (narrowing is docker-only), got: %s", runtimeName, argStr)
		}
	}
}

func TestNarrowGitAdminMounts_CreatesMissingHooksAndInfoDirs(t *testing.T) {
	// An empty/custom init.templateDir (or `git init --template=`) can
	// produce a base without .git/hooks or .git/info. Skipping the mount in
	// that case would fail open: the container could then create its own
	// writable hooks/info directly in the rw .git root. narrowGitAdminMounts
	// must create both on the host before mounting.
	home := t.TempDir()
	t.Setenv("HOME", home)
	repoRoot := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "myproject")
	gitDir := filepath.Join(repoRoot, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Deliberately do NOT create hooks/ or info/.
	workspace := filepath.Join(repoRoot, "worktrees", "agent-1")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "hooks")); err == nil {
		t.Fatal("test setup invariant broken: hooks/ should not exist yet")
	}

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  "docker",
		RepoRoot:     repoRoot,
		Workspace:    workspace,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	argStr := strings.Join(args, " ")

	for _, sub := range []string{"hooks", "info"} {
		if _, statErr := os.Stat(filepath.Join(gitDir, sub)); statErr != nil {
			t.Errorf("expected narrowGitAdminMounts to create %s on the host, got: %v", sub, statErr)
		}
		want := fmt.Sprintf("-v %s:/repo-root/.git/%s:ro", filepath.Join(gitDir, sub), sub)
		if !strings.Contains(argStr, want) {
			t.Errorf("expected read-only mount %q even though %s was initially missing, got: %s", want, sub, argStr)
		}
	}
}

func TestNarrowSharerRegistryMounts_HubNativeDocker(t *testing.T) {
	// Part B: for a hub-native worktree-per-agent base, Docker runs must
	// layer read-only mounts over the sharer registry directory and this
	// worktree's admin back-link file, alongside Part A's broader surface.
	repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
	gitDir := filepath.Join(repoRoot, ".git")
	backLinkDir := filepath.Join(gitDir, "worktrees", "agent-1")
	if err := os.MkdirAll(backLinkDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backLinkDir, "gitdir"), []byte(filepath.Join(workspace, ".git")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  "docker",
		RepoRoot:     repoRoot,
		Workspace:    workspace,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	argStr := strings.Join(args, " ")

	sharersDir := filepath.Join(gitDir, "scion-sharers")
	if _, statErr := os.Stat(sharersDir); statErr != nil {
		t.Errorf("expected narrowSharerRegistryMounts to create the sharer-registry dir on the host, got: %v", statErr)
	}
	wantSharers := fmt.Sprintf("-v %s:/repo-root/.git/scion-sharers:ro", sharersDir)
	if !strings.Contains(argStr, wantSharers) {
		t.Errorf("expected read-only sharer-registry mount %q, got: %s", wantSharers, argStr)
	}

	wantBackLink := fmt.Sprintf("-v %s:/repo-root/.git/worktrees/agent-1/gitdir:ro", filepath.Join(backLinkDir, "gitdir"))
	if !strings.Contains(argStr, wantBackLink) {
		t.Errorf("expected read-only admin back-link mount %q, got: %s", wantBackLink, argStr)
	}

	// Part A's broader surface must still apply too (both parts are additive
	// for a hub-native base).
	for _, sub := range []string{"config", "hooks", "info"} {
		want := fmt.Sprintf("-v %s:/repo-root/.git/%s:ro", filepath.Join(gitDir, sub), sub)
		if !strings.Contains(argStr, want) {
			t.Errorf("expected Part A mount %q to still apply, got: %s", want, argStr)
		}
	}
}

func TestNarrowSharerRegistryMounts_LocalDocker(t *testing.T) {
	// Part B's two low-risk paths extend to LOCAL (non-hub-managed) bases —
	// unlike Part A's broader admin surface, which must stay hub-native-only.
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoRoot := t.TempDir() // outside ~/.scion/projects — a linked/local base
	gitDir := filepath.Join(repoRoot, ".git")
	if err := os.MkdirAll(filepath.Join(gitDir, "hooks"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(repoRoot, "worktrees", "agent-1")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}
	backLinkDir := filepath.Join(gitDir, "worktrees", "agent-1")
	if err := os.MkdirAll(backLinkDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backLinkDir, "gitdir"), []byte(filepath.Join(workspace, ".git")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if isHubManagedWorktreeBase(repoRoot) {
		t.Fatal("test setup invariant broken: repoRoot must be a local (non-hub-managed) base")
	}

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  "docker",
		RepoRoot:     repoRoot,
		Workspace:    workspace,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	argStr := strings.Join(args, " ")

	// Part A must NOT apply to a local base.
	if strings.Contains(argStr, ":/repo-root/.git/config:ro") ||
		strings.Contains(argStr, ":/repo-root/.git/hooks:ro") ||
		strings.Contains(argStr, ":/repo-root/.git/info:ro") {
		t.Errorf("local base must not get Part A's broader admin-dir mount, got: %s", argStr)
	}

	// Part B's two paths MUST still apply to a local base.
	sharersDir := filepath.Join(gitDir, "scion-sharers")
	if _, statErr := os.Stat(sharersDir); statErr != nil {
		t.Errorf("expected narrowSharerRegistryMounts to create the sharer-registry dir on the host, got: %v", statErr)
	}
	wantSharers := fmt.Sprintf("-v %s:/repo-root/.git/scion-sharers:ro", sharersDir)
	if !strings.Contains(argStr, wantSharers) {
		t.Errorf("expected read-only sharer-registry mount %q on a local base, got: %s", wantSharers, argStr)
	}
	wantBackLink := fmt.Sprintf("-v %s:/repo-root/.git/worktrees/agent-1/gitdir:ro", filepath.Join(backLinkDir, "gitdir"))
	if !strings.Contains(argStr, wantBackLink) {
		t.Errorf("expected read-only admin back-link mount %q on a local base, got: %s", wantBackLink, argStr)
	}
}

// TestNarrowSharerRegistryMounts_LocalRealAgentLayout pins the headline local
// behavior against the REAL local worktree-mode layout, not just a
// convenient stand-in: pkg/agent.ProvisionAgent's local path puts an agent's
// worktree at <projectDir>/agents/<agentName>/workspace, and git's own admin
// directory name for that worktree (the last path element of the gitdir
// pointer in <workspace>/.git) does not have to match agentName — `git
// worktree add` appends a numeric suffix on a name collision, so
// resolveWorktreeAdminName reads the real name from that gitfile rather than
// assuming filepath.Base(workspace). This test builds exactly that gitfile
// (naming admin dir "workspace1", distinct from the agent name "agent-1")
// and confirms the back-link mount follows the resolved name, not the agent
// name, while the sharer-registry mount is unaffected by any of this.
func TestNarrowSharerRegistryMounts_LocalRealAgentLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	repoRoot := t.TempDir() // a local base, outside ~/.scion/projects
	gitDir := filepath.Join(repoRoot, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}

	// The real local layout: <projectDir>/agents/<agentName>/workspace, where
	// projectDir is <repoRoot>/.scion (config.SelectAgentsRoot's shape).
	workspace := filepath.Join(repoRoot, ".scion", "agents", "agent-1", "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	// The admin dir's resolved name ("workspace1") intentionally differs from
	// the agent name ("agent-1") — this is what forces resolveWorktreeAdminName
	// to actually read the gitfile rather than fall back to filepath.Base.
	adminDir := filepath.Join(gitDir, "worktrees", "workspace1")
	if err := os.MkdirAll(adminDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adminDir, "gitdir"), []byte(filepath.Join(workspace, ".git")+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: "+adminDir+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if isHubManagedWorktreeBase(repoRoot) {
		t.Fatal("test setup invariant broken: repoRoot must be a local (non-hub-managed) base")
	}

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  "docker",
		RepoRoot:     repoRoot,
		Workspace:    workspace,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	argStr := strings.Join(args, " ")

	wantBackLink := fmt.Sprintf("-v %s:/repo-root/.git/worktrees/workspace1/gitdir:ro", filepath.Join(adminDir, "gitdir"))
	if !strings.Contains(argStr, wantBackLink) {
		t.Errorf("expected the back-link mount to use the resolved admin name %q, got: %s", wantBackLink, argStr)
	}
	if strings.Contains(argStr, "worktrees/agent-1/gitdir") {
		t.Errorf("back-link mount must not use the agent name when the gitfile resolves to a different admin name, got: %s", argStr)
	}

	sharersDir := filepath.Join(gitDir, "scion-sharers")
	wantSharers := fmt.Sprintf("-v %s:/repo-root/.git/scion-sharers:ro", sharersDir)
	if !strings.Contains(argStr, wantSharers) {
		t.Errorf("expected the sharer-registry mount independent of admin-name resolution, got: %s", argStr)
	}
}

func TestNarrowSharerRegistryMounts_SkippedForNonDockerRuntime(t *testing.T) {
	// Part B stays gated on the same Docker-only runtime check as Part A —
	// only the isHubManagedWorktreeBase condition differs between them.
	repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")

	for _, runtimeName := range []string{"", "podman", "apple"} {
		args, err := buildCommonRunArgs(RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			RuntimeName:  runtimeName,
			RepoRoot:     repoRoot,
			Workspace:    workspace,
		})
		if err != nil {
			t.Fatalf("buildCommonRunArgs failed for runtime %q: %v", runtimeName, err)
		}
		argStr := strings.Join(args, " ")
		if strings.Contains(argStr, "scion-sharers") {
			t.Errorf("runtime %q must not get the sharer-registry mount (Docker-only), got: %s", runtimeName, argStr)
		}
	}
}

func TestNarrowSharerRegistryMounts_SkipsMissingBackLink(t *testing.T) {
	// The admin back-link file is written once by `git worktree add` before
	// this worktree's container ever starts. A base with no worktrees yet
	// legitimately has none — skip-if-missing, like config.worktree, not a
	// create-first fail-open gap like the sharer-registry directory.
	repoRoot, workspace := setupHubManagedBaseRepo(t, "agent-1")
	// Deliberately do NOT create .git/worktrees/agent-1/gitdir.

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RuntimeName:  "docker",
		RepoRoot:     repoRoot,
		Workspace:    workspace,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}
	argStr := strings.Join(args, " ")

	if strings.Contains(argStr, "/gitdir:ro") {
		t.Errorf("did not expect a gitdir back-link mount when the file does not exist, got: %s", argStr)
	}
	// The sharer-registry directory mount must still apply independently.
	wantSharers := fmt.Sprintf("-v %s:/repo-root/.git/scion-sharers:ro", filepath.Join(repoRoot, ".git", "scion-sharers"))
	if !strings.Contains(argStr, wantSharers) {
		t.Errorf("expected the sharer-registry mount regardless of the back-link file, got: %s", argStr)
	}
}

// TestSharerRegistryDirName_MatchesProvisionRegistryLayout is a drift
// tripwire, not a behavior test: sharerRegistryDirName is a literal
// duplicating pkg/provision's own (unexported) registry directory name,
// because that name isn't part of provision's public API. Duplicated
// literals can silently drift — if provision ever renamed its registry
// directory, this mount would fail open (silently narrowing nothing) rather
// than failing loudly. Exercising the real registry write path and asserting
// the marker lands under sharerRegistryDirName catches that drift here,
// without changing provision's API.
func TestSharerRegistryDirName_MatchesProvisionRegistryLayout(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	branch := "shared-branch"
	worktreePath := provision.WorktreePath(base, "agent-1")

	if err := provision.RegisterSharer(base, "", branch, worktreePath, "agent-1"); err != nil {
		t.Fatalf("RegisterSharer: %v", err)
	}

	markerDir := filepath.Join(base, ".git", sharerRegistryDirName)
	entries, err := os.ReadDir(markerDir)
	if err != nil {
		t.Fatalf("expected the sharer registry to write its marker under .git/%s (the exact directory "+
			"narrowSharerRegistryMounts narrows) — if this fails, pkg/provision's registry directory name "+
			"and sharerRegistryDirName in this package have drifted apart: %v", sharerRegistryDirName, err)
	}
	if len(entries) == 0 {
		t.Errorf("expected at least one marker file under %s, found none", markerDir)
	}
}

func TestIsHubManagedWorktreeBase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	hubNative := filepath.Join(home, config.GlobalDir, config.ProjectsDir, "myproject")
	linked := filepath.Join(home, "dev", "myproject")

	if !isHubManagedWorktreeBase(hubNative) {
		t.Errorf("expected %q (under GlobalDir/ProjectsDir) to be hub-managed", hubNative)
	}
	if isHubManagedWorktreeBase(linked) {
		t.Errorf("expected %q (outside GlobalDir) to NOT be hub-managed", linked)
	}
	if isHubManagedWorktreeBase("") {
		t.Errorf("expected empty repoRoot to NOT be hub-managed")
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
		Harness:      &harness.Generic{},
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

func TestEmptySourcePathSkippedInApplyResolvedAuth(t *testing.T) {
	// Regression test: in broker mode, SourcePath is intentionally empty.
	// applyResolvedAuth must skip these entries — attempting to copy an empty
	// path would fail at util.CopyFile("").
	agentHome := t.TempDir()

	var mountedFiles []string
	config := RunConfig{
		UnixUsername: "scion",
		HomeDir:      agentHome,
		ResolvedAuth: &api.ResolvedAuth{
			Files: []api.FileMapping{
				{SourcePath: "", ContainerPath: "~/.config/gcloud/application_default_credentials.json"},
				{SourcePath: "", ContainerPath: "~/.config/gcloud/credentials.db"},
			},
		},
	}

	err := applyResolvedAuth(config,
		func(k, v string) {},       // addEnv
		func(v api.VolumeMount) {}, // addVolume
		func(src, dst string, ro, bind bool) { mountedFiles = append(mountedFiles, src) }, // registerMount
	)
	if err != nil {
		t.Fatalf("applyResolvedAuth should succeed with empty SourcePaths, got: %v", err)
	}
	if len(mountedFiles) > 0 {
		t.Errorf("expected no mounts for empty SourcePath files, got %d: %v", len(mountedFiles), mountedFiles)
	}

	// Verify no files were copied into HomeDir
	entries, _ := os.ReadDir(agentHome)
	if len(entries) > 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected empty HomeDir, found: %v", names)
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

func TestResolveHostNetworking(t *testing.T) {
	tests := []struct {
		name        string
		runtimeName string
		env         map[string]string
		forceHost   bool // set SCION_FORCE_HOST_NETWORK for this case
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
		{
			name:        "force-host overrides domain endpoint",
			runtimeName: "docker",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "https://hub.example.com",
			},
			forceHost: true,
			wantMode:  "host",
			wantEP:    "https://hub.example.com",
		},
		{
			name:        "force-host with localhost endpoint",
			runtimeName: "docker",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "http://localhost:8080",
			},
			forceHost: true,
			wantMode:  "host",
			wantEP:    "http://localhost:8080",
		},
		{
			name:        "force-host applies to non-docker",
			runtimeName: "podman",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "http://localhost:8080",
			},
			forceHost: true,
			wantMode:  "host",
			wantEP:    "http://localhost:8080",
		},
		{
			name:        "force-host with podman bridge hostname rewrites to localhost",
			runtimeName: "podman",
			env: map[string]string{
				"SCION_HUB_ENDPOINT": "http://host.containers.internal:8080",
				"SCION_HUB_URL":      "http://host.containers.internal:8080",
			},
			forceHost: true,
			wantMode:  "host",
			wantEP:    "http://localhost:8080",
		},
		{
			name:        "force-host with no endpoint yields no override",
			runtimeName: "docker",
			env:         map[string]string{},
			forceHost:   true,
			wantMode:    "",
		},
		{
			name:        "force-host with no endpoint yields no override (non-docker)",
			runtimeName: "podman",
			env:         map[string]string{},
			forceHost:   true,
			wantMode:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.forceHost {
				t.Setenv(ForceHostNetworkEnvVar, "1")
			}
			// Copy env to avoid mutation across subtests
			env := make(map[string]string)
			for k, v := range tt.env {
				env[k] = v
			}

			got := ResolveHostNetworking(tt.runtimeName, env)
			if got != tt.wantMode {
				t.Errorf("ResolveHostNetworking(%q) = %q, want %q", tt.runtimeName, got, tt.wantMode)
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
		Harness:      &harness.Generic{},
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

// TestBuildCommonRunArgs_RejectsUnsafeWorkspaceSource is the fail-closed
// regression test for the check at this second gate: a workspace source
// that is not an allowed workspace path must still be refused here even
// when the caller is not pkg/agent's Start() (which already validates), and
// — critically — no mount args may be produced at all.
func TestBuildCommonRunArgs_RejectsUnsafeWorkspaceSource(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Workspace:    "/",
	}

	args, err := buildCommonRunArgs(config)
	if err == nil {
		t.Fatal("expected buildCommonRunArgs to fail for a workspace source of '/'")
	}
	if len(args) != 0 {
		t.Errorf("expected no args on rejection (fail closed), got: %v", args)
	}
}

// TestBuildCommonRunArgs_UsesResolvedWorkspaceSource proves the actual mount
// is set up from the validator's resolved, symlink-free path, not the
// nominal (symlinked) config.Workspace value.
func TestBuildCommonRunArgs_UsesResolvedWorkspaceSource(t *testing.T) {
	tmpDir := t.TempDir()

	realWorkspace := filepath.Join(tmpDir, "real-workspace")
	if err := os.MkdirAll(realWorkspace, 0755); err != nil {
		t.Fatal(err)
	}
	workspaceLink := filepath.Join(tmpDir, "workspace-link")
	if err := os.Symlink(realWorkspace, workspaceLink); err != nil {
		t.Fatal(err)
	}

	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Workspace:    workspaceLink,
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	realResolved, err := filepath.EvalSymlinks(realWorkspace)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", realWorkspace, err)
	}

	argStr := strings.Join(args, " ")
	if !strings.Contains(argStr, realResolved) {
		t.Errorf("expected mount args to reference the resolved path %q, got: %v", realResolved, args)
	}
	if strings.Contains(argStr, workspaceLink) {
		t.Errorf("expected mount args to NOT reference the nominal symlinked path %q, got: %v", workspaceLink, args)
	}
}

// TestBuildCommonRunArgs_ResolvesSymlinkedRepoRoot_WorktreeSubdir covers a
// gap in symlink resolution: buildCommonRunArgs resolved config.Workspace
// but not config.RepoRoot, so a symlinked repo path changed the mount
// layout depending on which of the two still carried the symlink by the
// time filepath.Rel compared them. Both must be resolved the same way
// before that comparison, so the worktree-subdirectory branch fires
// correctly (mounting .git and the workspace at their real, relative paths)
// instead of falling into the "outside repo root" fallback.
func TestBuildCommonRunArgs_ResolvesSymlinkedRepoRoot_WorktreeSubdir(t *testing.T) {
	realRepoRoot := t.TempDir()
	workspace := filepath.Join(realRepoRoot, ".scion", "agents", "test-agent", "workspace")
	if err := os.MkdirAll(workspace, 0755); err != nil {
		t.Fatal(err)
	}

	repoRootLink := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(realRepoRoot, repoRootLink); err != nil {
		t.Fatal(err)
	}

	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RepoRoot:     repoRootLink,
		Workspace:    workspace,
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	realResolvedRoot, err := filepath.EvalSymlinks(realRepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", realRepoRoot, err)
	}

	argStr := strings.Join(args, " ")
	if !strings.Contains(argStr, "-v "+filepath.Join(realResolvedRoot, ".git")+":/repo-root/.git") {
		t.Errorf("expected the worktree-subdir .git mount at the resolved repo root, got: %v", args)
	}
	if !strings.Contains(argStr, "--workdir /repo-root/.scion/agents/test-agent/workspace") {
		t.Errorf("expected the worktree-subdir workdir layout, got: %v", args)
	}
	if strings.Contains(argStr, "/repo-root:/repo-root") || strings.Contains(argStr, repoRootLink) {
		t.Errorf("expected no fallback full-repo-root mount and no reference to the nominal symlinked repo root, got: %v", args)
	}
}

// TestBuildCommonRunArgs_ResolvesSymlinkedRepoRoot_SharedWorkspace is the
// shared-workspace sibling of the worktree-subdir case above: when Workspace
// equals RepoRoot, resolving both through the same symlink must still land
// on the single-/workspace-mount branch, not the outside-repo-root fallback.
func TestBuildCommonRunArgs_ResolvesSymlinkedRepoRoot_SharedWorkspace(t *testing.T) {
	realRepoRoot := t.TempDir()
	repoRootLink := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(realRepoRoot, repoRootLink); err != nil {
		t.Fatal(err)
	}

	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		RepoRoot:     repoRootLink,
		Workspace:    repoRootLink,
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	realResolvedRoot, err := filepath.EvalSymlinks(realRepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", realRepoRoot, err)
	}

	argStr := strings.Join(args, " ")
	if !strings.Contains(argStr, "-v "+realResolvedRoot+":/workspace") {
		t.Errorf("expected the single shared /workspace mount at the resolved repo root, got: %v", args)
	}
	if strings.Contains(argStr, "/repo-root") {
		t.Errorf("expected no /repo-root mount for the shared-workspace case, got: %v", args)
	}
}

// TestBuildCommonRunArgs_AcceptsLegitimateScionHomeWorkspaces is the positive
// acceptance-set counterpart to TestBuildCommonRunArgs_RejectsUnsafeWorkspaceSource:
// buildCommonRunArgs has no per-project root to pass to the shared validator
// (see the no-root comment at its call site), so it depends entirely on the
// validator's named ~/.scion allow list to still admit real workspaces. Both
// the global project's own workspace and a hub-managed project's workspace
// must go through, not just fail to be rejected outright.
func TestBuildCommonRunArgs_AcceptsLegitimateScionHomeWorkspaces(t *testing.T) {
	tests := []struct {
		name    string
		relPath []string
	}{
		{name: "global project workspace", relPath: []string{".scion", "workspace"}},
		{name: "hub-managed project workspace", relPath: []string{".scion", "projects", "my-project", "workspace"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)

			parts := append([]string{tmpHome}, tt.relPath...)
			workspace := filepath.Join(parts...)
			if err := os.MkdirAll(workspace, 0755); err != nil {
				t.Fatal(err)
			}

			config := RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				Workspace:    workspace,
			}

			args, err := buildCommonRunArgs(config)
			if err != nil {
				t.Fatalf("expected %q to be accepted, got error: %v", workspace, err)
			}
			if !strings.Contains(strings.Join(args, " "), workspace) {
				t.Errorf("expected mount args to reference %q, got: %v", workspace, args)
			}
		})
	}
}

// TestBuildCommonRunArgs_RejectsUnsafeHomeDir is
// TestBuildCommonRunArgs_RejectsUnsafeWorkspaceSource's counterpart for
// config.HomeDir: a home directory that is not an allowed agent-home path
// must be refused before it becomes a bind mount.
func TestBuildCommonRunArgs_RejectsUnsafeHomeDir(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		HomeDir:      "/",
	}

	args, err := buildCommonRunArgs(config)
	if err == nil {
		t.Fatal("expected buildCommonRunArgs to fail for a home directory of '/'")
	}
	if len(args) != 0 {
		t.Errorf("expected no args on rejection (fail closed), got: %v", args)
	}
}

// TestBuildCommonRunArgs_AcceptsRealAgentHomes covers the positive
// acceptance set for config.HomeDir: the three real shapes
// config.GetAgentHomePath produces under ~/.scion, plus a plain in-repo
// project's own agent home (outside ~/.scion entirely, admitted through the
// fixed floors alone, the same as any other source with no per-project
// root to check against).
func TestBuildCommonRunArgs_AcceptsRealAgentHomes(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tests := []struct {
		name    string
		relPath []string
	}{
		{name: "global project agent home", relPath: []string{".scion", "agents", "a", "home"}},
		{name: "hub-managed project agent home", relPath: []string{".scion", "projects", "p", ".scion", "agents", "a", "home"}},
		{name: "externalized git project agent home", relPath: []string{".scion", "project-configs", "d__1", ".scion", "agents", "a", "home"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := append([]string{tmpHome}, tt.relPath...)
			homeDir := filepath.Join(parts...)
			if err := os.MkdirAll(homeDir, 0755); err != nil {
				t.Fatal(err)
			}

			config := RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        "scion-agent:latest",
				HomeDir:      homeDir,
			}

			args, err := buildCommonRunArgs(config)
			if err != nil {
				t.Fatalf("expected %q to be accepted, got error: %v", homeDir, err)
			}
			if !strings.Contains(strings.Join(args, " "), homeDir) {
				t.Errorf("expected mount args to reference %q, got: %v", homeDir, args)
			}
		})
	}

	t.Run("in-repo project agent home", func(t *testing.T) {
		tmpDir := t.TempDir()
		homeDir := filepath.Join(tmpDir, "repo", ".scion", "agents", "a", "home")
		if err := os.MkdirAll(homeDir, 0755); err != nil {
			t.Fatal(err)
		}

		config := RunConfig{
			Harness:      &harness.Generic{},
			Name:         "test-agent",
			UnixUsername: "scion",
			Image:        "scion-agent:latest",
			HomeDir:      homeDir,
		}

		args, err := buildCommonRunArgs(config)
		if err != nil {
			t.Fatalf("expected %q to be accepted, got error: %v", homeDir, err)
		}
		if !strings.Contains(strings.Join(args, " "), homeDir) {
			t.Errorf("expected mount args to reference %q, got: %v", homeDir, args)
		}
	})
}

func TestBuildCommonRunArgs_ExtraHosts(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.Generic{},
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

// TestBuildCommonRunArgs_ProjectLabels_ExactSet asserts that a config with
// Project and ProjectID set emits exactly the canonical scion.* labels and
// nothing else — in particular, no extra legacy label sneaks in alongside
// them.
func TestBuildCommonRunArgs_ProjectLabels_ExactSet(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.Generic{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Project:      "p",
		ProjectID:    "id",
	}

	args, err := buildCommonRunArgs(config)
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	var scionLabels []string
	for i, arg := range args {
		if arg == "--label" && i+1 < len(args) {
			if v := args[i+1]; strings.HasPrefix(v, "scion.") {
				scionLabels = append(scionLabels, v)
			}
		}
	}
	slices.Sort(scionLabels)

	want := []string{"scion.project=p", "scion.project_id=id"}
	if !slices.Equal(scionLabels, want) {
		t.Fatalf("scion.* --label values = %v, want %v", scionLabels, want)
	}
}

func TestSerializeSecrets_DeduplicatesByTarget(t *testing.T) {
	containerHome := "/home/scion"

	secrets := []api.ResolvedSecret{
		{Name: "user-cert", Type: "file", Target: "/tmp/my-secret.json", Value: "user-data", Source: "user"},
		{Name: "project-cert", Type: "file", Target: "/tmp/my-secret.json", Value: "project-data", Source: "project"},
		{Name: "other-file", Type: "file", Target: "/tmp/other.json", Value: "other-data", Source: "user"},
		{Name: "env-secret", Type: "environment", Target: "FOO", Value: "bar", Source: "user"},
	}

	encoded, err := serializeSecrets(containerHome, secrets)
	if err != nil {
		t.Fatalf("serializeSecrets failed: %v", err)
	}

	staged, err := stagedsecrets.Decode(encoded)
	if err != nil {
		t.Fatalf("stagedsecrets.Decode failed: %v", err)
	}

	// Duplicate targets are deduplicated (last entry wins), so we should have
	// 2 entries: one for /tmp/my-secret.json (project-cert) and one for /tmp/other.json.
	if len(staged.FileSecrets) != 2 {
		t.Fatalf("expected 2 file secrets after dedup, got %d", len(staged.FileSecrets))
	}
	if staged.FileSecrets[0].Name != "project-cert" {
		t.Errorf("expected project-cert to win for duplicate target, got %s", staged.FileSecrets[0].Name)
	}
	if staged.FileSecrets[1].Name != "other-file" {
		t.Errorf("expected other-file as second entry, got %s", staged.FileSecrets[1].Name)
	}
}

// TestSharedWorkspace_NoAgentStateInMounts asserts the structural invariant
// from .design/hub-shared-workspace-isolation.md: when an agent is launched
// in a shared-workspace project (workspace == repo root), the assembled run
// args must not bind-mount any host path under <project>/.scion/agents/ into
// the container. Per-agent state lives at the external project-configs path
// instead, so siblings cannot read it via /workspace.
func TestSharedWorkspace_NoAgentStateInMounts(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(filepath.Join(projectDir, ".scion", "agents", "agent-a"), 0755); err != nil {
		t.Fatalf("mkdir in-project agent dir: %v", err)
	}
	// External per-agent state for the agent under test (where prompt.md and
	// scion-agent.json are relocated to in shared-workspace mode).
	extAgentDir := filepath.Join(tmpDir, "external", "agents", "agent-a")
	homeDir := filepath.Join(extAgentDir, "home")
	if err := os.MkdirAll(homeDir, 0755); err != nil {
		t.Fatalf("mkdir homeDir: %v", err)
	}

	args, err := buildCommonRunArgs(RunConfig{
		Harness:      &harness.Generic{},
		Name:         "agent-a",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		// Shared-workspace shape: workspace == repo root. buildCommonRunArgs
		// hits the relWorkspace == "." branch and mounts project → /workspace.
		RepoRoot:  projectDir,
		Workspace: projectDir,
		HomeDir:   homeDir,
	})
	if err != nil {
		t.Fatalf("buildCommonRunArgs failed: %v", err)
	}

	forbidden := filepath.Join(projectDir, ".scion", "agents")
	for i, a := range args {
		if strings.Contains(a, forbidden) {
			t.Errorf("arg[%d] = %q references forbidden host path %s; per-agent state must live external (.design/hub-shared-workspace-isolation.md)", i, a, forbidden)
		}
	}
	// Sanity: the project directory itself should still be mounted at /workspace.
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, fmt.Sprintf("%s:/workspace", projectDir)) {
		t.Errorf("expected project dir %s to be mounted at /workspace, args: %s", projectDir, joined)
	}
}

func TestBuildCommonRunArgs_FuseMountArgOrdering(t *testing.T) {
	config := RunConfig{
		Harness:      &harness.Generic{},
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
				Harness:      &harness.Generic{},
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
			// Verify the prompt is single-quoted (not double-quoted). The harness
			// arg is single-quoted once (shellQuote), and the whole agent-window
			// script is single-quoted again for the `sh -c` exit-code wrapper, so
			// the inner single quotes are re-escaped as '\''.
			shCmd := args[len(args)-1]
			quoted := shellQuote(tt.task)
			reEscaped := strings.ReplaceAll(quoted, "'", `'\''`)
			if !strings.Contains(shCmd, reEscaped) {
				t.Errorf("expected re-escaped single-quoted prompt %q in sh -c arg, got: %s", reEscaped, shCmd)
			}
			// Ensure the prompt is never double-quoted (which would let the shell
			// interpret metacharacters like $ and backticks).
			if strings.Contains(shCmd, `"`+tt.task+`"`) {
				t.Errorf("prompt was double-quoted in sh -c arg, got: %s", shCmd)
			}
		})
	}
}

func TestExitCodeFromContainerStatus(t *testing.T) {
	tests := []struct {
		status   string
		wantCode int
		wantOK   bool
	}{
		{"Exited (0) 3 hours ago", 0, true},
		{"Exited (137) 2 minutes ago", 137, true},
		{"exited (1)", 1, true},
		{"Up 5 minutes", 0, false},
		{"running", 0, false},
		{"stopped", 0, false},
		{"Created", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			code, ok := ExitCodeFromContainerStatus(tc.status)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d", code, tc.wantCode)
			}
		})
	}
}

func TestParseDockerServerVersion(t *testing.T) {
	tests := []struct {
		in        string
		wantMajor int
		wantMinor int
		wantOK    bool
	}{
		{"24.0.7", 24, 0, true},
		{"v24.0.7", 24, 0, true},
		{"V24.0.7", 24, 0, true},
		{"20.10.21", 20, 10, true},
		{"19.03.15", 19, 3, true},
		{"27.5", 27, 5, true},
		{"  24.0.7  ", 24, 0, true},
		{"WARNING: something\n24.0.7", 24, 0, true},
		{"", 0, 0, false},
		{"garbage", 0, 0, false},
		{"x.y.z", 0, 0, false},
		{"20", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			major, minor, ok := parseDockerServerVersion(tt.in)
			if ok != tt.wantOK || major != tt.wantMajor || minor != tt.wantMinor {
				t.Errorf("parseDockerServerVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tt.in, major, minor, ok, tt.wantMajor, tt.wantMinor, tt.wantOK)
			}
		})
	}
}
