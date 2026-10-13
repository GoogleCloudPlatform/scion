// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type linkVerifyService interface {
	AllowVerify(string) bool
	Close()
}

func TestLinkServicesAllowVerifyRateLimit(t *testing.T) {
	tests := []struct {
		name string
		new  func() linkVerifyService
	}{
		{name: "telegram", new: func() linkVerifyService { return NewTelegramLinkService() }},
		{name: "discord", new: func() linkVerifyService { return NewDiscordLinkService() }},
		{name: "teams", new: func() linkVerifyService { return NewTeamsLinkService() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := tt.new()
			defer svc.Close()

			for i := 0; i < verifyBurst; i++ {
				assert.True(t, svc.AllowVerify("192.168.1.1"), "attempt %d should be allowed", i)
			}
			assert.False(t, svc.AllowVerify("192.168.1.1"))
			assert.True(t, svc.AllowVerify("10.0.0.1"))
		})
	}
}

func TestLinkVerifyLimiterCleanup(t *testing.T) {
	limiter := newLinkVerifyLimiter()
	require.True(t, limiter.Allow("stale"))
	require.True(t, limiter.Allow("fresh"))

	now := time.Now()
	limiter.mu.Lock()
	limiter.buckets["stale"].lastCheck = now.Add(-verifyLimiterMaxAge - time.Second)
	limiter.buckets["fresh"].lastCheck = now.Add(-verifyLimiterMaxAge + time.Second)
	limiter.mu.Unlock()

	limiter.Cleanup(now)

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	assert.NotContains(t, limiter.buckets, "stale")
	assert.Contains(t, limiter.buckets, "fresh")
}
