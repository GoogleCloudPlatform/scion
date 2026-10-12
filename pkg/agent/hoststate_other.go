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
)

// hostStateNoFollowParent is passed as agentinfolock.Options.NoFollowParent;
// agentinfolock has no lock on these platforms, so it has no effect.
const hostStateNoFollowParent = true

// hostStateDir is agentDir by path. Without openat, symlinks in parent
// components are followed on these platforms; only a symlink at
// HostStateFile itself is refused.
type hostStateDir struct {
	path string
}

func openHostStateDir(agentDir string) (*hostStateDir, error) {
	return &hostStateDir{path: agentDir}, nil
}

func (d *hostStateDir) close() {}

// openFile opens HostStateFile read-only, refusing a symlink at it.
// Without O_NOFOLLOW the check is an Lstat before the open, so it is not
// atomic.
func (d *hostStateDir) openFile() (*os.File, error) {
	path := HostStatePath(d.path)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: %s", errHostStateSymlink, path)
	}
	return os.Open(path)
}

// writeFile writes data to HostStateFile with writeAgentInfoFile, the
// package's temp-file-and-rename helper (random temp name in agentDir).
func (d *hostStateDir) writeFile(data []byte) error {
	return writeAgentInfoFile(HostStatePath(d.path), data, 0o600)
}
