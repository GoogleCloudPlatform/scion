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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// writeRootFile replaces name in root with content (mode 0644), atomically:
// a concurrent reader (a broker reading project-id or project-slug over NFS)
// sees the old content or the new, never a truncated or partly written file.
// name must be a regular file or absent. os.Root follows a symbolic link that
// stays inside root even with O_NOFOLLOW, so:
//   - the content goes to a new temporary file in the same directory
//     (O_CREATE|O_EXCL|O_NOFOLLOW, 0644), fsynced and closed;
//   - an absent name is published with a hard link (Root.Link), which fails
//     with EEXIST if anything appeared at name meanwhile (never replacing or
//     following it; the existing-file path below then decides);
//   - an existing name is checked with Lstat, opened without O_TRUNC
//     (O_NONBLOCK, so a FIFO never blocks) and compared with os.SameFile; just
//     before the replace it is checked again (same regular file), and only
//     then is the temporary file renamed over it (Root.Rename replaces the
//     directory entry and never follows a link).
//
// A symbolic link, FIFO or other special file is refused and left unchanged.
// The temporary file is removed on every error path. Accepted limits: a swap
// to a symlink between the re-check and the rename replaces the link entry
// itself (Rename never follows it, nothing is written through it); the share
// must support hard links (a documented requirement of the shared backend);
// a crash can leave a uniquely named .<name>.tmp-* file, which no reader
// enumerates.
func writeRootFile(root *os.Root, name, content string) error {
	tmp, err := writeRootTempFile(root, name, content)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = root.Remove(tmp)
		}
	}()

	before, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		err := root.Link(tmp, name)
		if err == nil {
			_ = root.Remove(tmp)
			committed = true
			return nil
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
	_ = f.Close()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return fmt.Errorf("%s is not a regular file", name)
	}
	// The re-check right before the replace: still the same regular file.
	again, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !again.Mode().IsRegular() || !os.SameFile(before, again) {
		return fmt.Errorf("%s changed while it was being replaced", name)
	}
	if err := root.Rename(tmp, name); err != nil {
		return err
	}
	committed = true
	return nil
}

// writeRootTempFile writes content to a new temporary file beside name in
// root (O_CREATE|O_EXCL|O_NOFOLLOW, mode 0644), fsyncs and closes it, and
// returns its name. The file is removed again on a write failure.
func writeRootTempFile(root *os.Root, name, content string) (string, error) {
	var lastErr error
	for range 8 {
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return "", err
		}
		tmp := "." + name + ".tmp-" + hex.EncodeToString(rnd[:])
		f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
		if errors.Is(err, fs.ErrExist) {
			lastErr = err
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.WriteString(content)
		if werr == nil {
			werr = f.Sync()
		}
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = root.Remove(tmp)
			return "", werr
		}
		return tmp, nil
	}
	return "", fmt.Errorf("create a temporary file for %s: %w", name, lastErr)
}
