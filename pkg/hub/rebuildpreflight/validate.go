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
	"strings"
)

// BranchRule states the allowed form of a branch name, listing every rule
// ValidBranch checks. It is the text of the 400 for an invalid branch
// parameter, so an admin can see what is allowed.
const BranchRule = "branch must be 1-200 characters of letters, digits, '.', '_', '/' and '-'; " +
	"start with a letter or digit; contain no '..', '@{' or '//'; " +
	"not end with '/', '.' or '.lock'; " +
	"have no path component that starts with '.' or ends with '.lock'; " +
	"and not be 'HEAD' or a 40- or 64-character hex string"

// maxBranchLen is the longest branch name ValidBranch accepts.
const maxBranchLen = 200

// ErrInvalidBranch is the error ValidBranch wraps.
var ErrInvalidBranch = errors.New("invalid branch name")

// ErrInvalidCommit is the error ValidCommit wraps.
var ErrInvalidCommit = errors.New("invalid commit")

// branchError is the error for a branch name that breaks one rule: the
// rule it breaks, then the full allowed form. It never quotes the name.
func branchError(reason string) error {
	return &ruleError{base: ErrInvalidBranch, reason: reason, rule: BranchRule}
}

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

// ValidBranch reports whether name may be passed to git as a branch. Every
// value that reaches a git argv as a branch (a request's branch parameter
// and the configured repository branch) passes it first, so none can start
// with '-' or be read as anything but a branch.
//
// The rules are pure Go (no git check-ref-format call) and at least as
// strict as check-ref-format for every name the character set admits. Each
// rule is checked on its own and reported by name, then the error states
// the whole allowed form (BranchRule).
func ValidBranch(name string) error {
	if name == "" {
		return branchError("it is empty")
	}
	if len(name) > maxBranchLen {
		return branchError("it is longer than 200 characters")
	}
	if strings.Contains(name, "..") {
		return branchError("it contains '..'")
	}
	if strings.Contains(name, "@{") {
		return branchError("it contains '@{'")
	}
	if strings.Contains(name, "//") {
		return branchError("it contains '//'")
	}
	if !isAlnum(name[0]) {
		return branchError("it does not start with a letter or digit")
	}
	for i := 0; i < len(name); i++ {
		if !isBranchChar(name[i]) {
			return branchError("it contains a character other than letters, digits, '.', '_', '/' and '-'")
		}
	}
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") {
		return branchError("it ends with '/', '.' or '.lock'")
	}
	for _, c := range strings.Split(name, "/") {
		if strings.HasPrefix(c, ".") || strings.HasSuffix(c, ".lock") {
			return branchError("a path component starts with '.' or ends with '.lock'")
		}
	}
	if name == "HEAD" {
		return branchError("'HEAD' is reserved")
	}
	// git reads a full-length hex name as an object name in either case,
	// so an upper-case one is just as ambiguous with a commit.
	if isHexObjectName(strings.ToLower(name)) {
		return branchError("a 40- or 64-character hex string is ambiguous with a commit")
	}
	return nil
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

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isBranchChar(c byte) bool {
	return isAlnum(c) || c == '.' || c == '_' || c == '/' || c == '-'
}
