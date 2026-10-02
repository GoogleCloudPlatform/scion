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
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
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
	runCommand func(ctx context.Context, name string, args ...string) ([]byte, error)
	// geteuid returns the effective uid. Defaults to os.Geteuid; overridden
	// in tests.
	geteuid func() int
}

// NewExecMountChecker creates a production MountChecker.
func NewExecMountChecker(log *slog.Logger) *ExecMountChecker {
	if log == nil {
		log = slog.Default()
	}
	return &ExecMountChecker{
		log:        log,
		runCommand: execRunCommand,
		geteuid:    os.Geteuid,
	}
}

// mountCommandTimeout bounds every mount-related command. mount.nfs retries
// an unreachable server for about two minutes in the foreground; the bound
// keeps a dispatch-time check or a reconcile pass from hanging longer.
const mountCommandTimeout = 90 * time.Second

// execRunCommand runs a command bounded by both parent (for a dispatch,
// the request context) and mountCommandTimeout, and returns its combined
// output.
func execRunCommand(parent context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, mountCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if ctxErr := ctx.Err(); ctxErr != nil {
		if parent.Err() != nil {
			return out, fmt.Errorf("%s cancelled: %w", name, parent.Err())
		}
		return out, fmt.Errorf("%s timed out after %s: %w", name, mountCommandTimeout, ctxErr)
	}
	return out, err
}

// MountPrivilegeError reports that mounting is not possible because the
// broker is not running as root (mount.nfs requires uid 0), so the
// reconciler does not shell out to a command that is bound to fail.
func (e *ExecMountChecker) MountPrivilegeError() error {
	if e.geteuid == nil {
		return nil
	}
	if uid := e.geteuid(); uid != 0 {
		return fmt.Errorf("mounting NFS requires root, and the broker runs as uid %d; "+
			"mount the export outside the broker, or run the broker as root", uid)
	}
	return nil
}

// IsMountpoint returns true if the given path is currently a mountpoint.
// Uses mountpoint(1) which is available on all modern Linux distributions.
func (e *ExecMountChecker) IsMountpoint(ctx context.Context, path string) (bool, error) {
	out, err := e.runCommand(ctx, "mountpoint", "-q", path)
	if err != nil {
		// mountpoint returns exit code 1 for non-mountpoints (not an error)
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return false, nil
		}
		// A timeout usually means a hung (stale) mount at path: report it
		// rather than treat the path as unmounted and mount over it.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return false, err
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
	defer func() { _ = f.Close() }()

	var source string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		// fields[0] = device (server:export for NFS), fields[1] = mountpoint
		// The last entry for a mountpoint is the topmost (visible) mount.
		if fields[1] == path {
			source = fields[0]
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading /proc/mounts: %w", err)
	}
	return source, nil
}

// Mount executes the NFS mount command.
// Requires mount privilege (root or CAP_SYS_ADMIN).
func (e *ExecMountChecker) Mount(ctx context.Context, server, export, target, options string) error {
	if err := e.MountPrivilegeError(); err != nil {
		return err
	}
	source := fmt.Sprintf("%s:%s", server, export)
	args := []string{"-t", "nfs", "-o", options, source, target}
	e.log.Info("Mounting NFS share", "source", source, "target", target, "options", options)

	out, err := e.runCommand(ctx, "mount", args...)
	if err != nil {
		return fmt.Errorf("mount %s on %s failed: %w (output: %s)", source, target, err, string(out))
	}
	return nil
}

// Unmount unmounts the given mountpoint.
func (e *ExecMountChecker) Unmount(ctx context.Context, target string) error {
	if err := e.MountPrivilegeError(); err != nil {
		return err
	}
	e.log.Info("Unmounting", "target", target)
	out, err := e.runCommand(ctx, "umount", target)
	if err != nil {
		return fmt.Errorf("umount %s failed: %w (output: %s)", target, err, string(out))
	}
	return nil
}

// MkdirAll creates the directory tree for the mountpoint.
func (e *ExecMountChecker) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}
