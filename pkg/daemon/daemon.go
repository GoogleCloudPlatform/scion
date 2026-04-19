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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const (
	// PIDFile is the default name for the broker PID file.
	// Deprecated: Use PIDFileName instead.
	PIDFile = "broker.pid"
	// LogFile is the default name for the broker log file.
	// Deprecated: Use LogFileName instead.
	LogFile = "broker.log"
)

var (
	// ErrAlreadyRunning indicates the daemon is already running.
	ErrAlreadyRunning = errors.New("daemon is already running")
	// ErrNotRunning indicates the daemon is not running.
	ErrNotRunning = errors.New("daemon is not running")
)

// PIDFileName returns the PID filename for the given component (e.g. "server" -> "server.pid").
func PIDFileName(component string) string {
	return component + ".pid"
}

// LogFileName returns the log filename for the given component (e.g. "server" -> "server.log").
func LogFileName(component string) string {
	return component + ".log"
}

// StartComponent launches a component as a background daemon using the given component name
// for PID/log file naming.
func StartComponent(component, executable string, args []string, globalDir string) error {
	running, _, err := StatusComponent(component, globalDir)
	if err != nil && !errors.Is(err, ErrNotRunning) {
		return fmt.Errorf("failed to check status: %w", err)
	}
	if running {
		return ErrAlreadyRunning
	}

	logPath := filepath.Join(globalDir, LogFileName(component))

	logDir := filepath.Dir(logPath)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	cmd := exec.Command(executable, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = globalDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	if err := WritePIDComponent(component, globalDir, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		logFile.Close()
		return fmt.Errorf("failed to write PID file: %w", err)
	}

	// Background reaper. This goroutine is intentionally untracked: a daemon
	// starter by definition fire-and-forgets the child, and the goroutine's
	// lifetime is bounded by the child process lifetime. We surface any Wait
	// error (which may carry OS-level exit status) so operators can tell a
	// clean exit from an unexpected one.
	go func() {
		defer logFile.Close()
		if err := cmd.Wait(); err != nil {
			slog.Warn("Daemon component exited with error",
				"component", component,
				"pid", cmd.Process.Pid,
				"error", err)
		}
	}()

	return nil
}

// StopComponent terminates a running daemon by reading its PID and sending SIGTERM.
func StopComponent(component, globalDir string) error {
	pid, err := ReadPIDComponent(component, globalDir)
	if err != nil {
		return ErrNotRunning
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		_ = RemovePIDComponent(component, globalDir)
		return ErrNotRunning
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		_ = RemovePIDComponent(component, globalDir)
		return ErrNotRunning
	}

	if err := RemovePIDComponent(component, globalDir); err != nil {
		return fmt.Errorf("failed to remove PID file: %w", err)
	}

	return nil
}

// StatusComponent checks if a component daemon is running.
func StatusComponent(component, globalDir string) (bool, int, error) {
	pid, err := ReadPIDComponent(component, globalDir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, ErrNotRunning
		}
		return false, 0, err
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		_ = RemovePIDComponent(component, globalDir)
		return false, 0, ErrNotRunning
	}

	if err := process.Signal(syscall.Signal(0)); err != nil {
		_ = RemovePIDComponent(component, globalDir)
		return false, pid, ErrNotRunning
	}

	return true, pid, nil
}

// WritePIDComponent writes the PID to the component-specific PID file.
func WritePIDComponent(component, globalDir string, pid int) error {
	pidPath := filepath.Join(globalDir, PIDFileName(component))
	return os.WriteFile(pidPath, []byte(strconv.Itoa(pid)), 0644)
}

// ReadPIDComponent reads the PID from the component-specific PID file.
func ReadPIDComponent(component, globalDir string) (int, error) {
	pidPath := filepath.Join(globalDir, PIDFileName(component))
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

// RemovePIDComponent removes the component-specific PID file.
func RemovePIDComponent(component, globalDir string) error {
	pidPath := filepath.Join(globalDir, PIDFileName(component))
	return os.Remove(pidPath)
}

// WaitForExitComponent polls until the daemon process has exited or the timeout is reached.
func WaitForExitComponent(component, globalDir string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, _, _ := StatusComponent(component, globalDir)
		if !running {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("daemon process did not exit within %s", timeout)
}

// GetLogPathComponent returns the path to the component-specific log file.
func GetLogPathComponent(component, globalDir string) string {
	return filepath.Join(globalDir, LogFileName(component))
}

// GetPIDPathComponent returns the path to the component-specific PID file.
func GetPIDPathComponent(component, globalDir string) string {
	return filepath.Join(globalDir, PIDFileName(component))
}

// ArgsFileName returns the saved-args filename for the given component (e.g. "server" -> "server-args.json").
func ArgsFileName(component string) string {
	return component + "-args.json"
}

// SaveArgs persists the daemon launch arguments so that restart can re-use them.
func SaveArgs(component, globalDir string, args []string) error {
	argsPath := filepath.Join(globalDir, ArgsFileName(component))
	data, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("failed to marshal args: %w", err)
	}
	return os.WriteFile(argsPath, data, 0644)
}

// LoadArgs reads previously saved daemon launch arguments.
// Returns nil if no saved args exist.
func LoadArgs(component, globalDir string) ([]string, error) {
	argsPath := filepath.Join(globalDir, ArgsFileName(component))
	data, err := os.ReadFile(argsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var args []string
	if err := json.Unmarshal(data, &args); err != nil {
		return nil, fmt.Errorf("failed to unmarshal args: %w", err)
	}
	return args, nil
}

// RemoveArgs removes the saved args file for a component.
func RemoveArgs(component, globalDir string) error {
	argsPath := filepath.Join(globalDir, ArgsFileName(component))
	err := os.Remove(argsPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// --- Legacy broker-specific functions (delegate to component-based API) ---

// Start launches the broker as a background daemon.
func Start(executable string, args []string, globalDir string) error {
	return StartComponent("broker", executable, args, globalDir)
}

// Stop terminates a running broker daemon.
func Stop(globalDir string) error {
	return StopComponent("broker", globalDir)
}

// Status checks if the broker daemon is running.
func Status(globalDir string) (bool, int, error) {
	return StatusComponent("broker", globalDir)
}

// WritePID writes the PID to the broker PID file.
func WritePID(globalDir string, pid int) error {
	return WritePIDComponent("broker", globalDir, pid)
}

// ReadPID reads the PID from the broker PID file.
func ReadPID(globalDir string) (int, error) {
	return ReadPIDComponent("broker", globalDir)
}

// RemovePID removes the broker PID file.
func RemovePID(globalDir string) error {
	return RemovePIDComponent("broker", globalDir)
}

// WaitForExit polls until the broker daemon process has exited or the timeout is reached.
func WaitForExit(globalDir string, timeout time.Duration) error {
	return WaitForExitComponent("broker", globalDir, timeout)
}

// GetLogPath returns the path to the broker log file.
func GetLogPath(globalDir string) string {
	return GetLogPathComponent("broker", globalDir)
}

// GetPIDPath returns the path to the broker PID file.
func GetPIDPath(globalDir string) string {
	return GetPIDPathComponent("broker", globalDir)
}
