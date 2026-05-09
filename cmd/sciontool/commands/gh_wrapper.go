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

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// ghWrapperCmd wraps the real `gh` CLI binary, injecting a fresh GitHub token
// from the token file before delegating execution. This ensures the `gh` CLI
// always uses a current token even after the background refresh loop has
// rotated it.
//
// Installed as a symlink or script at a higher PATH priority than the real `gh`.
var ghWrapperCmd = &cobra.Command{
	Use:                "gh-wrapper [gh args...]",
	Short:              "Wrapper for gh CLI that injects fresh GitHub App tokens",
	Hidden:             true,
	DisableFlagParsing: true,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(runGhWrapper(args))
	},
}

func init() {
	rootCmd.AddCommand(ghWrapperCmd)
}

func runGhWrapper(args []string) int {
	// If GitHub App is enabled, read the fresh token from the token file.
	// However, if the user has explicitly set their own GITHUB_TOKEN (flagged
	// by SCION_USER_GITHUB_TOKEN=true during provisioning), skip injection
	// so that gh uses the user's token from the environment instead.
	if hub.IsGitHubAppEnabled() {
		if os.Getenv(hub.EnvUserGitHubToken) == "true" {
			fmt.Fprintf(os.Stderr, "sciontool gh-wrapper: user-provided GITHUB_TOKEN detected; using it instead of GitHub App token\n")
		} else {
			tokenPath := hub.GitHubTokenPath()
			token := hub.ReadGitHubTokenFile(tokenPath)
			if token != "" {
				os.Setenv("GH_TOKEN", token)
			}
		}
	}

	// Find the real gh binary (skip ourselves if we're in PATH)
	ghPath, err := findRealGh()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sciontool gh-wrapper: %v\n", err)
		return 1
	}

	// Exec the real gh, replacing this process
	err = syscall.Exec(ghPath, append([]string{"gh"}, args...), os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "sciontool gh-wrapper: failed to exec %s: %v\n", ghPath, err)
		return 1
	}
	return 0
}

// findRealGh looks for the real `gh` binary, skipping any sciontool-based wrapper.
func findRealGh() (string, error) {
	// Look for gh in standard locations. The .real paths are checked first
	// because the Dockerfile renames the real binary to gh.real and installs
	// a wrapper script at the original path.
	paths := []string{
		"/usr/bin/gh.real",
		"/usr/local/bin/gh.real",
		"/usr/bin/gh",
		"/usr/local/bin/gh",
	}

	selfPath, _ := os.Executable()

	for _, p := range paths {
		if p == selfPath {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			// Check it's not a symlink to ourselves
			resolved, err := os.Readlink(p)
			if err == nil && resolved == selfPath {
				continue
			}
			return p, nil
		}
	}

	// Fall back to PATH-based lookup
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return "", fmt.Errorf("gh not found in PATH")
	}
	if ghPath == selfPath {
		return "", fmt.Errorf("only found sciontool gh-wrapper, no real gh binary")
	}
	return ghPath, nil
}
