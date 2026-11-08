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

package provision

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Bounded reads of files an agent can write (the workspace's git exclude
// file, worktree .git pointer files and git's admin gitdir files): a file
// above its cap is refused instead of being read into memory as a whole.

// gitExcludeMaxBytes caps how much of a workspace's .git/info/exclude is
// read (git's own exclude files are a few KiB).
const gitExcludeMaxBytes = 1 << 20

// gitPointerMaxBytes caps a worktree's .git file and an admin entry's gitdir
// file (one "gitdir: <path>" line).
const gitPointerMaxBytes = 64 << 10

// cloneManifestMaxBytes caps a clone scratch directory's manifest (a header
// and the clone's top-level entry names).
const cloneManifestMaxBytes = 1 << 20

// sharerMarkerMaxBytes caps a sharer registry marker (a small JSON object).
const sharerMarkerMaxBytes = 64 << 10

// errFileTooLarge marks a file above the cap of a bounded read.
var errFileTooLarge = errors.New("file is larger than the cap for this read")

// readBoundedFile reads the open file f from offset 0, refusing it when its
// size (fstat) is above max, and reading at most max+1 bytes through a
// limited reader, so a file that grows past the cap after the size check is
// refused too. The refusal wraps errFileTooLarge.
func readBoundedFile(f *os.File, max int64) ([]byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", f.Name(), err)
	}
	if fi.Size() > max {
		return nil, fmt.Errorf("%s: %w (%d bytes, cap %d)", f.Name(), errFileTooLarge, fi.Size(), max)
	}
	data, err := io.ReadAll(io.LimitReader(io.NewSectionReader(f, 0, max+1), max+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name(), err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s: %w (cap %d)", f.Name(), errFileTooLarge, max)
	}
	return data, nil
}
