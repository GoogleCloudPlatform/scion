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
	"context"
	"fmt"
	"strconv"
	"strings"
)

// validateBranchName refuses a rebuild branch (the admin "branch" param or
// the configured repo branch) that git would not accept as a branch name,
// before it reaches git checkout and git reset (ptone/scion#6987).
//
// A name starting with "-" is refused here, so it can never be read as an
// option. Every other name must pass `git check-ref-format --branch`
// unchanged: that rejects names such as "a..b", "x.lock" or "HEAD", and the
// unchanged check refuses "@{-N}", which git would expand to another branch.
// It runs through gitOutput, the package's one git door for plain reads.
func validateBranchName(ctx context.Context, repoPath, branch string) error {
	if branch == "" {
		return nil
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("invalid branch %q: a branch name cannot start with '-'", branch)
	}
	out, err := gitOutput(ctx, repoPath, "check-ref-format", "--branch", branch)
	if err != nil {
		return fmt.Errorf("invalid branch %q: %w", branch, err)
	}
	if out != branch {
		return fmt.Errorf("invalid branch %q: git resolves it to %q; name the branch itself", branch, out)
	}
	return nil
}

// endOfOptionsMinGit is the first git release whose option parser accepts
// --end-of-options (git 2.24).
var endOfOptionsMinGit = [2]int{2, 24}

// gitSupportsEndOfOptions reports whether the host git accepts
// --end-of-options. An unreadable version counts as unsupported, so an old
// or unusual git keeps working without it; the branch name is validated
// either way.
func gitSupportsEndOfOptions(ctx context.Context, dir string) bool {
	out, err := gitOutput(ctx, dir, "version")
	if err != nil {
		return false
	}
	major, minor, ok := parseGitVersion(out)
	if !ok {
		return false
	}
	return major > endOfOptionsMinGit[0] || (major == endOfOptionsMinGit[0] && minor >= endOfOptionsMinGit[1])
}

// parseGitVersion reads the major and minor version from `git version`
// output such as "git version 2.43.0" or "git version 2.39.5 (Apple Git-154)".
func parseGitVersion(out string) (major, minor int, ok bool) {
	rest, found := strings.CutPrefix(out, "git version ")
	fields := strings.Fields(rest)
	if !found || len(fields) == 0 {
		return 0, 0, false
	}
	parts := strings.SplitN(fields[0], ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// branchArgs returns git arguments that end with branch, preceded by
// --end-of-options when the host git supports it.
func branchArgs(endOfOptions bool, branch string, args ...string) []string {
	if endOfOptions {
		args = append(args, "--end-of-options")
	}
	return append(args, branch)
}
