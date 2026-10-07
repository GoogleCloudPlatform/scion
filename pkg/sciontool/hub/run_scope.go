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
	"errors"
	"net/http"
	"sync"
	"time"
)

// RunIDHeader carries the agent's run id on agent-token requests to the
// hub.
const RunIDHeader = "X-Scion-Run-Id"

// RunID returns the agent's run id ("" when unknown).
func (c *Client) RunID() string { return c.runID }

// setAgentAuth sets the agent-token headers on h.
func (c *Client) setAgentAuth(h http.Header, token string) {
	h.Set("X-Scion-Agent-Token", token)
	if c.runID != "" {
		h.Set(RunIDHeader, c.runID)
	}
}

// Pacing of status writes after the hub has refused the agent's token and
// a token refresh.
const (
	// refusedWriteBackoff is the pause after each refused write.
	refusedWriteBackoff = 60 * time.Second
	// refusedWriteStopAfter is how long refused writes continue before
	// writes stop until the token is replaced.
	refusedWriteStopAfter = 10 * time.Minute
)

// ErrHubWritesPaused is returned for a status write the client did not
// send because the hub refused the agent's token.
var ErrHubWritesPaused = errors.New("hub status writes paused: the hub refused this agent's token")

// writeGate paces status writes. After a token refresh is refused, each
// 401 on a write pauses writes for refusedWriteBackoff; once 401s have
// continued for refusedWriteStopAfter, writes stop. A successful refresh
// or a new token resets it.
type writeGate struct {
	mu        sync.Mutex
	now       func() time.Time // nil: time.Now
	refused   bool             // the last token refresh was refused
	since     time.Time        // first refused write since then
	nextWrite time.Time
	stopped   bool
}

func (g *writeGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// allow reports whether a write may be sent now.
func (g *writeGate) allow() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || (!g.nextWrite.IsZero() && g.clock().Before(g.nextWrite)) {
		return ErrHubWritesPaused
	}
	return nil
}

// observe records the status of a sent write.
func (g *writeGate) observe(status int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case status < 400:
		g.since, g.nextWrite = time.Time{}, time.Time{}
	case status == http.StatusUnauthorized && g.refused:
		now := g.clock()
		if g.since.IsZero() {
			g.since = now
		}
		if now.Sub(g.since) >= refusedWriteStopAfter {
			g.stopped = true
			return
		}
		g.nextWrite = now.Add(refusedWriteBackoff)
	}
}

// refreshRefused records that a token refresh was refused.
func (g *writeGate) refreshRefused() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refused = true
}

// reset clears the gate after a successful refresh or a new token.
func (g *writeGate) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refused, g.since, g.nextWrite, g.stopped = false, time.Time{}, time.Time{}, false
}
