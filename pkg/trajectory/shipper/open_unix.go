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

// openArtifactNoFollow opens nativeRelativePath relative to the trusted
// resolvedHome directory without following symlinks at any component below
// resolvedHome (rule-root ancestors, intermediate directories, or leaf),
// preventing TOCTOU symlink swaps between discovery and open.
//
// Every component below resolvedHome is opened via openat(2) with O_NOFOLLOW
// (and O_DIRECTORY for directory components, O_NONBLOCK for the leaf so FIFOs
// cannot block). The returned handle is verified to be a regular file via fstat.
func openArtifactNoFollow(resolvedHome, nativeRelativePath string) (*os.File, os.FileInfo, error) {
	if resolvedHome == "" || nativeRelativePath == "" {
		return nil, nil, errors.New("home directory and relative artifact path are required")
	}
	parts := strings.Split(nativeRelativePath, "/")
	for _, p := range parts {
		if err := pathcanon.ValidName(p); err != nil {
			return nil, nil, fmt.Errorf("invalid artifact path component %q: %w", p, err)
		}
	}

	dirFD, err := unix.Open(resolvedHome, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open home %s: %w", resolvedHome, err)
	}

	var homeUID uint32
	if geteuidFn() == 0 {
		var homeStat unix.Stat_t
		if err := unix.Fstat(dirFD, &homeStat); err != nil {
			_ = unix.Close(dirFD)
			return nil, nil, fmt.Errorf("fstat home %s: %w", resolvedHome, err)
		}
		homeUID, _ = statOwnerFn(resolvedHome, &homeStat)
	}

	for _, dirComp := range parts[:len(parts)-1] {
		nextFD, err := unix.Openat(dirFD, dirComp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(dirFD)
		if err != nil {
			if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
				return nil, nil, fmt.Errorf("refusing directory component %q: %w", dirComp, privatefs.ErrReparsePoint)
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

	f := os.NewFile(uintptr(leafFD), resolvedHome+"/"+nativeRelativePath)
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
		return nil, nil, fmt.Errorf("refusing %s: not a regular file", nativeRelativePath)
	}
	if geteuidFn() == 0 {
		var st unix.Stat_t
		if err := unix.Fstat(leafFD, &st); err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("fstat artifact: %w", err)
		}
		if st.Nlink != 1 {
			_ = f.Close()
			return nil, nil, fmt.Errorf("refusing hard-linked artifact %s (nlink=%d): %w", nativeRelativePath, st.Nlink, ErrHardLinkRefused)
		}
		ownerUID, _ := statOwnerFn(resolvedHome+"/"+nativeRelativePath, &st)
		if ownerUID != homeUID {
			_ = f.Close()
			return nil, nil, fmt.Errorf("refusing artifact %s owned by uid %d, want home owner %d: %w",
				nativeRelativePath, ownerUID, homeUID, privatefs.ErrNotPrivate)
		}
	}
	return f, fi, nil
}
