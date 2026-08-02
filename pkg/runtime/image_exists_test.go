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
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIsExitError(t *testing.T) {
	t.Run("nil error", func(t *testing.T) {
		if isExitError(nil) {
			t.Error("expected false for nil error")
		}
	})

	t.Run("exec.ExitError", func(t *testing.T) {
		// Run a command that exits with code 1.
		cmd := exec.Command("sh", "-c", "exit 1")
		err := cmd.Run()
		if !isExitError(err) {
			t.Errorf("expected true for exec.ExitError, got false (err=%T: %v)", err, err)
		}
	})

	t.Run("wrapped exec.ExitError", func(t *testing.T) {
		// Simulate runSimpleCommand wrapping the exit error.
		cmd := exec.Command("sh", "-c", "exit 1")
		err := cmd.Run()
		wrapped := fmt.Errorf("docker image inspect myimage failed: %w", err)
		if !isExitError(wrapped) {
			t.Errorf("expected true for wrapped exec.ExitError, got false (err=%T: %v)", wrapped, wrapped)
		}
	})

	t.Run("non-exit error", func(t *testing.T) {
		err := fmt.Errorf("connection refused")
		if isExitError(err) {
			t.Error("expected false for plain error")
		}
	})
}

func TestDockerRuntime_ImageExists_ErrorPropagation(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("image found", func(t *testing.T) {
		mockCmd := filepath.Join(tmpDir, "docker-found")
		if err := os.WriteFile(mockCmd, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
		rt := &DockerRuntime{Command: mockCmd}
		exists, err := rt.ImageExists(context.Background(), "myimage:latest")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if !exists {
			t.Error("expected exists=true")
		}
	})

	t.Run("image not found (exit code)", func(t *testing.T) {
		mockCmd := filepath.Join(tmpDir, "docker-notfound")
		if err := os.WriteFile(mockCmd, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
			t.Fatal(err)
		}
		rt := &DockerRuntime{Command: mockCmd}
		exists, err := rt.ImageExists(context.Background(), "nonexistent:latest")
		if err != nil {
			t.Errorf("unexpected error for not-found: %v", err)
		}
		if exists {
			t.Error("expected exists=false for not-found image")
		}
	})

	t.Run("fundamental failure (command not found)", func(t *testing.T) {
		rt := &DockerRuntime{Command: "/nonexistent/docker-binary"}
		exists, err := rt.ImageExists(context.Background(), "myimage:latest")
		if err == nil {
			t.Error("expected error for unreachable command, got nil")
		}
		if exists {
			t.Error("expected exists=false on failure")
		}
	})
}

func TestPodmanRuntime_ImageExists_ErrorPropagation(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("image found", func(t *testing.T) {
		mockCmd := filepath.Join(tmpDir, "podman-found")
		if err := os.WriteFile(mockCmd, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
		rt := &PodmanRuntime{Command: mockCmd}
		exists, err := rt.ImageExists(context.Background(), "myimage:latest")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if !exists {
			t.Error("expected exists=true")
		}
	})

	t.Run("image not found (exit code)", func(t *testing.T) {
		mockCmd := filepath.Join(tmpDir, "podman-notfound")
		if err := os.WriteFile(mockCmd, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
			t.Fatal(err)
		}
		rt := &PodmanRuntime{Command: mockCmd}
		exists, err := rt.ImageExists(context.Background(), "nonexistent:latest")
		if err != nil {
			t.Errorf("unexpected error for not-found: %v", err)
		}
		if exists {
			t.Error("expected exists=false for not-found image")
		}
	})

	t.Run("fundamental failure (command not found)", func(t *testing.T) {
		rt := &PodmanRuntime{Command: "/nonexistent/podman-binary"}
		exists, err := rt.ImageExists(context.Background(), "myimage:latest")
		if err == nil {
			t.Error("expected error for unreachable command, got nil")
		}
		if exists {
			t.Error("expected exists=false on failure")
		}
	})
}

func TestAppleContainerRuntime_ImageExists_ErrorPropagation(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("image found", func(t *testing.T) {
		mockCmd := filepath.Join(tmpDir, "container-found")
		if err := os.WriteFile(mockCmd, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
		rt := &AppleContainerRuntime{Command: mockCmd}
		exists, err := rt.ImageExists(context.Background(), "myimage:latest")
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if !exists {
			t.Error("expected exists=true")
		}
	})

	t.Run("image not found (exit code)", func(t *testing.T) {
		mockCmd := filepath.Join(tmpDir, "container-notfound")
		if err := os.WriteFile(mockCmd, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
			t.Fatal(err)
		}
		rt := &AppleContainerRuntime{Command: mockCmd}
		exists, err := rt.ImageExists(context.Background(), "nonexistent:latest")
		if err != nil {
			t.Errorf("unexpected error for not-found: %v", err)
		}
		if exists {
			t.Error("expected exists=false for not-found image")
		}
	})

	t.Run("fundamental failure (command not found)", func(t *testing.T) {
		rt := &AppleContainerRuntime{Command: "/nonexistent/container-binary"}
		exists, err := rt.ImageExists(context.Background(), "myimage:latest")
		if err == nil {
			t.Error("expected error for unreachable command, got nil")
		}
		if exists {
			t.Error("expected exists=false on failure")
		}
	})
}
