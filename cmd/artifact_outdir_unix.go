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

package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// linkat is unix.Linkat; tests replace it to act as a file system without
// hard links.
var linkat = unix.Linkat

// outDir writes files under one directory without following a symbolic
// link at any level below it. Each folder is opened relative to its parent
// with O_NOFOLLOW, and each file is written to a temporary name and then
// moved into place relative to the folder's descriptor, so no step checks
// a path and then uses it again.
type outDir struct {
	fd   int
	name string
}

// openOutDir opens dir, creating it (and its parents) if needed. The
// directory itself is the caller's choice and may be reached through links.
func openOutDir(dir string) (*outDir, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	return &outDir{fd: fd, name: dir}, nil
}

func (d *outDir) Close() error { return unix.Close(d.fd) }

// openFolder opens (creating if needed) the folders of rel's parent, one
// at a time, never following a link. It returns the descriptor of the
// innermost folder; the caller closes it unless it is d.fd.
func (d *outDir) openFolder(parts []string) (int, error) {
	fd := d.fd
	for _, p := range parts {
		next, err := unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) {
			if err := unix.Mkdirat(fd, p, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
				d.closeIfOwned(fd)
				return -1, fmt.Errorf("create folder %s: %w", p, err)
			}
			next, err = unix.Openat(fd, p, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		d.closeIfOwned(fd)
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return -1, fmt.Errorf("%s is a symbolic link or not a folder; not writing through it", p)
			}
			return -1, fmt.Errorf("open folder %s: %w", p, err)
		}
		fd = next
	}
	return fd, nil
}

func (d *outDir) closeIfOwned(fd int) {
	if fd != d.fd {
		_ = unix.Close(fd)
	}
}

// WriteFile writes the slash-separated path rel under the directory: the
// bytes go to a temporary file that write fills, and are moved into place
// only if write succeeds. An existing file is replaced only when replace
// is set; otherwise the move fails if the name exists, atomically.
func (d *outDir) WriteFile(rel string, replace bool, write func(io.Writer) error) error {
	parts := strings.Split(rel, "/")
	base := parts[len(parts)-1]
	fd, err := d.openFolder(parts[:len(parts)-1])
	if err != nil {
		return err
	}
	defer d.closeIfOwned(fd)

	var st unix.Stat_t
	if err := unix.Fstatat(fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && !replace {
		return fmt.Errorf("%s already exists; use --force to replace it", rel)
	}

	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmpName := ".artifact-" + hex.EncodeToString(rnd[:])
	tfd, err := unix.Openat(fd, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", rel, err)
	}
	tmp := os.NewFile(uintptr(tfd), tmpName)
	defer func() { _ = unix.Unlinkat(fd, tmpName, 0) }()
	if err := write(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if replace {
		// Rename replaces whatever entry has the name (a link is replaced,
		// not followed).
		if err := unix.Renameat(fd, tmpName, fd, base); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
		return nil
	}
	// A hard link fails if the name exists, so a file that appeared since
	// the check above is not replaced either.
	err = linkat(fd, tmpName, fd, base, 0)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%s already exists; use --force to replace it", rel)
	case errors.Is(err, unix.EPERM), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EXDEV):
		// The file system has no hard links (FAT, exFAT, some network
		// and FUSE mounts). Check the name again and rename: a file
		// created in between could be replaced, on these file systems
		// only.
		if err := unix.Fstatat(fd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			return fmt.Errorf("%s already exists; use --force to replace it", rel)
		}
		if err := unix.Renameat(fd, tmpName, fd, base); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
		return nil
	default:
		return fmt.Errorf("write %s: %w", rel, err)
	}
}
