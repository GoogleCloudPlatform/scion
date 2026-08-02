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

package config

import (
	"fmt"
	"runtime"
	"testing"
)

// mockRuntimeDetection sets up mock functions for lookPathFunc and runCheckFunc
// so that only the specified runtime is reported as available. Call in tests that
// invoke InitProject/InitMachine to avoid requiring actual container runtimes.
func mockRuntimeDetection(t *testing.T, availableRuntime string) {
	t.Helper()
	origLookPath := lookPathFunc
	origRunCheck := runCheckFunc
	t.Cleanup(func() {
		lookPathFunc = origLookPath
		runCheckFunc = origRunCheck
		resetDetectLocalRuntimeCache()
	})
	resetDetectLocalRuntimeCache()
	lookPathFunc = func(file string) (string, error) {
		if file == availableRuntime {
			return "/usr/bin/" + file, nil
		}
		return "", fmt.Errorf("not found: %s", file)
	}
	runCheckFunc = func(binary string, args []string) error {
		if binary == availableRuntime {
			return nil
		}
		return fmt.Errorf("not working: %s", binary)
	}
}

// mockRuntimeDetectionMulti sets up mock functions where multiple runtimes are available.
func mockRuntimeDetectionMulti(t *testing.T, available map[string]bool) {
	t.Helper()
	origLookPath := lookPathFunc
	origRunCheck := runCheckFunc
	t.Cleanup(func() {
		lookPathFunc = origLookPath
		runCheckFunc = origRunCheck
		resetDetectLocalRuntimeCache()
	})
	resetDetectLocalRuntimeCache()
	lookPathFunc = func(file string) (string, error) {
		if available[file] {
			return "/usr/bin/" + file, nil
		}
		return "", fmt.Errorf("not found: %s", file)
	}
	runCheckFunc = func(binary string, args []string) error {
		if available[binary] {
			return nil
		}
		return fmt.Errorf("not working: %s", binary)
	}
}

// mockRuntimeDetectionNone mocks an environment where no container runtime is available.
func mockRuntimeDetectionNone(t *testing.T) {
	t.Helper()
	origLookPath := lookPathFunc
	origRunCheck := runCheckFunc
	t.Cleanup(func() {
		lookPathFunc = origLookPath
		runCheckFunc = origRunCheck
		resetDetectLocalRuntimeCache()
	})
	resetDetectLocalRuntimeCache()
	lookPathFunc = func(file string) (string, error) {
		return "", fmt.Errorf("not found: %s", file)
	}
	runCheckFunc = func(binary string, args []string) error {
		return fmt.Errorf("not working: %s", binary)
	}
}

func mockIsGitRepo(t *testing.T, result bool) {
	t.Helper()
	orig := isGitRepoFunc
	t.Cleanup(func() { isGitRepoFunc = orig })
	isGitRepoFunc = func() bool { return result }
}

func TestDetectLocalRuntime_PodmanPreferred(t *testing.T) {
	mockRuntimeDetectionMulti(t, map[string]bool{"podman": true, "docker": true})

	result, err := DetectLocalRuntime()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "podman" {
		t.Errorf("expected podman (preferred), got %s", result)
	}
}

func TestDetectLocalRuntime_DockerFallback(t *testing.T) {
	mockRuntimeDetection(t, "docker")

	result, err := DetectLocalRuntime()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "docker" {
		t.Errorf("expected docker, got %s", result)
	}
}

func TestDetectLocalRuntime_ContainerOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("container runtime is only checked on macOS")
	}

	// container is second preference after podman; make only container available
	mockRuntimeDetection(t, "container")

	result, err := DetectLocalRuntime()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "container" {
		t.Errorf("expected container, got %s", result)
	}
}

func TestDetectLocalRuntime_ContainerSkippedOnLinux(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this test verifies container is skipped on non-darwin")
	}

	// Only make container available; on Linux it should be skipped
	mockRuntimeDetection(t, "container")

	_, err := DetectLocalRuntime()
	if err == nil {
		t.Error("expected error when only container (macOS-only) is available on Linux")
	}
}

func TestDetectLocalRuntime_NoRuntime(t *testing.T) {
	mockRuntimeDetectionNone(t)

	_, err := DetectLocalRuntime()
	if err == nil {
		t.Fatal("expected error when no runtime is available")
	}

	// Error message should mention install instructions
	errMsg := err.Error()
	if runtime.GOOS == "darwin" {
		if errMsg != "no supported container runtime found: install podman, container (Apple Virtualization), or docker" {
			t.Errorf("unexpected error message for darwin: %s", errMsg)
		}
	} else {
		if errMsg != "no supported container runtime found: install podman or docker" {
			t.Errorf("unexpected error message for linux: %s", errMsg)
		}
	}
}

func TestDetectLocalRuntime_OnPathButNotFunctioning(t *testing.T) {
	// Binary is on PATH but fails to execute — should skip to next candidate
	origLookPath := lookPathFunc
	origRunCheck := runCheckFunc
	t.Cleanup(func() {
		lookPathFunc = origLookPath
		runCheckFunc = origRunCheck
	})

	lookPathFunc = func(file string) (string, error) {
		// All binaries found on PATH
		if file == "podman" || file == "docker" {
			return "/usr/bin/" + file, nil
		}
		return "", fmt.Errorf("not found")
	}
	runCheckFunc = func(binary string, args []string) error {
		// podman found but not functioning, docker works
		if binary == "docker" {
			return nil
		}
		return fmt.Errorf("command failed")
	}

	result, err := DetectLocalRuntime()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "docker" {
		t.Errorf("expected docker (podman not functioning), got %s", result)
	}
}
