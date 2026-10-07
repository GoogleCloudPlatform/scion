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
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShouldOfferProjectProvider(t *testing.T) {
	assert.True(t, shouldOfferProjectProvider("p1", true, false))
	assert.False(t, shouldOfferProjectProvider("p1", true, true), "global is never offered as a hub project")
	assert.False(t, shouldOfferProjectProvider("", true, false), "unlinked project")
	assert.False(t, shouldOfferProjectProvider("p1", false, false), "hub mode off")
}

func TestBrokerRecentlyStarted(t *testing.T) {
	assert.True(t, brokerRecentlyStarted("3s"))
	assert.False(t, brokerRecentlyStarted("2m0s"))
	assert.False(t, brokerRecentlyStarted(""))
	assert.False(t, brokerRecentlyStarted("garbage"))
}

func TestPollBrokerHubConnections_WaitsForFirstHeartbeat(t *testing.T) {
	prev := brokerStatusSleep
	brokerStatusSleep = func(time.Duration) {}
	t.Cleanup(func() { brokerStatusSleep = prev })

	answers := []*BrokerHubConnectionsResponse{
		nil, // broker not answering yet
		{Connections: []BrokerHubConnectionInfo{}}, // connections not loaded
		{Connections: []BrokerHubConnectionInfo{{Name: "hub-a", Status: "disconnected"}}},
		{Connections: []BrokerHubConnectionInfo{{Name: "hub-a", Status: "connected"}}},
		{Connections: []BrokerHubConnectionInfo{{Name: "hub-a", Status: "error"}}},
	}
	calls := 0
	query := func(timeout time.Duration) *BrokerHubConnectionsResponse {
		assert.Greater(t, timeout, time.Duration(0))
		assert.LessOrEqual(t, timeout, brokerStatusPollTimeout, "probe timeout capped by the budget")
		r := answers[calls]
		calls++
		return r
	}
	live := pollBrokerHubConnections(newStatusWaitBudget(), query, []string{"hub-a"}, time.Millisecond)
	require.NotNil(t, live)
	assert.Equal(t, "connected", live.Connections[0].Status)
	assert.Equal(t, 4, calls)
}

func TestPollBrokerHubConnections_Bounded(t *testing.T) {
	prev := brokerStatusSleep
	brokerStatusSleep = func(d time.Duration) { time.Sleep(d) }
	t.Cleanup(func() { brokerStatusSleep = prev })

	calls := 0
	start := time.Now()
	budget := &statusWaitBudget{left: 50 * time.Millisecond}
	live := pollBrokerHubConnections(budget, func(time.Duration) *BrokerHubConnectionsResponse {
		calls++
		return &BrokerHubConnectionsResponse{Connections: []BrokerHubConnectionInfo{{Name: "hub-a", Status: "disconnected"}}}
	}, []string{"hub-a"}, 10*time.Millisecond)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Greater(t, calls, 1)
	require.NotNil(t, live)
	assert.Equal(t, "disconnected", live.Connections[0].Status, "a hub that stays down is reported as it is")
	assert.Zero(t, budget.left)
}

// TestStatusWaitBudget_SharedAcrossPolls: the health wait and the hub
// connection wait share one budget, so together they never exceed it, and
// each probe's timeout is capped by the time left.
func TestStatusWaitBudget_SharedAcrossPolls(t *testing.T) {
	prev := brokerStatusSleep
	brokerStatusSleep = func(d time.Duration) { time.Sleep(d) }
	t.Cleanup(func() { brokerStatusSleep = prev })

	budget := &statusWaitBudget{left: 200 * time.Millisecond}
	start := time.Now()

	// Phase 1: a slow probe that never succeeds (a hung listener) uses the
	// whole budget; it gets no more than the time left as its timeout.
	assert.False(t, budget.poll(func(timeout time.Duration) bool {
		time.Sleep(min(timeout, 60*time.Millisecond))
		return false
	}, 10*time.Millisecond))

	// Phase 2 has nothing left: it must not probe at all.
	probed := false
	live := pollBrokerHubConnections(budget, func(time.Duration) *BrokerHubConnectionsResponse {
		probed = true
		return nil
	}, []string{"hub-a"}, 10*time.Millisecond)
	assert.Nil(t, live)
	assert.False(t, probed)
	assert.Less(t, time.Since(start), time.Second, "total wait stays within the budget")
}

func TestStatusWaitBudget_RemainderCarriesOver(t *testing.T) {
	prev := brokerStatusSleep
	brokerStatusSleep = func(time.Duration) {}
	t.Cleanup(func() { brokerStatusSleep = prev })

	budget := newStatusWaitBudget()
	assert.True(t, budget.poll(func(time.Duration) bool { return true }, time.Millisecond))
	var got time.Duration
	budget.poll(func(timeout time.Duration) bool { got = timeout; return true }, time.Millisecond)
	assert.Greater(t, got, brokerStatusPollTimeout-time.Second, "a quick first phase leaves the budget for the second")
	assert.LessOrEqual(t, got, brokerStatusPollTimeout)
}

func TestBrokerFileRecent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "broker.pid")
	assert.False(t, brokerFileRecent(""))
	assert.False(t, brokerFileRecent(p), "missing file")
	require.NoError(t, os.WriteFile(p, []byte("1"), 0o644))
	assert.True(t, brokerFileRecent(p))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(p, old, old))
	assert.False(t, brokerFileRecent(p))
}

func TestBrokerHubConnectionDisplayStatus(t *testing.T) {
	live := map[string]string{"hub-a": "connected", "hub-b": ""}
	assert.Equal(t, "connected", brokerHubConnectionDisplayStatus(live, "hub-a", true))
	assert.Contains(t, brokerHubConnectionDisplayStatus(live, "hub-b", true), "pending")
	assert.Contains(t, brokerHubConnectionDisplayStatus(nil, "hub-c", true), "pending")
	assert.Contains(t, brokerHubConnectionDisplayStatus(nil, "hub-c", false), "unknown")
}

func TestBrokerLocalStatePaths(t *testing.T) {
	paths := brokerLocalStatePaths("/g", "/h/.scion", "b-1")
	assert.Equal(t, []string{"/g/broker.log", "/h/.scion/runtime-broker-state/b-1", "/h/.scion/cache/templates"}, paths)
	for _, bad := range []string{"", "..", "a/b"} {
		assert.Equal(t, []string{"/g/broker.log", "/h/.scion/cache/templates"}, brokerLocalStatePaths("/g", "/h/.scion", bad), bad)
	}
}

// brokerStateFixture creates a scion home with broker-local state, a second
// broker's state, settings and a hub-id file, and returns the state paths.
func brokerStateFixture(t *testing.T) (home string, paths []string) {
	t.Helper()
	home = t.TempDir()
	for _, d := range []string{"hub-credentials", "runtime-broker-state/b-1", "runtime-broker-state/other", "cache/templates/x"} {
		require.NoError(t, os.MkdirAll(filepath.Join(home, d), 0o755))
	}
	for _, f := range []string{"broker.log", "settings.yaml", "hub-id", "runtime-broker-state/b-1/state.db"} {
		require.NoError(t, os.WriteFile(filepath.Join(home, f), []byte("x"), 0o644))
	}
	return home, brokerLocalStatePaths(home, home, "b-1")
}

func TestCleanupAfterDeregister_PurgeLocal(t *testing.T) {
	home, paths := brokerStateFixture(t)
	var out bytes.Buffer
	require.NoError(t, cleanupAfterDeregister(&out, filepath.Join(home, "hub-credentials"), nil, nil, false, true, paths))

	for _, gone := range []string{"hub-credentials", "broker.log", "runtime-broker-state/b-1", "cache/templates"} {
		assert.NoFileExists(t, filepath.Join(home, gone))
		assert.NoDirExists(t, filepath.Join(home, gone))
	}
	for _, kept := range []string{"settings.yaml", "hub-id"} {
		assert.FileExists(t, filepath.Join(home, kept))
	}
	assert.DirExists(t, filepath.Join(home, "runtime-broker-state", "other"), "another broker ID's state is kept")
	assert.Contains(t, out.String(), "Removed "+filepath.Join(home, "broker.log"))
}

func TestCleanupAfterDeregister_NoPurgeListsResidue(t *testing.T) {
	home, paths := brokerStateFixture(t)
	var out bytes.Buffer
	require.NoError(t, cleanupAfterDeregister(&out, filepath.Join(home, "hub-credentials"), nil, nil, false, false, paths))
	assert.NoDirExists(t, filepath.Join(home, "hub-credentials"), "the empty credentials dir is always removed")
	assert.FileExists(t, filepath.Join(home, "broker.log"))
	assert.Contains(t, out.String(), "Local broker state left in place")
	assert.Contains(t, out.String(), "--purge-local")
}

// TestCleanupAfterDeregister_PurgeSkipped: a purge that cannot run leaves
// everything in place and returns an error (non-zero exit).
func TestCleanupAfterDeregister_PurgeSkipped(t *testing.T) {
	tests := []struct {
		name      string
		remaining []brokercredentials.BrokerCredentials
		listErr   error
		running   bool
		wantErr   string
	}{
		{name: "other connections remain", remaining: []brokercredentials.BrokerCredentials{{Name: "other", BrokerID: "b-2"}}, wantErr: "deregister them first with 'scion runtime-broker deregister --name <name>' (other)"},
		{name: "connection without broker ID remains", remaining: []brokercredentials.BrokerCredentials{{Name: "other"}}, wantErr: "remove them by hand"},
		{name: "connections cannot be listed", listErr: errors.New("boom"), wantErr: "could not list hub connections"},
		{name: "broker running", running: true, wantErr: "the broker is running"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, paths := brokerStateFixture(t)
			require.NoError(t, os.WriteFile(filepath.Join(home, "hub-credentials", "other.json"), []byte("{}"), 0o600))
			var out bytes.Buffer
			err := cleanupAfterDeregister(&out, filepath.Join(home, "hub-credentials"), tt.remaining, tt.listErr, tt.running, true, paths)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--purge-local skipped")
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Contains(t, out.String(), "Local broker state left in place", "a refused purge lists what is left")
			assert.Contains(t, out.String(), filepath.Join(home, "broker.log"))
			assert.FileExists(t, filepath.Join(home, "hub-credentials", "other.json"))
			assert.FileExists(t, filepath.Join(home, "broker.log"))
			assert.DirExists(t, filepath.Join(home, "runtime-broker-state", "b-1"))
			assert.DirExists(t, filepath.Join(home, "cache", "templates"))
		})
	}
}

// freeTCPPort returns a local port nothing listens on.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// TestPurgeLocalBrokerStateOnly_RefusesWhileConnectionsRemain covers the
// path deregister takes when the selected credentials have no broker ID:
// the purge must still refuse while any hub connection remains.
func TestPurgeLocalBrokerStateOnly_RefusesWhileConnectionsRemain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionHome := filepath.Join(home, ".scion")
	credsDir := filepath.Join(scionHome, "hub-credentials")
	require.NoError(t, os.MkdirAll(credsDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(credsDir, "no-id.json"), []byte(`{"name":"no-id","hubEndpoint":"https://a"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(credsDir, "other.json"), []byte(`{"name":"other","brokerId":"b-2","hubEndpoint":"https://b"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(scionHome, "broker.log"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(scionHome, "cache", "templates"), 0o755))

	cmd := &cobra.Command{}
	cmd.Flags().Int("port", 0, "")
	require.NoError(t, cmd.Flags().Set("port", strconv.Itoa(freeTCPPort(t))))

	err := purgeLocalBrokerStateOnly(cmd, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 hub connection(s) remain")
	assert.Contains(t, err.Error(), filepath.Join(credsDir, "no-id.json"), "a connection without broker ID is named by file")
	assert.FileExists(t, filepath.Join(scionHome, "broker.log"))
	assert.DirExists(t, filepath.Join(scionHome, "cache", "templates"))
	assert.FileExists(t, filepath.Join(credsDir, "other.json"))
}

func TestPurgeLocalBrokerStateOnly_PurgesWithNoConnections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionHome := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(filepath.Join(scionHome, "hub-credentials"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(scionHome, "broker.log"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(scionHome, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0o644))

	cmd := &cobra.Command{}
	cmd.Flags().Int("port", 0, "")
	require.NoError(t, cmd.Flags().Set("port", strconv.Itoa(freeTCPPort(t))))

	require.NoError(t, purgeLocalBrokerStateOnly(cmd, ""))
	assert.NoFileExists(t, filepath.Join(scionHome, "broker.log"))
	assert.NoDirExists(t, filepath.Join(scionHome, "hub-credentials"))
	assert.FileExists(t, filepath.Join(scionHome, "settings.yaml"))
}

func TestConfirmProvide(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		autoConfirm bool
		isTTY       bool
		want        bool
		wantErr     bool
	}{
		{name: "global --yes", autoConfirm: true, want: true},
		{name: "global --yes without a terminal", autoConfirm: true, isTTY: false, want: true},
		{name: "no terminal aborts", isTTY: false, wantErr: true},
		{name: "EOF aborts", isTTY: true, input: "", wantErr: true},
		{name: "enter is yes", isTTY: true, input: "\n", want: true},
		{name: "y", isTTY: true, input: "y\n", want: true},
		{name: "yes without newline before EOF", isTTY: true, input: "yes", want: true},
		{name: "n", isTTY: true, input: "n\n", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := confirmProvide(strings.NewReader(tt.input), &out, "proj", "host", tt.autoConfirm, tt.isTTY)
			if tt.wantErr {
				require.ErrorIs(t, err, errProvideNeedsConfirmation)
				assert.Contains(t, err.Error(), "--yes")
				assert.False(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// purgeOnlyFixture sets up a scion home whose credentials store holds the
// given files, with broker-local state, and returns the scion home and a
// command whose --port points at nothing.
func purgeOnlyFixture(t *testing.T, creds map[string]string) (string, *cobra.Command) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	scionHome := filepath.Join(home, ".scion")
	credsDir := filepath.Join(scionHome, "hub-credentials")
	require.NoError(t, os.MkdirAll(credsDir, 0o700))
	for name, body := range creds {
		require.NoError(t, os.WriteFile(filepath.Join(credsDir, name+".json"), []byte(body), 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(scionHome, "broker.log"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(scionHome, "cache", "templates"), 0o755))
	cmd := &cobra.Command{}
	cmd.Flags().Int("port", 0, "")
	require.NoError(t, cmd.Flags().Set("port", strconv.Itoa(freeTCPPort(t))))
	return scionHome, cmd
}

// TestPurgeLocalBrokerStateOnly_SelectedNoIDConnectionDoesNotBlock: when
// the only connection left is the selected one and it has no broker ID,
// the purge runs (that connection is the one being deregistered), and its
// credentials file is left in place and named.
func TestPurgeLocalBrokerStateOnly_SelectedNoIDConnectionDoesNotBlock(t *testing.T) {
	scionHome, cmd := purgeOnlyFixture(t, map[string]string{"no-id": `{"name":"no-id","hubEndpoint":"https://a"}`})
	var err error
	out := captureStdout(t, func() { err = purgeLocalBrokerStateOnly(cmd, "no-id") })
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(scionHome, "broker.log"))
	assert.NoDirExists(t, filepath.Join(scionHome, "cache", "templates"))
	noID := filepath.Join(scionHome, "hub-credentials", "no-id.json")
	assert.FileExists(t, noID)
	assert.Contains(t, out, noID)
}

// TestPurgeLocalBrokerStateOnly_OtherNoIDConnectionNamed: another
// connection without a broker ID still blocks the purge, and the error
// names its file for removal by hand.
func TestPurgeLocalBrokerStateOnly_OtherNoIDConnectionNamed(t *testing.T) {
	scionHome, cmd := purgeOnlyFixture(t, map[string]string{
		"no-id":   `{"name":"no-id","hubEndpoint":"https://a"}`,
		"stale-b": `{"name":"stale-b","hubEndpoint":"https://b"}`,
	})
	var err error
	out := captureStdout(t, func() { err = purgeLocalBrokerStateOnly(cmd, "no-id") })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 hub connection(s) remain")
	assert.Contains(t, err.Error(), filepath.Join(scionHome, "hub-credentials", "stale-b.json"))
	assert.Contains(t, err.Error(), "remove them by hand")
	assert.NotContains(t, err.Error(), "no-id.json", "the selected connection is not counted")
	assert.FileExists(t, filepath.Join(scionHome, "broker.log"))
	assert.Contains(t, out, "Local broker state left in place")
}
