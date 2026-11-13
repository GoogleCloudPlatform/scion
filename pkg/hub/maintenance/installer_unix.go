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

package maintenance

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// signalSelf sends SIGTERM to this process. The hub's shutdown handler stops
// gracefully and systemd (Restart=always) starts the binary current points to.
func signalSelf() error {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

// IsStopSignalExit reports whether err is a child process exit caused by
// SIGTERM. When systemd stops the hub's unit (KillMode=control-group), it
// sends its stop signal to every process in the cgroup, including the
// sudo/systemctl child that requested a --no-block restart.
//
// This intentionally matches only SIGTERM — systemd's default KillSignal=
// used by the starter-hub unit (scripts/starter-hub/scion-hub.service.tmpl) —
// so force-kills or crash signals (SIGKILL, SIGSEGV, SIGABRT) during a
// synchronous --no-block command still surface as failures rather than false
// restarts. A unit configured with a custom KillSignal= under --no-block is
// not matched here (whereas the plain `systemctl restart` fallback treats any
// signal termination within plainRestartGrace as a unit stop).
func IsStopSignalExit(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return ws.Signaled() && ws.Signal() == syscall.SIGTERM
}

// fileOwnerUID returns the owning uid of fi, when the platform reports it.
func fileOwnerUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// isWritableDir reports whether this process may create entries in dir.
func isWritableDir(dir string) bool {
	return unix.Access(dir, unix.W_OK|unix.X_OK) == nil
}
