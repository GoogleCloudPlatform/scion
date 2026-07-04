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

// Package testgit provides helpers for running git commands in tests without
// triggering interactive prompts or macOS keychain access.
package testgit

import (
	"os"
	"os/exec"
)

// Env returns a copy of the current process environment with git credentials
// and interactive prompts disabled. This prevents macOS keychain popups and
// git credential helper prompts during test execution.
func Env() []string {
	env := os.Environ()
	env = append(env,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"SSH_ASKPASS_REQUIRE=never",
	)
	return env
}

// Setup sets environment variables in the current process to prevent git from
// triggering macOS keychain prompts or interactive credential helpers during
// tests. Call this from TestMain before running tests in packages that execute
// git commands.
func Setup() {
	os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	os.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	os.Setenv("GIT_TERMINAL_PROMPT", "0")
	os.Setenv("GIT_ASKPASS", "")
	os.Setenv("SSH_ASKPASS", "")
	os.Setenv("SSH_ASKPASS_REQUIRE", "never")
}

// Command creates an exec.Cmd for a git command with environment variables set
// to prevent keychain access and interactive prompts.
func Command(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Env = Env()
	return cmd
}
