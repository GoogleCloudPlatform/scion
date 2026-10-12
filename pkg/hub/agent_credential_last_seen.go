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
	"context"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Bounds of the agent credential last_seen_at throttle.
const (
	// agentCredentialLastSeenInterval is the least time between two
	// last_seen_at writes for one credential.
	agentCredentialLastSeenInterval = 60 * time.Second
	// agentCredentialLastSeenMax bounds the credentials remembered.
	agentCredentialLastSeenMax = 4096
)

// agentCredentialLastSeenThrottle limits the last_seen_at write of agent
// token verification to once per credential per interval, so a busy agent
// does not cost a database write (serialised on SQLite) on every request.
//
// The state is an in-process set of recently written credential ids,
// bounded and safe for concurrent use (runScopeLogDedup: when full,
// expired ids are dropped first, then the set is cleared; a clear only
// costs extra writes). A conditional UPDATE would not help: on SQLite an
// UPDATE that matches no row still takes the write lock. Each hub process
// throttles on its own. A failed write is not retried before the interval
// ends; it never fails the request.
type agentCredentialLastSeenThrottle struct {
	now  func() time.Time
	seen *runScopeLogDedup
	// spawn runs a write; nil runs it on a new goroutine. Tests run it
	// inline.
	spawn func(func())
}

// newAgentCredentialLastSeenThrottle remembers up to max credentials for
// interval.
func newAgentCredentialLastSeenThrottle(max int, interval time.Duration) *agentCredentialLastSeenThrottle {
	return &agentCredentialLastSeenThrottle{
		now:  time.Now,
		seen: newRunScopeLogDedup(max, interval),
	}
}

// touch writes credID's last_seen_at, in the background and ignoring the
// error, unless it was written within the interval. A nil throttle writes
// every time.
func (t *agentCredentialLastSeenThrottle) touch(creds store.AgentCredentialStore, credID string) {
	if t == nil {
		go func() {
			_ = creds.UpdateAgentCredentialLastSeen(context.Background(), credID, time.Now())
		}()
		return
	}
	now := t.now()
	if !t.seen.first(credID, now) {
		return
	}
	write := func() {
		_ = creds.UpdateAgentCredentialLastSeen(context.Background(), credID, now)
	}
	if t.spawn != nil {
		t.spawn(write)
		return
	}
	go write()
}
