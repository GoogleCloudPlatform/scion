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

package testutil

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var (
	assumeDifferentOwnerOnce sync.Once
	assumeDifferentOwnerOK   bool
)

// RequireGitAssumeDifferentOwner skips the test unless the git found on PATH
// is 2.35.2 or newer, the first release honouring
// GIT_TEST_ASSUME_DIFFERENT_OWNER. It checks the PATH binary because that is
// the one the repository checks under test run.
func RequireGitAssumeDifferentOwner(t testing.TB) {
	t.Helper()
	assumeDifferentOwnerOnce.Do(func() {
		out, err := exec.Command("git", "--version").Output()
		if err != nil {
			return
		}
		version := strings.TrimPrefix(strings.TrimSpace(string(out)), "git version ")
		assumeDifferentOwnerOK = GitVersionAtLeast(version, 2, 35, 2)
	})
	if !assumeDifferentOwnerOK {
		t.Skip("git 2.35.2 or newer is required for GIT_TEST_ASSUME_DIFFERENT_OWNER")
	}
}

// GitVersionAtLeast reports whether a "git --version" number such as
// "2.56.0" or "2.39.3 (Apple Git-145)" is at least major.minor.patch.
func GitVersionAtLeast(version string, major, minor, patch int) bool {
	fields := strings.Fields(version)
	if len(fields) == 0 {
		return false
	}
	want := []int{major, minor, patch}
	parts := strings.Split(fields[0], ".")
	for i, w := range want {
		got := 0
		if i < len(parts) {
			n, err := strconv.Atoi(parts[i])
			if err != nil {
				return false
			}
			got = n
		}
		if got != w {
			return got > w
		}
	}
	return true
}
