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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
)

// TestPTYKeepalive_WiresStartKeepalive proves that PTYSession.Run actually
// wires wsprotocol.StartKeepalive into the client connection, with no change
// in observable behavior from before the migration: a WebSocket ping arrives
// at the browser within the configured interval, and the session stays open
// as long as the browser keeps answering (the default gorilla/websocket
// behavior, as long as it keeps pumping reads).
//
// ptyKeepaliveConfig is temporarily overridden with short intervals so this
// test runs in milliseconds rather than needing to wait out the real 30s
// ping interval; production callers always use the package-level default.
func TestPTYKeepalive_WiresStartKeepalive(t *testing.T) {
	orig := ptyKeepaliveConfig
	ptyKeepaliveConfig = wsprotocol.ConnectionConfig{
		PingInterval: 20 * time.Millisecond,
		PongWait:     100 * time.Millisecond,
		WriteWait:    200 * time.Millisecond,
	}
	t.Cleanup(func() { ptyKeepaliveConfig = orig })

	f := startCloseCodeSession(t)

	pingCh := make(chan struct{}, 8)
	f.browser.SetPingHandler(func(appData string) error {
		select {
		case pingCh <- struct{}{}:
		default:
		}
		return f.browser.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})
	go func() {
		for {
			if _, _, err := f.browser.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case <-pingCh:
	case <-time.After(2 * time.Second):
		t.Fatal("Hub PTY session never sent a keepalive ping to the client")
	}

	// The browser is answering every ping, so the session must not be ended
	// for inactivity while that continues.
	select {
	case err := <-f.done:
		t.Fatalf("session ended while the client was still answering pings: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
}
