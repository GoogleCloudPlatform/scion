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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memDiscordPendingLinkStore is an in-memory implementation of
// store.DiscordPendingLinkStore for testing the DiscordLinkService without
// a database.
type memDiscordPendingLinkStore struct {
	mu    sync.Mutex
	links map[string]*store.DiscordPendingLink // code → link
	seq   int
}

func newMemDiscordPendingLinkStore() *memDiscordPendingLinkStore {
	return &memDiscordPendingLinkStore{
		links: make(map[string]*store.DiscordPendingLink),
	}
}

func (m *memDiscordPendingLinkStore) CreateDiscordPendingLink(_ context.Context, link *store.DiscordPendingLink) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.links[link.Code]; exists {
		return store.ErrAlreadyExists
	}
	m.seq++
	if link.ID == "" {
		link.ID = "mem-" + link.Code
	}
	m.links[link.Code] = link
	return nil
}

func (m *memDiscordPendingLinkStore) GetDiscordPendingLinkByCode(_ context.Context, code string) (*store.DiscordPendingLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.links[code]
	if !ok {
		return nil, store.ErrNotFound
	}
	return l, nil
}

func (m *memDiscordPendingLinkStore) GetDiscordPendingLinkByDiscordUser(_ context.Context, discordUserID string) (*store.DiscordPendingLink, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.links {
		if l.DiscordUserID == discordUserID {
			return l, nil
		}
	}
	return nil, store.ErrNotFound
}

func (m *memDiscordPendingLinkStore) UpdateDiscordPendingLink(_ context.Context, link *store.DiscordPendingLink) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for code, l := range m.links {
		if l.ID == link.ID {
			link.Code = code
			m.links[code] = link
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *memDiscordPendingLinkStore) DeleteDiscordPendingLink(_ context.Context, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.links, code)
	return nil
}

func (m *memDiscordPendingLinkStore) DeleteDiscordPendingLinksByDiscordUser(_ context.Context, discordUserID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for code, l := range m.links {
		if l.DiscordUserID == discordUserID {
			delete(m.links, code)
		}
	}
	return nil
}

func (m *memDiscordPendingLinkStore) DeleteExpiredDiscordPendingLinks(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	now := time.Now()
	for code, l := range m.links {
		if now.After(l.ExpiresAt) {
			delete(m.links, code)
			n++
		}
	}
	return n, nil
}

func newTestDiscordLinkService() (*DiscordLinkService, *memDiscordPendingLinkStore) {
	ms := newMemDiscordPendingLinkStore()
	svc := NewDiscordLinkService(ms)
	return svc, ms
}

func TestDiscordLinkService_RegisterAndVerify(t *testing.T) {
	svc, _ := newTestDiscordLinkService()
	defer svc.Close()

	svc.RegisterCode("abc123", "discord-user-1")

	discordUserID, errReason := svc.VerifyCode(context.Background(), "ABC123", "user-1", "user1@example.com")
	assert.Empty(t, errReason)
	assert.Equal(t, "discord-user-1", discordUserID)
}

func TestDiscordLinkService_VerifyNotFound(t *testing.T) {
	svc, _ := newTestDiscordLinkService()
	defer svc.Close()

	_, errReason := svc.VerifyCode(context.Background(), "NONEXISTENT", "user-1", "user1@example.com")
	assert.Equal(t, "code_not_found", errReason)
}

func TestDiscordLinkService_VerifyExpired(t *testing.T) {
	svc, ms := newTestDiscordLinkService()
	defer svc.Close()

	svc.RegisterCode("exp001", "discord-user-2")

	// Manually expire the link.
	ms.mu.Lock()
	ms.links["EXP001"].ExpiresAt = time.Now().Add(-1 * time.Minute)
	ms.mu.Unlock()

	_, errReason := svc.VerifyCode(context.Background(), "EXP001", "user-2", "user2@example.com")
	assert.Equal(t, "code_expired", errReason)
}

func TestDiscordLinkService_RegisterReplacesExisting(t *testing.T) {
	svc, _ := newTestDiscordLinkService()
	defer svc.Close()

	svc.RegisterCode("first", "discord-user-3")
	svc.RegisterCode("second", "discord-user-3")

	// First code should be gone.
	_, errReason := svc.VerifyCode(context.Background(), "FIRST", "user-3", "user3@example.com")
	assert.Equal(t, "code_not_found", errReason)

	// Second code should work.
	discordUserID, errReason := svc.VerifyCode(context.Background(), "SECOND", "user-3", "user3@example.com")
	assert.Empty(t, errReason)
	assert.Equal(t, "discord-user-3", discordUserID)
}

func TestDiscordLinkService_GetStatusByDiscordUser(t *testing.T) {
	svc, _ := newTestDiscordLinkService()
	defer svc.Close()

	// Not found before registration.
	status, _, _ := svc.GetStatusByDiscordUser("discord-user-4")
	assert.Equal(t, "not_found", status)

	svc.RegisterCode("stat001", "discord-user-4")

	// Pending after registration.
	status, _, _ = svc.GetStatusByDiscordUser("discord-user-4")
	assert.Equal(t, "pending", status)

	// Confirmed after verification.
	svc.VerifyCode(context.Background(), "STAT001", "user-4", "user4@example.com")
	status, userID, userEmail := svc.GetStatusByDiscordUser("discord-user-4")
	assert.Equal(t, "confirmed", status)
	assert.Equal(t, "user-4", userID)
	assert.Equal(t, "user4@example.com", userEmail)
}

func TestDiscordLinkService_ConsumePending(t *testing.T) {
	svc, _ := newTestDiscordLinkService()
	defer svc.Close()

	svc.RegisterCode("con001", "discord-user-5")
	svc.VerifyCode(context.Background(), "CON001", "user-5", "user5@example.com")
	svc.ConsumePending("discord-user-5")

	status, _, _ := svc.GetStatusByDiscordUser("discord-user-5")
	assert.Equal(t, "not_found", status)
}

func TestDiscordLinkService_AllowVerify_RateLimit(t *testing.T) {
	svc, _ := newTestDiscordLinkService()
	defer svc.Close()

	ip := "192.0.2.1"
	// Should allow the first verifyBurst attempts.
	for i := 0; i < verifyBurst; i++ {
		require.True(t, svc.AllowVerify(ip), "attempt %d should be allowed", i)
	}
	// Next attempt should be rate limited.
	assert.False(t, svc.AllowVerify(ip))
}

func TestDiscordLinkService_VerifyAlreadyConfirmed(t *testing.T) {
	svc, _ := newTestDiscordLinkService()
	defer svc.Close()

	svc.RegisterCode("conf001", "discord-user-6")
	discordUserID, errReason := svc.VerifyCode(context.Background(), "CONF001", "user-6", "user6@example.com")
	assert.Empty(t, errReason)
	assert.Equal(t, "discord-user-6", discordUserID)

	// Verify again should return the confirmed discord user ID.
	discordUserID, errReason = svc.VerifyCode(context.Background(), "CONF001", "user-other", "other@example.com")
	assert.Empty(t, errReason)
	assert.Equal(t, "discord-user-6", discordUserID)
}
