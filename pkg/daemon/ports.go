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

package daemon

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DetectOccupiedPorts probes each port by attempting to bind it.
// Returns the subset of ports that are already in use.
func DetectOccupiedPorts(ports []int) []int {
	var occupied []int
	for _, port := range ports {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			occupied = append(occupied, port)
			continue
		}
		_ = ln.Close()
	}
	return occupied
}

// FindPIDOnPort returns the PID of the process listening on the given TCP port,
// or 0 if no process is found. Uses lsof (macOS/Linux).
func FindPIDOnPort(port int) int {
	out, err := exec.Command("lsof", "-ti", fmt.Sprintf(":%d", port)).Output()
	if err != nil {
		return 0
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return 0
	}
	pid, err := strconv.Atoi(lines[0])
	if err != nil {
		return 0
	}
	return pid
}

// ForceKillPort finds the process listening on the given port and kills it.
// Sends SIGTERM first, waits up to 3 seconds, then SIGKILL if still running.
// Returns the PID that was killed, or 0 if no process was found.
func ForceKillPort(port int) (int, error) {
	pid := FindPIDOnPort(port)
	if pid == 0 {
		return 0, nil
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return pid, fmt.Errorf("failed to send SIGTERM to PID %d: %w", pid, err)
	}

	// Wait up to 3 seconds for the process to exit.
	for i := 0; i < 12; i++ {
		time.Sleep(250 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err != nil {
			return pid, nil
		}
	}

	// Still running — escalate to SIGKILL.
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if err == syscall.ESRCH {
			return pid, nil
		}
		return pid, fmt.Errorf("failed to send SIGKILL to PID %d: %w", pid, err)
	}
	return pid, nil
}
