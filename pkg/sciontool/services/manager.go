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

// Package services manages sidecar process lifecycles for agent containers.
package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

const maxConsecutiveFailures = 3

// Manager manages sidecar service lifecycles.
type Manager struct {
	services    []*managedService
	gracePeriod time.Duration
	mu          sync.Mutex
}

type managedService struct {
	// immutable after construction
	spec     api.ServiceSpec
	logDir   string
	uid, gid int
	username string
	env      []string // merged environment

	// log file handles (opened once, closed on shutdown)
	stdoutFile    *os.File
	stderrFile    *os.File
	lifecycleFile *os.File

	// mu guards all fields below. They are mutated from several goroutines:
	// the supervisor loop (monitorService), the Wait goroutine spawned in
	// start(), the reset-failures goroutine, and the Manager.Shutdown path.
	mu        sync.Mutex
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	done      chan struct{}
	exited    bool
	exitCode  int
	failures  int  // consecutive failure count
	abandoned bool // true after maxConsecutiveFailures consecutive failures
}

// snapshotDone returns the current done channel. Callers receive on it to
// wait for the (current) process to exit; the channel is replaced on every
// restart, so callers must re-snapshot after calling start().
func (svc *managedService) snapshotDone() <-chan struct{} {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.done
}

// snapshotProcess returns the current *exec.Cmd and whether it has exited.
func (svc *managedService) snapshotProcess() (*exec.Cmd, bool) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.cmd, svc.exited
}

// isExited reports whether the current process has exited.
func (svc *managedService) isExited() bool {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.exited
}

// exitCodeLocked returns the most recently recorded exit code.
func (svc *managedService) snapshotExitCode() int {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.exitCode
}

// isAbandoned reports whether the supervisor has given up restarting.
func (svc *managedService) isAbandoned() bool {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.abandoned
}

// markAbandoned marks the service as permanently stopped.
func (svc *managedService) markAbandoned() {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.abandoned = true
}

// recordFailure increments and returns the consecutive failure counter.
func (svc *managedService) recordFailure() int {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.failures++
	return svc.failures
}

// currentFailures returns the current failure counter without mutating it.
func (svc *managedService) currentFailures() int {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return svc.failures
}

// resetFailuresIfRunning clears the failure counter, but only if the current
// process has not yet exited. Called from the delayed reset goroutine.
func (svc *managedService) resetFailuresIfRunning() {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if !svc.exited {
		svc.failures = 0
	}
}

// markExited records the process exit code, flips the exited flag, and
// closes the done channel to wake up waiters. Must be called exactly once
// per start() invocation.
func (svc *managedService) markExited(exitCode int) {
	svc.mu.Lock()
	svc.exitCode = exitCode
	svc.exited = true
	done := svc.done
	svc.mu.Unlock()
	close(done)
}

// New creates a new service Manager with the given grace period for shutdown.
func New(gracePeriod time.Duration) *Manager {
	return &Manager{
		gracePeriod: gracePeriod,
	}
}

// Start launches all services in order, honoring ready checks between them.
func (m *Manager) Start(ctx context.Context, specs []api.ServiceSpec, uid, gid int, username string) error {
	home := os.Getenv("HOME")
	logDir := filepath.Join(home, ".scion", "services", "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("failed to create service log directory: %w", err)
	}

	// Chown the log directory to target user if running as root
	if uid > 0 && gid > 0 {
		_ = os.Chown(filepath.Join(home, ".scion", "services"), uid, gid)
		_ = os.Chown(logDir, uid, gid)
	}

	m.mu.Lock()
	m.services = make([]*managedService, 0, len(specs))
	m.mu.Unlock()

	for _, spec := range specs {
		svc := &managedService{
			spec:     spec,
			done:     make(chan struct{}),
			logDir:   logDir,
			uid:      uid,
			gid:      gid,
			username: username,
			env:      mergeEnv(os.Environ(), spec.Env, uid, username),
		}

		if err := svc.openLogs(); err != nil {
			return fmt.Errorf("service %s: failed to open log files: %w", spec.Name, err)
		}

		// Chown log files if running as non-root target
		if uid > 0 && gid > 0 {
			for _, f := range []*os.File{svc.stdoutFile, svc.stderrFile, svc.lifecycleFile} {
				if f != nil {
					_ = os.Chown(f.Name(), uid, gid)
				}
			}
		}

		if err := svc.start(); err != nil {
			svc.writeLifecycle("Service failed to start: %v", err)
			log.TaggedInfo("service:"+spec.Name, "Failed to start: %v", err)
			return fmt.Errorf("service %s: failed to start: %w", spec.Name, err)
		}

		m.mu.Lock()
		m.services = append(m.services, svc)
		m.mu.Unlock()

		// Wait for ready check if configured
		if spec.ReadyCheck != nil {
			svc.writeLifecycle("Waiting for ready check (%s: %s, timeout: %s)", spec.ReadyCheck.Type, spec.ReadyCheck.Target, spec.ReadyCheck.Timeout)
			if err := waitForReady(spec.ReadyCheck); err != nil {
				svc.writeLifecycle("Ready check failed: %v", err)
				log.TaggedInfo("service:"+spec.Name, "Ready check failed: %v", err)
				// Don't fail startup — log and continue
			} else {
				svc.writeLifecycle("Ready check passed")
				log.TaggedInfo("service:"+spec.Name, "Ready check passed")
			}
		}

		// Start restart monitor in background
		go m.monitorService(ctx, svc)
	}

	return nil
}

// Shutdown gracefully stops all running services.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	services := make([]*managedService, len(m.services))
	copy(services, m.services)
	m.mu.Unlock()

	// Send SIGTERM to all running services
	for _, svc := range services {
		cmd, exited := svc.snapshotProcess()
		if cmd != nil && cmd.Process != nil && !exited {
			svc.writeLifecycle("Service stopped (shutdown)")
			log.TaggedInfo("service:"+svc.spec.Name, "Sending SIGTERM for shutdown")
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
	}

	// Wait for all to exit or context deadline
	allDone := make(chan struct{})
	go func() {
		for _, svc := range services {
			if !svc.isExited() {
				select {
				case <-svc.snapshotDone():
				case <-ctx.Done():
					return
				}
			}
		}
		close(allDone)
	}()

	select {
	case <-allDone:
		// All exited gracefully
	case <-ctx.Done():
		// Grace period expired, SIGKILL remaining
		for _, svc := range services {
			cmd, exited := svc.snapshotProcess()
			if cmd != nil && cmd.Process != nil && !exited {
				log.TaggedInfo("service:"+svc.spec.Name, "Grace period expired, sending SIGKILL")
				_ = cmd.Process.Signal(syscall.SIGKILL)
			}
		}
		// Wait briefly for SIGKILL to take effect
		for _, svc := range services {
			if !svc.isExited() {
				select {
				case <-svc.snapshotDone():
				case <-time.After(2 * time.Second):
				}
			}
		}
	}

	// Close all log files
	for _, svc := range services {
		svc.closeLogs()
	}

	return nil
}

func (svc *managedService) start() error {
	cmd := exec.Command(svc.spec.Command[0], svc.spec.Command[1:]...)
	cmd.Stdout = svc.stdoutFile
	cmd.Stderr = svc.stderrFile
	cmd.Env = svc.env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if svc.uid > 0 && svc.gid > 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid: uint32(svc.uid),
			Gid: uint32(svc.gid),
		}
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan struct{})
	svc.mu.Lock()
	svc.cmd = cmd
	svc.exited = false
	svc.done = done
	svc.mu.Unlock()

	svc.writeLifecycle("Service started (PID %d)", cmd.Process.Pid)
	log.TaggedInfo("service:"+svc.spec.Name, "Started (PID %d)", cmd.Process.Pid)

	// Wait for the process in background
	go func() {
		err := cmd.Wait()
		exitCode := 0
		if err != nil {
			var exitErr *exec.ExitError
			if ok := isExitError(err, &exitErr); ok {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = 1
			}
		}
		svc.writeLifecycle("Service exited (code %d)", exitCode)
		log.TaggedInfo("service:"+svc.spec.Name, "Exited (code %d)", exitCode)
		svc.markExited(exitCode)
	}()

	return nil
}

func (m *Manager) monitorService(ctx context.Context, svc *managedService) {
	for {
		// Wait for the current process to exit
		select {
		case <-svc.snapshotDone():
		case <-ctx.Done():
			return
		}

		if svc.isAbandoned() {
			return
		}

		// Determine restart policy
		restartPolicy := svc.spec.Restart
		if restartPolicy == "" {
			restartPolicy = "no"
		}

		shouldRestart := false
		switch restartPolicy {
		case "no":
			return
		case "on-failure":
			shouldRestart = svc.snapshotExitCode() != 0
		case "always":
			shouldRestart = true
		}

		if !shouldRestart {
			return
		}

		failures := svc.recordFailure()
		if failures >= maxConsecutiveFailures {
			svc.markAbandoned()
			svc.writeLifecycle("Restart limit reached (%d consecutive failures)", maxConsecutiveFailures)
			log.TaggedInfo("service:"+svc.spec.Name, "Restart limit reached (%d consecutive failures)", maxConsecutiveFailures)
			return
		}

		// Exponential backoff: 1s, 2s, 4s
		backoff := time.Duration(1<<uint(failures-1)) * time.Second
		svc.writeLifecycle("Restart attempt %d (backoff %s)", failures, backoff)
		log.TaggedInfo("service:"+svc.spec.Name, "Restart attempt %d (backoff %s)", failures, backoff)

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}

		if err := svc.start(); err != nil {
			svc.writeLifecycle("Restart failed: %v", err)
			log.TaggedInfo("service:"+svc.spec.Name, "Restart failed: %v", err)
			// Count as failure, loop will check limit again
			continue
		}

		// If the new process runs for more than 10 seconds, reset the failure
		// counter. Capture the done channel AFTER svc.start() so this goroutine
		// watches the new process, not the old one.
		newDone := svc.snapshotDone()
		go func() {
			select {
			case <-time.After(10 * time.Second):
				svc.resetFailuresIfRunning()
			case <-newDone:
				// Exited before 10s — failure counter stays
			}
		}()
	}
}

func (svc *managedService) openLogs() error {
	var err error
	flags := os.O_APPEND | os.O_CREATE | os.O_WRONLY

	svc.stdoutFile, err = os.OpenFile(filepath.Join(svc.logDir, svc.spec.Name+".stdout.log"), flags, 0644)
	if err != nil {
		return err
	}
	svc.stderrFile, err = os.OpenFile(filepath.Join(svc.logDir, svc.spec.Name+".stderr.log"), flags, 0644)
	if err != nil {
		return err
	}
	svc.lifecycleFile, err = os.OpenFile(filepath.Join(svc.logDir, svc.spec.Name+".lifecycle.log"), flags, 0644)
	if err != nil {
		return err
	}
	return nil
}

func (svc *managedService) closeLogs() {
	if svc.stdoutFile != nil {
		svc.stdoutFile.Close()
	}
	if svc.stderrFile != nil {
		svc.stderrFile.Close()
	}
	if svc.lifecycleFile != nil {
		svc.lifecycleFile.Close()
	}
}

func (svc *managedService) writeLifecycle(format string, args ...interface{}) {
	if svc.lifecycleFile == nil {
		return
	}
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(svc.lifecycleFile, "[%s] %s\n", timestamp, msg)
}

// mergeEnv merges service-specific env vars into the parent environment.
// Service env vars override parent values.
func mergeEnv(parent []string, serviceEnv map[string]string, uid int, username string) []string {
	env := make([]string, len(parent))
	copy(env, parent)

	// If running as a different user, update HOME/USER/LOGNAME
	if uid > 0 && username != "" {
		home := "/home/" + username
		env = setEnvVar(env, "HOME", home)
		env = setEnvVar(env, "USER", username)
		env = setEnvVar(env, "LOGNAME", username)
	}

	for k, v := range serviceEnv {
		env = setEnvVar(env, k, v)
	}
	return env
}

// setEnvVar sets or replaces an environment variable in a list of KEY=VALUE strings.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// isExitError checks if the error is an *exec.ExitError and sets the target.
func isExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}
