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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// envGhWrapperTokenHash stores the SHA-256 hex digest of the GitHub App token
// that gh-wrapper injected into GH_TOKEN. If a child process spawned by gh
// inherits GH_TOKEN and invokes gh again after the token file has rotated, the
// wrapper matches GH_TOKEN against this digest to recognize that GH_TOKEN came
// from an earlier wrapper invocation (and should be refreshed) rather than
// from an explicit caller override.
const envGhWrapperTokenHash = "SCION_GH_WRAPPER_TOKEN_HASH"

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
	applyGhWrapperTokenEnv(os.Stderr)

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

// applyGhWrapperTokenEnv prepares GH_TOKEN in the process environment before
// execing the real gh binary.
//
// When GitHub App token refresh is enabled and SCION_USER_GITHUB_TOKEN is not
// true, the wrapper normally injects the fresh App token from the token file
// into GH_TOKEN so gh uses it instead of the stale initial GITHUB_TOKEN
// inherited from container startup.
//
// If the caller explicitly set GH_TOKEN to a different token for this call,
// the wrapper leaves GH_TOKEN untouched and prints a one-line notice to stderr
// (never printing any token value). To ensure stale hub-minted App tokens are
// still refreshed, GH_TOKEN is treated as wrapper/hub-managed when it matches:
//   - the current token file (already fresh; no notice),
//   - the container's initial GITHUB_TOKEN (stale initial App token), or
//   - the SHA-256 digest in SCION_GH_WRAPPER_TOKEN_HASH from an earlier
//     gh-wrapper invocation in this process tree (stale wrapper-injected token
//     inherited across token rotation).
func applyGhWrapperTokenEnv(stderr io.Writer) {
	if !hub.IsGitHubAppEnabled() {
		return
	}
	if util.ParseBoolEnv(hub.EnvUserGitHubToken, false) {
		_, _ = fmt.Fprintf(stderr, "sciontool gh-wrapper: user-provided GITHUB_TOKEN detected; using it instead of GitHub App token\n")
		return
	}

	tokenPath := hub.GitHubTokenPath()
	appToken := hub.ReadGitHubTokenFile(tokenPath)
	callerToken := os.Getenv("GH_TOKEN")

	if callerToken != "" {
		if appToken != "" && tokenEqual(callerToken, appToken) {
			_ = os.Setenv(envGhWrapperTokenHash, tokenSHA256Hex(appToken))
			return
		}
		if !isManagedHubToken(callerToken, os.Getenv("GITHUB_TOKEN"), os.Getenv(envGhWrapperTokenHash)) {
			if os.Getenv(envGhWrapperTokenHash) != "" {
				_ = os.Setenv(envGhWrapperTokenHash, "")
			}
			_, _ = fmt.Fprintf(stderr, "sciontool gh-wrapper: caller-provided GH_TOKEN detected; using it instead of GitHub App token\n")
			return
		}
	}

	if appToken != "" {
		_ = os.Setenv("GH_TOKEN", appToken)
		_ = os.Setenv(envGhWrapperTokenHash, tokenSHA256Hex(appToken))
	}
}

func isManagedHubToken(callerToken, initialEnvToken, wrapperTokenHash string) bool {
	if initialEnvToken != "" && tokenEqual(callerToken, initialEnvToken) {
		return true
	}
	if wrapperTokenHash != "" && tokenEqual(tokenSHA256Hex(callerToken), wrapperTokenHash) {
		return true
	}
	return false
}

func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func tokenSHA256Hex(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
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
