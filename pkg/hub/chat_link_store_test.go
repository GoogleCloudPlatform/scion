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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHashCode_Deterministic(t *testing.T) {
	h1 := hashCode("ABC123")
	h2 := hashCode("ABC123")
	assert.Equal(t, h1, h2, "same input should produce same hash")
}

func TestHashCode_CaseInsensitive(t *testing.T) {
	h1 := hashCode("abc123")
	h2 := hashCode("ABC123")
	assert.Equal(t, h1, h2, "hash should be case-insensitive (uppercased before hashing)")
}

func TestHashCode_DifferentCodes(t *testing.T) {
	h1 := hashCode("ABC123")
	h2 := hashCode("XYZ789")
	assert.NotEqual(t, h1, h2, "different codes should produce different hashes")
}

func TestHashCode_NotEmpty(t *testing.T) {
	h := hashCode("TESTCODE")
	assert.NotEmpty(t, h)
	assert.Len(t, h, 64, "SHA-256 hex digest should be 64 characters")
}

func TestTelegramLinkService_FallsBackToInMemory(t *testing.T) {
	// When no DB store is set, the service should use in-memory storage.
	svc := NewTelegramLinkService()
	defer svc.Close()

	svc.RegisterCode("TELE01", "tg-user-1")

	uid, reason := svc.VerifyCode("TELE01", "user-1", "user@test.com")
	assert.Empty(t, reason)
	assert.Equal(t, "tg-user-1", uid)

	status, userID, email := svc.GetStatusByTelegramUser("tg-user-1")
	assert.Equal(t, "confirmed", status)
	assert.Equal(t, "user-1", userID)
	assert.Equal(t, "user@test.com", email)
}

func TestDiscordLinkService_FallsBackToInMemory(t *testing.T) {
	svc := NewDiscordLinkService()
	defer svc.Close()

	svc.RegisterCode("DISC01", "dc-user-1")

	uid, reason := svc.VerifyCode("DISC01", "user-1", "user@test.com")
	assert.Empty(t, reason)
	assert.Equal(t, "dc-user-1", uid)

	status, userID, email := svc.GetStatusByDiscordUser("dc-user-1")
	assert.Equal(t, "confirmed", status)
	assert.Equal(t, "user-1", userID)
	assert.Equal(t, "user@test.com", email)
}

func TestTeamsLinkService_FallsBackToInMemory_WithStore(t *testing.T) {
	// Even with no DB configured, the service should work via in-memory.
	svc := NewTeamsLinkService()
	defer svc.Close()

	svc.RegisterCode("TEAM01", "teams-user-1")

	uid, reason := svc.VerifyCode("TEAM01", "user-1", "user@test.com")
	assert.Empty(t, reason)
	assert.Equal(t, "teams-user-1", uid)

	svc.ConsumePending("teams-user-1")

	status, _, _ := svc.GetStatusByTeamsUser("teams-user-1")
	assert.Equal(t, "not_found", status)
}
