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
	"testing"
	"time"
)

func TestGCPTokenRateLimiter_Allow(t *testing.T) {
	rl := NewGCPTokenRateLimiter(10, 5) // 10/sec, burst 5

	// First 5 requests should be allowed (burst)
	for i := 0; i < 5; i++ {
		if !rl.Allow(tid("agent-1")) {
			t.Fatalf("request %d should be allowed", i)
		}
	}

	// 6th request should be denied (burst exhausted)
	if rl.Allow(tid("agent-1")) {
		t.Fatal("6th request should be denied")
	}

	// Different agent should still be allowed
	if !rl.Allow(tid("agent-2")) {
		t.Fatal("different agent should be allowed")
	}
}

func TestGCPTokenRateLimiter_Refill(t *testing.T) {
	rl := NewGCPTokenRateLimiter(100, 1) // 100/sec, burst 1

	if !rl.Allow(tid("agent-1")) {
		t.Fatal("first request should be allowed")
	}

	if rl.Allow(tid("agent-1")) {
		t.Fatal("second request should be denied")
	}

	// Wait for refill
	time.Sleep(20 * time.Millisecond)

	if !rl.Allow(tid("agent-1")) {
		t.Fatal("request after refill should be allowed")
	}
}

func TestGCPTokenRateLimiter_CleanupExitsOnCancel(t *testing.T) {
	rl := NewGCPTokenRateLimiter(10, 5)
	rl.cleanup = 50 * time.Millisecond // fast cleanup for test

	ctx, cancel := context.WithCancel(context.Background())
	rl.StartCleanup(ctx)

	// Use the limiter
	rl.Allow(tid("agent-1"))

	// Cancel and verify goroutine exits (no hang)
	cancel()
	time.Sleep(100 * time.Millisecond)

	// Limiter should still work after cleanup goroutine exits
	if !rl.Allow(tid("agent-2")) {
		t.Fatal("limiter should still work after cleanup exits")
	}
}
