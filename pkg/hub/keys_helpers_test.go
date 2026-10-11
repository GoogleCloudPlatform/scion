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
	"bytes"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// keysContentSentinel is a distinctive value used as the "keys" field across
// every test in this file. Per .design/agent-keys-contract.md §5 ("Never
// logged in a keys path, including debug logs and errors: the input
// itself... request JSON") and AK-32 ("captured logs contain no key content
// when searched for a distinctive test secret"), no error message or log
// line produced by any BrokerClient adapter may ever contain it.
const keysContentSentinel = "SENTINEL-KEYS-7f3a"

// installSentinelLogCapture points the slog default logger at an in-memory
// buffer, at Debug level so brokerHTTPTransport's debug-only logging is
// captured too, and restores the previous default logger on test cleanup.
func installSentinelLogCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func newKeysFakeClock() *keysFakeClock {
	return &keysFakeClock{now: time.Unix(1_700_000_000, 0)}
}

// keysBudget is the most a token bucket starting full may admit over a
// closed window of the given length: burst + floor(rate * elapsed).
// Durations used by these tests keep rate*elapsed integral.
func keysBudget(rate, burst float64, elapsed time.Duration) int {
	return int(burst + rate*elapsed.Seconds())
}

// keysFakeClock is a goroutine-safe, manually advanced clock for
// newKeysRateLimiterWithClock.
type keysFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *keysFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *keysFakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
