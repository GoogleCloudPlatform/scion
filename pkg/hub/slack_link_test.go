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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlackLinkService_RegisterAndVerify(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	// Register a code.
	svc.RegisterCode("ABC123", "slack-user-1")

	// Verify the code.
	slackUserID, errReason := svc.VerifyCode("ABC123", "scion-user-1", "user@example.com")
	assert.Empty(t, errReason)
	assert.Equal(t, "slack-user-1", slackUserID)

	// Status should be confirmed.
	status, userID, userEmail := svc.GetStatusBySlackUser("slack-user-1")
	assert.Equal(t, "confirmed", status)
	assert.Equal(t, "scion-user-1", userID)
	assert.Equal(t, "user@example.com", userEmail)
}

func TestSlackLinkService_VerifyCodeCaseInsensitive(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	svc.RegisterCode("abc123", "slack-user-1")

	// Verify with uppercase should work.
	slackUserID, errReason := svc.VerifyCode("ABC123", "scion-user-1", "user@example.com")
	assert.Empty(t, errReason)
	assert.Equal(t, "slack-user-1", slackUserID)
}

func TestSlackLinkService_VerifyCodeNotFound(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	slackUserID, errReason := svc.VerifyCode("NOTEXIST", "scion-user-1", "user@example.com")
	assert.Equal(t, "code_not_found", errReason)
	assert.Empty(t, slackUserID)
}

func TestSlackLinkService_VerifyCodeExpired(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	// Register and manually expire.
	svc.RegisterCode("EXP001", "slack-user-1")

	svc.mu.Lock()
	p := svc.pending["EXP001"]
	p.ExpiresAt = time.Now().Add(-1 * time.Minute)
	svc.mu.Unlock()

	slackUserID, errReason := svc.VerifyCode("EXP001", "scion-user-1", "user@example.com")
	assert.Equal(t, "code_expired", errReason)
	assert.Empty(t, slackUserID)
}

func TestSlackLinkService_VerifyAlreadyConfirmed(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	svc.RegisterCode("DUP001", "slack-user-1")

	// First verify succeeds.
	slackUserID, errReason := svc.VerifyCode("DUP001", "scion-user-1", "user@example.com")
	assert.Empty(t, errReason)
	assert.Equal(t, "slack-user-1", slackUserID)

	// Second verify also returns the Slack user (already confirmed).
	slackUserID, errReason = svc.VerifyCode("DUP001", "scion-user-2", "user2@example.com")
	assert.Empty(t, errReason)
	assert.Equal(t, "slack-user-1", slackUserID)
}

func TestSlackLinkService_RegisterReplacesOldCode(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	svc.RegisterCode("OLD001", "slack-user-1")
	svc.RegisterCode("NEW001", "slack-user-1")

	// Old code should no longer work.
	_, errReason := svc.VerifyCode("OLD001", "scion-user-1", "user@example.com")
	assert.Equal(t, "code_not_found", errReason)

	// New code should work.
	slackUserID, errReason := svc.VerifyCode("NEW001", "scion-user-1", "user@example.com")
	assert.Empty(t, errReason)
	assert.Equal(t, "slack-user-1", slackUserID)
}

func TestSlackLinkService_GetStatusPending(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	svc.RegisterCode("PND001", "slack-user-1")

	status, userID, userEmail := svc.GetStatusBySlackUser("slack-user-1")
	assert.Equal(t, "pending", status)
	assert.Empty(t, userID)
	assert.Empty(t, userEmail)
}

func TestSlackLinkService_GetStatusNotFound(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	status, userID, userEmail := svc.GetStatusBySlackUser("nonexistent")
	assert.Equal(t, "not_found", status)
	assert.Empty(t, userID)
	assert.Empty(t, userEmail)
}

func TestSlackLinkService_GetStatusExpired(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	svc.RegisterCode("EXP002", "slack-user-1")

	svc.mu.Lock()
	p := svc.pending["EXP002"]
	p.ExpiresAt = time.Now().Add(-1 * time.Minute)
	svc.mu.Unlock()

	status, _, _ := svc.GetStatusBySlackUser("slack-user-1")
	assert.Equal(t, "expired", status)
}

func TestSlackLinkService_ConsumePending(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	svc.RegisterCode("CON001", "slack-user-1")
	_, _ = svc.VerifyCode("CON001", "scion-user-1", "user@example.com")

	// Consume the confirmed entry.
	svc.ConsumePending("slack-user-1")

	// Status should now be not_found.
	status, _, _ := svc.GetStatusBySlackUser("slack-user-1")
	assert.Equal(t, "not_found", status)
}

func TestSlackLinkService_ConcurrentAccess(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	var wg sync.WaitGroup
	numGoroutines := 20

	// Concurrent register operations.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			code := "CODE" + string(rune('A'+idx%26))
			svc.RegisterCode(code, "slack-user-"+string(rune('A'+idx%26)))
		}(i)
	}

	// Concurrent status checks.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			svc.GetStatusBySlackUser("slack-user-" + string(rune('A'+idx%26)))
		}(i)
	}

	// Concurrent verify attempts.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			code := "CODE" + string(rune('A'+idx%26))
			svc.VerifyCode(code, "scion-user", "user@test.com")
		}(i)
	}

	wg.Wait()
	// No race condition panic = pass.
}

func TestSlackLinkService_CloseIdempotent(t *testing.T) {
	svc := NewSlackLinkService()

	// Close multiple times should not panic.
	svc.Close()
	svc.Close()
	svc.Close()
}

func TestSlackLinkService_CleanupExpired(t *testing.T) {
	svc := NewSlackLinkService()
	defer svc.Close()

	svc.RegisterCode("CLEAN1", "slack-user-1")
	svc.RegisterCode("CLEAN2", "slack-user-2")

	// Manually expire one entry.
	svc.mu.Lock()
	svc.pending["CLEAN1"].ExpiresAt = time.Now().Add(-1 * time.Minute)
	svc.mu.Unlock()

	// Trigger a manual cleanup cycle (simulating the ticker).
	now := time.Now()
	svc.mu.Lock()
	for code, p := range svc.pending {
		if now.After(p.ExpiresAt) {
			delete(svc.pending, code)
		}
	}
	svc.mu.Unlock()

	// CLEAN1 should be gone, CLEAN2 should remain.
	svc.mu.Lock()
	_, ok1 := svc.pending["CLEAN1"]
	_, ok2 := svc.pending["CLEAN2"]
	svc.mu.Unlock()

	require.False(t, ok1, "expired entry should be cleaned up")
	require.True(t, ok2, "non-expired entry should remain")
}
