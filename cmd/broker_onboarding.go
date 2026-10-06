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
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// brokerFileRecent reports whether path (a daemon PID file, written when
// the daemon starts) was modified within brokerRecentStartWindow.
func brokerFileRecent(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && time.Since(fi.ModTime()) < brokerRecentStartWindow
}

// statusWaitBudget is the single time budget runtime-broker status may spend
// waiting after a (re)start, shared by all its polls, so the total wait
// stays within brokerStatusPollTimeout (ptone/scion#3536).
type statusWaitBudget struct {
	left time.Duration
}

func newStatusWaitBudget() *statusWaitBudget {
	return &statusWaitBudget{left: brokerStatusPollTimeout}
}

// poll calls probe, passing the time left as the probe's own timeout, until
// it returns true or the budget runs out, sleeping interval (capped by the
// time left) between calls. The time spent is taken from the budget. It
// reports the last probe result; with no budget left it does not probe.
func (b *statusWaitBudget) poll(probe func(timeout time.Duration) bool, interval time.Duration) bool {
	start := time.Now()
	deadline := start.Add(b.left)
	defer func() {
		b.left -= time.Since(start)
		if b.left < 0 {
			b.left = 0
		}
	}()
	for {
		rem := time.Until(deadline)
		if rem <= 0 {
			return false
		}
		if probe(rem) {
			return true
		}
		rem = time.Until(deadline)
		if rem <= 0 {
			return false
		}
		brokerStatusSleep(min(interval, rem))
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

// pollBrokerHubConnections queries the live hub connections, within budget,
// until all named connections are settled, and returns the last answer (nil
// when the budget allowed no query).
func pollBrokerHubConnections(budget *statusWaitBudget, query func(timeout time.Duration) *BrokerHubConnectionsResponse, names []string, interval time.Duration) *BrokerHubConnectionsResponse {
	var live *BrokerHubConnectionsResponse
	budget.poll(func(timeout time.Duration) bool {
		live = query(timeout)
		return brokerHubConnectionsSettled(live, names)
	}, interval)
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

// cleanupAfterDeregister handles local broker state after deregister.
// remaining is the number of hub connections left in the credentials store
// and listErr the error listing them, if any. It removes the credentials
// directory when it is known to be left empty. With purge it also removes
// statePaths (brokerLocalStatePaths), but only when the store was listed,
// no hub connection remains (the state is shared by all of them) and no
// broker is running (it is in use). A purge that cannot run returns an
// error, so a script sees a non-zero exit. Without purge it lists what is
// left behind.
func cleanupAfterDeregister(out io.Writer, credsDir string, remaining int, listErr error, brokerRunning, purge bool, statePaths []string) error {
	if listErr == nil && remaining == 0 && credsDir != "" {
		if removed, err := removeDirIfEmpty(credsDir); err != nil {
			_, _ = fmt.Fprintf(out, "Warning: failed to remove empty %s: %v\n", credsDir, err)
		} else if removed {
			_, _ = fmt.Fprintf(out, "Removed empty %s\n", credsDir)
		}
	}
	left := existingPaths(statePaths)
	if purge {
		switch {
		case listErr != nil:
			return fmt.Errorf("--purge-local skipped: could not list hub connections (%v); local broker state was left in place", listErr)
		case remaining > 0:
			return fmt.Errorf("--purge-local skipped: %d hub connection(s) remain and share the local broker state; deregister them first (see 'scion runtime-broker hubs')", remaining)
		case brokerRunning:
			return errors.New("--purge-local skipped: the broker is running; stop it with 'scion runtime-broker stop', then run 'scion runtime-broker deregister --purge-local'")
		case len(left) == 0:
			_, _ = fmt.Fprintln(out, "No local broker state to remove.")
			return nil
		default:
			return purgeBrokerLocalState(left, out)
		}
	}
	if len(left) == 0 {
		return nil
	}
	_, _ = fmt.Fprintln(out, "Local broker state left in place:")
	for _, p := range left {
		_, _ = fmt.Fprintf(out, "  %s\n", p)
	}
	if listErr == nil && remaining == 0 {
		_, _ = fmt.Fprintln(out, "Remove it with 'scion runtime-broker deregister --purge-local' once the broker is stopped.")
	}
	return nil
}

// errProvideNeedsConfirmation is returned when runtime-broker provide cannot
// ask for confirmation: stdin is not a terminal, or input ended.
var errProvideNeedsConfirmation = errors.New("cannot confirm adding the broker as a provider without an interactive terminal; re-run with --yes to confirm")

// confirmProvide asks the runtime-broker provide confirmation. With
// autoConfirm (global --yes) it confirms without reading. Without a terminal
// on stdin it does not prompt, and an end of input (or read error) at the
// prompt aborts rather than counting as the default answer: both return
// errProvideNeedsConfirmation, so a provide run from a script or over ssh
// without a TTY fails fast with a 'use --yes' message instead of blocking
// on 'Continue? (Y/n)'. An empty answer is yes.
func confirmProvide(in io.Reader, out io.Writer, projectName, brokerName string, autoConfirm, isTTY bool) (bool, error) {
	if autoConfirm {
		_, _ = fmt.Fprintf(out, "Add broker '%s' as a provider for project '%s': auto-confirmed Yes\n", brokerName, projectName)
		return true, nil
	}
	if !isTTY {
		return false, errProvideNeedsConfirmation
	}
	_, _ = fmt.Fprintf(out, "\nAdd broker '%s' as a provider for project '%s'?\n\n", brokerName, projectName)
	_, _ = fmt.Fprintln(out, "This will allow the broker to execute agents for this project.")
	_, _ = fmt.Fprint(out, "Continue? (Y/n): ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		_, _ = fmt.Fprintln(out)
		return false, errProvideNeedsConfirmation
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
