//go:build unix

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

package runtimebroker

import (
	"context"
	"os/exec"
	"syscall"
)

// execRunCommand runs a command bounded by both parent (for a dispatch,
// the request context) and mountCommandTimeout, and returns its combined
// output.
//
// The command runs in its own process group, and the whole group is killed
// when the context is done: mount(8) runs the mount.nfs helper as a child
// that inherits the output pipes, and killing only mount would leave the
// helper running and the call waiting on the pipes. WaitDelay is a backstop
// for a descendant that left the group.
func execRunCommand(parent context.Context, name string, args ...string) ([]byte, error) {
	timeout := mountCommandTimeout
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = commandWaitDelay
	out, err := cmd.CombinedOutput()
	return out, classifyCommandError(name, timeout, err, ctx, parent)
}
