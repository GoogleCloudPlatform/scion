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
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

// ExecMountChecker is the production MountChecker that shells out to
// mount(8)/umount(8)/mountpoint(1) to manage NFS mounts.
//
// Privilege requirements:
//   - The broker process must have mount privilege (root, CAP_SYS_ADMIN, or
//     sudoers entry for mount/umount). Without it, Mount/Unmount will fail.
//   - mountpoint(1) and /proc/mounts (Linux) require no special privilege.
type ExecMountChecker struct {
	log *slog.Logger
	// runCommand is the function used to run external commands.
	// Defaults to execRunCommand; overridden in tests.
	runCommand func(name string, args ...string) ([]byte, error)
}

// NewExecMountChecker creates a production MountChecker.
func NewExecMountChecker(log *slog.Logger) *ExecMountChecker {
	if log == nil {
		log = slog.Default()
	}
	return &ExecMountChecker{
		log:        log,
		runCommand: execRunCommand,
	}
}

// execRunCommand runs a command and returns its combined output.
func execRunCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// IsMountpoint returns true if the given path is currently a mountpoint.
// Uses mountpoint(1) which is available on all modern Linux distributions.
func (e *ExecMountChecker) IsMountpoint(path string) (bool, error) {
	out, err := e.runCommand("mountpoint", "-q", path)
	if err != nil {
		// mountpoint returns exit code 1 for non-mountpoints (not an error)
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		// Other errors (path doesn't exist, permission denied)
		e.log.Debug("mountpoint check failed", "path", path, "error", err, "output", string(out))
		return false, nil // treat check failure as "not mounted" so we try to mount
	}
	return true, nil
}

// MountInfo returns the server:export for a given mountpoint by parsing
// /proc/mounts (Linux). Returns ("", nil) if the path is not found.
func (e *ExecMountChecker) MountInfo(path string) (string, error) {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return "", fmt.Errorf("failed to read /proc/mounts: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		// fields[0] = device (server:export for NFS), fields[1] = mountpoint
		if fields[1] == path {
			return fields[0], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading /proc/mounts: %w", err)
	}
	return "", nil
}

// Mount executes the NFS mount command.
// Requires mount privilege (root or CAP_SYS_ADMIN).
func (e *ExecMountChecker) Mount(server, export, target, options string) error {
	source := fmt.Sprintf("%s:%s", server, export)
	args := []string{"-t", "nfs", "-o", options, source, target}
	e.log.Info("Mounting NFS share", "source", source, "target", target, "options", options)

	out, err := e.runCommand("mount", args...)
	if err != nil {
		return fmt.Errorf("mount %s on %s failed: %w (output: %s)", source, target, err, string(out))
	}
	return nil
}

// Unmount unmounts the given mountpoint.
func (e *ExecMountChecker) Unmount(target string) error {
	e.log.Info("Unmounting", "target", target)
	out, err := e.runCommand("umount", target)
	if err != nil {
		return fmt.Errorf("umount %s failed: %w (output: %s)", target, err, string(out))
	}
	return nil
}

// MkdirAll creates the directory tree for the mountpoint.
func (e *ExecMountChecker) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}
