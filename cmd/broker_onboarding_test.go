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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	query := func() *BrokerHubConnectionsResponse {
		r := answers[calls]
		calls++
		return r
	}
	live := pollBrokerHubConnections(query, []string{"hub-a"}, time.Hour, time.Millisecond)
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
	live := pollBrokerHubConnections(func() *BrokerHubConnectionsResponse {
		calls++
		return &BrokerHubConnectionsResponse{Connections: []BrokerHubConnectionInfo{{Name: "hub-a", Status: "disconnected"}}}
	}, []string{"hub-a"}, 50*time.Millisecond, 10*time.Millisecond)
	assert.Less(t, time.Since(start), 2*time.Second)
	assert.Greater(t, calls, 1)
	require.NotNil(t, live)
	assert.Equal(t, "disconnected", live.Connections[0].Status, "a hub that stays down is reported as it is")
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
	require.NoError(t, cleanupAfterDeregister(&out, filepath.Join(home, "hub-credentials"), 0, false, true, paths))

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
	require.NoError(t, cleanupAfterDeregister(&out, filepath.Join(home, "hub-credentials"), 0, false, false, paths))
	assert.NoDirExists(t, filepath.Join(home, "hub-credentials"), "the empty credentials dir is always removed")
	assert.FileExists(t, filepath.Join(home, "broker.log"))
	assert.Contains(t, out.String(), "Local broker state left in place")
	assert.Contains(t, out.String(), "--purge-local")
}

func TestCleanupAfterDeregister_PurgeSkipped(t *testing.T) {
	t.Run("other connections remain", func(t *testing.T) {
		home, paths := brokerStateFixture(t)
		require.NoError(t, os.WriteFile(filepath.Join(home, "hub-credentials", "other.json"), []byte("{}"), 0o600))
		var out bytes.Buffer
		require.NoError(t, cleanupAfterDeregister(&out, filepath.Join(home, "hub-credentials"), 1, false, true, paths))
		assert.FileExists(t, filepath.Join(home, "hub-credentials", "other.json"))
		assert.FileExists(t, filepath.Join(home, "broker.log"))
		assert.Contains(t, out.String(), "other hub connection(s) remain")
	})
	t.Run("broker running", func(t *testing.T) {
		home, paths := brokerStateFixture(t)
		var out bytes.Buffer
		require.NoError(t, cleanupAfterDeregister(&out, filepath.Join(home, "hub-credentials"), 0, true, true, paths))
		assert.FileExists(t, filepath.Join(home, "broker.log"))
		assert.DirExists(t, filepath.Join(home, "runtime-broker-state", "b-1"))
		assert.Contains(t, out.String(), "the broker is running")
	})
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
