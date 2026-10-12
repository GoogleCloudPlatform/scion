// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !unix

package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

// hostStateNoFollowParent is passed as agentinfolock.Options.NoFollowParent;
// agentinfolock has no lock on these platforms, so it has no effect.
const hostStateNoFollowParent = true

// hostStateDir is agentDir by path. Without openat, the components below
// the trusted root are checked with Lstat in openHostStateDir and the file
// with Lstat in openFile; the checks are not atomic.
type hostStateDir struct {
	path string
}

// openHostStateDir refuses a symlink or non-directory in any component of
// l.rel below the resolved root. A missing component gives an error that
// matches fs.ErrNotExist.
func openHostStateDir(l hostStateLayout) (*hostStateDir, error) {
	cur := l.root
	for i, name := range l.rel {
		cur = filepath.Join(cur, name)
		fi, err := os.Lstat(cur)
		if err != nil {
			return nil, hostStateComponentErr(cur, i, 0, false, err)
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return nil, hostStateComponentErr(cur, i, fi.Mode(), true, nil)
		}
	}
	return &hostStateDir{path: cur}, nil
}

func (d *hostStateDir) close() {}

// openFile opens HostStateFile read-only, refusing a symlink at it.
// Without O_NOFOLLOW the check is an Lstat before the open, so it is not
// atomic.
func (d *hostStateDir) openFile() (*os.File, error) {
	path := filepath.Join(d.path, HostStateFile)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", errHostStateSymlink, path)
	}
	return os.Open(path)
}

// writeFile writes data to HostStateFile through a temp file with a random
// name in agentDir, chmod 0600, fsync, then rename. It is not
// writeAgentInfoFile because that helper does not fsync: after a crash a
// zero-length host-state.json would be refused as corrupt and block every
// later update.
func (d *hostStateDir) writeFile(data []byte) error {
	path := filepath.Join(d.path, HostStateFile)
	tmp, err := os.CreateTemp(d.path, "."+HostStateFile+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	err = func() error {
		if err := tmp.Chmod(0o600); err != nil {
			_ = tmp.Close()
			return err
		}
		if _, err := tmp.Write(data); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			return err
		}
		if err := tmp.Close(); err != nil {
			return err
		}
		return os.Rename(tmpPath, path)
	}()
	if err != nil {
		_ = os.Remove(tmpPath)
	}
	return err
}
