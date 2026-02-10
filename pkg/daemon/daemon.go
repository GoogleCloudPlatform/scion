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

// Package daemon provides utilities for running scion components as background daemons.
package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

const (
	// PIDFile is the default name for the broker PID file.
	PIDFile = "broker.pid"
	// LogFile is the default name for the broker log file.
	LogFile = "broker.log"
)

var (
	// ErrAlreadyRunning indicates the daemon is already running.
	ErrAlreadyRunning = errors.New("daemon is already running")
	// ErrNotRunning indicates the daemon is not running.
	ErrNotRunning = errors.New("daemon is not running")
)

// Start launches the broker as a background daemon.
// It creates a new process that runs independently of the parent.
// The PID is written to the PID file in globalDir.
// Stdout/stderr are redirected to the log file in globalDir.
func Start(executable string, args []string, globalDir string) error {
	// Check if already running
	running, _, err := Status(globalDir)
	if err != nil && !errors.Is(err, ErrNotRunning) {
		return fmt.Errorf("failed to check status: %w", err)
	}
	if running {
		return ErrAlreadyRunning
	}

	// Prepare log file
	logPath := filepath.Join(globalDir, LogFile)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	// Create the command
	cmd := exec.Command(executable, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = globalDir

	// Detach from parent process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Start the process
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Write PID file
	if err := WritePID(globalDir, cmd.Process.Pid); err != nil {
		// Try to kill the process if we can't write the PID file
		_ = cmd.Process.Kill()
		logFile.Close()
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	// Don't wait for the process - it's a daemon
	// The logFile will stay open in the child process
	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	return nil
}

// Stop terminates a running daemon by reading its PID and sending SIGTERM.
func Stop(globalDir string) error {
	pid, err := ReadPID(globalDir)
	if err != nil {
		return ErrNotRunning
	}

	// Find the process
	process, err := os.FindProcess(pid)
	if err != nil {
		_ = RemovePID(globalDir)
		return ErrNotRunning
	}

	// Send SIGTERM for graceful shutdown
	if err := process.Signal(syscall.SIGTERM); err != nil {
		// Process might already be dead
		_ = RemovePID(globalDir)
		return ErrNotRunning
	}

	// Remove PID file after stopping
	if err := RemovePID(globalDir); err != nil {
		return fmt.Errorf("failed to remove PID file: %w", err)
	}

	return nil
}

// Status checks if the daemon is running.
// Returns (running, pid, error).
func Status(globalDir string) (bool, int, error) {
	pid, err := ReadPID(globalDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, ErrNotRunning
		}
		return false, 0, err
	}

	// Check if process is running
	process, err := os.FindProcess(pid)
	if err != nil {
		_ = RemovePID(globalDir)
		return false, 0, ErrNotRunning
	}

	// On Unix, FindProcess always succeeds, so we need to send signal 0 to check
	if err := process.Signal(syscall.Signal(0)); err != nil {
		// Process is not running, clean up stale PID file
		_ = RemovePID(globalDir)
		return false, pid, ErrNotRunning
	}

	return true, pid, nil
}

// WritePID writes the PID to the PID file in globalDir.
func WritePID(globalDir string, pid int) error {
	pidPath := filepath.Join(globalDir, PIDFile)
	return os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644)
}

// ReadPID reads the PID from the PID file in globalDir.
func ReadPID(globalDir string) (int, error) {
	pidPath := filepath.Join(globalDir, PIDFile)
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0, err
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("invalid PID in file: %w", err)
	}

	return pid, nil
}

// RemovePID removes the PID file from globalDir.
func RemovePID(globalDir string) error {
	pidPath := filepath.Join(globalDir, PIDFile)
	return os.Remove(pidPath)
}

// GetLogPath returns the path to the daemon log file.
func GetLogPath(globalDir string) string {
	return filepath.Join(globalDir, LogFile)
}

// GetPIDPath returns the path to the daemon PID file.
func GetPIDPath(globalDir string) string {
	return filepath.Join(globalDir, PIDFile)
}
