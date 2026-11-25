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

package bundle

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// maxExcludedHardlinkScanEntries caps the total number of directory entries
// inspected under excluded credential directory prefixes when collecting
// hard-linked excluded regular files. If the budget is exhausted while
// scanning a credential directory, collectExcludedInodes fails closed with
// *ErrUnportableHome rather than silently skipping remaining credentials.
var maxExcludedHardlinkScanEntries = 4096

type fileIdentity struct {
	dev uint64
	ino uint64
}

type excludedInodeSet map[fileIdentity]struct{}

func fileIdentityIfHardlinked(info fs.FileInfo) (fileIdentity, bool) {
	if info == nil || !info.Mode().IsRegular() {
		return fileIdentity{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink <= 1 {
		return fileIdentity{}, false
	}
	return fileIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}

// collectExcludedInodes stats each excluded credential prefix inside root (and
// walks excluded credential directory prefixes up to
// maxExcludedHardlinkScanEntries total entries without following symlinks),
// recording the (dev, ino) of any regular file with nlink > 1 so hard links to
// excluded secrets at non-excluded paths are also excluded during the walk.
// If the scan budget is exhausted under a credential directory prefix, it
// fails closed with *ErrUnportableHome.
func collectExcludedInodes(root *os.Root, secretPrefixes []string) (excludedInodeSet, error) {
	if root == nil || len(secretPrefixes) == 0 {
		return nil, nil
	}
	var set excludedInodeSet
	record := func(info fs.FileInfo) {
		id, ok := fileIdentityIfHardlinked(info)
		if !ok {
			return
		}
		if set == nil {
			set = make(excludedInodeSet)
		}
		set[id] = struct{}{}
	}

	remaining := maxExcludedHardlinkScanEntries
	var walkDir func(rootPrefix, rel string) error
	walkDir = func(rootPrefix, rel string) error {
		f, err := rootopen.OpenDir(root, rel)
		if err != nil {
			return nil
		}
		entries, err := f.ReadDir(-1)
		_ = f.Close()
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if remaining <= 0 {
				return &ErrUnportableHome{
					Path:   rootPrefix,
					Reason: "excluded credential directory exceeds hardlink scan budget",
				}
			}
			remaining--
			mt := e.Type()
			if mt&fs.ModeSymlink != 0 {
				continue
			}
			child := path.Join(rel, e.Name())
			if e.IsDir() {
				if err := walkDir(rootPrefix, child); err != nil {
					return err
				}
				continue
			}
			if mt.IsRegular() {
				if info, err := root.Lstat(filepath.FromSlash(child)); err == nil {
					record(info)
				}
			}
		}
		return nil
	}

	for _, p := range secretPrefixes {
		if p == "" {
			continue
		}
		fi, err := root.Lstat(filepath.FromSlash(p))
		if err != nil {
			continue
		}
		if fi.Mode().IsRegular() {
			record(fi)
		} else if fi.IsDir() {
			if err := walkDir(p, p); err != nil {
				return nil, err
			}
		}
	}
	return set, nil
}

func (s excludedInodeSet) contains(d fs.DirEntry) bool {
	if len(s) == 0 {
		return false
	}
	info, err := d.Info()
	if err != nil {
		return false
	}
	id, ok := fileIdentityIfHardlinked(info)
	if !ok {
		return false
	}
	_, found := s[id]
	return found
}
