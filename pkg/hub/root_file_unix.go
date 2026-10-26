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

package hub

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// writeRootFile replaces name in root with content (mode 0644). name must
// be a regular file or absent. os.Root follows a symbolic link that stays
// inside root even with O_NOFOLLOW, so:
//   - an absent name is created with O_EXCL, which never follows a link at
//     the leaf (a link created meanwhile makes it fail with EEXIST, and the
//     existing-file path below then refuses it);
//   - an existing name is checked with Lstat, opened without O_TRUNC
//     (O_NONBLOCK, so a FIFO never blocks), and truncated only once the
//     opened file is the same regular file.
//
// A symbolic link, FIFO or other special file is refused and left unchanged.
func writeRootFile(root *os.Root, name, content string) error {
	before, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
		if err == nil {
			return writeOpenedRootFile(f, content, false)
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if before, err = root.Lstat(name); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	f, err := root.OpenFile(name, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = f.Close()
		return fmt.Errorf("%s is not a regular file", name)
	}
	return writeOpenedRootFile(f, content, true)
}

// writeOpenedRootFile writes content to f (truncating it first when
// truncate is set) and closes it.
func writeOpenedRootFile(f *os.File, content string, truncate bool) error {
	if truncate {
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return err
		}
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
