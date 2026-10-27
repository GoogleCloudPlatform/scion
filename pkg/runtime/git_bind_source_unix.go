//go:build unix

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
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// checkedBindSource is the single fail-closed inspection every git-metadata bind
// SOURCE passes through before it is bound into an agent. Starting at the
// broker-trusted gitRoot, it walks each component in comps with os.Lstat: a
// component that is a symlink, or a non-directory intermediate component, refuses
// the start — so a planted symlink, at the leaf or at any intermediate component,
// can never redirect a bind to, or create a file or directory at, a path outside
// the repository. A symlink is never followed and nothing is ever created through
// one. The leaf is handled by mode:
//   - bindDirRequire:  the leaf must be an existing real directory; a missing leaf
//     or a missing intermediate component reports absent (present=false).
//   - bindDirCreate:   a missing component is created one level at a time (its
//     parent already verified a real directory); an existing non-dir/symlink refuses.
//   - bindFileRequire: the leaf must be an existing regular file; missing reports absent.
//   - bindFileCreate:  a missing leaf is created as a regular file with
//     O_NOFOLLOW|O_EXCL in the verified parent (so a dangling symlink at the leaf is
//     refused rather than created through); an existing non-regular/symlink refuses.
//
// For the *Create modes, after the leaf is created it re-walks the whole component
// chain (reverifyChain): O_NOFOLLOW guards only the leaf, so a component verified as
// a real directory and then swapped for a symlink before the create would land the
// new entry outside the repository. The post-create re-walk fails closed on any such
// swap, removing what was created and refusing the start.
//
// It returns the validated absolute source path and whether it is present to bind.
func checkedBindSource(gitRoot string, mode bindSourceMode, comps ...string) (src string, present bool, err error) {
	if len(comps) == 0 {
		return "", false, fmt.Errorf("checkedBindSource: empty source under %s", gitRoot)
	}
	cur := gitRoot
	last := len(comps) - 1
	// Walk the intermediate components (all but the leaf): each must be an existing
	// real directory. A symlink or non-directory refuses; a missing intermediate is
	// created one level at a time for the *Create modes (its parent already verified
	// a real directory) or reports absent for the *Require modes.
	for i, c := range comps {
		if c == "" || c == "." || c == ".." {
			return "", false, fmt.Errorf("checkedBindSource: unsafe component %q under %s", c, gitRoot)
		}
		cur = filepath.Join(cur, c)
		if i == last {
			break
		}
		fi, statErr := os.Lstat(cur)
		switch {
		case statErr == nil:
			if fi.Mode()&os.ModeSymlink != 0 {
				return "", false, fmt.Errorf("git metadata bind source %s is a symlink", cur)
			}
			if !fi.IsDir() {
				return "", false, fmt.Errorf("git metadata bind source parent %s is not a directory", cur)
			}
		case os.IsNotExist(statErr):
			if mode == bindDirRequire || mode == bindFileRequire {
				return "", false, nil
			}
			if mkErr := os.Mkdir(cur, 0755); mkErr != nil {
				return "", false, fmt.Errorf("create git metadata directory %s: %w", cur, mkErr)
			}
		default:
			return "", false, fmt.Errorf("stat git metadata bind source %s: %w", cur, statErr)
		}
	}

	// The leaf. cur is now gitRoot/comps.
	switch mode {
	case bindDirRequire, bindDirCreate:
		fi, statErr := os.Lstat(cur)
		switch {
		case statErr == nil:
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				return "", false, fmt.Errorf("git metadata bind source %s is not a directory", cur)
			}
			return cur, true, nil
		case os.IsNotExist(statErr):
			if mode == bindDirRequire {
				return "", false, nil
			}
			if bindSourceRaceHook != nil {
				bindSourceRaceHook(cur)
			}
			if mkErr := os.Mkdir(cur, 0755); mkErr != nil {
				return "", false, fmt.Errorf("create git metadata directory %s: %w", cur, mkErr)
			}
			// Close the create-window TOCTOU: if a verified component was swapped for
			// a symlink before the Mkdir, the directory landed outside the repository.
			// Re-walk and, on any swap, remove it and refuse.
			if rErr := reverifyChain(gitRoot, comps, true); rErr != nil {
				_ = os.Remove(cur)
				return "", false, rErr
			}
			return cur, true, nil
		default:
			return "", false, fmt.Errorf("stat git metadata bind source %s: %w", cur, statErr)
		}
	case bindFileRequire:
		fi, statErr := os.Lstat(cur)
		switch {
		case statErr == nil:
			if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
				return "", false, fmt.Errorf("git metadata bind source %s is not a regular file", cur)
			}
			return cur, true, nil
		case os.IsNotExist(statErr):
			return "", false, nil
		default:
			return "", false, fmt.Errorf("stat git metadata bind source %s: %w", cur, statErr)
		}
	case bindFileCreate:
		// Create the leaf as a regular file with O_EXCL|O_NOFOLLOW so that any
		// existing path at cur — including a symlink (even a dangling one) — is
		// refused rather than followed or created through. O_EXCL|O_NOFOLLOW is the
		// load-bearing guard here: it is what keeps a symlinked leaf from being
		// written through at the broker's uid.
		if bindSourceRaceHook != nil {
			bindSourceRaceHook(cur)
		}
		f, oErr := os.OpenFile(cur, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0644)
		if oErr == nil {
			_ = f.Close()
			// Close the create-window TOCTOU: O_NOFOLLOW guards only the leaf, not an
			// intermediate component. If a verified component was swapped for a symlink
			// before the open, the file landed outside the repository. Re-walk and, on
			// any swap, remove it and refuse.
			if rErr := reverifyChain(gitRoot, comps, false); rErr != nil {
				_ = os.Remove(cur)
				return "", false, rErr
			}
			return cur, true, nil
		}
		if !errors.Is(oErr, os.ErrExist) {
			return "", false, fmt.Errorf("create git metadata file %s: %w", cur, oErr)
		}
		// It already exists: bind only a real regular file (not a symlink/dir/other).
		fi, lErr := os.Lstat(cur)
		if lErr != nil {
			return "", false, fmt.Errorf("stat git metadata bind source %s: %w", cur, lErr)
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return "", false, fmt.Errorf("git metadata bind source %s is not a regular file", cur)
		}
		return cur, true, nil
	default:
		return "", false, fmt.Errorf("checkedBindSource: unknown mode %d", mode)
	}
}

// checkPrivateGitConfigSource verifies a seeded private-config path is safe to
// bind: a regular file (not a symlink or special file), link count 1 (not a hard
// link that also names another file), owned by this process. It uses Lstat so a
// symlink is rejected rather than followed.
func checkPrivateGitConfigSource(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot read ownership/link count", path)
	}
	if st.Nlink != 1 {
		return fmt.Errorf("%s has link count %d (want 1)", path, st.Nlink)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d, not the broker (uid %d)", path, st.Uid, os.Getuid())
	}
	return nil
}

// readSmallConfigFile reads a .git/config, refusing a symlink or non-regular file
// (O_NOFOLLOW) and capping the size. A missing file returns nil with no error.
func readSmallConfigFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only; a close error cannot affect the bytes already read
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	const maxConfig = 1 << 20
	// Read one byte past the cap and refuse a file larger than it, so the whole
	// config is covered by the credential check: every byte that is checked is the
	// whole file, with no unread remainder.
	raw, err := io.ReadAll(io.LimitReader(f, maxConfig+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxConfig {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxConfig)
	}
	return raw, nil
}
