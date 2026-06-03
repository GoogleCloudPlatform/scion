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

package hub

import (
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlChannelManager_OnDisconnectCallback(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())

	var mu sync.Mutex
	var receivedBrokerID string
	var receivedSessionID string
	done := make(chan struct{})

	mgr.SetOnDisconnect(func(brokerID, sessionID string) {
		mu.Lock()
		defer mu.Unlock()
		receivedBrokerID = brokerID
		receivedSessionID = sessionID
		close(done)
	})

	// Manually add a connection entry so removeConnection has something to remove
	mgr.mu.Lock()
	mgr.connections[tid("broker-1")] = &BrokerConnection{brokerID: tid("broker-1"), sessionID: "sess-1"}
	mgr.mu.Unlock()

	mgr.removeConnection(tid("broker-1"), "sess-1")

	// Wait for async callback
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onDisconnect callback")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, tid("broker-1"), receivedBrokerID)
	assert.Equal(t, "sess-1", receivedSessionID)

	// Verify connection was removed
	require.False(t, mgr.IsConnected(tid("broker-1")))
}

// TestControlChannelManager_RemoveStaleSessionNoop verifies that a teardown for
// an OLD session does not remove a NEWER connection that replaced it (flap), and
// does not fire onDisconnect for the stale session.
func TestControlChannelManager_RemoveStaleSessionNoop(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())

	var fired bool
	var mu sync.Mutex
	mgr.SetOnDisconnect(func(brokerID, sessionID string) {
		mu.Lock()
		defer mu.Unlock()
		fired = true
	})

	// Current live connection is session "new".
	mgr.mu.Lock()
	mgr.connections[tid("broker-1")] = &BrokerConnection{brokerID: tid("broker-1"), sessionID: "new"}
	mgr.mu.Unlock()

	// The old session's teardown must be a no-op.
	mgr.removeConnection(tid("broker-1"), "old")

	// Give any (erroneous) async callback a chance to run.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	assert.False(t, fired, "onDisconnect must not fire for a stale session")
	mu.Unlock()
	// The live (new) connection must still be present.
	require.True(t, mgr.IsConnected(tid("broker-1")))
}

func TestControlChannelManager_OnDisconnectCallback_NilSafe(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())

	// Don't set any callback - verify removeConnection doesn't panic
	mgr.mu.Lock()
	mgr.connections[tid("broker-2")] = &BrokerConnection{brokerID: tid("broker-2"), sessionID: "sess-2"}
	mgr.mu.Unlock()

	// This should not panic
	mgr.removeConnection(tid("broker-2"), "sess-2")

	require.False(t, mgr.IsConnected(tid("broker-2")))
}
