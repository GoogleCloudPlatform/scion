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

package telemetry

import (
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/wakesock"
)

// agyWakeAfterBindHook is a test-only seam invoked immediately after binding
// the staged Unix socket and before fchmodat/fchownat/renameat. Always nil in
// production.
var agyWakeAfterBindHook func(parentDir, stageName string)

// verifyAgyParentDir checks via Lstat that dir is a real directory (never a
// symlink) and, when running as root with an expected agent owner, is owned by
// owner.UID.
func verifyAgyParentDir(dir string, owner *OutboxOwner) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("antigravity parent dir %s is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("antigravity parent dir %s is not a directory", dir)
	}
	if owner != nil && os.Geteuid() == 0 && owner.UID > 0 {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("antigravity parent dir %s: cannot inspect owner", dir)
		}
		if int(st.Uid) != owner.UID {
			return fmt.Errorf("antigravity parent dir %s owned by uid %d, want %d", dir, st.Uid, owner.UID)
		}
	}
	return nil
}

// startAgySQLiteWakeListener binds a Unix domain datagram socket at sockPath
// via the shared hardened pkg/sciontool/wakesock helper so short-lived
// `sciontool hook` processes can wake the live Antigravity SQLite tailer.
func startAgySQLiteWakeListener(sockPath string, owner *OutboxOwner, onWake func()) (io.Closer, error) {
	var wsOwner *wakesock.Owner
	if owner != nil {
		wsOwner = &wakesock.Owner{UID: owner.UID, GID: owner.GID}
	}
	return wakesock.Listen(sockPath, wakesock.Options{
		Owner:         wsOwner,
		AfterBindHook: agyWakeAfterBindHook,
	}, func(_ byte) {
		if onWake != nil {
			onWake()
		}
	})
}

// NotifyAgySQLiteWake sends a best-effort, non-blocking 1-byte datagram to the
// live Antigravity SQLite tailer's Unix domain socket. Safe to call from
// short-lived `sciontool hook` processes; never blocks or fails the hook if
// the daemon is not listening.
func NotifyAgySQLiteWake(agentHome string) {
	NotifyAgySQLiteWakePath(AgySQLiteWakePath(agentHome))
}

// NotifyAgySQLiteWakePath sends a best-effort 1-byte wake datagram to sockPath.
func NotifyAgySQLiteWakePath(sockPath string) {
	wakesock.Notify(sockPath, 1)
}
