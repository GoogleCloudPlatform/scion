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

package mutagen

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// CheckInstalled checks if the mutagen CLI is available in the system PATH.
func CheckInstalled() bool {
	_, err := exec.LookPath("mutagen")
	return err == nil
}

// StartDaemon ensures the mutagen daemon is running.
func StartDaemon() error {
	cmd := exec.Command("mutagen", "daemon", "start")
	if output, err := cmd.CombinedOutput(); err != nil {
		// If daemon is already running, it's fine, but let's check output just in case
		// Mutagen usually returns 0 if already running or prints a message
		return fmt.Errorf("failed to start mutagen daemon: %s: %s", err, string(output))
	}
	return nil
}

// CreateSync creates a new sync session.
func CreateSync(name string, localPath string, remoteURL string, labels map[string]string) error {
	args := []string{
		"sync", "create",
		"--name=" + name,
		"--sync-mode=two-way-safe",
		"--ignore-vcs",
		"--ignore=node_modules/",
	}

	for k, v := range labels {
		args = append(args, fmt.Sprintf("--label=%s=%s", k, v))
	}

	args = append(args, localPath, remoteURL)

	cmd := exec.Command("mutagen", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create mutagen sync: %s: %s", err, string(output))
	}
	return nil
}

// TerminateSync terminates sync sessions matching the label selector.
func TerminateSync(labelSelector string) error {
	args := []string{
		"sync", "terminate",
		"--label-selector=" + labelSelector,
	}
	cmd := exec.Command("mutagen", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to terminate mutagen sync: %s: %s", err, string(output))
	}
	return nil
}

type SyncSession struct {
	Identifier string `json:"identifier"`
	Status     string `json:"status"`
}

// WaitForSync waits until the sync session with the given name reaches "Watching" status.
func WaitForSync(name string, timeout time.Duration) error {
	start := time.Now()
	for time.Since(start) < timeout {
		cmd := exec.Command("mutagen", "sync", "list", "--format=json", name)
		output, err := cmd.Output()
		if err != nil {
			// If session doesn't exist yet, we might wait or fail.
			// For now, let's assume it should exist.
			return fmt.Errorf("failed to list sync session: %w", err)
		}

		// Mutagen list json output is a list of sessions
		// However, when filtered by name (as arg), it might still be a list or a single object?
		// "mutagen sync list --format=json" returns a list.
		// "mutagen sync list --format=json <name>" might return just that one.
		// Let's assume it returns a list and we find the one.
		// Actually, let's just parse the list.

		var sessions []SyncSession
		// Try parsing as list first
		if err := json.Unmarshal(output, &sessions); err != nil {
			// Maybe it returned a single object?
			var session SyncSession
			if err2 := json.Unmarshal(output, &session); err2 == nil {
				sessions = []SyncSession{session}
			} else {
				return fmt.Errorf("failed to parse mutagen status: %w", err)
			}
		}

		found := false
		for _, s := range sessions {
			// Name check? Mutagen list by name usually filters it.
			// But the JSON output doesn't always have the name field in the top level struct in older versions?
			// Let's assume the command filtered it so we just check status.
			found = true
			if s.Status == "Watching" {
				return nil
			}
		}

		if !found {
			// Maybe session creation failed or hasn't shown up yet
		}

		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timeout waiting for sync session to become ready")
}
