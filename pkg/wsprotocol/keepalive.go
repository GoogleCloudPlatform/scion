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

package wsprotocol

import (
	"context"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// StartKeepalive arms conn's read deadline for cfg.PongWait, installs a pong
// handler that extends the deadline by cfg.PongWait every time a pong
// arrives, and starts a background goroutine that writes a WebSocket ping
// every cfg.PingInterval. The ping loop runs until ctx is done or a ping
// write fails (which it treats as the connection being gone, and stops).
//
// Every write StartKeepalive makes goes through mu, so a caller that also
// writes application data on the same conn must pass the same mutex it uses
// for those writes: gorilla/websocket connections support at most one
// concurrent writer.
//
// StartKeepalive itself only performs the initial setup (arming the
// deadline, installing the pong handler) before returning; the recurring
// ping loop is started in a goroutine and does not block the caller.
//
// A zero or negative PingInterval, PongWait, or WriteWait in cfg falls back
// to the matching package default instead of being used as-is: a zero
// PingInterval would panic in time.NewTicker, and a zero PongWait would arm
// a read deadline in the past, failing the very next read immediately. If
// the resulting PingInterval is not below PongWait — for example a partial
// config that sets only a small PongWait, pairing it with the PingInterval
// default — PingInterval falls back to half of PongWait instead (see
// pingIntervalFor), so the first ping always has a comfortable margin
// before the deadline it is meant to extend.
func StartKeepalive(ctx context.Context, conn *websocket.Conn, mu *sync.Mutex, cfg ConnectionConfig) error {
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = DefaultPingInterval
	}
	if cfg.PongWait <= 0 {
		cfg.PongWait = DefaultPongWait
	}
	if cfg.WriteWait <= 0 {
		cfg.WriteWait = DefaultWriteWait
	}
	cfg.PingInterval = pingIntervalFor(cfg.PingInterval, cfg.PongWait)

	if err := conn.SetReadDeadline(time.Now().Add(cfg.PongWait)); err != nil {
		return err
	}
	conn.SetPongHandler(func(appData string) error {
		return conn.SetReadDeadline(time.Now().Add(cfg.PongWait))
	})

	go keepaliveLoop(ctx, conn, mu, cfg)
	return nil
}

// pingIntervalFor returns the ping interval to use, given the
// (already-defaulted) pingInterval and pongWait. If pingInterval is already
// below pongWait, it is left alone. Otherwise — which only happens with a
// partial config, since both production callers set every field — it falls
// back to half of pongWait: half matches the ratio between the package
// defaults (a 30s ping interval against a 60s pong wait) and leaves
// comfortable room for jitter, rather than a value close enough to pongWait
// that a single delayed ping could still miss the deadline. Half of pongWait
// is zero only when pongWait is 1ns, the smallest positive duration; the
// result is floored at 1ns in that case, since it cannot go any lower than
// pongWait itself.
func pingIntervalFor(pingInterval, pongWait time.Duration) time.Duration {
	if pingInterval < pongWait {
		return pingInterval
	}
	if half := pongWait / 2; half > 0 {
		return half
	}
	return 1
}

// keepaliveLoop writes a WebSocket ping frame every cfg.PingInterval until
// ctx is done or a write fails.
func keepaliveLoop(ctx context.Context, conn *websocket.Conn, mu *sync.Mutex, cfg ConnectionConfig) {
	ticker := time.NewTicker(cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mu.Lock()
			err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(cfg.WriteWait))
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}
