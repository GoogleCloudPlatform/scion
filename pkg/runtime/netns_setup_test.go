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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestBuildNetnsSetupArgs(t *testing.T) {
	cfg := RunConfig{
		Name:  "agent-1",
		Image: "scion-agent:test",
		Env: []string{
			"SCION_METADATA_MODE=block",
			"SCION_METADATA_PORT=18380",
			"SCION_INIT_FIREWALL=true",
			"SCION_AUTH_TOKEN=super-secret-token",
			"OPENAI_API_KEY=sk-secret",
		},
		Labels: map[string]string{
			"scion.agent": "true",
			"scion.name":  "agent-1",
		},
		Volumes: []api.VolumeMount{
			{Source: "/host/data", Target: "/data"},
		},
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "SECRET", Type: "environment", Target: "SECRET_VAL", Value: "top-secret"},
		},
	}

	got := buildNetnsSetupArgs("agent-cid-123", cfg)
	want := []string{
		"run", "--rm",
		"--network", "container:agent-cid-123",
		"--cap-add", "NET_ADMIN",
		"--user", "0",
		"--entrypoint", "/usr/local/bin/sciontool",
		"-e", "SCION_METADATA_MODE=block",
		"-e", "SCION_METADATA_PORT=18380",
		"-e", "SCION_INIT_FIREWALL=true",
		"--label", "scion.netns-setup=agent-cid-123",
		"scion-agent:test",
		"netns-setup",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildNetnsSetupArgs() =\n  %v\nwant:\n  %v", got, want)
	}
	joined := strings.Join(got, " ")
	for _, forbidden := range []string{"scion.agent", "scion.name", "super-secret-token", "sk-secret", "top-secret", "-v", "--mount", "/host/data"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("sidecar argv must not contain %q; got %v", forbidden, got)
		}
	}
}

func TestBuildNetnsSetupArgs_OmitsUnsetOptionalEnv(t *testing.T) {
	cfg := RunConfig{
		Image: "scion-agent:test",
		Env:   []string{"SCION_METADATA_MODE=assign"},
	}
	got := buildNetnsSetupArgs("cid-456", cfg)
	want := []string{
		"run", "--rm",
		"--network", "container:cid-456",
		"--cap-add", "NET_ADMIN",
		"--user", "0",
		"--entrypoint", "/usr/local/bin/sciontool",
		"-e", "SCION_METADATA_MODE=assign",
		"--label", "scion.netns-setup=cid-456",
		"scion-agent:test",
		"netns-setup",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildNetnsSetupArgs() =\n  %v\nwant:\n  %v", got, want)
	}
}

func TestDockerRuntime_Run_NetnsSetupSuccess(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")
	callLog := filepath.Join(tmpDir, "calls.log")

	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "run" ]; then
  case "$*" in
    *"netns-setup"*)
      exit 0
      ;;
    *)
      echo "deadbeef1234"
      exit 0
      ;;
  esac
fi
`, callLog)
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	rt := &DockerRuntime{Command: mockDocker}
	id, err := rt.Run(context.Background(), RunConfig{
		Harness:              &harness.Generic{},
		Name:                 "test-agent",
		UnixUsername:         "scion",
		Image:                "scion-agent:latest",
		Task:                 "hello",
		Env:                  []string{"SCION_METADATA_MODE=block", "SCION_METADATA_PORT=18380"},
		MetadataInterception: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if id != "deadbeef1234" {
		t.Fatalf("Run() id = %q, want %q", id, "deadbeef1234")
	}

	raw, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read calls.log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d docker invocations, want 2:\n%s", len(lines), string(raw))
	}
	if strings.Contains(lines[0], "--cap-add NET_ADMIN") {
		t.Errorf("agent run must not pass --cap-add NET_ADMIN: %s", lines[0])
	}
	if !strings.Contains(lines[0], "SCION_METADATA_INTERCEPTION=required") || !strings.Contains(lines[0], "SCION_METADATA_INTERCEPTION_SOURCE=runtime") {
		t.Errorf("agent run missing interception markers: %s", lines[0])
	}
	if !strings.Contains(lines[1], "--network container:deadbeef1234") || !strings.Contains(lines[1], "--cap-add NET_ADMIN") || !strings.Contains(lines[1], "netns-setup") {
		t.Errorf("sidecar invocation unexpected: %s", lines[1])
	}
}

func TestDockerRuntime_Run_NetnsSetupFailureRemovesAgentContainer(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")
	rmLog := filepath.Join(tmpDir, "rm.log")

	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "rm" ]; then
  echo "$@" >> %q
  exit 0
fi
if [ "$1" = "run" ]; then
  case "$*" in
    *"netns-setup"*)
      echo "iptables failed" >&2
      exit 1
      ;;
    *)
      echo "deadbeef5678"
      exit 0
      ;;
  esac
fi
`, rmLog)
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	rt := &DockerRuntime{Command: mockDocker}
	_, err := rt.Run(context.Background(), RunConfig{
		Harness:              &harness.Generic{},
		Name:                 "test-agent",
		UnixUsername:         "scion",
		Image:                "scion-agent:latest",
		Task:                 "hello",
		Env:                  []string{"SCION_METADATA_MODE=block"},
		MetadataInterception: true,
	})
	if err == nil {
		t.Fatal("Run() = nil on sidecar failure, want error")
	}

	raw, readErr := os.ReadFile(rmLog)
	if readErr != nil {
		t.Fatalf("expected docker rm -f after sidecar failure, but rm.log was not written: %v", readErr)
	}
	if got := strings.TrimSpace(string(raw)); got != "rm -f deadbeef5678" {
		t.Errorf("cleanup invocation = %q, want %q", got, "rm -f deadbeef5678")
	}
}

func TestDockerRuntime_Run_NetnsSetupTimeoutRemovesAgentContainer(t *testing.T) {
	tmpDir := t.TempDir()
	mockDocker := filepath.Join(tmpDir, "mock-docker")
	rmLog := filepath.Join(tmpDir, "rm.log")

	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "rm" ]; then
  echo "$@" >> %q
  exit 0
fi
if [ "$1" = "run" ]; then
  case "$*" in
    *"netns-setup"*)
      exec sleep 30
      ;;
    *)
      echo "deadbeef9999"
      exit 0
      ;;
  esac
fi
`, rmLog)
	if err := os.WriteFile(mockDocker, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	rt := &DockerRuntime{Command: mockDocker}
	_, err := rt.Run(ctx, RunConfig{
		Harness:              &harness.Generic{},
		Name:                 "test-agent",
		UnixUsername:         "scion",
		Image:                "scion-agent:latest",
		Task:                 "hello",
		Env:                  []string{"SCION_METADATA_MODE=block"},
		MetadataInterception: true,
	})
	if err == nil {
		t.Fatal("Run() = nil on sidecar timeout, want error")
	}

	raw, readErr := os.ReadFile(rmLog)
	if readErr != nil {
		t.Fatalf("expected docker rm -f after sidecar timeout, but rm.log was not written: %v", readErr)
	}
	if got := strings.TrimSpace(string(raw)); got != "rm -f deadbeef9999" {
		t.Errorf("cleanup invocation = %q, want %q", got, "rm -f deadbeef9999")
	}
}

func TestPodmanRuntime_Run_NetnsSetupRootfulAndRootless(t *testing.T) {
	t.Run("rootful runs sidecar", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockPodman := filepath.Join(tmpDir, "mock-podman")
		callLog := filepath.Join(tmpDir, "calls.log")

		script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "run" ]; then
  case "$*" in
    *"netns-setup"*)
      exit 0
      ;;
    *)
      echo %q
      exit 0
      ;;
  esac
fi
`, callLog, mockPodmanID)
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}

		rt := &PodmanRuntime{Command: mockPodman, Rootless: false}
		id, err := rt.Run(context.Background(), RunConfig{
			Harness:              &harness.Generic{},
			Name:                 "test-agent",
			UnixUsername:         "scion",
			Image:                "scion-agent:latest",
			Task:                 "hello",
			Env:                  []string{"SCION_METADATA_MODE=block", "SCION_INIT_FIREWALL=true"},
			MetadataInterception: true,
		})
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if id != mockPodmanID {
			t.Fatalf("Run() id = %q, want %q", id, mockPodmanID)
		}

		raw, err := os.ReadFile(callLog)
		if err != nil {
			t.Fatalf("read calls.log: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) != 2 {
			t.Fatalf("got %d podman invocations, want 2:\n%s", len(lines), string(raw))
		}
		if !strings.Contains(lines[1], "--network container:"+mockPodmanID) || !strings.Contains(lines[1], "SCION_INIT_FIREWALL=true") || !strings.Contains(lines[1], "netns-setup") {
			t.Errorf("sidecar invocation unexpected: %s", lines[1])
		}
	})

	t.Run("rootful sidecar failure removes agent container", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockPodman := filepath.Join(tmpDir, "mock-podman")
		rmLog := filepath.Join(tmpDir, "rm.log")

		script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "rm" ]; then
  echo "$@" >> %q
  exit 0
fi
if [ "$1" = "run" ]; then
  case "$*" in
    *"netns-setup"*)
      echo "iptables failed" >&2
      exit 1
      ;;
    *)
      echo %q
      exit 0
      ;;
  esac
fi
`, rmLog, mockPodmanID)
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}

		rt := &PodmanRuntime{Command: mockPodman, Rootless: false}
		if _, err := rt.Run(context.Background(), RunConfig{
			Harness:              &harness.Generic{},
			Name:                 "test-agent",
			UnixUsername:         "scion",
			Image:                "scion-agent:latest",
			Task:                 "hello",
			Env:                  []string{"SCION_METADATA_MODE=block"},
			MetadataInterception: true,
		}); err == nil {
			t.Fatal("Run() = nil on sidecar failure, want error")
		}

		raw, readErr := os.ReadFile(rmLog)
		if readErr != nil {
			t.Fatalf("expected podman rm -f after sidecar failure: %v", readErr)
		}
		if got := strings.TrimSpace(string(raw)); got != "rm -f "+mockPodmanID {
			t.Errorf("cleanup invocation = %q, want %q", got, "rm -f "+mockPodmanID)
		}
	})

	t.Run("rootless skips sidecar", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockPodman := filepath.Join(tmpDir, "mock-podman")
		callLog := filepath.Join(tmpDir, "calls.log")

		script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
echo %q
`, callLog, mockPodmanID)
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}

		rt := &PodmanRuntime{Command: mockPodman, Rootless: true}
		if _, err := rt.Run(context.Background(), RunConfig{
			Harness:              &harness.Generic{},
			Name:                 "test-agent",
			UnixUsername:         "scion",
			Image:                "scion-agent:latest",
			Task:                 "hello",
			Env:                  []string{"SCION_METADATA_MODE=block"},
			MetadataInterception: true,
		}); err != nil {
			t.Fatalf("Run() error = %v", err)
		}

		raw, err := os.ReadFile(callLog)
		if err != nil {
			t.Fatalf("read calls.log: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) != 1 {
			t.Fatalf("rootless podman made %d invocations, want 1 (no sidecar):\n%s", len(lines), string(raw))
		}
	})
}
