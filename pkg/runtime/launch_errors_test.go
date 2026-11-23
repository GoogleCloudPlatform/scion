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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestPodmanRuntime_PullImage_ClassifiesLaunchFailures(t *testing.T) {
	cases := []struct {
		name     string
		image    string
		stderr   string
		wantCode string
	}{
		{
			name:     "missing tag",
			image:    "example.com/scion/agent:no-such-tag",
			stderr:   "Trying to pull example.com/scion/agent:no-such-tag...\nError: initializing source docker://example.com/scion/agent:no-such-tag: reading manifest no-such-tag in example.com/scion/agent: manifest unknown",
			wantCode: api.LaunchCodeImagePullFailed,
		},
		{
			name:     "unreachable registry",
			image:    "registry.invalid:5000/scion/agent:v1",
			stderr:   `Error: initializing source docker://registry.invalid:5000/scion/agent:v1: pinging container registry registry.invalid:5000: Get "https://registry.invalid:5000/v2/": dial tcp: lookup registry.invalid: no such host`,
			wantCode: api.LaunchCodeImagePullFailed,
		},
		{
			name:     "pull access denied",
			image:    "example.com/private/agent:v1",
			stderr:   "Error: initializing source docker://example.com/private/agent:v1: reading manifest v1 in example.com/private/agent: requested access to the resource is denied",
			wantCode: api.LaunchCodeImagePullFailed,
		},
		{
			name:     "short-name resolution enforced",
			image:    "scion-antigravity:latest",
			stderr:   `Error: short-name "scion-antigravity:latest" did not resolve to an alias and no unqualified-search registries are defined in "/etc/containers/registries.conf"`,
			wantCode: api.LaunchCodeImagePullFailed,
		},
		{
			name:     "malformed reference",
			image:    "INVALID::IMAGE",
			stderr:   `Error: parsing reference "INVALID::IMAGE": invalid reference format`,
			wantCode: api.LaunchCodeInvalidImage,
		},
		{
			name:     "uppercase repository reference",
			image:    "Example.com/UpperCase/Agent:v1",
			stderr:   "Error: invalid reference format: repository name must be lowercase",
			wantCode: api.LaunchCodeInvalidImage,
		},
		{
			name:     "whitespace in image reference",
			image:    "example.com/bad image:v1",
			stderr:   "Error: invalid argument",
			wantCode: api.LaunchCodeInvalidImage,
		},
		{
			name:     "podman machine stopped",
			image:    "example.com/scion/agent:v1",
			stderr:   "Cannot connect to Podman. Please verify your connection to the Linux system using `podman system connection list`, or try `podman machine init` and `podman machine start` to manage a new Linux VM",
			wantCode: api.LaunchCodeRuntimeUnavailable,
		},
		{
			name:     "podman unix socket connection refused",
			image:    "example.com/scion/agent:v1",
			stderr:   `Error: unable to connect to Podman socket: Get "http://d/v4.9.0/libpod/_ping": dial unix /run/user/1000/podman/podman.sock: connect: connection refused`,
			wantCode: api.LaunchCodeRuntimeUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			mockPodman := filepath.Join(tmpDir, "mock-podman")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s >&2\nexit 125\n", shellQuote(tc.stderr))
			if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
				t.Fatalf("failed to write mock podman: %v", err)
			}

			var steps []string
			ctx := WithLaunchProgress(context.Background(), func(step, message string) {
				steps = append(steps, step+": "+message)
			})

			rt := &PodmanRuntime{Command: mockPodman}
			err := rt.PullImage(ctx, tc.image)
			if err == nil {
				t.Fatal("expected PullImage to fail, got nil")
			}

			wantProgress := api.LaunchStepImagePull + ": pulling the image"
			if len(steps) != 1 || steps[0] != wantProgress {
				t.Errorf("progress steps = %v, want [%q]", steps, wantProgress)
			}

			var lf *LaunchFailure
			if !errors.As(err, &lf) {
				t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
			}
			if lf.Code != tc.wantCode {
				t.Errorf("LaunchFailure.Code = %q, want %q", lf.Code, tc.wantCode)
			}
			if lf.Step != api.LaunchStepImagePull {
				t.Errorf("LaunchFailure.Step = %q, want %q", lf.Step, api.LaunchStepImagePull)
			}
			if lf.Reason != "" {
				t.Errorf("LaunchFailure.Reason = %q, want empty", lf.Reason)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("pull %q:", tc.image)) {
				t.Errorf("error text %q lost pull prefix", err.Error())
			}
		})
	}

	t.Run("successful pull reports step", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockPodman := filepath.Join(tmpDir, "mock-podman")
		if err := os.WriteFile(mockPodman, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatalf("failed to write mock podman: %v", err)
		}
		var steps []string
		ctx := WithLaunchProgress(context.Background(), func(step, message string) {
			steps = append(steps, step+": "+message)
		})
		rt := &PodmanRuntime{Command: mockPodman}
		if err := rt.PullImage(ctx, "example.com/scion/agent:v1"); err != nil {
			t.Fatalf("PullImage: %v", err)
		}
		wantProgress := api.LaunchStepImagePull + ": pulling the image"
		if len(steps) != 1 || steps[0] != wantProgress {
			t.Errorf("progress steps = %v, want [%q]", steps, wantProgress)
		}
	})

	t.Run("missing binary classifies as runtime_unavailable", func(t *testing.T) {
		rt := &PodmanRuntime{Command: filepath.Join(t.TempDir(), "no-such-podman")}
		err := rt.PullImage(context.Background(), "example.com/scion/agent:v1")
		if err == nil {
			t.Fatal("expected error for missing binary")
		}
		var lf *LaunchFailure
		if !errors.As(err, &lf) {
			t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
		}
		if lf.Code != api.LaunchCodeRuntimeUnavailable || lf.Step != api.LaunchStepImagePull {
			t.Errorf("got (%q, %q), want (%q, %q)", lf.Code, lf.Step, api.LaunchCodeRuntimeUnavailable, api.LaunchStepImagePull)
		}
	})
}

func TestPodmanRuntime_Run_ClassifiesLaunchFailures(t *testing.T) {
	cases := []struct {
		name      string
		image     string
		stderr    string
		wantCode  string
		wantStep  string
		wantTyped bool
	}{
		{
			name:      "missing tag during run",
			image:     "example.com/scion/agent:missing",
			stderr:    "Trying to pull example.com/scion/agent:missing...\nError: initializing source docker://example.com/scion/agent:missing: manifest unknown",
			wantCode:  api.LaunchCodeImagePullFailed,
			wantStep:  api.LaunchStepImagePull,
			wantTyped: true,
		},
		{
			name:      "malformed reference during run",
			image:     "INVALID::IMAGE",
			stderr:    `Error: parsing reference "INVALID::IMAGE": invalid reference format`,
			wantCode:  api.LaunchCodeInvalidImage,
			wantStep:  api.LaunchStepImagePull,
			wantTyped: true,
		},
		{
			name:      "podman machine stopped during run",
			image:     "example.com/scion/agent:v1",
			stderr:    "Cannot connect to Podman. Is the podman machine running?",
			wantCode:  api.LaunchCodeRuntimeUnavailable,
			wantStep:  api.LaunchStepLaunching,
			wantTyped: true,
		},
		{
			name:      "non-image container failure stays untyped",
			image:     "example.com/scion/agent:v1",
			stderr:    "Error: statfs /nonexistent/host/path: no such file or directory",
			wantTyped: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			mockPodman := filepath.Join(tmpDir, "mock-podman")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s >&2\nexit 125\n", shellQuote(tc.stderr))
			if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
				t.Fatalf("failed to write mock podman: %v", err)
			}

			var steps []string
			rt := &PodmanRuntime{Command: mockPodman}
			cfg := RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        tc.image,
				Task:         "hello",
				Progress: func(step, message string) {
					steps = append(steps, step+": "+message)
				},
			}

			_, err := rt.Run(context.Background(), cfg)
			if err == nil {
				t.Fatal("expected Run to fail, got nil")
			}

			var lf *LaunchFailure
			if !tc.wantTyped {
				if errors.As(err, &lf) {
					t.Fatalf("expected untyped error, got LaunchFailure %+v", lf)
				}
				if len(steps) != 0 {
					t.Fatalf("expected no progress step, got %v", steps)
				}
				return
			}
			if !errors.As(err, &lf) {
				t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
			}
			if lf.Code != tc.wantCode {
				t.Errorf("LaunchFailure.Code = %q, want %q", lf.Code, tc.wantCode)
			}
			if lf.Step != tc.wantStep {
				t.Errorf("LaunchFailure.Step = %q, want %q", lf.Step, tc.wantStep)
			}
			if tc.wantStep == api.LaunchStepImagePull {
				wantProgress := api.LaunchStepImagePull + ": pulling the image"
				if len(steps) != 1 || steps[0] != wantProgress {
					t.Errorf("progress steps = %v, want [%q]", steps, wantProgress)
				}
			} else if len(steps) != 0 {
				t.Errorf("expected no image_pull progress step for step %q, got %v", tc.wantStep, steps)
			}
		})
	}
}

func TestAppleContainerRuntime_PullImage_ClassifiesLaunchFailures(t *testing.T) {
	cases := []struct {
		name     string
		image    string
		stderr   string
		wantCode string
	}{
		{
			name:     "missing tag",
			image:    "example.com/scion/agent:no-such-tag",
			stderr:   `Error: notFound: "failed to fetch manifest for example.com/scion/agent:no-such-tag: manifest unknown"`,
			wantCode: api.LaunchCodeImagePullFailed,
		},
		{
			name:     "unreachable registry",
			image:    "registry.invalid:5000/scion/agent:v1",
			stderr:   "Error: failed to pull image registry.invalid:5000/scion/agent:v1: connection refused",
			wantCode: api.LaunchCodeImagePullFailed,
		},
		{
			name:     "pull access denied",
			image:    "example.com/private/agent:v1",
			stderr:   "Error: pull access denied for example.com/private/agent:v1",
			wantCode: api.LaunchCodeImagePullFailed,
		},
		{
			name:     "malformed reference",
			image:    "INVALID::IMAGE",
			stderr:   `Error: invalidArgument: "invalid image reference: INVALID::IMAGE"`,
			wantCode: api.LaunchCodeInvalidImage,
		},
		{
			name:     "invalid reference format",
			image:    "Example.com/UpperCase/Agent:v1",
			stderr:   "Error: invalid reference: repository name must be lowercase",
			wantCode: api.LaunchCodeInvalidImage,
		},
		{
			name:     "whitespace in image reference",
			image:    "example.com/bad image:v1",
			stderr:   "Error: invalid argument",
			wantCode: api.LaunchCodeInvalidImage,
		},
		{
			name:     "container system not started",
			image:    "example.com/scion/agent:v1",
			stderr:   "Error: container system is not running. Run 'container system start' to start it.",
			wantCode: api.LaunchCodeRuntimeUnavailable,
		},
		{
			name:     "unable to connect to container service",
			image:    "example.com/scion/agent:v1",
			stderr:   "Error: unable to connect to container runtime service",
			wantCode: api.LaunchCodeRuntimeUnavailable,
		},
		{
			name:     "xpc apiserver connection invalid",
			image:    "example.com/scion/agent:v1",
			stderr:   "Error: XPC connection to com.apple.container.apiserver interrupted: connection invalid",
			wantCode: api.LaunchCodeRuntimeUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			mockContainer := filepath.Join(tmpDir, "mock-container")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s >&2\nexit 1\n", shellQuote(tc.stderr))
			if err := os.WriteFile(mockContainer, []byte(script), 0755); err != nil {
				t.Fatalf("failed to write mock container: %v", err)
			}

			var steps []string
			ctx := WithLaunchProgress(context.Background(), func(step, message string) {
				steps = append(steps, step+": "+message)
			})

			rt := &AppleContainerRuntime{Command: mockContainer}
			err := rt.PullImage(ctx, tc.image)
			if err == nil {
				t.Fatal("expected PullImage to fail, got nil")
			}

			wantProgress := api.LaunchStepImagePull + ": pulling the image"
			if len(steps) != 1 || steps[0] != wantProgress {
				t.Errorf("progress steps = %v, want [%q]", steps, wantProgress)
			}

			var lf *LaunchFailure
			if !errors.As(err, &lf) {
				t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
			}
			if lf.Code != tc.wantCode {
				t.Errorf("LaunchFailure.Code = %q, want %q", lf.Code, tc.wantCode)
			}
			if lf.Step != api.LaunchStepImagePull {
				t.Errorf("LaunchFailure.Step = %q, want %q", lf.Step, api.LaunchStepImagePull)
			}
			if lf.Reason != "" {
				t.Errorf("LaunchFailure.Reason = %q, want empty", lf.Reason)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("pull %q:", tc.image)) {
				t.Errorf("error text %q lost pull prefix", err.Error())
			}
		})
	}

	t.Run("successful pull reports step", func(t *testing.T) {
		tmpDir := t.TempDir()
		mockContainer := filepath.Join(tmpDir, "mock-container")
		if err := os.WriteFile(mockContainer, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatalf("failed to write mock container: %v", err)
		}
		var steps []string
		ctx := WithLaunchProgress(context.Background(), func(step, message string) {
			steps = append(steps, step+": "+message)
		})
		rt := &AppleContainerRuntime{Command: mockContainer}
		if err := rt.PullImage(ctx, "example.com/scion/agent:v1"); err != nil {
			t.Fatalf("PullImage: %v", err)
		}
		wantProgress := api.LaunchStepImagePull + ": pulling the image"
		if len(steps) != 1 || steps[0] != wantProgress {
			t.Errorf("progress steps = %v, want [%q]", steps, wantProgress)
		}
	})

	t.Run("missing binary classifies as runtime_unavailable", func(t *testing.T) {
		rt := &AppleContainerRuntime{Command: filepath.Join(t.TempDir(), "no-such-container")}
		err := rt.PullImage(context.Background(), "example.com/scion/agent:v1")
		if err == nil {
			t.Fatal("expected error for missing binary")
		}
		var lf *LaunchFailure
		if !errors.As(err, &lf) {
			t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
		}
		if lf.Code != api.LaunchCodeRuntimeUnavailable || lf.Step != api.LaunchStepImagePull {
			t.Errorf("got (%q, %q), want (%q, %q)", lf.Code, lf.Step, api.LaunchCodeRuntimeUnavailable, api.LaunchStepImagePull)
		}
	})
}

func TestAppleContainerRuntime_Run_ClassifiesLaunchFailures(t *testing.T) {
	cases := []struct {
		name      string
		image     string
		stderr    string
		wantCode  string
		wantStep  string
		wantTyped bool
	}{
		{
			name:      "missing tag during run",
			image:     "example.com/scion/agent:missing",
			stderr:    `Error: failed to pull image example.com/scion/agent:missing: manifest unknown`,
			wantCode:  api.LaunchCodeImagePullFailed,
			wantStep:  api.LaunchStepImagePull,
			wantTyped: true,
		},
		{
			name:      "malformed reference during run",
			image:     "INVALID::IMAGE",
			stderr:    `Error: invalid image reference: INVALID::IMAGE`,
			wantCode:  api.LaunchCodeInvalidImage,
			wantStep:  api.LaunchStepImagePull,
			wantTyped: true,
		},
		{
			name:      "container system stopped during run",
			image:     "example.com/scion/agent:v1",
			stderr:    "Error: container system is not running. Run 'container system start' first.",
			wantCode:  api.LaunchCodeRuntimeUnavailable,
			wantStep:  api.LaunchStepLaunching,
			wantTyped: true,
		},
		{
			name:      "non-image container failure stays untyped",
			image:     "example.com/scion/agent:v1",
			stderr:    "Error: mount source path does not exist",
			wantTyped: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			mockContainer := filepath.Join(tmpDir, "mock-container")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s >&2\nexit 1\n", shellQuote(tc.stderr))
			if err := os.WriteFile(mockContainer, []byte(script), 0755); err != nil {
				t.Fatalf("failed to write mock container: %v", err)
			}

			var steps []string
			rt := &AppleContainerRuntime{Command: mockContainer}
			cfg := RunConfig{
				Harness:      &harness.Generic{},
				Name:         "test-agent",
				UnixUsername: "scion",
				Image:        tc.image,
				Task:         "hello",
				Progress: func(step, message string) {
					steps = append(steps, step+": "+message)
				},
			}

			_, err := rt.Run(context.Background(), cfg)
			if err == nil {
				t.Fatal("expected Run to fail, got nil")
			}

			var lf *LaunchFailure
			if !tc.wantTyped {
				if errors.As(err, &lf) {
					t.Fatalf("expected untyped error, got LaunchFailure %+v", lf)
				}
				if len(steps) != 0 {
					t.Fatalf("expected no progress step, got %v", steps)
				}
				return
			}
			if !errors.As(err, &lf) {
				t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
			}
			if lf.Code != tc.wantCode {
				t.Errorf("LaunchFailure.Code = %q, want %q", lf.Code, tc.wantCode)
			}
			if lf.Step != tc.wantStep {
				t.Errorf("LaunchFailure.Step = %q, want %q", lf.Step, tc.wantStep)
			}
			if tc.wantStep == api.LaunchStepImagePull {
				wantProgress := api.LaunchStepImagePull + ": pulling the image"
				if len(steps) != 1 || steps[0] != wantProgress {
					t.Errorf("progress steps = %v, want [%q]", steps, wantProgress)
				}
			} else if len(steps) != 0 {
				t.Errorf("expected no image_pull progress step for step %q, got %v", tc.wantStep, steps)
			}
		})
	}
}

func TestLocalRuntimes_ImageExists_ClassifiesRuntimeUnavailable(t *testing.T) {
	runtimes := []struct {
		name         string
		daemonStderr string
		makeRT       func(cmd string) Runtime
	}{
		{
			name:         "docker",
			daemonStderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?",
			makeRT:       func(cmd string) Runtime { return &DockerRuntime{Command: cmd} },
		},
		{
			name:         "podman",
			daemonStderr: "Cannot connect to Podman. Is the podman machine running?",
			makeRT:       func(cmd string) Runtime { return &PodmanRuntime{Command: cmd} },
		},
		{
			name:         "apple_container",
			daemonStderr: "Error: unable to connect to container runtime service",
			makeRT:       func(cmd string) Runtime { return &AppleContainerRuntime{Command: cmd} },
		},
	}

	for _, rtCase := range runtimes {
		t.Run(rtCase.name+"/daemon_stopped", func(t *testing.T) {
			mockCmd := filepath.Join(t.TempDir(), "mock-rt")
			script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s >&2\nexit 1\n", shellQuote(rtCase.daemonStderr))
			if err := os.WriteFile(mockCmd, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			exists, err := rtCase.makeRT(mockCmd).ImageExists(context.Background(), "example.com/scion/agent:v1")
			if exists {
				t.Fatal("expected exists=false")
			}
			var lf *LaunchFailure
			if !errors.As(err, &lf) {
				t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
			}
			if lf.Code != api.LaunchCodeRuntimeUnavailable || lf.Step != api.LaunchStepLaunching {
				t.Errorf("got (%q, %q), want (%q, %q)", lf.Code, lf.Step, api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching)
			}
		})

		t.Run(rtCase.name+"/missing_binary", func(t *testing.T) {
			exists, err := rtCase.makeRT(filepath.Join(t.TempDir(), "missing-bin")).ImageExists(context.Background(), "example.com/scion/agent:v1")
			if exists {
				t.Fatal("expected exists=false")
			}
			var lf *LaunchFailure
			if !errors.As(err, &lf) {
				t.Fatalf("expected *LaunchFailure, got %T: %v", err, err)
			}
			if lf.Code != api.LaunchCodeRuntimeUnavailable || lf.Step != api.LaunchStepLaunching {
				t.Errorf("got (%q, %q), want (%q, %q)", lf.Code, lf.Step, api.LaunchCodeRuntimeUnavailable, api.LaunchStepLaunching)
			}
		})
	}
}
