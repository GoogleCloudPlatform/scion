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

//go:build unix

package shipper

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
	"golang.org/x/sys/unix"
)

// openArtifactNoFollow opens relWithinRuleRoot relative to resolvedRuleRoot
// without following symlinks at any intermediate directory or at the leaf,
// preventing TOCTOU symlink swaps between discovery and open.
//
// Every component is opened via openat(2) with O_NOFOLLOW (and O_DIRECTORY for
// intermediate components, O_NONBLOCK for the leaf so FIFOs cannot block).
// The returned handle is verified to be a regular file via fstat.
func openArtifactNoFollow(resolvedRuleRoot, relWithinRuleRoot string) (*os.File, os.FileInfo, error) {
	if resolvedRuleRoot == "" || relWithinRuleRoot == "" {
		return nil, nil, errors.New("rule root and relative artifact path are required")
	}
	parts := strings.Split(relWithinRuleRoot, "/")
	for _, p := range parts {
		if err := pathcanon.ValidName(p); err != nil {
			return nil, nil, fmt.Errorf("invalid artifact path component %q: %w", p, err)
		}
	}

	dirFD, err := unix.Open(resolvedRuleRoot, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open rule root %s: %w", resolvedRuleRoot, err)
	}

	for _, dirComp := range parts[:len(parts)-1] {
		nextFD, err := unix.Openat(dirFD, dirComp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(dirFD)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
				return nil, nil, fmt.Errorf("refusing intermediate component %q: %w", dirComp, privatefs.ErrReparsePoint)
			}
			return nil, nil, fmt.Errorf("open dir component %q: %w", dirComp, err)
		}
		dirFD = nextFD
	}

	leaf := parts[len(parts)-1]
	leafFD, err := unix.Openat(dirFD, leaf, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	_ = unix.Close(dirFD)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, fmt.Errorf("refusing symlink leaf %q: %w", leaf, privatefs.ErrReparsePoint)
		}
		return nil, nil, fmt.Errorf("open artifact leaf %q: %w", leaf, err)
	}

	f := os.NewFile(uintptr(leafFD), resolvedRuleRoot+"/"+relWithinRuleRoot)
	if f == nil {
		_ = unix.Close(leafFD)
		return nil, nil, errors.New("failed to wrap artifact file descriptor")
	}

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("fstat artifact: %w", err)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("refusing %s: not a regular file", relWithinRuleRoot)
	}
	return f, fi, nil
}
