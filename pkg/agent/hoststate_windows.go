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

//go:build windows

package agent

import (
	"errors"
	"fmt"
	"os"
)

// Agent host state is not supported on Windows: the agent lifecycle that
// reads and writes HostStateFile runs on a Runtime Broker, and the Windows
// CLI refuses the commands that reach it (cmd/platform_gate.go). The
// unix code refuses links with openat and O_NOFOLLOW and writes the file
// 0600; a path-based Lstat walk and a chmod would protect nothing here
// (Lstat does not report a junction as a symlink, and a chmod only sets
// the read-only attribute), so every entry point returns an error wrapping
// errors.ErrUnsupported instead of a weaker fallback. The
// callers tolerate it: startFailedForDisplay is fail-quiet, RecordedRunOwner
// reports the owner unknown so files are kept, and the writers return the
// error for their caller to log.

// hostStateNoFollowParent is passed as agentinfolock.Options.NoFollowParent;
// UpdateHostState never reaches the lock on Windows.
const hostStateNoFollowParent = true

// errHostStateUnsupported is returned by every host state entry point.
var errHostStateUnsupported = fmt.Errorf("agent host state: %w on Windows (agent lifecycle runs on a Runtime Broker)", errors.ErrUnsupported)

// hostStateDir is never created on Windows.
type hostStateDir struct {
	path string
}

// openHostStateDir refuses: host state is not supported on Windows.
func openHostStateDir(hostStateLayout) (*hostStateDir, error) {
	return nil, errHostStateUnsupported
}

func (d *hostStateDir) close() {}

// openFile refuses: host state is not supported on Windows.
func (d *hostStateDir) openFile() (*os.File, error) {
	return nil, errHostStateUnsupported
}

// writeFile refuses: host state is not supported on Windows.
func (d *hostStateDir) writeFile([]byte) error {
	return errHostStateUnsupported
}
