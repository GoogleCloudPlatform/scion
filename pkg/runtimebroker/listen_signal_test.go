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

package runtimebroker

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerStart_ListeningClosesAfterBind: Listening is closed once Start
// has bound its listener, and not before.
func TestServerStart_ListeningClosesAfterBind(t *testing.T) {
	srv := newTestServer(t)
	// Start builds its listener address from the config; port 0 binds a
	// kernel-chosen loopback port, never the default.
	srv.config.Host = "127.0.0.1"
	srv.config.Port = 0
	select {
	case <-srv.Listening():
		t.Fatal("Listening closed before Start")
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			// Start returns Shutdown's result; an idle server drains to nil.
			assert.NoError(t, err, "Start returns nil after a clean cancel")
		case <-time.After(30 * time.Second):
			t.Error("Start did not return")
		}
	})
	select {
	case <-srv.Listening():
	case err := <-done:
		t.Fatalf("Start returned before the bind: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("Listening not closed after the bind")
	}
	srv.mu.RLock()
	hs := srv.httpServer
	srv.mu.RUnlock()
	require.NotNil(t, hs)
	require.Equal(t, "127.0.0.1:0", hs.Addr, "the configured address, not the default port")
}

// TestServerStart_ListeningNeverClosesOnBindError: a Start whose bind fails
// returns the error and leaves Listening open.
func TestServerStart_ListeningNeverClosesOnBindError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = busy.Close() })

	srv := newTestServer(t)
	srv.config.Host = "127.0.0.1"
	srv.config.Port = busy.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.Error(t, srv.Start(ctx))
	select {
	case <-srv.Listening():
		t.Fatal("Listening closed although the bind failed")
	default:
	}
}
