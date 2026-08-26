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

// This file is a workaround for an upstream Cloud Run Sandbox defect:
// `sandbox delete --force` never returns. The deletion IS effective --
// the sandbox really is gone -- but the CLI process hangs indefinitely.
//
// See .design/project-log/defect-sandbox-delete-hang.md for the full
// investigation, captured argv, and control matrix.
//
// KNOWN-BAD BUILD: runsc google-958767651 (spec 1.2.1, 2026-08-04).
//
// EXIT CRITERIA -- remove this file when ALL of the following hold
// on a runsc build NEWER than google-958767651:
//   1. `sandbox delete --force` returns within DefaultDeleteTimeout on a
//      sandbox with a live process (not just idle sandboxes).
//   2. No orphaned `runsc delete` process remains after the command returns.
//   3. The above holds across concurrent deletes (our actual access pattern).
//   4. The self-detecting WARN log ("upstream defect may be fixed") fires
//      on normal delete returns -- this is the primary removal trigger
//      since there is no public bug to watch.
//
// To remove: delete this file, revert Stop()/Delete() in
// cloudrun_sandbox_runtime.go to a plain exec of `sandbox delete --force`,
// and drop the SCION_CLOUDRUN_DELETE_WORKAROUND env var check.

package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultDeleteTimeout is the timeout for sandbox delete --force.
// This value is picked blind -- we have no data on the completion-time
// distribution because the command never completes (platform defect).
// 10 seconds is a conservative guess.
const DefaultDeleteTimeout = 10 * time.Second

// deleteDefectRef identifies the platform defect this file works around.
// Tracked internally by the Cloud Run team -- there is no public issue to cite.
// Observed on runsc google-958767651 (spec 1.2.1, 2026-08-04).
// Evidence and control matrix: .design/project-log/defect-sandbox-delete-hang.md
const deleteDefectRef = "cloudrun sandbox: 'sandbox delete --force' never returns; " +
	"see .design/project-log/defect-sandbox-delete-hang.md"

// deleteWorkaroundEnabled controls whether the timeout/reaper workaround
// is active. Set SCION_CLOUDRUN_DELETE_WORKAROUND=off to bypass.
var deleteWorkaroundEnabled = true

// deleteWorkaroundFixDetected fires a one-time WARN when delete returns
// normally, signaling that the upstream defect may have been fixed and
// this workaround is a candidate for removal.
var deleteWorkaroundFixDetected sync.Once

func init() {
	if os.Getenv("SCION_CLOUDRUN_DELETE_WORKAROUND") == "off" {
		deleteWorkaroundEnabled = false
		slog.Warn("Cloud Run delete workaround DISABLED via SCION_CLOUDRUN_DELETE_WORKAROUND=off",
			"defect", deleteDefectRef)
	}
}

// deleteWithTimeout runs `sandbox delete --force` with a timeout. When the
// command times out (the expected case with the platform bug), the process
// group is killed, orphaned runsc processes are reaped, and the delete is
// treated as successful (the sandbox really is gone despite the hang).
//
// Platform defect: `sandbox delete --force` never returns (see
// defect-sandbox-delete-hang.md). The deletion IS effective -- the sandbox
// really is gone -- but the CLI process hangs indefinitely.
//
// TODO(OQ-16): Every observation of the hang is from serial deletes.
// Fan-out is the actual pattern (fleet teardown). If the hang is
// contention-related, concurrent teardown could be qualitatively worse.
// Explicitly accepted: the timeout bounds the worst case per-sandbox,
// but aggregate behavior under concurrency is unverified.
func (r *CloudRunSandboxRuntime) deleteWithTimeout(ctx context.Context, id string) error {
	timeout := r.deleteTimeout
	if timeout == 0 {
		timeout = DefaultDeleteTimeout
	}

	cmd := exec.CommandContext(ctx, r.bin, "delete", "--force", id)
	// Use a process group so we can kill the entire tree (the sandbox CLI
	// spawns runsc subprocesses that inherit pipe fds).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cloudrun-sandbox: failed to start delete --force: %w", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			slog.Warn("sandbox delete --force returned with error",
				"sandbox", id, "error", err)
		} else {
			deleteWorkaroundFixDetected.Do(func() {
				slog.Warn("sandbox delete --force returned normally -- "+
					"upstream defect may be fixed; this workaround is a candidate for removal",
					"sandbox", id, "defect", deleteDefectRef)
			})
			slog.Info("sandbox delete --force completed normally",
				"sandbox", id)
		}
	case <-time.After(timeout):
		slog.Warn("sandbox delete --force timed out, treating as success",
			"sandbox", id, "timeout", timeout,
			"defect", deleteDefectRef)
		killProcessGroup(cmd)
		<-done // reap the zombie
		// Only reap orphaned runsc processes when the timeout actually fired.
		// If the delete completed normally, there is by definition no orphan.
		// CRITICAL: when the platform bug is fixed, a working delete returns
		// promptly and we must NOT SIGKILL a healthy in-flight operation.
		reapOrphanedRunsc(id)
	case <-ctx.Done():
		killProcessGroup(cmd)
		<-done
		return ctx.Err()
	}

	// Remove from state store.
	r.state.remove(id)
	return nil
}

// killProcessGroup sends SIGKILL to the entire process group of cmd.
// This ensures that child processes (e.g., runsc) are also killed when
// we need to forcibly terminate a hanging delete.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// Kill the entire process group (negative PID).
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// reapOrphanedRunsc scans /proc for orphaned `runsc delete` processes
// targeting the given sandbox and kills them. These orphans are left
// behind when `sandbox delete --force` hangs and we kill its process
// group -- the runsc subprocess may have been reparented to init.
//
// Only called from the timeout branch of deleteWithTimeout -- never when
// the delete completed normally.
func reapOrphanedRunsc(sandboxName string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if isOrphanedRunscProcess(cmdline, sandboxName) {
			argv := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
			slog.Info("reaping orphaned runsc process",
				"sandbox", sandboxName, "pid", pid,
				"cmdline", strings.Join(argv, " "))
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Kill()
				waitDone := make(chan struct{})
				go func() {
					_, _ = proc.Wait()
					close(waitDone)
				}()
				select {
				case <-waitDone:
				case <-time.After(2 * time.Second):
				}
			}
		}
	}
}

// isOrphanedRunscProcess checks whether raw /proc/<pid>/cmdline bytes
// represent an orphaned `runsc delete` process for the given sandbox.
//
// /proc/<pid>/cmdline is NUL-separated. We split on NUL to get exact argv
// fields and match on:
//   - argv[0] basename contains "runsc"
//   - argv contains "delete"
//   - the last element is exactly sandboxName (not a substring match)
//
// Captured orphan argv from defect-sandbox-delete-hang.md section 3:
//
//	/usr/local/gcp/bin/runsc --platform=xemu --platform_device_path=/dev/xemu \
//	  --root=/tmp/runsc-root --ignore-cgroups --TESTONLY-unsafe-nonroot \
//	  --overlay2=root:memory --network=none delete --force <sandbox-id>
//
// The sandbox name is always the final argument. We match on exact equality
// of argv's last element rather than substring matching, which prevents
// "app" from matching "my-app" without depending on filesystem layout.
func isOrphanedRunscProcess(cmdline []byte, sandboxName string) bool {
	// Split on NUL, filtering empty trailing entries from the
	// kernel's NUL-terminated format.
	parts := strings.Split(string(cmdline), "\x00")
	var argv []string
	for _, p := range parts {
		if p != "" {
			argv = append(argv, p)
		}
	}
	if len(argv) < 3 {
		return false
	}
	return strings.Contains(filepath.Base(argv[0]), "runsc") &&
		slices.Contains(argv, "delete") &&
		argv[len(argv)-1] == sandboxName
}
