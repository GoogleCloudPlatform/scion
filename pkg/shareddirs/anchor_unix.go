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

package shareddirs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// afterWalkHook is a deterministic test seam: a package var so a test can
// inject a directory-to-symlink swap at the exact instant described in the
// comment inside OpenAnchoredRoot, below, rather than relying on a
// background goroutine racing a real syscall. Production always uses the
// no-op default.
var afterWalkHook = func() {}

// OpenAnchoredRoot opens an *os.Root on <hostBase>/<rel>, anchored to the
// directory identity (device + inode) an independent, symlink-safe,
// open-only walk just observed there: it performs its own fresh open-only,
// O_NOFOLLOW-at-every-component walk to the leaf (never creating anything),
// Fstats the fd that walk produced, opens the os.Root, Stats the Root's own
// "." entry, and refuses unless the two identities (device and inode)
// match. A mismatch means something changed the directory tree between the
// walk and the open -- issued back to back -- and is treated as a hard
// refusal.
//
// This guarantees that every operation through the returned Root reaches
// the exact directory the walk verified, even if a structural component
// (e.g. the project's <pid> directory, or shared-dirs/ itself) is replaced
// with a symlink between an earlier path resolution and this call.
//
// Returns an fs.ErrNotExist-compatible error if the leaf does not exist,
// and never creates it. A missing leaf that should be created on this call
// must go through EnsureLeaf first, which does its own independent
// O_NOFOLLOW walk to create it safely, then this function.
func OpenAnchoredRoot(hostBase, rel string) (*os.Root, error) {
	baseFd, err := unix.Open(hostBase, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, &os.PathError{Op: "open", Path: hostBase, Err: unix.ENOENT}
		}
		return nil, fmt.Errorf("open host base %q: %w", hostBase, err)
	}
	defer func() { _ = unix.Close(baseFd) }()

	path := filepath.Join(hostBase, rel)

	leafFd, existed, err := openExistingDirPathNoFollow(baseFd, rel)
	if err != nil {
		return nil, err
	}
	if !existed {
		return nil, &os.PathError{Op: "open", Path: path, Err: unix.ENOENT}
	}
	defer func() { _ = unix.Close(leafFd) }()

	var walkStat unix.Stat_t
	if err := unix.Fstat(leafFd, &walkStat); err != nil {
		return nil, fmt.Errorf("stat shared dir %q: %w", path, err)
	}

	// A test seam: production never assigns anything here. A test can use
	// it to deterministically swap a real directory for a symlink at this
	// exact instant, between the walk's own identity check and the open
	// below, rather than relying on a background goroutine to win a real
	// race often enough.
	afterWalkHook()

	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}

	rootInfo, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok {
		_ = root.Close()
		return nil, fmt.Errorf("cannot verify shared dir %q identity: no raw stat available", path)
	}
	if uint64(rootStat.Dev) != uint64(walkStat.Dev) || uint64(rootStat.Ino) != uint64(walkStat.Ino) {
		_ = root.Close()
		return nil, fmt.Errorf(
			"shared dir %q changed between resolution and open (possible symlink swap); refusing", path)
	}

	return root, nil
}
