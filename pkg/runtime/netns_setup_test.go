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

	got := buildNetnsSetupArgs("agent-cid-1234567890", cfg)
	want := []string{
		"run", "--rm",
		"--name", "scion-netns-setup-agent-cid-12",
		"--network", "container:agent-cid-1234567890",
		"--cap-drop", "ALL",
		"--cap-add", "NET_ADMIN",
		"--cap-add", "NET_RAW",
		"--security-opt", "no-new-privileges",
		"--user", "0",
		"--entrypoint", "/usr/local/bin/sciontool",
		"-e", "SCION_METADATA_MODE=block",
		"-e", "SCION_METADATA_PORT=18380",
		"-e", "SCION_INIT_FIREWALL=true",
		"--label", "scion.netns-setup=agent-cid-1234567890",
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
		"--name", "scion-netns-setup-cid-456",
		"--network", "container:cid-456",
		"--cap-drop", "ALL",
		"--cap-add", "NET_ADMIN",
		"--cap-add", "NET_RAW",
		"--security-opt", "no-new-privileges",
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
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  echo '{"scion.sciontool.capabilities":"netns-setup"}'
  exit 0
fi
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
	if len(lines) != 4 {
		t.Fatalf("got %d docker invocations, want 4:\n%s", len(lines), string(raw))
	}
	if !strings.HasPrefix(lines[0], "image inspect ") {
		t.Errorf("first invocation should inspect image capabilities, got: %s", lines[0])
	}
	if strings.Contains(lines[1], "--cap-add NET_ADMIN") {
		t.Errorf("agent run must not pass --cap-add NET_ADMIN: %s", lines[1])
	}
	if !strings.Contains(lines[1], "SCION_METADATA_INTERCEPTION=required") || !strings.Contains(lines[1], "SCION_METADATA_INTERCEPTION_SOURCE=runtime") {
		t.Errorf("agent run missing interception markers: %s", lines[1])
	}
	if lines[2] != "rm -f scion-netns-setup-deadbeef1234" {
		t.Errorf("third invocation should pre-clear setup container by name, got: %s", lines[2])
	}
	if !strings.Contains(lines[3], "--name scion-netns-setup-deadbeef1234") ||
		!strings.Contains(lines[3], "--network container:deadbeef1234") ||
		!strings.Contains(lines[3], "--cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW --security-opt no-new-privileges") ||
		!strings.Contains(lines[3], "netns-setup") {
		t.Errorf("sidecar invocation unexpected: %s", lines[3])
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
	wantRM := []string{
		"rm -f scion-netns-setup-deadbeef5678",
		"rm -f scion-netns-setup-deadbeef5678",
		"rm -f deadbeef5678",
	}
	if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !reflect.DeepEqual(got, wantRM) {
		t.Errorf("cleanup invocations = %q, want %q", got, wantRM)
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
	wantRM := []string{
		"rm -f scion-netns-setup-deadbeef9999",
		"rm -f scion-netns-setup-deadbeef9999",
		"rm -f deadbeef9999",
	}
	if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !reflect.DeepEqual(got, wantRM) {
		t.Errorf("cleanup invocations = %q, want %q", got, wantRM)
	}
}

func TestPodmanRuntime_Run_NetnsSetupRootfulAndRootless(t *testing.T) {
	t.Run("rootful runs sidecar", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockPodman := filepath.Join(tmpDir, "mock-podman")
		callLog := filepath.Join(tmpDir, "calls.log")

		script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  echo '{"scion.sciontool.capabilities":"netns-setup"}'
  exit 0
fi
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
		if len(lines) != 4 {
			t.Fatalf("got %d podman invocations, want 4:\n%s", len(lines), string(raw))
		}
		if !strings.HasPrefix(lines[0], "image inspect ") {
			t.Errorf("first invocation should inspect image capabilities, got: %s", lines[0])
		}
		wantPreRM := "rm -f " + netnsSetupContainerName(mockPodmanID)
		if lines[2] != wantPreRM {
			t.Errorf("third invocation = %q, want %q", lines[2], wantPreRM)
		}
		if !strings.Contains(lines[3], "--network container:"+mockPodmanID) || !strings.Contains(lines[3], "SCION_INIT_FIREWALL=true") || !strings.Contains(lines[3], "netns-setup") {
			t.Errorf("sidecar invocation unexpected: %s", lines[3])
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
		wantRM := []string{
			"rm -f " + netnsSetupContainerName(mockPodmanID),
			"rm -f " + netnsSetupContainerName(mockPodmanID),
			"rm -f " + mockPodmanID,
		}
		if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !reflect.DeepEqual(got, wantRM) {
			t.Errorf("cleanup invocations = %q, want %q", got, wantRM)
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

func TestImageHasCapability(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"nil labels", nil, false},
		{"empty labels", map[string]string{}, false},
		{"unrelated label", map[string]string{"org.opencontainers.image.revision": "128ce1ed"}, false},
		{"exact capability", map[string]string{ImageSciontoolCapabilitiesLabel: SciontoolCapNetnsSetup}, true},
		{"comma separated capabilities", map[string]string{ImageSciontoolCapabilitiesLabel: "foo, netns-setup ,bar"}, true},
		{"substring mismatch", map[string]string{ImageSciontoolCapabilitiesLabel: "netns-setup-v2"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imageHasCapability(tt.labels, SciontoolCapNetnsSetup); got != tt.want {
				t.Errorf("imageHasCapability(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestDockerRuntime_Run_UnlabeledImageWithNetnsSetupSucceeds(t *testing.T) {
	for _, inspectOut := range []string{
		`{"org.opencontainers.image.revision":"fd6981b9"}`,
		`null`,
		`{}`,
	} {
		t.Run("labels="+inspectOut, func(t *testing.T) {
			tmpDir := t.TempDir()
			mockDocker := filepath.Join(tmpDir, "mock-docker")
			callLog := filepath.Join(tmpDir, "calls.log")

			script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  echo '%s'
  exit 0
fi
if [ "$1" = "run" ]; then
  case "$*" in
    *"netns-setup"*)
      exit 0
      ;;
    *)
      echo "deadbeef2222"
      exit 0
      ;;
  esac
fi
`, callLog, inspectOut)
			if err := os.WriteFile(mockDocker, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			rt := &DockerRuntime{Command: mockDocker}
			id, err := rt.Run(context.Background(), RunConfig{
				Harness:              &harness.Generic{},
				Name:                 "test-agent",
				UnixUsername:         "scion",
				Image:                "scion-agent:post5246-unlabeled",
				Task:                 "hello",
				Env:                  []string{"SCION_METADATA_MODE=block"},
				MetadataInterception: true,
			})
			if err != nil {
				t.Fatalf("Run() error = %v, want nil for unlabeled image with working netns-setup", err)
			}
			if id != "deadbeef2222" {
				t.Fatalf("Run() id = %q, want %q", id, "deadbeef2222")
			}

			raw, err := os.ReadFile(callLog)
			if err != nil {
				t.Fatalf("read calls.log: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) != 4 {
				t.Fatalf("got %d invocations, want 4 (inspect, run, pre-clear rm, sidecar):\n%s", len(lines), string(raw))
			}
		})
	}
}

func TestPodmanRuntime_Run_RootfulUnlabeledImageWithNetnsSetupSucceeds(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")
	callLog := filepath.Join(tmpDir, "calls.log")

	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  echo '{"org.opencontainers.image.revision":"fd6981b9"}'
  exit 0
fi
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
		Image:                "scion-agent:post5246-unlabeled",
		Task:                 "hello",
		Env:                  []string{"SCION_METADATA_MODE=block"},
		MetadataInterception: true,
	})
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for unlabeled image with working netns-setup", err)
	}
	if id != mockPodmanID {
		t.Fatalf("Run() id = %q, want %q", id, mockPodmanID)
	}
}

func TestDockerRuntime_Run_OldImageClassifiesNetnsSetupSkew(t *testing.T) {
	for _, tc := range []struct {
		name          string
		inspectScript string
	}{
		{
			name:          "cached unlabeled old image",
			inspectScript: `echo '{"org.opencontainers.image.revision":"128ce1ed"}'; exit 0`,
		},
		{
			name:          "uncached old image",
			inspectScript: `echo "Error: No such image: scion-agent:old" >&2; exit 1`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			mockDocker := filepath.Join(tmpDir, "mock-docker")
			rmLog := filepath.Join(tmpDir, "rm.log")

			script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  %s
fi
if [ "$1" = "rm" ]; then
  echo "$@" >> %q
  exit 0
fi
if [ "$1" = "run" ]; then
  case "$*" in
    *"netns-setup"*)
      echo 'Error: unknown command "netns-setup" for "sciontool"' >&2
      exit 1
      ;;
    *)
      echo "deadbeef0001"
      exit 0
      ;;
  esac
fi
`, tc.inspectScript, rmLog)
			if err := os.WriteFile(mockDocker, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}

			rt := &DockerRuntime{Command: mockDocker}
			_, err := rt.Run(context.Background(), RunConfig{
				Harness:              &harness.Generic{},
				Name:                 "test-agent",
				UnixUsername:         "scion",
				Image:                "scion-agent:old",
				Task:                 "hello",
				Env:                  []string{"SCION_METADATA_MODE=block"},
				MetadataInterception: true,
			})
			if !errors.Is(err, ErrImageLacksNetnsSetup) {
				t.Fatalf("Run() error = %v, want ErrImageLacksNetnsSetup", err)
			}
			var lf *LaunchFailure
			if !errors.As(err, &lf) {
				t.Fatalf("Run() error = %v, want wrapped *LaunchFailure", err)
			}
			if lf.Code != api.LaunchCodeImageLacksNetnsSetup || lf.Step != api.LaunchStepContainerStart {
				t.Fatalf("LaunchFailure = (%q, %q), want (%q, %q)", lf.Code, lf.Step, api.LaunchCodeImageLacksNetnsSetup, api.LaunchStepContainerStart)
			}

			raw, readErr := os.ReadFile(rmLog)
			if readErr != nil {
				t.Fatalf("expected docker rm -f after sidecar skew failure: %v", readErr)
			}
			wantRM := []string{
				"rm -f scion-netns-setup-deadbeef0001",
				"rm -f scion-netns-setup-deadbeef0001",
				"rm -f deadbeef0001",
			}
			if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !reflect.DeepEqual(got, wantRM) {
				t.Errorf("cleanup invocations = %q, want %q", got, wantRM)
			}
		})
	}
}

func TestPodmanRuntime_Run_RootfulOldImageClassifiesNetnsSetupSkew(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")
	rmLog := filepath.Join(tmpDir, "rm.log")

	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  echo "Error: image not known" >&2
  exit 1
fi
if [ "$1" = "rm" ]; then
  echo "$@" >> %q
  exit 0
fi
if [ "$1" = "run" ]; then
  case "$*" in
    *"netns-setup"*)
      echo 'Error: unknown command "netns-setup" for "sciontool"' >&2
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
	_, err := rt.Run(context.Background(), RunConfig{
		Harness:              &harness.Generic{},
		Name:                 "test-agent",
		UnixUsername:         "scion",
		Image:                "scion-agent:old",
		Task:                 "hello",
		Env:                  []string{"SCION_METADATA_MODE=block"},
		MetadataInterception: true,
	})
	if !errors.Is(err, ErrImageLacksNetnsSetup) {
		t.Fatalf("Run() error = %v, want ErrImageLacksNetnsSetup", err)
	}
	var lf *LaunchFailure
	if !errors.As(err, &lf) {
		t.Fatalf("Run() error = %v, want wrapped *LaunchFailure", err)
	}
	if lf.Code != api.LaunchCodeImageLacksNetnsSetup || lf.Step != api.LaunchStepContainerStart {
		t.Fatalf("LaunchFailure = (%q, %q), want (%q, %q)", lf.Code, lf.Step, api.LaunchCodeImageLacksNetnsSetup, api.LaunchStepContainerStart)
	}

	raw, readErr := os.ReadFile(rmLog)
	if readErr != nil {
		t.Fatalf("expected podman rm -f after sidecar skew failure: %v", readErr)
	}
	wantRM := []string{
		"rm -f " + netnsSetupContainerName(mockPodmanID),
		"rm -f " + netnsSetupContainerName(mockPodmanID),
		"rm -f " + mockPodmanID,
	}
	if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !reflect.DeepEqual(got, wantRM) {
		t.Errorf("cleanup invocations = %q, want %q", got, wantRM)
	}
}

func TestEffectiveInitFirewall(t *testing.T) {
	t.Run("unset everywhere", func(t *testing.T) {
		t.Setenv("SCION_INIT_FIREWALL", "")
		if err := os.Unsetenv("SCION_INIT_FIREWALL"); err != nil {
			t.Fatal(err)
		}
		v, ok := effectiveInitFirewall(RunConfig{
			Env: []string{"OTHER=1"},
		})
		if ok || v != "" {
			t.Fatalf("effectiveInitFirewall() = (%q, %v), want (\"\", false)", v, ok)
		}
	})

	t.Run("cfg.Env K=V", func(t *testing.T) {
		v, ok := effectiveInitFirewall(RunConfig{
			Env: []string{"SCION_INIT_FIREWALL=true"},
		})
		if !ok || v != "true" {
			t.Fatalf("effectiveInitFirewall() = (%q, %v), want (\"true\", true)", v, ok)
		}
	})

	t.Run("later cfg.Env entry wins", func(t *testing.T) {
		v, ok := effectiveInitFirewall(RunConfig{
			Env: []string{"SCION_INIT_FIREWALL=true", "SCION_INIT_FIREWALL=false"},
		})
		if !ok || v != "false" {
			t.Fatalf("effectiveInitFirewall() = (%q, %v), want (\"false\", true)", v, ok)
		}
	})

	t.Run("bare K inherits broker env", func(t *testing.T) {
		t.Setenv("SCION_INIT_FIREWALL", "true")
		v, ok := effectiveInitFirewall(RunConfig{
			Env: []string{"SCION_INIT_FIREWALL"},
		})
		if !ok || v != "true" {
			t.Fatalf("effectiveInitFirewall() = (%q, %v), want (\"true\", true)", v, ok)
		}
	})

	t.Run("bare K after K=V overrides when set in broker env", func(t *testing.T) {
		t.Setenv("SCION_INIT_FIREWALL", "true")
		v, ok := effectiveInitFirewall(RunConfig{
			Env: []string{"SCION_INIT_FIREWALL=false", "SCION_INIT_FIREWALL"},
		})
		if !ok || v != "true" {
			t.Fatalf("effectiveInitFirewall() = (%q, %v), want (\"true\", true)", v, ok)
		}
	})

	t.Run("unset bare K does not clobber earlier K=V", func(t *testing.T) {
		t.Setenv("SCION_INIT_FIREWALL", "")
		if err := os.Unsetenv("SCION_INIT_FIREWALL"); err != nil {
			t.Fatal(err)
		}
		v, ok := effectiveInitFirewall(RunConfig{
			Env: []string{"SCION_INIT_FIREWALL=true", "SCION_INIT_FIREWALL"},
		})
		if !ok || v != "true" {
			t.Fatalf("effectiveInitFirewall() = (%q, %v), want (\"true\", true)", v, ok)
		}
	})

	t.Run("env-type ResolvedSecret", func(t *testing.T) {
		v, ok := effectiveInitFirewall(RunConfig{
			ResolvedSecrets: []api.ResolvedSecret{
				{Name: "fw-file", Type: "file", Target: "SCION_INIT_FIREWALL", Value: "false"},
				{Name: "fw-env", Type: "environment", Target: "SCION_INIT_FIREWALL", Value: "true"},
			},
		})
		if !ok || v != "true" {
			t.Fatalf("effectiveInitFirewall() = (%q, %v), want (\"true\", true)", v, ok)
		}
	})

	t.Run("empty Type ResolvedSecret treated as environment and later secret wins", func(t *testing.T) {
		v, ok := effectiveInitFirewall(RunConfig{
			ResolvedSecrets: []api.ResolvedSecret{
				{Name: "fw1", Type: "environment", Target: "SCION_INIT_FIREWALL", Value: "true"},
				{Name: "fw2", Type: "", Target: "SCION_INIT_FIREWALL", Value: "false"},
			},
		})
		if !ok || v != "false" {
			t.Fatalf("effectiveInitFirewall() = (%q, %v), want (\"false\", true)", v, ok)
		}
	})

	t.Run("cfg.Env blocks ResolvedSecret even when cfg.Env sets empty or false", func(t *testing.T) {
		for _, envEntry := range []string{"SCION_INIT_FIREWALL=false", "SCION_INIT_FIREWALL="} {
			v, ok := effectiveInitFirewall(RunConfig{
				Env: []string{envEntry},
				ResolvedSecrets: []api.ResolvedSecret{
					{Name: "fw", Type: "environment", Target: "SCION_INIT_FIREWALL", Value: "true"},
				},
			})
			_, wantV, _ := strings.Cut(envEntry, "=")
			if !ok || v != wantV {
				t.Errorf("env %q + secret: got (%q, %v), want (%q, true)", envEntry, v, ok, wantV)
			}
		}
	})

	t.Run("bare cfg.Env blocks ResolvedSecret even when unset in broker env", func(t *testing.T) {
		t.Setenv("SCION_INIT_FIREWALL", "")
		if err := os.Unsetenv("SCION_INIT_FIREWALL"); err != nil {
			t.Fatal(err)
		}
		v, ok := effectiveInitFirewall(RunConfig{
			Env: []string{"SCION_INIT_FIREWALL"},
			ResolvedSecrets: []api.ResolvedSecret{
				{Name: "fw", Type: "environment", Target: "SCION_INIT_FIREWALL", Value: "true"},
			},
		})
		if ok || v != "" {
			t.Fatalf("got (%q, %v), want (\"\", false)", v, ok)
		}
	})

	t.Run("ResolvedAuth.EnvVars only", func(t *testing.T) {
		v, ok := effectiveInitFirewall(RunConfig{
			Harness: &harness.Generic{},
			ResolvedAuth: &api.ResolvedAuth{
				EnvVars: map[string]string{"SCION_INIT_FIREWALL": "true"},
			},
		})
		if !ok || v != "true" {
			t.Fatalf("got (%q, %v), want (\"true\", true)", v, ok)
		}
	})

	t.Run("ResolvedAuth.EnvVars ignored when Harness is nil or value is empty", func(t *testing.T) {
		if v, ok := effectiveInitFirewall(RunConfig{
			ResolvedAuth: &api.ResolvedAuth{
				EnvVars: map[string]string{"SCION_INIT_FIREWALL": "true"},
			},
		}); ok || v != "" {
			t.Fatalf("nil Harness: got (%q, %v), want (\"\", false)", v, ok)
		}
		if v, ok := effectiveInitFirewall(RunConfig{
			Harness: &harness.Generic{},
			ResolvedAuth: &api.ResolvedAuth{
				EnvVars: map[string]string{"SCION_INIT_FIREWALL": ""},
			},
		}); ok || v != "" {
			t.Fatalf("empty auth value: got (%q, %v), want (\"\", false)", v, ok)
		}
	})

	t.Run("ResolvedAuth.EnvVars overridden by cfg.Env", func(t *testing.T) {
		v, ok := effectiveInitFirewall(RunConfig{
			Harness: &harness.Generic{},
			ResolvedAuth: &api.ResolvedAuth{
				EnvVars: map[string]string{"SCION_INIT_FIREWALL": "true"},
			},
			Env: []string{"SCION_INIT_FIREWALL=false"},
		})
		if !ok || v != "false" {
			t.Fatalf("got (%q, %v), want (\"false\", true)", v, ok)
		}
	})

	t.Run("ResolvedAuth.EnvVars overridden by ResolvedSecret", func(t *testing.T) {
		v, ok := effectiveInitFirewall(RunConfig{
			Harness: &harness.Generic{},
			ResolvedAuth: &api.ResolvedAuth{
				EnvVars: map[string]string{"SCION_INIT_FIREWALL": "true"},
			},
			ResolvedSecrets: []api.ResolvedSecret{
				{Name: "fw", Type: "environment", Target: "SCION_INIT_FIREWALL", Value: "false"},
			},
		})
		if !ok || v != "false" {
			t.Fatalf("got (%q, %v), want (\"false\", true)", v, ok)
		}
	})
}

func TestBuildNetnsSetupArgs_InitFirewallSources(t *testing.T) {
	t.Run("bare env from broker", func(t *testing.T) {
		t.Setenv("SCION_INIT_FIREWALL", "true")
		got := buildNetnsSetupArgs("abcdef0123456789", RunConfig{
			Image: "scion-agent:test",
			Env:   []string{"SCION_METADATA_MODE=block", "SCION_INIT_FIREWALL"},
		})
		if !strings.Contains(strings.Join(got, " "), "-e SCION_INIT_FIREWALL=true") {
			t.Fatalf("buildNetnsSetupArgs() missing forwarded bare SCION_INIT_FIREWALL=true: %v", got)
		}
	})

	t.Run("env-type ResolvedSecret", func(t *testing.T) {
		got := buildNetnsSetupArgs("abcdef0123456789", RunConfig{
			Image: "scion-agent:test",
			Env:   []string{"SCION_METADATA_MODE=block"},
			ResolvedSecrets: []api.ResolvedSecret{
				{Name: "fw", Type: "environment", Target: "SCION_INIT_FIREWALL", Value: "true"},
			},
		})
		if !strings.Contains(strings.Join(got, " "), "-e SCION_INIT_FIREWALL=true") {
			t.Fatalf("buildNetnsSetupArgs() missing forwarded secret SCION_INIT_FIREWALL=true: %v", got)
		}
	})

	t.Run("normalizes secret and non-boolean values without leaking raw value", func(t *testing.T) {
		cases := []struct {
			name      string
			raw       string
			wantFlag  string
			wantOmit  bool
			forbidRaw bool
		}{
			{name: "truthy 1", raw: " 1 ", wantFlag: "SCION_INIT_FIREWALL=true", forbidRaw: true},
			{name: "truthy TRUE", raw: "TRUE", wantFlag: "SCION_INIT_FIREWALL=true", forbidRaw: true},
			{name: "falsy 0", raw: "0", wantFlag: "SCION_INIT_FIREWALL=false", forbidRaw: true},
			{name: "falsy FALSE", raw: "FALSE", wantFlag: "SCION_INIT_FIREWALL=false", forbidRaw: true},
			{name: "invalid secret", raw: "secret-payload-do-not-leak", wantFlag: "SCION_INIT_FIREWALL=invalid", forbidRaw: true},
			{name: "empty", raw: "", wantOmit: true},
			{name: "whitespace", raw: "  \t ", wantOmit: true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := buildNetnsSetupArgs("abcdef0123456789", RunConfig{
					Image: "scion-agent:test",
					Env:   []string{"SCION_METADATA_MODE=block"},
					ResolvedSecrets: []api.ResolvedSecret{
						{Name: "fw", Type: "environment", Target: "SCION_INIT_FIREWALL", Value: tc.raw},
					},
				})
				joined := strings.Join(got, " ")
				if tc.wantOmit {
					if strings.Contains(joined, "SCION_INIT_FIREWALL") {
						t.Fatalf("expected SCION_INIT_FIREWALL to be omitted for %q, got %v", tc.raw, got)
					}
					return
				}
				if !strings.Contains(joined, "-e "+tc.wantFlag) {
					t.Fatalf("buildNetnsSetupArgs() missing %q for raw %q; got %v", tc.wantFlag, tc.raw, got)
				}
				if tc.forbidRaw && strings.Contains(joined, "SCION_INIT_FIREWALL="+tc.raw) {
					t.Fatalf("setup argv must not contain raw secret value %q; got %v", tc.raw, got)
				}
			})
		}
	})

	t.Run("buildCommonRunArgs and buildNetnsSetupArgs agree across auth, env, and secret sources", func(t *testing.T) {
		extractEffectiveEnv := func(args []string, key string) (string, bool) {
			var val string
			var set bool
			for i := 0; i < len(args); i++ {
				if args[i] != "-e" || i+1 >= len(args) {
					continue
				}
				i++
				k, v, hasEq := strings.Cut(args[i], "=")
				if k != key {
					continue
				}
				if hasEq {
					val = v
					set = true
				} else if ev, ok := os.LookupEnv(k); ok {
					val = ev
					set = true
				}
			}
			return val, set
		}

		cases := []struct {
			name     string
			cfg      RunConfig
			wantNorm string
			wantEmit bool
		}{
			{
				name: "auth only",
				cfg: RunConfig{
					ResolvedAuth: &api.ResolvedAuth{
						EnvVars: map[string]string{"SCION_INIT_FIREWALL": "1"},
					},
				},
				wantNorm: "true",
				wantEmit: true,
			},
			{
				name: "auth overridden by cfg.Env",
				cfg: RunConfig{
					ResolvedAuth: &api.ResolvedAuth{
						EnvVars: map[string]string{"SCION_INIT_FIREWALL": "true"},
					},
					Env: []string{"SCION_INIT_FIREWALL=0"},
				},
				wantNorm: "false",
				wantEmit: true,
			},
			{
				name: "auth overridden by ResolvedSecret",
				cfg: RunConfig{
					ResolvedAuth: &api.ResolvedAuth{
						EnvVars: map[string]string{"SCION_INIT_FIREWALL": "true"},
					},
					ResolvedSecrets: []api.ResolvedSecret{
						{Name: "fw", Type: "environment", Target: "SCION_INIT_FIREWALL", Value: "FALSE"},
					},
				},
				wantNorm: "false",
				wantEmit: true,
			},
			{
				name: "auth overridden by empty cfg.Env omits flag",
				cfg: RunConfig{
					ResolvedAuth: &api.ResolvedAuth{
						EnvVars: map[string]string{"SCION_INIT_FIREWALL": "true"},
					},
					Env: []string{"SCION_INIT_FIREWALL="},
				},
				wantEmit: false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				cfg := tc.cfg
				cfg.RuntimeName = "docker"
				cfg.Harness = &harness.Generic{}
				cfg.Name = "agent-parity"
				cfg.UnixUsername = "scion"
				cfg.Image = "scion-agent:test"
				cfg.Task = "hello"
				cfg.MetadataInterception = true
				cfg.Env = append([]string{"SCION_METADATA_MODE=block"}, cfg.Env...)

				agentArgs, err := buildCommonRunArgs(cfg)
				if err != nil {
					t.Fatalf("buildCommonRunArgs() error = %v", err)
				}
				agentRaw, agentSet := extractEffectiveEnv(agentArgs, "SCION_INIT_FIREWALL")
				if !agentSet {
					t.Fatalf("expected agent argv to set SCION_INIT_FIREWALL; got %v", agentArgs)
				}
				agentNorm, agentEmit := normalizeInitFirewallArg(agentRaw)

				setupArgs := buildNetnsSetupArgs("abcdef0123456789", cfg)
				setupVal, setupSet := extractEffectiveEnv(setupArgs, "SCION_INIT_FIREWALL")

				if agentEmit != tc.wantEmit || setupSet != tc.wantEmit {
					t.Fatalf("emit mismatch: agent=(%q, emit=%v) setup=(%q, set=%v) wantEmit=%v",
						agentNorm, agentEmit, setupVal, setupSet, tc.wantEmit)
				}
				if tc.wantEmit && (agentNorm != tc.wantNorm || setupVal != tc.wantNorm) {
					t.Fatalf("value mismatch: agent=%q setup=%q want=%q", agentNorm, setupVal, tc.wantNorm)
				}
			})
		}
	})
}

func TestRunNetnsSetupWith_LifecycleAndCleanup(t *testing.T) {
	type call struct {
		args   []string
		ctxErr error
	}

	const cid = "0123456789abcdef0123456789abcdef"
	const wantName = "scion-netns-setup-0123456789ab"
	cfg := RunConfig{
		Image: "scion-agent:test",
		Env:   []string{"SCION_METADATA_MODE=block"},
	}

	t.Run("pre-clears before run on success", func(t *testing.T) {
		var calls []call
		runner := func(ctx context.Context, _ string, args ...string) (string, error) {
			calls = append(calls, call{args: append([]string(nil), args...), ctxErr: ctx.Err()})
			if len(calls) == 1 {
				return "No such container", errors.New("exit status 1")
			}
			return "", nil
		}
		if err := runNetnsSetupWith(context.Background(), runner, "docker", cid, cfg); err != nil {
			t.Fatalf("runNetnsSetupWith() error = %v", err)
		}
		if len(calls) != 2 {
			t.Fatalf("got %d calls, want 2: %+v", len(calls), calls)
		}
		if !reflect.DeepEqual(calls[0].args, []string{"rm", "-f", wantName}) {
			t.Errorf("call[0] = %v, want [rm -f %s]", calls[0].args, wantName)
		}
		if calls[1].args[0] != "run" {
			t.Errorf("call[1] = %v, want run ...", calls[1].args)
		}
	})

	t.Run("removes setup container with live context on command failure", func(t *testing.T) {
		var calls []call
		runner := func(ctx context.Context, _ string, args ...string) (string, error) {
			calls = append(calls, call{args: append([]string(nil), args...), ctxErr: ctx.Err()})
			if args[0] == "run" {
				return "iptables failed", errors.New("exit status 1")
			}
			return "", nil
		}
		err := runNetnsSetupWith(context.Background(), runner, "docker", cid, cfg)
		if err == nil {
			t.Fatal("runNetnsSetupWith() = nil, want error")
		}
		if len(calls) != 3 {
			t.Fatalf("got %d calls, want 3 (pre-rm, run, cleanup-rm): %+v", len(calls), calls)
		}
		if !reflect.DeepEqual(calls[2].args, []string{"rm", "-f", wantName}) {
			t.Errorf("cleanup call = %v, want [rm -f %s]", calls[2].args, wantName)
		}
		if calls[2].ctxErr != nil {
			t.Errorf("cleanup context was not live: %v", calls[2].ctxErr)
		}
	})

	t.Run("removes setup container with live context when parent is cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var calls []call
		runner := func(c context.Context, _ string, args ...string) (string, error) {
			calls = append(calls, call{args: append([]string(nil), args...), ctxErr: c.Err()})
			if args[0] == "run" {
				cancel()
				return "", context.Canceled
			}
			return "", nil
		}
		err := runNetnsSetupWith(ctx, runner, "docker", cid, cfg)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runNetnsSetupWith() error = %v, want context.Canceled", err)
		}
		if len(calls) != 3 {
			t.Fatalf("got %d calls, want 3: %+v", len(calls), calls)
		}
		if !reflect.DeepEqual(calls[2].args, []string{"rm", "-f", wantName}) {
			t.Errorf("cleanup call = %v, want [rm -f %s]", calls[2].args, wantName)
		}
		if calls[2].ctxErr != nil {
			t.Errorf("cleanup context must be live even when parent is cancelled, got %v", calls[2].ctxErr)
		}
	})

	t.Run("removes setup container with live context on timeout", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		var calls []call
		runner := func(c context.Context, _ string, args ...string) (string, error) {
			calls = append(calls, call{args: append([]string(nil), args...), ctxErr: c.Err()})
			if args[0] == "run" {
				return "", context.DeadlineExceeded
			}
			return "", nil
		}
		err := runNetnsSetupWith(ctx, runner, "docker", cid, cfg)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("runNetnsSetupWith() error = %v, want context.DeadlineExceeded", err)
		}
		if len(calls) != 3 {
			t.Fatalf("got %d calls, want 3: %+v", len(calls), calls)
		}
		if !reflect.DeepEqual(calls[2].args, []string{"rm", "-f", wantName}) {
			t.Errorf("cleanup call = %v, want [rm -f %s]", calls[2].args, wantName)
		}
		if calls[2].ctxErr != nil {
			t.Errorf("cleanup context must be live even after parent deadline expired, got %v", calls[2].ctxErr)
		}
	})
}

func TestRemoveNetnsSetupContainersAndDeletePaths(t *testing.T) {
	t.Run("removes containers matching label and ignores invalid lines", func(t *testing.T) {
		var calls [][]string
		runner := func(ctx context.Context, _ string, args ...string) (string, error) {
			if ctx.Err() != nil {
				t.Errorf("cleanup ctx should be live, got %v", ctx.Err())
			}
			calls = append(calls, append([]string(nil), args...))
			if args[0] == "ps" {
				return "aabbccddeeff\nnot-a-valid-id\n0123456789abcdef\n", nil
			}
			return "", nil
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		removeNetnsSetupContainersWith(cancelled, runner, "docker", "agent-cid-1")
		if len(calls) != 2 {
			t.Fatalf("got %d calls, want 2: %v", len(calls), calls)
		}
		wantPS := []string{"ps", "-aq", "--no-trunc", "--filter", "label=scion.netns-setup=agent-cid-1"}
		if !reflect.DeepEqual(calls[0], wantPS) {
			t.Errorf("ps call = %v, want %v", calls[0], wantPS)
		}
		wantRM := []string{"rm", "-f", "aabbccddeeff", "0123456789abcdef"}
		if !reflect.DeepEqual(calls[1], wantRM) {
			t.Errorf("rm call = %v, want %v", calls[1], wantRM)
		}
	})

	t.Run("idempotent when none found or ps fails", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			out  string
			err  error
		}{
			{"empty", "", nil},
			{"ps error", "daemon error", errors.New("exit status 1")},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var calls [][]string
				runner := func(_ context.Context, _ string, args ...string) (string, error) {
					calls = append(calls, append([]string(nil), args...))
					return tc.out, tc.err
				}
				removeNetnsSetupContainersWith(context.Background(), runner, "docker", "agent-cid-2")
				if len(calls) != 1 {
					t.Fatalf("got %d calls, want 1 (ps only): %v", len(calls), calls)
				}
			})
		}
	})

	t.Run("DockerRuntime.Delete cleans labelled setup containers", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockDocker := filepath.Join(tmpDir, "mock-docker")
		callLog := filepath.Join(tmpDir, "calls.log")
		script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "ps" ]; then
  echo "fedcba987654"
  exit 0
fi
exit 0
`, callLog)
		if err := os.WriteFile(mockDocker, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		rt := &DockerRuntime{Command: mockDocker}
		if err := rt.Delete(context.Background(), RunRef{ID: "deadbeef1234"}); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		raw, err := os.ReadFile(callLog)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"ps -aq --no-trunc --filter label=scion.netns-setup=deadbeef1234",
			"rm -f fedcba987654",
			"rm -f deadbeef1234",
		}
		if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !reflect.DeepEqual(got, want) {
			t.Errorf("Delete() calls = %q, want %q", got, want)
		}
	})

	t.Run("PodmanRuntime.Delete cleans labelled setup containers", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockPodman := filepath.Join(tmpDir, "mock-podman")
		callLog := filepath.Join(tmpDir, "calls.log")
		script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "ps" ]; then
  echo "112233445566"
  exit 0
fi
exit 0
`, callLog)
		if err := os.WriteFile(mockPodman, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		rt := &PodmanRuntime{Command: mockPodman}
		if err := rt.Delete(context.Background(), RunRef{ID: mockPodmanID}); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		raw, err := os.ReadFile(callLog)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"ps -aq --no-trunc --filter label=scion.netns-setup=" + mockPodmanID,
			"rm -f 112233445566",
			"rm -f " + mockPodmanID,
		}
		if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !reflect.DeepEqual(got, want) {
			t.Errorf("Delete() calls = %q, want %q", got, want)
		}
	})

	t.Run("DockerRuntime.DeleteResource cleans labelled setup containers", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockDocker := filepath.Join(tmpDir, "mock-docker")
		callLog := filepath.Join(tmpDir, "calls.log")
		script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "ps" ]; then
  echo "aabbccddeeff"
  exit 0
fi
exit 0
`, callLog)
		if err := os.WriteFile(mockDocker, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		rt := &DockerRuntime{Command: mockDocker}
		err := rt.DeleteResource(context.Background(), api.ResourceHandle{
			Kind: api.ResourceKindContainer,
			Name: "test-agent",
			UID:  "deadbeef9876",
		})
		if err != nil {
			t.Fatalf("DeleteResource() error = %v", err)
		}
		raw, err := os.ReadFile(callLog)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"ps -aq --no-trunc --filter label=scion.netns-setup=deadbeef9876",
			"rm -f aabbccddeeff",
			"rm -f deadbeef9876",
		}
		if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !reflect.DeepEqual(got, want) {
			t.Errorf("DeleteResource() calls = %q, want %q", got, want)
		}
	})

	t.Run("Run sweeps labelled setup containers on netns-setup failure", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			agentID string
			run     func(cli string, cfg RunConfig) (string, error)
		}{
			{
				name:    "docker",
				agentID: "deadbeef7777",
				run: func(cli string, cfg RunConfig) (string, error) {
					return (&DockerRuntime{Command: cli}).Run(context.Background(), cfg)
				},
			},
			{
				name:    "podman rootful",
				agentID: mockPodmanID,
				run: func(cli string, cfg RunConfig) (string, error) {
					return (&PodmanRuntime{Command: cli, Rootless: false}).Run(context.Background(), cfg)
				},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				tmpDir := t.TempDir()
				mockCLI := filepath.Join(tmpDir, "mock-cli")
				cleanupLog := filepath.Join(tmpDir, "cleanup.log")
				script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "ps" ]; then
  echo "$@" >> %q
  echo "cafe01234567"
  exit 0
fi
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
exit 0
`, cleanupLog, cleanupLog, tc.agentID)
				if err := os.WriteFile(mockCLI, []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				_, err := tc.run(mockCLI, RunConfig{
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
				raw, readErr := os.ReadFile(cleanupLog)
				if readErr != nil {
					t.Fatalf("read cleanup.log: %v", readErr)
				}
				setupName := netnsSetupContainerName(tc.agentID)
				want := []string{
					"rm -f " + setupName,
					"rm -f " + setupName,
					"ps -aq --no-trunc --filter label=scion.netns-setup=" + tc.agentID,
					"rm -f cafe01234567",
					"rm -f " + tc.agentID,
				}
				if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !reflect.DeepEqual(got, want) {
					t.Errorf("cleanup calls = %q, want %q", got, want)
				}
			})
		}
	})
}

func TestDockerAndPodmanRun_FirewallWithoutMetadataInterceptionRefused(t *testing.T) {
	makeFailCLI := func(t *testing.T) string {
		t.Helper()
		bin := filepath.Join(t.TempDir(), "must-not-run")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'CLI must not be invoked' >&2\nexit 99\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return bin
	}

	sources := []struct {
		name  string
		setup func(t *testing.T, cfg *RunConfig)
	}{
		{
			name: "cfg.Env K=V",
			setup: func(_ *testing.T, cfg *RunConfig) {
				cfg.Env = append(cfg.Env, "SCION_INIT_FIREWALL=true")
			},
		},
		{
			name: "bare cfg.Env from broker",
			setup: func(t *testing.T, cfg *RunConfig) {
				t.Setenv("SCION_INIT_FIREWALL", "1")
				cfg.Env = append(cfg.Env, "SCION_INIT_FIREWALL")
			},
		},
		{
			name: "env-type ResolvedSecret",
			setup: func(_ *testing.T, cfg *RunConfig) {
				cfg.ResolvedSecrets = append(cfg.ResolvedSecrets, api.ResolvedSecret{
					Name:   "fw",
					Type:   "environment",
					Target: "SCION_INIT_FIREWALL",
					Value:  "true",
				})
			},
		},
	}

	runtimes := []struct {
		name string
		run  func(cli string, cfg RunConfig) (string, error)
		// forceInterception sets cfg.MetadataInterception=true to verify
		// rootless Podman still refuses because it turns interception off.
		forceInterception bool
	}{
		{
			name: "docker passthrough",
			run: func(cli string, cfg RunConfig) (string, error) {
				return (&DockerRuntime{Command: cli}).Run(context.Background(), cfg)
			},
		},
		{
			name: "podman rootful passthrough",
			run: func(cli string, cfg RunConfig) (string, error) {
				return (&PodmanRuntime{Command: cli, Rootless: false}).Run(context.Background(), cfg)
			},
		},
		{
			name:              "podman rootless",
			forceInterception: true,
			run: func(cli string, cfg RunConfig) (string, error) {
				return (&PodmanRuntime{Command: cli, Rootless: true}).Run(context.Background(), cfg)
			},
		},
	}

	for _, rt := range runtimes {
		for _, src := range sources {
			t.Run(rt.name+"/"+src.name, func(t *testing.T) {
				cli := makeFailCLI(t)
				cfg := RunConfig{
					Harness:              &harness.Generic{},
					Name:                 "test-agent",
					UnixUsername:         "scion",
					Image:                "scion-agent:latest",
					Task:                 "hello",
					Env:                  []string{"SCION_METADATA_MODE=passthrough"},
					MetadataInterception: rt.forceInterception,
				}
				src.setup(t, &cfg)

				_, err := rt.run(cli, cfg)
				if !errors.Is(err, ErrFirewallRequiresMetadataInterception) {
					t.Fatalf("Run() error = %v, want ErrFirewallRequiresMetadataInterception", err)
				}
				var lf *LaunchFailure
				if !errors.As(err, &lf) {
					t.Fatalf("Run() error = %v, want *LaunchFailure", err)
				}
				if lf.Code != api.LaunchCodeFirewallRequiresMetadataInterception || lf.Step != api.LaunchStepLaunching {
					t.Fatalf("LaunchFailure = (%q, %q), want (%q, %q)",
						lf.Code, lf.Step, api.LaunchCodeFirewallRequiresMetadataInterception, api.LaunchStepLaunching)
				}
			})
		}
	}

	t.Run("false value is allowed without interception", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockDocker := filepath.Join(tmpDir, "mock-docker")
		if err := os.WriteFile(mockDocker, []byte("#!/bin/sh\necho deadbeef4444\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		rt := &DockerRuntime{Command: mockDocker}
		id, err := rt.Run(context.Background(), RunConfig{
			Harness:              &harness.Generic{},
			Name:                 "test-agent",
			UnixUsername:         "scion",
			Image:                "scion-agent:latest",
			Task:                 "hello",
			Env:                  []string{"SCION_METADATA_MODE=passthrough", "SCION_INIT_FIREWALL=false"},
			MetadataInterception: false,
		})
		if err != nil {
			t.Fatalf("Run() error = %v, want nil when SCION_INIT_FIREWALL=false", err)
		}
		if id != "deadbeef4444" {
			t.Fatalf("Run() id = %q, want deadbeef4444", id)
		}
	})
}
