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

package trigger

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestComputeRefillPeriod_ClampsOverflowForTinyRates(t *testing.T) {
	assert.Equal(t, 5*time.Second, computeRefillPeriod(20, 100))
	assert.Equal(t, time.Duration(0), computeRefillPeriod(0, 100))
	assert.Equal(t, time.Duration(0), computeRefillPeriod(-1, 100))
	assert.Equal(t, time.Duration(0), computeRefillPeriod(10, 0))
	assert.Equal(t, time.Duration(0), computeRefillPeriod(math.NaN(), 100))
	assert.Equal(t, time.Duration(0), computeRefillPeriod(math.Inf(1), 100))

	for _, rate := range []float64{1e-12, 1e-20, 1e-300, math.SmallestNonzeroFloat64} {
		rp := computeRefillPeriod(rate, 100)
		assert.Equal(t, time.Duration(math.MaxInt64), rp, "tiny rate %g must clamp to MaxInt64, not wrap negative", rate)
		assert.Greater(t, rp, time.Duration(0))
	}

	// Verify a keyedBucketSet configured with an extremely small rate never
	// wraps refillPeriod negative and does not prematurely evict drained buckets.
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ks := newKeyedBucketSet(1e-12, 10, 1, time.Hour, nil, nil)
	assert.Equal(t, time.Duration(math.MaxInt64), ks.refillPeriod)
	assert.True(t, ks.allow("k1", now))
	// After 1 second, k1 is NOT refilled and NOT expired, so k2 cannot evict it.
	assert.False(t, ks.allow("k2", now.Add(time.Second)))
}

func TestAllowEndpointWithLimit_LowerAndClamp(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	lim := NewIngressLimiter(LimiterConfig{Now: func() time.Time { return now }})

	// Endpoint with lower burst=2 allows 2, rejects 3rd.
	assert.True(t, lim.AllowEndpointWithLimit("ep-low", 1, 2))
	assert.True(t, lim.AllowEndpointWithLimit("ep-low", 1, 2))
	assert.False(t, lim.AllowEndpointWithLimit("ep-low", 1, 2))

	// Advancing 1s refills 1 token at rate=1/s.
	now = now.Add(time.Second)
	assert.True(t, lim.AllowEndpointWithLimit("ep-low", 1, 2))
	assert.False(t, lim.AllowEndpointWithLimit("ep-low", 1, 2))

	// Attempting to exceed hub constants (rate=999, burst=999) clamps to hub burst (60).
	for i := 0; i < PerEndpointBurst; i++ {
		assert.True(t, lim.AllowEndpointWithLimit("ep-clamped", 999, 999), "request %d should pass", i)
	}
	assert.False(t, lim.AllowEndpointWithLimit("ep-clamped", 999, 999), "burst above hub constant must be clamped")
}
