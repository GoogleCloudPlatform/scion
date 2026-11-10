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

package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// GitReleasePrefix starts the release name of a hub binary built from Git
// (rebuild-server): git-<sha12>. On the symlink layout it is also the
// directory under <install_root>/releases/ the build is installed into.
const GitReleasePrefix = "git-"

// gitReleaseSHALen is how many hex digits of the commit a Git release name
// carries.
const gitReleaseSHALen = 12

// versionPackage is where the build identity variables live (-X targets).
const versionPackage = "github.com/GoogleCloudPlatform/scion/pkg/version"

var commitSHARE = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

// GitReleaseName returns git-<first 12 hex digits of commit>, or "" when
// commit is not a hex commit id of at least 12 digits.
func GitReleaseName(commit string) string {
	c := strings.ToLower(strings.TrimSpace(commit))
	if !commitSHARE.MatchString(c) {
		return ""
	}
	return GitReleasePrefix + c[:gitReleaseSHALen]
}

// RunningReleaseName names a build the way the update lifecycle does
// (pending_update "to", update-state.json "target", the symlink layout's
// release directory): its release version when it has one, else the Git
// release name of its commit, else "".
func RunningReleaseName(version, commit string) string {
	if version != "" {
		return version
	}
	return GitReleaseName(commit)
}

// GitBuildLDFlags is the -ldflags value of a rebuild from Git: the commit
// and build time, so the build identifies itself (version --format json,
// update-status, RunningReleaseName). Version is deliberately not set,
// even on a tagged commit: a Git build is named git-<sha12>, and boot
// reconciliation verifies it by that name.
func GitBuildLDFlags(commit string, buildTime time.Time) string {
	return fmt.Sprintf("-X %s.Commit=%s -X %s.BuildTime=%s",
		versionPackage, commit, versionPackage, buildTime.UTC().Format(time.RFC3339))
}

// VerifyGitBuild runs `<bin> version --format json` and checks that the
// binary reports commit (so the -X ldflags took effect) and no release
// version.
func VerifyGitBuild(ctx context.Context, bin, commit string, logger io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, "version", "--format", "json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_, _ = fmt.Fprintf(logger, "Running: %s version --format json\n", bin)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("binary version check failed: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	var out struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return fmt.Errorf("failed to parse version output: %w (raw: %s)", err, strings.TrimSpace(stdout.String()))
	}
	if out.Commit != commit {
		return fmt.Errorf("commit mismatch: binary reports %q, expected %q", out.Commit, commit)
	}
	if out.Version != "" {
		return fmt.Errorf("a Git build must not carry a release version, binary reports %q", out.Version)
	}
	_, _ = fmt.Fprintf(logger, "Build verified: commit %s\n", commit)
	return nil
}
