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

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/harness"
)

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
			wantIn: []string{"run", "-d", "-i", "--name", "test-agent", "scion-agent:latest", "gemini", "--prompt-interactive", "hello"},
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
				Harness:      &harness.GeminiCLI{},
				Name: "test-agent",
				Auth: api.AuthConfig{
					GeminiAPIKey: "sk-123",
				},
				Image: "scion-agent:latest",
			},
			wantIn:  []string{"-e", "GEMINI_API_KEY=sk-123", "-e", "GEMINI_DEFAULT_AUTH_TYPE=gemini-api-key"},
			wantOut: []string{"--prompt-interactive"},
		},
		{
			name: "labels and tmux",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name: "test-agent",
				Labels: map[string]string{
					"foo": "bar",
				},
				UseTmux: true,
				Image:   "scion-agent:latest",
				Task:    "hello",
			},
			wantIn: []string{
				"--label", "foo=bar",
				"--label", "scion.tmux=true",
				"tmux", "new-session", "-s", "scion",
			},
		},
		{
			name: "oauth propagation with home",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:         "test-agent",
				UnixUsername: "scion",
				HomeDir:      tmpHome,
				Auth: api.AuthConfig{
					OAuthCreds: oauthFile,
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
				Auth: api.AuthConfig{
					GoogleAppCredentials: adcFile,
				},
				Image: "scion-agent:latest",
			},
			wantIn: []string{
				"-v", fmt.Sprintf("%s:/home/scion/.config/gcp/application_default_credentials.json:ro", adcFile),
				"-e", "GOOGLE_APPLICATION_CREDENTIALS=/home/scion/.config/gcp/application_default_credentials.json",
				"-e", "GEMINI_DEFAULT_AUTH_TYPE=compute-default-credentials",
			},
		},
		{
			name: "other auth and model",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name: "test-agent",
				Auth: api.AuthConfig{
					GoogleAPIKey:       "google-123",
					VertexAPIKey:       "vertex-123",
					GoogleCloudProject: "my-project",
				},
				Env:   []string{"GEMINI_MODEL=gemini-1.5-pro"},
				Image: "scion-agent:latest",
			},
			wantIn: []string{
				"-e GOOGLE_API_KEY=google-123",
				"-e VERTEX_API_KEY=vertex-123",
				"-e GOOGLE_CLOUD_PROJECT=my-project",
				"-e GEMINI_MODEL=gemini-1.5-pro",
			},
		},
		{
			name: "resume and env",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:  "test-agent",
				Image: "scion-agent:latest",
				Env:   []string{"FOO=BAR"},
				Task:  "hello",
				Resume: true,
			},
			wantIn: []string{
				"-e FOO=BAR",
				"gemini --yolo --resume",
				"--prompt-interactive hello",
			},
		},
		{
			name: "resume and tmux",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
				Name:    "test-agent",
				Image:   "scion-agent:latest",
				UseTmux: true,
				Task:    "hello",
				Resume:  true,
			},
			wantIn: []string{
				"tmux new-session -s scion gemini --yolo --resume --prompt-interactive hello",
			},
		},
		{
			name: "template label",
			config: RunConfig{
				Harness:      &harness.GeminiCLI{},
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
				Auth: api.AuthConfig{
					OAuthCreds: oauthFile,
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

				Harness:      &harness.GeminiCLI{},

				Name:         "test-agent",

				UnixUsername: "scion",

				Image:        "scion-agent:latest",

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

		

	