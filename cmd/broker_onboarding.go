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

package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
)

// shouldOfferProjectProvider reports whether runtime-broker register should
// offer to add the newly registered broker as a provider for the current
// project. It needs a hub-linked project with hub mode on, and the project
// must not be the global directory: "global" is a local pseudo-project and is
// never linked to, or created on, the hub (ptone/scion#3534).
func shouldOfferProjectProvider(projectID string, hubEnabled, isGlobal bool) bool {
	return projectID != "" && hubEnabled && !isGlobal
}

// Bounds for the brief wait runtime-broker status does right after a broker
// (re)start, so it reports the hub connection the broker is about to have
// rather than "unknown" (ptone/scion#3536).
const (
	// brokerStatusPollTimeout caps the whole wait.
	brokerStatusPollTimeout = 5 * time.Second
	// brokerStatusPollInterval is the delay between polls.
	brokerStatusPollInterval = 500 * time.Millisecond
	// brokerRecentStartWindow is how long after start a broker counts as
	// still settling: only then does status wait for its hub connections.
	brokerRecentStartWindow = 30 * time.Second
)

// brokerStatusSleep is time.Sleep; tests replace it.
var brokerStatusSleep = time.Sleep

// brokerRecentlyStarted reports whether a broker /healthz uptime (a Go
// duration string) is within brokerRecentStartWindow. An unparseable uptime
// counts as not recent, so status never waits on an unknown broker.
func brokerRecentlyStarted(uptime string) bool {
	d, err := time.ParseDuration(uptime)
	return err == nil && d < brokerRecentStartWindow
}

// pollUntil calls probe until it returns true or timeout elapses, sleeping
// interval between calls. It reports the last probe result.
func pollUntil(probe func() bool, timeout, interval time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if probe() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		brokerStatusSleep(interval)
	}
}

// brokerHubConnectionsSettled reports whether live has a settled status for
// every named connection. A missing connection, an empty status, or a
// disconnected or reconnecting one is still settling right after a start.
func brokerHubConnectionsSettled(live *BrokerHubConnectionsResponse, names []string) bool {
	if live == nil {
		return false
	}
	byName := make(map[string]string, len(live.Connections))
	for _, c := range live.Connections {
		byName[c.Name] = c.Status
	}
	for _, n := range names {
		switch byName[n] {
		case "", "disconnected", "reconnecting":
			return false
		}
	}
	return true
}

// pollBrokerHubConnections queries the live hub connections until all named
// connections are settled or timeout elapses, and returns the last answer.
func pollBrokerHubConnections(query func() *BrokerHubConnectionsResponse, names []string, timeout, interval time.Duration) *BrokerHubConnectionsResponse {
	var live *BrokerHubConnectionsResponse
	pollUntil(func() bool {
		live = query()
		return brokerHubConnectionsSettled(live, names)
	}, timeout, interval)
	return live
}

// brokerHubConnectionDisplayStatus is the status runtime-broker status prints
// for a registered hub connection: the broker's live status when it has one,
// "pending" while a running broker has not reported the connection yet (it
// connects on its first heartbeat), and "unknown" when no broker answers.
func brokerHubConnectionDisplayStatus(live map[string]string, name string, serverRunning bool) string {
	if s := live[name]; s != "" {
		return s
	}
	if serverRunning {
		return "pending (waiting for the broker's first heartbeat)"
	}
	return "unknown (broker server not responding)"
}

// removeDirIfEmpty removes dir when it exists and has no entries. It reports
// whether it removed the directory.
func removeDirIfEmpty(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if len(entries) > 0 {
		return false, nil
	}
	if err := os.Remove(dir); err != nil {
		return false, err
	}
	return true, nil
}

// brokerLocalStatePaths lists the broker-local state that
// 'runtime-broker deregister --purge-local' removes for brokerID: the
// standalone broker daemon log, the broker's state directory and the broker
// template cache (ptone/scion#3538). It never lists settings files, other
// hubs' credentials, another broker ID's state directory, or the hub-id file
// (that belongs to a local hub, not to the broker). scionHome is the
// directory the broker keeps its state under (~/.scion).
func brokerLocalStatePaths(globalDir, scionHome, brokerID string) []string {
	paths := []string{daemon.GetLogPath(globalDir)}
	if brokerID != "" && brokerID == filepath.Base(brokerID) && brokerID != "." && brokerID != ".." {
		paths = append(paths, filepath.Join(scionHome, "runtime-broker-state", brokerID))
	}
	return append(paths, filepath.Join(scionHome, "cache", "templates"))
}

// existingPaths returns the entries of paths that exist.
func existingPaths(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// purgeBrokerLocalState removes paths and reports each removal to out.
func purgeBrokerLocalState(paths []string, out io.Writer) error {
	var errs []error
	for _, p := range existingPaths(paths) {
		if err := os.RemoveAll(p); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove %s: %w", p, err))
			continue
		}
		_, _ = fmt.Fprintf(out, "Removed %s\n", p)
	}
	return errors.Join(errs...)
}

// cleanupAfterDeregister handles local broker state once deregister has
// removed a registration (brokerID; empty when there was none). It removes
// the credentials directory when it is left empty. With purge it also removes
// brokerLocalStatePaths, but only when no hub connections remain (the state
// is shared by all of them) and no broker is running (it is in use);
// otherwise, and without purge, it lists what is left behind.
func cleanupAfterDeregister(out io.Writer, credsDir string, remaining int, brokerRunning, purge bool, statePaths []string) error {
	if remaining == 0 && credsDir != "" {
		if removed, err := removeDirIfEmpty(credsDir); err != nil {
			_, _ = fmt.Fprintf(out, "Warning: failed to remove empty %s: %v\n", credsDir, err)
		} else if removed {
			_, _ = fmt.Fprintf(out, "Removed empty %s\n", credsDir)
		}
	}
	left := existingPaths(statePaths)
	if len(left) == 0 {
		return nil
	}
	switch {
	case purge && remaining > 0:
		_, _ = fmt.Fprintf(out, "Skipped --purge-local: %d other hub connection(s) remain and share the local broker state.\n", remaining)
	case purge && brokerRunning:
		_, _ = fmt.Fprintln(out, "Skipped --purge-local: the broker is running. Stop it with 'scion runtime-broker stop', then run 'scion runtime-broker deregister --purge-local'.")
	case purge:
		return purgeBrokerLocalState(left, out)
	default:
		_, _ = fmt.Fprintln(out, "Local broker state left in place:")
		for _, p := range left {
			_, _ = fmt.Fprintf(out, "  %s\n", p)
		}
		if remaining == 0 {
			_, _ = fmt.Fprintln(out, "Remove it with 'scion runtime-broker deregister --purge-local' once the broker is stopped.")
		}
	}
	return nil
}
