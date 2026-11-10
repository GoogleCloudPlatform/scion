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
	"errors"
	"io/fs"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
)

// serverDaemonMayRun reports whether the server daemon's PID file names a
// live process. Read-only: unlike daemon.StatusComponent it never removes
// a stale PID file. An absent PID file is not running; an unreadable or
// invalid one is an error, which callers treat as running (fail closed).
func serverDaemonMayRun(globalDir string) (bool, error) {
	pid, err := daemon.ReadPIDComponent(serverDaemonComponent, globalDir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pid <= 0 {
		return false, errors.New("invalid server PID file")
	}
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM), nil
}
