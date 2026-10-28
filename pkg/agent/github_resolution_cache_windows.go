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

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// The Windows form of the resolution cache's "tighten an existing
// directory or file" step. Mode bits mean nothing here and
// no Chmod is called; decisions are made on handles through privatefs.

// tightenCacheDir checks the existing cache directory, opened without
// following a reparse point, with privatefs.CheckPrivate. A directory that
// fails is reported and left as it is: an existing directory is never
// silently repaired. New directories are created private by
// the caller.
func tightenCacheDir(dir string) error {
	tree, err := privatefs.OpenTree(filepath.Dir(dir), privatefs.TreeOptions{})
	if err != nil {
		return fmt.Errorf("cannot check the resolution cache directory: %w", err)
	}
	defer func() { _ = tree.Close() }()
	d, err := tree.OpenDir(filepath.Base(dir))
	if err != nil {
		return fmt.Errorf("cannot check the resolution cache directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := privatefs.CheckPrivate(d); err != nil {
		return fmt.Errorf("resolution cache directory is not private (left as it is): %w", err)
	}
	return nil
}

// tightenCacheFile opens the existing cache file for writing through
// privatefs (Tree.OpenPrivate with O_RDWR), which applies the
// verify-or-repair rule on that handle: a private file passes, a too-open file
// owned by the current user gets the owner-only ACL before anything else
// happens to it (repair), and a file owned by someone else is refused and
// reported. Nothing is written. A missing file is not an error.
func tightenCacheFile(path string) error {
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot protect the resolution cache file: %w", err)
	}
	defer func() { _ = tree.Close() }()
	f, err := tree.OpenPrivate(filepath.Base(path), os.O_RDWR)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot protect the resolution cache file: %w", err)
	}
	return f.Close()
}
