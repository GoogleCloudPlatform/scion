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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

const maxConsecutiveFailures = 3

// ErrPrivilegeDropRequired is returned by managedService.start when the
// service's own requirePrivilegeDrop is set but uid/gid do not both pass
// the same UID>0 && GID>0 predicate the Credential-setting block below
// uses, instead of silently starting the service with no Credential (i.e.
// as whatever this process is, root in production) — the same fail-closed
// principle supervisor.Supervisor.Run's own ErrPrivilegeDropRequired
// applies to the harness child process. In practice, RunInit's own
// requirePrivilegeDropOrFail already refuses to reach Manager.Start at all
// under those conditions, so this is belt-and-suspenders against a future
// caller that calls Start directly, bypassing that check.
var ErrPrivilegeDropRequired = errors.New("privilege drop required but UID/GID were not both set; refusing to start the service as root")

// Manager manages sidecar service lifecycles.
type Manager struct {
	services    []*managedService
	gracePeriod time.Duration
	mu          sync.Mutex
}

type managedService struct {
	// immutable after construction
	spec                 api.ServiceSpec
	logDir               string
	uid, gid             int
	username             string
	env                  []string // merged environment
	requirePrivilegeDrop bool

	// log file handles (opened once, closed on shutdown)
	stdoutFile    *os.File
	stderrFile    *os.File
	lifecycleFile *os.File

	// mu guards all fields below. They are mutated from several goroutines:
	// the supervisor loop (monitorService), the Wait goroutine spawned in
	// start(), the reset-failures goroutine, and the Manager.Shutdown path.
	mu        sync.Mutex
	cmd       *exec.Cmd
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

// snapshotExitCode returns the most recently recorded exit code.
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
//
// Every service's log files are opened (and, if running as root, chowned)
// before that service is started; a service whose logs fail to open (e.g. a
// symlink or hardlink planted at one of its log paths) is dropped on its
// own, logged, and skipped: it does NOT prevent any other service, including
// ones later in specs, from starting. An all-or-nothing "no service starts if any one
// service's logs fail to open" policy would hand a scion-uid process a
// denial-of-service lever against every sidecar merely by planting one
// symlink — exactly the kind of workload-triggerable startup failure the
// hardening in cmd/sciontool/commands/init.go deliberately avoids
// ("a planted symlink must not be able to stop the workload from
// starting"). Log fds already opened for a service that is then dropped
// (either because its own logs failed to open, or because a later service's
// start() call fails and this one never got a chance to start) are always
// closed before Start returns — a dropped service never reaches m.services,
// so Manager.Shutdown would otherwise never close them.
//
// requirePrivilegeDrop is the caller's own opts.RequirePrivilegeDrop (true
// only for substrate); see openLogs' doc comment for what it gates there,
// and managedService.start's Credential block for the exec-time fail-closed
// guarantee it gates here: a service whose uid/gid do not both pass the
// Credential predicate returns ErrPrivilegeDropRequired instead of starting
// with no Credential.
func (m *Manager) Start(ctx context.Context, specs []api.ServiceSpec, uid, gid int, username string, requirePrivilegeDrop bool) error {
	home := os.Getenv("HOME")
	scionDir := filepath.Join(home, ".scion")
	servicesDir := filepath.Join(scionDir, "services")
	logDir := filepath.Join(servicesDir, "logs")

	// EnsureDirNoFollow only creates its own leaf (it requires the parent
	// chain to already exist, unlike os.MkdirAll) — walk the fixed nesting
	// depth under $HOME one level at a time so an as-yet-missing $HOME/.scion
	// doesn't fail the resolution of $HOME/.scion/services below it.
	scionDirFile, err := dirfd.EnsureDirNoFollow(scionDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create .scion directory: %w", err)
	}
	_ = scionDirFile.Close()

	svcDirFile, err := dirfd.EnsureDirNoFollow(servicesDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create service directory: %w", err)
	}
	defer func() { _ = svcDirFile.Close() }()
	if uid > 0 && gid > 0 {
		_ = svcDirFile.Chown(uid, gid)
	}

	logDirFile, err := dirfd.EnsureDirNoFollow(logDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create service log directory: %w", err)
	}
	defer func() { _ = logDirFile.Close() }()
	if uid > 0 && gid > 0 {
		_ = logDirFile.Chown(uid, gid)
	}

	m.mu.Lock()
	m.services = make([]*managedService, 0, len(specs))
	m.mu.Unlock()

	svcs := make([]*managedService, 0, len(specs))
	var openErrs []string
	for _, spec := range specs {
		// Belt-and-suspenders: the authoritative gate is at YAML parse time
		// (cmd/sciontool/commands, right after yaml.Unmarshal — see
		// ValidateServiceName's own doc comment for why), but Start itself
		// must not trust an already-invalid Name either, in case a future
		// or test caller reaches it directly without going through that
		// gate. Drop only this one service, exactly like an open-logs
		// failure below.
		if err := ValidateServiceName(spec.Name); err != nil {
			safeName := SafeNameForLog(spec.Name)
			log.Error("service %s: %v — service will not start", safeName, err)
			openErrs = append(openErrs, fmt.Sprintf("%s: %v", safeName, err))
			continue
		}
		svc := &managedService{
			spec:                 spec,
			done:                 make(chan struct{}),
			logDir:               logDir,
			uid:                  uid,
			gid:                  gid,
			username:             username,
			env:                  mergeEnv(os.Environ(), spec.Env, uid, username),
			requirePrivilegeDrop: requirePrivilegeDrop,
		}

		if err := svc.openLogs(int(logDirFile.Fd()), requirePrivilegeDrop); err != nil {
			// Close whatever this one service managed to open before
			// failing (openLogs opens three files in sequence; a failure on
			// the second or third otherwise leaks the first) and drop only
			// this service — every other service, including ones later in
			// specs, still gets a chance to start. See Start's own doc
			// comment for why an all-or-nothing policy here would be a new
			// denial-of-service lever.
			svc.closeLogs()
			log.Error("service %s: failed to open log files: %v — service will not start", spec.Name, err)
			openErrs = append(openErrs, fmt.Sprintf("%s: %v", spec.Name, err))
			continue
		}

		// Chown log files (fd-based fchown; never a path-based chown that
		// could be redirected by a symlink swapped in after the open) if
		// running as non-root target.
		if uid > 0 && gid > 0 {
			for _, f := range []*os.File{svc.stdoutFile, svc.stderrFile, svc.lifecycleFile} {
				if f != nil {
					_ = f.Chown(uid, gid)
				}
			}
		}

		svcs = append(svcs, svc)
	}

	var startErr error
	for i, svc := range svcs {
		spec := svc.spec
		if err := svc.start(); err != nil {
			svc.writeLifecycle("Service failed to start: %v", err)
			log.TaggedInfo("service:"+spec.Name, "Failed to start: %v", err)
			// This service and every remaining one in svcs already have
			// their log fds open but will never reach m.services (and
			// therefore never get closed by Shutdown) since we're about to
			// return: close them all here instead of leaking them.
			for _, remaining := range svcs[i:] {
				remaining.closeLogs()
			}
			startErr = fmt.Errorf("service %s: failed to start: %w", spec.Name, err)
			break
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

	if startErr != nil {
		return startErr
	}
	if len(openErrs) > 0 {
		return fmt.Errorf("failed to initialize %d service(s): %s", len(openErrs), strings.Join(openErrs, "; "))
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
		// Keeps the runtime-granted nfs shared-dir groups (ptone/scion#3155).
		cmd.SysProcAttr.Credential = suppgroups.Credential(uint32(svc.uid), uint32(svc.gid))
	} else if svc.requirePrivilegeDrop {
		return ErrPrivilegeDropRequired
	}

	// Start and register the child's PID as a single gated step so
	// sciontool init's SIGCHLD reaper cannot observe it as
	// exited-and-unmanaged in the gap between Start() returning and
	// registration (see pkg/sciontool/procreap for why).
	var execToken *procreap.Token
	if err := procreap.Gated(func() error {
		if err := cmd.Start(); err != nil {
			return err
		}
		execToken = procreap.RegisterManagedPID(cmd.Process.Pid)
		return nil
	}); err != nil {
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
		procreap.UnregisterManagedPID(cmd.Process.Pid, execToken)
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

		// Check context again to avoid race condition where backoff expires
		// just as context is cancelled.
		select {
		case <-ctx.Done():
			return
		default:
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

// openLogs opens this service's three log files relative to logDirFd — an
// already-open, symlink-safe fd for svc.logDir (see dirfd.EnsureDirNoFollow)
// — via openat(2) with O_NOFOLLOW, so a symlink planted at any of the leaf
// names is refused rather than followed, and refuses to hand back anything
// that isn't a regular file. Both of those checks apply on every runtime:
// they are behaviour-preserving fd-handling (the file still ends up opened
// and used exactly as a plain os.OpenFile's result would be, just via a
// symlink-safe path) with no legitimate case that depends on the old,
// symlink-following behaviour.
//
// requirePrivilegeDrop (the caller's own opts.RequirePrivilegeDrop, true
// only for substrate) additionally gates a hard-link guard: when true, a
// log path that resolves to a regular file with more than one hard link is
// also refused. A workload process can pre-plant a hard link to a file it
// does not own (hard-linking only needs write access to the directory the
// link is created in, not ownership of the target), so without this guard
// root could be tricked into opening and appending to an unrelated
// (possibly root-owned) file that merely happens to still be a "regular
// file". This is new, security-motivated behaviour, not a compatibility
// fix, so it is scoped to substrate: a legitimately hard-linked log file
// under a non-substrate container's home directory must keep working.
func (svc *managedService) openLogs(logDirFd int, requirePrivilegeDrop bool) error {
	flags := syscall.O_APPEND | syscall.O_CREAT | syscall.O_WRONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK

	var err error
	svc.stdoutFile, err = openLogNoFollow(logDirFd, svc.spec.Name+".stdout.log", flags, 0644, requirePrivilegeDrop)
	if err != nil {
		return err
	}
	svc.stderrFile, err = openLogNoFollow(logDirFd, svc.spec.Name+".stderr.log", flags, 0644, requirePrivilegeDrop)
	if err != nil {
		return err
	}
	svc.lifecycleFile, err = openLogNoFollow(logDirFd, svc.spec.Name+".lifecycle.log", flags, 0644, requirePrivilegeDrop)
	if err != nil {
		return err
	}
	return nil
}

// openLogNoFollow opens name relative to dirFd and refuses to hand back
// anything other than a regular file; when checkNlink is true, it also
// refuses a regular file with more than one hard link (see openLogs' doc
// comment for both).
func openLogNoFollow(dirFd int, name string, flags int, mode os.FileMode, checkNlink bool) (*os.File, error) {
	f, err := dirfd.OpenAt(dirFd, name, flags, mode)
	if err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		_ = f.Close()
		return nil, fmt.Errorf("refusing to open %s: not a regular file", name)
	}
	if checkNlink && st.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("refusing to open %s: hard-linked regular file (Nlink=%d)", name, st.Nlink)
	}
	return f, nil
}

func (svc *managedService) closeLogs() {
	if svc.stdoutFile != nil {
		_ = svc.stdoutFile.Close()
	}
	if svc.stderrFile != nil {
		_ = svc.stderrFile.Close()
	}
	if svc.lifecycleFile != nil {
		_ = svc.lifecycleFile.Close()
	}
}

func (svc *managedService) writeLifecycle(format string, args ...interface{}) {
	if svc.lifecycleFile == nil {
		return
	}
	timestamp := log.Timestamp(time.Now())
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(svc.lifecycleFile, "[%s] %s\n", timestamp, msg)
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
