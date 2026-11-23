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

package rebuildpreflight

import (
	"fmt"
	"strings"
)

// EffectiveBranch resolves the branch name and detached state for an
// unforced run from the request's branch param, the configured RepoBranch,
// and the output of `git rev-parse --abbrev-ref HEAD`.
//
// A rev-parse output of "HEAD" (or empty) returns ("detached", true).
// A real branch literally named "detached" returns ("detached", false),
// which ValidBranch rejects so branchPinnable is recorded as false.
func EffectiveBranch(param, repoBranch, revParse string) (branch string, detached bool) {
	if p := strings.TrimSpace(param); p != "" {
		return p, false
	}
	if rb := strings.TrimSpace(repoBranch); rb != "" {
		return rb, false
	}
	rev := strings.TrimSpace(revParse)
	if rev == "" || rev == "HEAD" {
		return DetachedSentinel, true
	}
	return rev, false
}

// RecordCommit validates the output of `git rev-parse HEAD` at record time
// so a bad value fails the run before the build rather than surfacing later
// as a force 409.
func RecordCommit(revParseOut string) (string, error) {
	sha := strings.TrimSpace(revParseOut)
	if err := ValidCommit(sha); err != nil {
		return "", err
	}
	return sha, nil
}

// RunRecord holds the branch/commit metadata recorded on a rebuild-server run.
type RunRecord struct {
	Branch         string
	Detached       bool
	BranchPinnable bool
	Commit         string
}

// RecordForRun builds a run's branch/commit record.
//
// A forced run (grant != nil) never uses EffectiveBranch: it records the
// grant's pins and verifies that `git rev-parse HEAD` landed on
// grant.PinnedCommit, failing before the build on mismatch.
// An unforced run records effBranch/effDetached and sets BranchPinnable
// based on effDetached or ValidBranch(effBranch).
func RecordForRun(grant *ForceGrant, effBranch string, effDetached bool, revParseHead string) (RunRecord, error) {
	commit, err := RecordCommit(revParseHead)
	if err != nil {
		return RunRecord{}, err
	}
	if grant != nil {
		if commit != grant.PinnedCommit {
			return RunRecord{}, fmt.Errorf("pinned checkout did not land on %s (got %s)", grant.PinnedCommit, commit)
		}
		return RunRecord{
			Branch:         grant.PinnedBranch,
			Detached:       grant.Detached,
			BranchPinnable: true,
			Commit:         grant.PinnedCommit,
		}, nil
	}
	pinnable := effDetached || (effBranch != DetachedSentinel && ValidBranch(effBranch) == nil)
	return RunRecord{
		Branch:         effBranch,
		Detached:       effDetached,
		BranchPinnable: pinnable,
		Commit:         commit,
	}, nil
}

// Pin identifies a pinned commit for a forced rebuild.
type Pin struct {
	Branch   string
	Commit   string
	Detached bool
}

// Step is one git command in a rebuild checkout sequence.
type Step struct {
	Name string
	Args []string
}

// GitSteps returns the git command sequence for updating the checkout before
// a rebuild.
//
// When pin != nil:
//   - Branch pin: fetch origin, checkout <branch>, merge-base --is-ancestor
//     <sha> refs/remotes/origin/<branch>, reset --hard <sha>.
//   - Detached pin: fetch origin, cat-file -e <sha>^{commit}, checkout
//     --detach <sha>.
//
// When pin == nil (unforced):
//   - Named branch: fetch origin, checkout <branch>, reset --hard
//     origin/<branch>.
//   - Detached (empty or DetachedSentinel): fetch origin, reset --hard
//     origin/HEAD.
func GitSteps(branch string, pin *Pin) []Step {
	if pin != nil {
		if pin.Detached {
			return []Step{
				{Name: "Fetching latest code", Args: []string{"fetch", "origin"}},
				{Name: "Checking pinned commit " + pin.Commit, Args: []string{"cat-file", "-e", pin.Commit + "^{commit}"}},
				{Name: "Checking out pinned commit " + pin.Commit, Args: []string{"checkout", "--detach", pin.Commit}},
			}
		}
		return []Step{
			{Name: "Fetching latest code", Args: []string{"fetch", "origin"}},
			{Name: "Checking out branch " + pin.Branch, Args: []string{"checkout", pin.Branch}},
			{Name: "Verifying commit " + pin.Commit + " is on origin/" + pin.Branch, Args: []string{"merge-base", "--is-ancestor", pin.Commit, "refs/remotes/origin/" + pin.Branch}},
			{Name: "Resetting to pinned commit " + pin.Commit, Args: []string{"reset", "--hard", pin.Commit}},
		}
	}
	if branch == "" || branch == DetachedSentinel {
		return []Step{
			{Name: "Fetching latest code", Args: []string{"fetch", "origin"}},
			{Name: "Resetting to origin/HEAD", Args: []string{"reset", "--hard", "origin/HEAD"}},
		}
	}
	return []Step{
		{Name: "Fetching latest code", Args: []string{"fetch", "origin"}},
		{Name: "Checking out branch " + branch, Args: []string{"checkout", branch}},
		{Name: "Resetting to origin/" + branch, Args: []string{"reset", "--hard", "origin/" + branch}},
	}
}
