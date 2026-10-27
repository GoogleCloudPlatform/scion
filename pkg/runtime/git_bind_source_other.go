//go:build !unix

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

package runtime

import (
	"errors"
	"fmt"
)

// checkedBindSource always fails with errors.ErrUnsupported: the git-metadata
// bind checks need O_NOFOLLOW; see git_bind_source_unix.go.
func checkedBindSource(gitRoot string, mode bindSourceMode, comps ...string) (src string, present bool, err error) {
	return "", false, fmt.Errorf("check git metadata bind source under %s: %w on this platform", gitRoot, errors.ErrUnsupported)
}

// checkPrivateGitConfigSource always fails with errors.ErrUnsupported: the
// ownership and link-count check needs syscall.Stat_t; see
// git_bind_source_unix.go.
func checkPrivateGitConfigSource(path string) error {
	return fmt.Errorf("check private git config %s: %w on this platform", path, errors.ErrUnsupported)
}

// readSmallConfigFile always fails with errors.ErrUnsupported: the read needs
// O_NOFOLLOW; see git_bind_source_unix.go.
func readSmallConfigFile(path string) ([]byte, error) {
	return nil, fmt.Errorf("read git config %s: %w on this platform", path, errors.ErrUnsupported)
}
