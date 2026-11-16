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

// Package rebuildpreflight holds the decision logic of the Rebuild from git
// maintenance actions (rebuild-server and rebuild-web): request and
// parameter validation, and how a run's final result is assembled. It is
// pure logic: the hub (pkg/hub) does the wiring, the exec calls and the
// store writes, and this package does not import it.
package rebuildpreflight

import (
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/gitref"
)

// BranchRule states the allowed form of a branch name, listing every rule
// ValidBranch checks. It is the text of the 400 for an invalid branch
// parameter, so an admin can see what is allowed.
const BranchRule = gitref.BranchRule

// ErrInvalidBranch is the error ValidBranch wraps.
var ErrInvalidBranch = gitref.ErrInvalidBranch

// ErrInvalidCommit is the error ValidCommit wraps.
var ErrInvalidCommit = errors.New("invalid commit")

// ruleError is an invalid-value error: base, the reason, then the rule.
type ruleError struct {
	base   error
	reason string
	rule   string
}

func (e *ruleError) Error() string {
	return e.base.Error() + ": " + e.reason + "; " + e.rule
}

func (e *ruleError) Unwrap() error { return e.base }

// ValidBranch reports whether name may be passed to git as a branch.
// CheckRunRequest applies it to a run request's branch parameter, at the
// request, before a run is created. Separately, the executor checks every
// branch it uses (the request's or the configured repository branch) with
// its own validateBranchName before the branch reaches a git argv.
//
// The rules are pure Go (no git check-ref-format call) and at least as
// strict as check-ref-format for every name the character set admits. Each
// rule is checked on its own and reported by name, then the error states
// the whole allowed form (BranchRule).
func ValidBranch(name string) error {
	return gitref.ValidBranch(name)
}

// commitRule states the allowed form of a commit.
const commitRule = "commit must be a 40- or 64-character lower-case hex object name"

// ValidCommit reports whether sha is a full lower-case hex object name: 40
// characters (SHA-1) or 64 (SHA-256).
func ValidCommit(sha string) error {
	if !isHexObjectName(sha) {
		return &ruleError{base: ErrInvalidCommit, reason: "it is not a full object name", rule: commitRule}
	}
	return nil
}

// isHexObjectName reports whether s is 40 or 64 lower-case hex characters.
func isHexObjectName(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
