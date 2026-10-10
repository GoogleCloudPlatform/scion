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

// Concurrency tests for keys rate admission (ptone/scion#2647): many
// concurrent callers across several principals against one target, driven
// by a fake clock, asserting that neither the per-principal+project bucket
// nor the per-target bucket ever admits more than burst + rate*elapsed.
// A non-atomic check-and-take (within one bucket, or across the two buckets
// allowBoth pairs) shows up here as over-admission.

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
)

// keysFakeClock is a goroutine-safe, manually advanced clock for
// newKeysRateLimiterWithClock.
type keysFakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newKeysFakeClock() *keysFakeClock {
	return &keysFakeClock{now: time.Unix(1_700_000_000, 0)}
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

// keysBudget is the most a token bucket starting full may admit over a
// closed window of the given length: burst + floor(rate * elapsed).
// Durations used by these tests keep rate*elapsed integral.
func keysBudget(rate, burst float64, elapsed time.Duration) int {
	return int(burst + rate*elapsed.Seconds())
}

// runConcurrentAllowBoth releases callersPerPrincipal goroutines per
// principal at once (a start barrier maximizes overlap), each attempting
// attemptsPerCaller allowBoth calls against the same target, and returns
// how many calls were admitted per principal.
func runConcurrentAllowBoth(principals, target *keysRateLimiter, principalKeys []string, targetKey string, callersPerPrincipal, attemptsPerCaller int) []int {
	admitted := make([]int, len(principalKeys))
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for p, key := range principalKeys {
		for c := 0; c < callersPerPrincipal; c++ {
			wg.Add(1)
			go func(p int, key string) {
				defer wg.Done()
				<-start
				n := 0
				for i := 0; i < attemptsPerCaller; i++ {
					if ok, _ := allowBoth(principals, key, target, targetKey); ok {
						n++
					}
				}
				mu.Lock()
				admitted[p] += n
				mu.Unlock()
			}(p, key)
		}
	}
	close(start)
	wg.Wait()
	return admitted
}

// TestKeysAllowBoth_ConcurrentAdmissionWithinBudgets runs phases of
// concurrent allowBoth calls from several principals against one target
// with production rates and bursts. The fake clock only moves between
// phases, so each phase's admissions all happen at one known instant.
//
// The check is the token-bucket invariant over every window of phases
// [i, j]: admissions in the window must not exceed burst + rate*(t_j-t_i),
// for the target bucket and for each principal bucket. With the target as
// the binding bucket (aggregate principal budget always exceeds it), the
// target count per phase is also exact, so under-admission fails too.
func TestKeysAllowBoth_ConcurrentAdmissionWithinBudgets(t *testing.T) {
	const (
		numPrincipals       = 3
		callersPerPrincipal = 8
		attemptsPerCaller   = 4 // 32 attempts per principal per phase, above every per-phase budget
	)
	// Steps between phases. One second refills the target by 10 and each
	// principal by 5 (15 in aggregate), so the target stays the binding
	// bucket. The sub-second steps keep rate*step integral for both buckets.
	steps := []time.Duration{0, time.Second, 2 * time.Second, 400 * time.Millisecond, time.Second, 600 * time.Millisecond, 0, 3 * time.Second}

	// The exact per-phase target counts below hold only while the target is
	// the binding bucket: the principals together must out-refill and
	// out-burst it. Fail clearly if the agentkeys constants stop meeting that.
	if numPrincipals*agentkeys.PrincipalProjectRateLimit <= agentkeys.TargetRateLimit ||
		numPrincipals*agentkeys.PrincipalProjectBurst <= agentkeys.TargetBurst {
		t.Fatalf("test assumes the target bucket binds: need %d*PrincipalProjectRateLimit (%d) > TargetRateLimit (%d) and %d*PrincipalProjectBurst (%d) > TargetBurst (%d); retune numPrincipals or steps",
			numPrincipals, numPrincipals*agentkeys.PrincipalProjectRateLimit, agentkeys.TargetRateLimit,
			numPrincipals, numPrincipals*agentkeys.PrincipalProjectBurst, agentkeys.TargetBurst)
	}

	for round := 0; round < 20; round++ {
		clock := newKeysFakeClock()
		principals := newKeysRateLimiterWithClock(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst, clock.Now)
		target := newKeysRateLimiterWithClock(agentkeys.TargetRateLimit, agentkeys.TargetBurst, clock.Now)

		principalKeys := make([]string, numPrincipals)
		for p := range principalKeys {
			principalKeys[p] = fmt.Sprintf("agent:caller-%d:project-a", p)
		}

		var (
			phaseTimes     []time.Duration // offset of each phase from the first
			targetPerPhase []int
			principalPhase [][]int // [phase][principal]
			elapsed        time.Duration
		)
		for phase, step := range steps {
			clock.Advance(step)
			if phase > 0 {
				elapsed += step
			}
			got := runConcurrentAllowBoth(principals, target, principalKeys, "target-agent", callersPerPrincipal, attemptsPerCaller)
			total := 0
			for _, n := range got {
				total += n
			}
			phaseTimes = append(phaseTimes, elapsed)
			targetPerPhase = append(targetPerPhase, total)
			principalPhase = append(principalPhase, got)
		}

		// Exact target admissions: a full burst first, then exactly the
		// refill accrued since the previous phase (capped at burst).
		for phase, got := range targetPerPhase {
			want := agentkeys.TargetBurst
			if phase > 0 {
				// The bucket was emptied by the previous phase, so it now
				// holds the refill since then, capped at burst.
				want = min(agentkeys.TargetBurst, keysBudget(agentkeys.TargetRateLimit, 0, phaseTimes[phase]-phaseTimes[phase-1]))
			}
			if got != want {
				t.Fatalf("round %d phase %d: target admitted %d, want exactly %d (per-principal %v)", round, phase, got, want, principalPhase[phase])
			}
		}

		// Sliding-window bound over every window of phases.
		for i := range phaseTimes {
			targetSum := 0
			principalSum := make([]int, numPrincipals)
			for j := i; j < len(phaseTimes); j++ {
				window := phaseTimes[j] - phaseTimes[i]
				targetSum += targetPerPhase[j]
				if limit := keysBudget(agentkeys.TargetRateLimit, agentkeys.TargetBurst, window); targetSum > limit {
					t.Fatalf("round %d: target admitted %d over phases [%d,%d] (%v), budget %d", round, targetSum, i, j, window, limit)
				}
				for p := range principalSum {
					principalSum[p] += principalPhase[j][p]
					if limit := keysBudget(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst, window); principalSum[p] > limit {
						t.Fatalf("round %d: principal %d admitted %d over phases [%d,%d] (%v), budget %d", round, p, principalSum[p], i, j, window, limit)
					}
				}
			}
		}
	}
}

// TestKeysAllowBoth_ConcurrentAdmissionWithMovingClock lets the fake clock
// advance while the callers run, so refills interleave with admissions in
// an arbitrary order. Admission counts then vary between runs, but the
// totals must still fit each bucket's budget for the elapsed fake time.
func TestKeysAllowBoth_ConcurrentAdmissionWithMovingClock(t *testing.T) {
	const (
		numPrincipals       = 3
		callersPerPrincipal = 8
		attemptsPerCaller   = 200
		tick                = 10 * time.Millisecond
	)
	for round := 0; round < 10; round++ {
		clock := newKeysFakeClock()
		startTime := clock.Now()
		principals := newKeysRateLimiterWithClock(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst, clock.Now)
		target := newKeysRateLimiterWithClock(agentkeys.TargetRateLimit, agentkeys.TargetBurst, clock.Now)

		principalKeys := make([]string, numPrincipals)
		for p := range principalKeys {
			principalKeys[p] = fmt.Sprintf("agent:caller-%d:project-a", p)
		}

		stop := make(chan struct{})
		ticker := make(chan struct{})
		go func() {
			defer close(ticker)
			for {
				select {
				case <-stop:
					return
				default:
					clock.Advance(tick)
					time.Sleep(50 * time.Microsecond)
				}
			}
		}()
		got := runConcurrentAllowBoth(principals, target, principalKeys, "target-agent", callersPerPrincipal, attemptsPerCaller)
		close(stop)
		<-ticker

		elapsed := clock.Now().Sub(startTime)
		total := 0
		for p, n := range got {
			total += n
			if limit := keysBudget(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst, elapsed); n > limit {
				t.Fatalf("round %d: principal %d admitted %d in %v of fake time, budget %d", round, p, n, elapsed, limit)
			}
		}
		if limit := keysBudget(agentkeys.TargetRateLimit, agentkeys.TargetBurst, elapsed); total > limit {
			t.Fatalf("round %d: target admitted %d in %v of fake time, budget %d (per-principal %v)", round, total, elapsed, limit, got)
		}
		if total < agentkeys.TargetBurst {
			t.Fatalf("round %d: target admitted only %d, want at least its burst %d", round, total, agentkeys.TargetBurst)
		}
	}
}
