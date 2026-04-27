/*
Copyright 2025 The Scion Authors.
*/

// Package supervisor provides process lifecycle management for sciontool init.
// It handles spawning child processes, signal forwarding, and graceful shutdown.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// ErrNoCommand is returned when no command is specified for the supervisor to run.
var ErrNoCommand = errors.New("no command specified")

// Config holds configuration for the Supervisor.
type Config struct {
	// GracePeriod is the time to wait after SIGTERM before sending SIGKILL.
	GracePeriod time.Duration
	// UID is the target UID for the child process (0 = no change)
	UID int
	// GID is the target GID for the child process (0 = no change)
	GID int
	// Username is the target username for the child process (used to set HOME, USER, LOGNAME)
	Username string
	// Rootless indicates the container is running in a rootless user namespace
	// (e.g. rootless Podman). When true, the supervisor skips the credential
	// drop (UID 0 inside the container IS the unprivileged host user) but
	// still sets HOME/USER/LOGNAME to the Username so harnesses find their
	// config in the right place.
	Rootless bool
	// EnvOverlay are additional environment variables produced by the harness
	// pre-start provisioner (e.g. resolved API keys read from secret files).
	// Existing entries in the runtime environment win on conflict; overlay
	// values only fill keys that are not already set.
	EnvOverlay map[string]string
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		GracePeriod: 10 * time.Second,
	}
}

// Supervisor manages child process lifecycle including signal forwarding
// and graceful shutdown.
type Supervisor struct {
	config Config
	cmd    *exec.Cmd

	// mu protects the process state
	mu        sync.Mutex
	started   bool
	exited    bool
	exitCode  int
	exitError error

	// done is closed when the child process exits
	done chan struct{}
}

// New creates a new Supervisor with the given configuration.
func New(config Config) *Supervisor {
	return &Supervisor{
		config: config,
		done:   make(chan struct{}),
	}
}

// Run starts and supervises the given command until it exits or the context
// is cancelled. It returns the exit code of the child process.
func (s *Supervisor) Run(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return 1, ErrNoCommand
	}

	// Create the child process
	s.cmd = exec.Command(args[0], args[1:]...)
	s.cmd.Stdin = os.Stdin
	s.cmd.Stdout = os.Stdout
	s.cmd.Stderr = os.Stderr

	// Start in a new process group so we can signal the whole group
	s.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Drop privileges if UID/GID specified (skip in rootless mode where
	// UID 0 inside the container is already the unprivileged host user).
	if s.config.UID > 0 && s.config.GID > 0 {
		s.cmd.SysProcAttr.Credential = &syscall.Credential{
			Uid: uint32(s.config.UID),
			Gid: uint32(s.config.GID),
		}
		log.Debug("Child will run as UID=%d, GID=%d", s.config.UID, s.config.GID)
	}

	// Set the child's user environment when dropping privileges OR in
	// rootless mode. In rootless containers, init runs as UID 0 (which
	// maps to the unprivileged host user), so no Credential is needed,
	// but HOME/USER/LOGNAME must still point to the scion user's home
	// so harnesses find their configuration.
	if s.config.Username != "" && (s.config.UID > 0 || s.config.Rootless) {
		home := "/home/" + s.config.Username
		env := os.Environ()
		env = setEnvVar(env, "HOME", home)
		env = setEnvVar(env, "USER", s.config.Username)
		env = setEnvVar(env, "LOGNAME", s.config.Username)
		s.cmd.Env = env
		log.Debug("Child env: HOME=%s, USER=%s, LOGNAME=%s", home, s.config.Username, s.config.Username)
	}

	// Fix ownership of the home directory when dropping privileges.
	// Pre-start hooks run as root and may create files (e.g. .claude/, agent-info.json)
	// that the child process needs to write to. Without this, the child (running as
	// UID/GID from the credential drop) gets permission denied on its own home.
	if s.config.UID > 0 && s.config.GID > 0 && s.config.Username != "" {
		home := "/home/" + s.config.Username
		err := chownRecursive(home, s.config.UID, s.config.GID)
		if err != nil {
			log.Error("Failed to chown home directory %s: %v", home, err)
		} else {
			log.Debug("Chowned %s to %d:%d", home, s.config.UID, s.config.GID)
		}
	}

	// Apply SCION_EXTRA_PATH: prepend its value to PATH, then remove it from env.
	// Initialize s.cmd.Env from os.Environ() if the privilege-drop block above didn't set it.
	if s.cmd.Env == nil {
		s.cmd.Env = os.Environ()
	}
	if extraPath := getEnvVar(s.cmd.Env, "SCION_EXTRA_PATH"); extraPath != "" {
		currentPath := getEnvVar(s.cmd.Env, "PATH")
		var newPath string
		if currentPath != "" {
			newPath = extraPath + ":" + currentPath
		} else {
			newPath = extraPath
		}
		s.cmd.Env = setEnvVar(s.cmd.Env, "PATH", newPath)
		s.cmd.Env = removeEnvVar(s.cmd.Env, "SCION_EXTRA_PATH")
		log.Debug("Applied SCION_EXTRA_PATH: PATH=%s", newPath)
	}

	// Merge harness-generated env overlay. Runtime env wins on conflict so a
	// container-script harness cannot mask a value set by the broker/CLI.
	if len(s.config.EnvOverlay) > 0 {
		before := len(s.cmd.Env)
		s.cmd.Env = mergeEnvOverlay(s.cmd.Env, s.config.EnvOverlay)
		log.Debug("Applied harness env overlay: %d entries (added %d)", len(s.config.EnvOverlay), len(s.cmd.Env)-before)
	}

	if err := s.cmd.Start(); err != nil {
		return 1, fmt.Errorf("failed to start command: %w", err)
	}
	log.Debug("Started child process %d: %v", s.cmd.Process.Pid, args)

	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	// Wait for the child in a goroutine
	go s.waitForChild()

	// Wait for either context cancellation or child exit
	select {
	case <-ctx.Done():
		log.Info("Context cancelled, initiating graceful shutdown")
		return s.shutdown()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		log.Debug("Child process %d exited naturally", s.cmd.Process.Pid)
		return s.exitCode, s.exitError
	}
}

// Signal sends a signal to the child process.
func (s *Supervisor) Signal(sig os.Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started || s.exited || s.cmd.Process == nil {
		return nil
	}

	return s.cmd.Process.Signal(sig)
}

// waitForChild waits for the child process to exit and records its exit status.
func (s *Supervisor) waitForChild() {
	err := s.cmd.Wait()

	s.mu.Lock()
	s.exited = true
	s.exitError = err

	if err == nil {
		s.exitCode = 0
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			s.exitCode = exitErr.ExitCode()
			s.exitError = nil // Exit with non-zero is not an error condition
		} else {
			s.exitCode = 1
		}
	}
	s.mu.Unlock()

	close(s.done)
}

// shutdown performs a graceful shutdown of the child process.
func (s *Supervisor) shutdown() (int, error) {
	s.mu.Lock()
	if s.exited {
		exitCode := s.exitCode
		exitErr := s.exitError
		s.mu.Unlock()
		return exitCode, exitErr
	}
	s.mu.Unlock()

	log.Info("Sending SIGTERM to child process group")
	// Send SIGTERM first
	if err := s.Signal(syscall.SIGTERM); err != nil {
		// If we can't signal, try to get exit status anyway
		s.mu.Lock()
		if s.exited {
			exitCode := s.exitCode
			exitErr := s.exitError
			s.mu.Unlock()
			return exitCode, exitErr
		}
		s.mu.Unlock()
		return 1, fmt.Errorf("failed to send SIGTERM: %w", err)
	}

	// Wait for graceful exit or timeout
	select {
	case <-s.done:
		log.Info("Child process exited gracefully after SIGTERM")
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exitCode, s.exitError
	case <-time.After(s.config.GracePeriod):
		log.Info("Grace period %s expired, sending SIGKILL to child process group", s.config.GracePeriod)
		// Grace period expired, force kill
		if err := s.Signal(syscall.SIGKILL); err != nil {
			s.mu.Lock()
			if s.exited {
				exitCode := s.exitCode
				exitErr := s.exitError
				s.mu.Unlock()
				return exitCode, exitErr
			}
			s.mu.Unlock()
		}
		// Wait for process to actually exit after SIGKILL
		<-s.done
		log.Info("Child process terminated with SIGKILL")
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exitCode, s.exitError
	}
}

// Done returns a channel that is closed when the child process exits.
func (s *Supervisor) Done() <-chan struct{} {
	return s.done
}

// ExitCode returns the exit code of the child process.
// Only valid after Done() is closed.
func (s *Supervisor) ExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
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

// getEnvVar returns the value of an environment variable from a list of KEY=VALUE strings.
// Returns empty string if the key is not found.
func getEnvVar(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return e[len(prefix):]
		}
	}
	return ""
}

// removeEnvVar removes an environment variable from a list of KEY=VALUE strings.
func removeEnvVar(env []string, key string) []string {
	prefix := key + "="
	result := env[:0:0]
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			continue
		}
		result = append(result, e)
	}
	return result
}

// mergeEnvOverlay folds overlay key/value pairs into env. Existing entries
// in env take precedence so runtime/CLI-provided env vars are never masked
// by harness output.
func mergeEnvOverlay(env []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return env
	}
	taken := make(map[string]struct{}, len(env))
	for _, e := range env {
		if i := indexByte(e, '='); i > 0 {
			taken[e[:i]] = struct{}{}
		}
	}
	// Deterministic order for reproducibility.
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	for _, k := range keys {
		if _, ok := taken[k]; ok {
			continue
		}
		env = append(env, k+"="+overlay[k])
	}
	return env
}

// indexByte avoids the strings import for a tight inner loop.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// chownRecursive changes ownership of a directory and all its contents.
func chownRecursive(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}
