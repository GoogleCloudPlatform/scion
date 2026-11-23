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

package runtime

import (
	"context"
	"math"
	"sync"
	"time"
)

// dynSemaphore is a FIFO concurrency semaphore whose effective limit is
// supplied at acquire time so per-instance MaxInFlight updates take effect
// without rebuilding the Runtime. A limit <= 0 means unlimited.
//
// Waiting callers block only on their own goroutine until ctx expires (bounded
// by min(admissionWait, attemptDeadline)), so there is no unbounded queue.
type dynSemaphore struct {
	mu       sync.Mutex
	inFlight int
	waiters  []*semWaiter
}

type semWaiter struct {
	limit   int
	ch      chan struct{}
	granted bool
}

// acquire acquires one permit when limit > 0, waiting up to ctx.Done().
// When limit <= 0, it succeeds immediately with a no-op release function.
func (s *dynSemaphore) acquire(ctx context.Context, limit int) (func(), bool) {
	if limit <= 0 {
		return func() {}, true
	}
	if ctx.Err() != nil {
		return nil, false
	}

	s.mu.Lock()
	if s.inFlight < limit && len(s.waiters) == 0 {
		s.inFlight++
		s.mu.Unlock()
		return s.releaseOnce(), true
	}
	w := &semWaiter{
		limit: limit,
		ch:    make(chan struct{}),
	}
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()

	select {
	case <-w.ch:
		return s.releaseOnce(), true
	case <-ctx.Done():
		s.mu.Lock()
		if w.granted {
			// A release transferred a permit to us concurrently with ctx.Done().
			// Hand it back so no permit leaks.
			s.mu.Unlock()
			s.release()
			return nil, false
		}
		for i, cur := range s.waiters {
			if cur == w {
				copy(s.waiters[i:], s.waiters[i+1:])
				s.waiters[len(s.waiters)-1] = nil
				s.waiters = s.waiters[:len(s.waiters)-1]
				break
			}
		}
		s.mu.Unlock()
		return nil, false
	}
}

func (s *dynSemaphore) releaseOnce() func() {
	var once sync.Once
	return func() {
		once.Do(s.release)
	}
}

func (s *dynSemaphore) release() {
	s.mu.Lock()
	s.inFlight--
	if s.inFlight < 0 {
		s.inFlight = 0
	}
	// Wake the earliest waiter whose limit permits another in-flight call.
	for i := 0; i < len(s.waiters); i++ {
		w := s.waiters[i]
		if s.inFlight < w.limit {
			s.inFlight++
			w.granted = true
			copy(s.waiters[i:], s.waiters[i+1:])
			s.waiters[len(s.waiters)-1] = nil
			s.waiters = s.waiters[:len(s.waiters)-1]
			close(w.ch)
			break
		}
	}
	s.mu.Unlock()
}

// tokenBucket is a per-replica token bucket supporting dynamic RatePerSec
// updates (runtime-design §7.2). A rate <= 0 means unlimited.
//
// Limits are per Hub replica: on a multi-replica Hub, the aggregate rate across
// replicas is approximately RatePerSec * replicaCount (runtime-design §7.2).
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
}

// wait reserves one token at ratePerSec, waiting at most until waitDeadline
// (never beyond ctx.Done()). If the required wait exceeds the remaining time
// until waitDeadline, wait returns false immediately without sleeping
// (runtime-design §7.2).
func (b *tokenBucket) wait(ctx context.Context, ratePerSec float64, waitDeadline time.Time) bool {
	if ratePerSec <= 0 || math.IsNaN(ratePerSec) || math.IsInf(ratePerSec, 0) {
		return true
	}
	if ctx.Err() != nil {
		return false
	}

	now := time.Now()
	maxWait := waitDeadline.Sub(now)
	if maxWait < 0 {
		maxWait = 0
	}

	b.mu.Lock()
	burst := math.Max(1.0, math.Ceil(ratePerSec))
	if b.last.IsZero() {
		b.rate = ratePerSec
		b.burst = burst
		b.tokens = burst
		b.last = now
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = math.Min(b.burst, b.tokens+elapsed*b.rate)
			b.last = now
		}
		if b.rate != ratePerSec {
			b.rate = ratePerSec
			b.burst = burst
			if b.tokens > b.burst {
				b.tokens = b.burst
			}
		}
	}

	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		b.mu.Unlock()
		return true
	}

	deficit := 1.0 - b.tokens
	delay := time.Duration(math.Ceil((deficit / ratePerSec) * float64(time.Second)))
	if delay > maxWait {
		// Fail fast without sleeping when the token will not arrive before
		// the admission/attempt deadline.
		b.mu.Unlock()
		return false
	}

	// Reserve the token now (tokens may go negative up to burst-bounded waiters)
	// and sleep for delay; restore it if ctx cancels before the timer fires.
	b.tokens -= 1.0
	b.mu.Unlock()

	if delay <= 0 {
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		b.mu.Lock()
		b.tokens = math.Min(b.burst, b.tokens+1.0)
		b.mu.Unlock()
		return false
	}
}

type instanceLimiter struct {
	sem    dynSemaphore
	bucket tokenBucket
}

// admissionController enforces per-replica execution admission bounds
// (runtime-design §7.2):
//   - per-project in-flight semaphore (maxInFlightPerProject; <=0 = unlimited)
//   - per-instance in-flight semaphore (inst.Limits.MaxInFlight; <=0 = unlimited)
//   - per-instance token bucket (inst.Limits.RatePerSec; <=0 = unlimited)
//
// All limits are per replica. Acquisition is bounded by min(admissionWait,
// attemptDeadline) and acquires in a fixed order (project -> instance -> rate)
// so deadlocks cannot occur and one saturated project cannot starve another
// beyond its own limit.
type admissionController struct {
	maxInFlightPerProject int
	admissionWait         time.Duration

	mu        sync.Mutex
	instances map[string]*instanceLimiter
	projects  map[string]*dynSemaphore
}

func newAdmissionController(maxInFlightPerProject int, admissionWait time.Duration) *admissionController {
	return &admissionController{
		maxInFlightPerProject: maxInFlightPerProject,
		admissionWait:         admissionWait,
		instances:             make(map[string]*instanceLimiter),
		projects:              make(map[string]*dynSemaphore),
	}
}

func instanceLimiterKey(inst Instance) string {
	if inst.ID != "" {
		return "id:" + inst.ID
	}
	return "name:" + inst.Name
}

func (c *admissionController) instanceFor(inst Instance) *instanceLimiter {
	key := instanceLimiterKey(inst)
	c.mu.Lock()
	defer c.mu.Unlock()
	lim, ok := c.instances[key]
	if !ok {
		lim = &instanceLimiter{}
		c.instances[key] = lim
	}
	return lim
}

func (c *admissionController) projectFor(projectID string) *dynSemaphore {
	c.mu.Lock()
	defer c.mu.Unlock()
	sem, ok := c.projects[projectID]
	if !ok {
		sem = &dynSemaphore{}
		c.projects[projectID] = sem
	}
	return sem
}

func (c *admissionController) forgetInstance(id string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	delete(c.instances, "id:"+id)
	c.mu.Unlock()
}

// acquire acquires the per-project semaphore (when projectID != ""), the
// per-instance semaphore, and one per-instance rate token, bounded by
// min(now + admissionWait, attemptDeadline). On success, callers MUST invoke
// the returned release function once the attempt finishes.
func (c *admissionController) acquire(
	attemptCtx context.Context,
	projectID string,
	inst Instance,
) (func(), bool) {
	if c == nil {
		return func() {}, true
	}
	needsProject := projectID != "" && c.maxInFlightPerProject > 0
	needsInstSem := inst.Limits.MaxInFlight > 0
	needsRate := inst.Limits.RatePerSec > 0
	if !needsProject && !needsInstSem && !needsRate {
		return func() {}, true
	}
	if attemptCtx.Err() != nil {
		return nil, false
	}

	now := time.Now()
	waitDeadline := now.Add(c.admissionWait)
	if dl, ok := attemptCtx.Deadline(); ok && dl.Before(waitDeadline) {
		waitDeadline = dl
	}

	waitCtx, cancel := context.WithDeadline(attemptCtx, waitDeadline)
	defer cancel()

	var releaseProj func()
	if needsProject {
		var ok bool
		releaseProj, ok = c.projectFor(projectID).acquire(waitCtx, c.maxInFlightPerProject)
		if !ok {
			return nil, false
		}
	}

	var releaseInst func()
	if needsInstSem || needsRate {
		il := c.instanceFor(inst)
		if needsInstSem {
			var ok bool
			releaseInst, ok = il.sem.acquire(waitCtx, inst.Limits.MaxInFlight)
			if !ok {
				if releaseProj != nil {
					releaseProj()
				}
				return nil, false
			}
		}
		if needsRate {
			if !il.bucket.wait(waitCtx, inst.Limits.RatePerSec, waitDeadline) {
				if releaseInst != nil {
					releaseInst()
				}
				if releaseProj != nil {
					releaseProj()
				}
				return nil, false
			}
		}
	}

	return func() {
		if releaseInst != nil {
			releaseInst()
		}
		if releaseProj != nil {
			releaseProj()
		}
	}, true
}
