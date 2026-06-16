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
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"
)

// runtimeCandidate defines a container runtime to probe during init.
type runtimeCandidate struct {
	Name       string
	Binary     string
	DarwinOnly bool
	CheckArgs  []string
}

// localRuntimeCandidates lists container runtimes in preference order.
var localRuntimeCandidates = []runtimeCandidate{
	{Name: "container", Binary: "container", DarwinOnly: true, CheckArgs: []string{"--version"}},
	{Name: "podman", Binary: "podman", CheckArgs: []string{"--version"}},
	{Name: "docker", Binary: "docker", CheckArgs: []string{"--version"}},
}

// lookPathFunc is the function used to look up binaries on PATH.
// Override in tests to simulate different environments.
var lookPathFunc = exec.LookPath

// runCheckFunc verifies a runtime binary is functional by executing it.
// Override in tests to simulate different environments.
var runCheckFunc = func(binary string, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, binary, args...).Run()
}

// DetectLocalRuntime probes the system for an available container runtime.
// It checks OS compatibility and verifies each candidate is both on PATH
// and can execute successfully. Candidates are checked in preference order:
// container (macOS only), podman, docker.
// Returns the runtime name or an error if no supported runtime is found.
func DetectLocalRuntime() (string, error) {
	for _, c := range localRuntimeCandidates {
		if c.DarwinOnly && runtime.GOOS != "darwin" {
			continue
		}
		if _, err := lookPathFunc(c.Binary); err != nil {
			continue
		}
		if err := runCheckFunc(c.Binary, c.CheckArgs); err != nil {
			continue
		}
		return c.Name, nil
	}

	if runtime.GOOS == "darwin" {
		return "", fmt.Errorf("no supported container runtime found: install podman, container (Apple Virtualization), or docker")
	}
	return "", fmt.Errorf("no supported container runtime found: install podman or docker")
}

// DetectAllLocalRuntimes probes the system for all available container runtimes.
// Unlike DetectLocalRuntime which returns the first match, this function returns
// every runtime that is both on PATH and can execute successfully.
func DetectAllLocalRuntimes() []string {
	var available []string
	for _, c := range localRuntimeCandidates {
		if c.DarwinOnly && runtime.GOOS != "darwin" {
			continue
		}
		if _, err := lookPathFunc(c.Binary); err != nil {
			continue
		}
		if err := runCheckFunc(c.Binary, c.CheckArgs); err != nil {
			continue
		}
		available = append(available, c.Name)
	}
	return available
}

// OverrideRuntimeDetection replaces the functions used by DetectLocalRuntime
// to look up and verify container runtimes. This is intended for use in tests
// across packages. Returns a restore function that should be deferred.
func OverrideRuntimeDetection(
	lookPath func(string) (string, error),
	runCheck func(string, []string) error,
) func() {
	origLP := lookPathFunc
	origRC := runCheckFunc
	lookPathFunc = lookPath
	runCheckFunc = runCheck
	return func() {
		lookPathFunc = origLP
		runCheckFunc = origRC
	}
}
