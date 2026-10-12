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

// Package backoff computes retry delays for sciontool's long-running hub
// loops: exponential growth from a base delay to a cap on consecutive
// failures, with jitter, and a reset on success.
package backoff

import (
	"math/rand/v2"
	"time"
)

// DefaultBase replaces a non-positive Backoff.Base, so a misconfigured
// Backoff never retries in a hot loop.
const DefaultBase = time.Second

// DefaultJitter is the fraction of each delay that may be randomly removed
// so that many agents do not retry in lockstep.
const DefaultJitter = 0.2

// Backoff tracks consecutive failures of one loop. It is not safe for
// concurrent use; each loop owns its own Backoff.
type Backoff struct {
	// Base is the first failure delay. A non-positive Base is treated as
	// DefaultBase.
	Base time.Duration
	// Max caps the delay. A Max below the effective Base is treated as
	// that Base (no growth).
	Max time.Duration
	// Jitter is the fraction (0..1) of a delay that may be removed at
	// random. Jitter only shortens a delay, so Max is never exceeded.
	Jitter float64
	// Rand returns a value in [0,1). nil means math/rand/v2.Float64.
	Rand func() float64

	failures int
}

// Reset clears the failure count after a success.
func (b *Backoff) Reset() { b.failures = 0 }

// Failure records a failure and returns the delay before the next attempt:
// Base doubled once per consecutive failure, capped at Max, with jitter.
func (b *Backoff) Failure() time.Duration {
	b.failures++
	base, limit := b.bounds()
	d := base
	for i := 1; i < b.failures && d < limit; i++ {
		d *= 2
	}
	return b.jitter(min(d, limit))
}

// AtMax records a failure and returns the capped delay with jitter. Loops
// use it when retrying sooner cannot help, such as a refused credential.
func (b *Backoff) AtMax() time.Duration {
	b.failures++
	_, limit := b.bounds()
	return b.jitter(limit)
}

// bounds returns the effective base and cap.
func (b *Backoff) bounds() (base, limit time.Duration) {
	base = b.Base
	if base <= 0 {
		base = DefaultBase
	}
	return base, max(b.Max, base)
}

func (b *Backoff) jitter(d time.Duration) time.Duration {
	if b.Jitter <= 0 {
		return d
	}
	r := rand.Float64
	if b.Rand != nil {
		r = b.Rand
	}
	return d - time.Duration(float64(d)*b.Jitter*r())
}
