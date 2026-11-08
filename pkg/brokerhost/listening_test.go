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

package brokerhost

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestHost_ListeningClosesAfterBind: Listening is closed once Run has bound
// the listener.
func TestHost_ListeningClosesAfterBind(t *testing.T) {
	f := newFixture(t)
	cfg := authConfig(t, f, dockerInstance("docker-a", "a"))
	cfg.Listener = ListenerConfig{Host: "127.0.0.1", Port: 0}
	h, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	select {
	case <-h.Listening():
		t.Fatal("Listening closed before Run")
	default:
	}
	runHost(t, h)
	select {
	case <-h.Listening():
	default:
		t.Fatal("Listening not closed after the bind")
	}
}

// TestHost_ListeningNeverClosesOnBindError: a Run whose bind fails returns
// the error and leaves Listening open.
func TestHost_ListeningNeverClosesOnBindError(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = busy.Close() })
	port := busy.Addr().(*net.TCPAddr).Port

	f := newFixture(t)
	cfg := authConfig(t, f, dockerInstance("docker-a", "a"))
	cfg.Listener = ListenerConfig{Host: "127.0.0.1", Port: port}
	h, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	require.Error(t, h.Run(context.Background()))
	select {
	case <-h.Listening():
		t.Fatal("Listening closed although the bind failed")
	default:
	}
}
