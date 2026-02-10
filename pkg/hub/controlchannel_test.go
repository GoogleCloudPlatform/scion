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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlChannelManager_OnDisconnectCallback(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig())

	var mu sync.Mutex
	var receivedBrokerID string
	done := make(chan struct{})

	mgr.SetOnDisconnect(func(brokerID string) {
		mu.Lock()
		defer mu.Unlock()
		receivedBrokerID = brokerID
		close(done)
	})

	// Manually add a connection entry so removeConnection has something to remove
	mgr.mu.Lock()
	mgr.connections["broker-1"] = &BrokerConnection{brokerID: "broker-1"}
	mgr.mu.Unlock()

	mgr.removeConnection("broker-1")

	// Wait for async callback
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onDisconnect callback")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "broker-1", receivedBrokerID)

	// Verify connection was removed
	require.False(t, mgr.IsConnected("broker-1"))
}

func TestControlChannelManager_OnDisconnectCallback_NilSafe(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig())

	// Don't set any callback - verify removeConnection doesn't panic
	mgr.mu.Lock()
	mgr.connections["broker-2"] = &BrokerConnection{brokerID: "broker-2"}
	mgr.mu.Unlock()

	// This should not panic
	mgr.removeConnection("broker-2")

	require.False(t, mgr.IsConnected("broker-2"))
}
