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

//go:build !no_sqlite

package hub

// Route-level counterpart to keys_rate_limit_concurrency_test.go
// (ptone/scion#2647). It needs the SQLite-backed keys route fixture, so it
// carries the same build constraint as execute_agent_keys_test.go.

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
)

// TestExecuteAgentKeys_ConcurrentPrincipalsShareTargetBudget drives the
// keys route itself (allowBoth's production caller) with the shape of the
// Phase 4.1 acceptance run: three principals, each under its own burst,
// sending concurrent requests to one target. With the fake clock held
// still, exactly the target burst is admitted; after the clock moves by
// 300ms, exactly the 3 refilled tokens are admitted. Every admitted
// request is dispatched once, and no refused request is.
//
// This test covers route wiring and dispatch accounting; race detection in
// admission lives in the TestKeysAllowBoth_Concurrent* tests, since HTTP
// overhead largely serializes these requests.
func TestExecuteAgentKeys_ConcurrentPrincipalsShareTargetBudget(t *testing.T) {
	f, d, _, _ := newExecuteAgentKeysFixture(t)
	clock := newKeysFakeClock()
	f.srv.keysPrincipalLimiter = newKeysRateLimiterWithClock(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst, clock.Now)
	f.srv.keysTargetLimiter = newKeysRateLimiterWithClock(agentkeys.TargetRateLimit, agentkeys.TargetBurst, clock.Now)

	const (
		numPrincipals        = 3
		requestsPerPrincipal = 9 // under PrincipalProjectBurst (10), so only the target bucket can refuse
	)
	tokens := make([]string, numPrincipals)
	for p := range tokens {
		tokens[p] = f.agentToken(t, tid(fmt.Sprintf("execkeys-concurrent-caller-%d", p)), f.projectA.ID, ScopeAgentLifecycle)
	}
	path := "/api/v1/agents/" + f.agentInA.ID + "/keys"

	// burst fires every request concurrently and returns the status codes
	// seen, per principal.
	burst := func() [][]int {
		codes := make([][]int, numPrincipals)
		for p := range codes {
			codes[p] = make([]int, requestsPerPrincipal)
		}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for p := range tokens {
			for i := 0; i < requestsPerPrincipal; i++ {
				wg.Add(1)
				go func(p, i int) {
					defer wg.Done()
					<-start
					codes[p][i] = doRequestWithAgentToken(t, f.srv, http.MethodPost, path, validKeysBody, tokens[p]).Code
				}(p, i)
			}
		}
		close(start)
		wg.Wait()
		return codes
	}
	count := func(codes [][]int) (ok, limited int) {
		for _, perPrincipal := range codes {
			for _, code := range perPrincipal {
				switch code {
				case http.StatusOK:
					ok++
				case http.StatusTooManyRequests:
					limited++
				default:
					t.Fatalf("unexpected status %d (codes %v)", code, codes)
				}
			}
		}
		return ok, limited
	}

	total := numPrincipals * requestsPerPrincipal
	ok, limited := count(burst())
	if ok != agentkeys.TargetBurst || limited != total-agentkeys.TargetBurst {
		t.Fatalf("frozen clock: %d admitted, %d rate limited; want exactly %d and %d", ok, limited, agentkeys.TargetBurst, total-agentkeys.TargetBurst)
	}
	if got := d.callCount(); got != ok {
		t.Fatalf("dispatcher called %d times, want %d (one per admitted request)", got, ok)
	}

	// 300ms at 10/s refills exactly 3 target tokens. Each principal has
	// spent at most 9 of its 10 and refilled 1.5 more, so the target is
	// still the only binding bucket.
	clock.Advance(300 * time.Millisecond)
	refill := keysBudget(agentkeys.TargetRateLimit, 0, 300*time.Millisecond)
	ok2, limited2 := count(burst())
	if ok2 != refill || limited2 != total-refill {
		t.Fatalf("after 300ms: %d admitted, %d rate limited; want exactly %d and %d", ok2, limited2, refill, total-refill)
	}
	if got := d.callCount(); got != ok+ok2 {
		t.Fatalf("dispatcher called %d times, want %d (one per admitted request)", got, ok+ok2)
	}
}
