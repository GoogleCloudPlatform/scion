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

package entadapter_test

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDiscordPendingLinkStore(t *testing.T) *entadapter.DiscordPendingLinkStore {
	t.Helper()
	return entadapter.NewDiscordPendingLinkStore(enttest.NewClient(t))
}

func TestDiscordPendingLink_CreateAndGetByCode(t *testing.T) {
	s := newDiscordPendingLinkStore(t)
	ctx := context.Background()

	link := &store.DiscordPendingLink{
		Code:          "ABC123",
		DiscordUserID: "discord-user-1",
		Status:        "pending",
		ExpiresAt:     time.Now().Add(15 * time.Minute),
	}
	require.NoError(t, s.CreateDiscordPendingLink(ctx, link))
	assert.NotEmpty(t, link.ID)

	got, err := s.GetDiscordPendingLinkByCode(ctx, "ABC123")
	require.NoError(t, err)
	assert.Equal(t, "ABC123", got.Code)
	assert.Equal(t, "discord-user-1", got.DiscordUserID)
	assert.Equal(t, "pending", got.Status)
}

func TestDiscordPendingLink_GetByCode_NotFound(t *testing.T) {
	s := newDiscordPendingLinkStore(t)
	ctx := context.Background()

	_, err := s.GetDiscordPendingLinkByCode(ctx, "NONEXISTENT")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDiscordPendingLink_GetByDiscordUser(t *testing.T) {
	s := newDiscordPendingLinkStore(t)
	ctx := context.Background()

	link := &store.DiscordPendingLink{
		Code:          "XYZ789",
		DiscordUserID: "discord-user-2",
		Status:        "pending",
		ExpiresAt:     time.Now().Add(15 * time.Minute),
	}
	require.NoError(t, s.CreateDiscordPendingLink(ctx, link))

	got, err := s.GetDiscordPendingLinkByDiscordUser(ctx, "discord-user-2")
	require.NoError(t, err)
	assert.Equal(t, "XYZ789", got.Code)
}

func TestDiscordPendingLink_Update(t *testing.T) {
	s := newDiscordPendingLinkStore(t)
	ctx := context.Background()

	link := &store.DiscordPendingLink{
		Code:          "UPD001",
		DiscordUserID: "discord-user-3",
		Status:        "pending",
		ExpiresAt:     time.Now().Add(15 * time.Minute),
	}
	require.NoError(t, s.CreateDiscordPendingLink(ctx, link))

	link.Status = "confirmed"
	link.UserID = "user-42"
	link.UserEmail = "user@example.com"
	require.NoError(t, s.UpdateDiscordPendingLink(ctx, link))

	got, err := s.GetDiscordPendingLinkByCode(ctx, "UPD001")
	require.NoError(t, err)
	assert.Equal(t, "confirmed", got.Status)
	assert.Equal(t, "user-42", got.UserID)
	assert.Equal(t, "user@example.com", got.UserEmail)
}

func TestDiscordPendingLink_DeleteByCode(t *testing.T) {
	s := newDiscordPendingLinkStore(t)
	ctx := context.Background()

	link := &store.DiscordPendingLink{
		Code:          "DEL001",
		DiscordUserID: "discord-user-4",
		Status:        "pending",
		ExpiresAt:     time.Now().Add(15 * time.Minute),
	}
	require.NoError(t, s.CreateDiscordPendingLink(ctx, link))
	require.NoError(t, s.DeleteDiscordPendingLink(ctx, "DEL001"))

	_, err := s.GetDiscordPendingLinkByCode(ctx, "DEL001")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDiscordPendingLink_DeleteByDiscordUser(t *testing.T) {
	s := newDiscordPendingLinkStore(t)
	ctx := context.Background()

	link := &store.DiscordPendingLink{
		Code:          "DEL002",
		DiscordUserID: "discord-user-5",
		Status:        "pending",
		ExpiresAt:     time.Now().Add(15 * time.Minute),
	}
	require.NoError(t, s.CreateDiscordPendingLink(ctx, link))
	require.NoError(t, s.DeleteDiscordPendingLinksByDiscordUser(ctx, "discord-user-5"))

	_, err := s.GetDiscordPendingLinkByCode(ctx, "DEL002")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDiscordPendingLink_DeleteExpired(t *testing.T) {
	s := newDiscordPendingLinkStore(t)
	ctx := context.Background()

	// Create one expired and one valid link.
	expired := &store.DiscordPendingLink{
		Code:          "EXP001",
		DiscordUserID: "discord-expired",
		Status:        "pending",
		ExpiresAt:     time.Now().Add(-1 * time.Minute),
	}
	valid := &store.DiscordPendingLink{
		Code:          "VAL001",
		DiscordUserID: "discord-valid",
		Status:        "pending",
		ExpiresAt:     time.Now().Add(15 * time.Minute),
	}
	require.NoError(t, s.CreateDiscordPendingLink(ctx, expired))
	require.NoError(t, s.CreateDiscordPendingLink(ctx, valid))

	n, err := s.DeleteExpiredDiscordPendingLinks(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// Expired link should be gone.
	_, err = s.GetDiscordPendingLinkByCode(ctx, "EXP001")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Valid link should still exist.
	got, err := s.GetDiscordPendingLinkByCode(ctx, "VAL001")
	require.NoError(t, err)
	assert.Equal(t, "VAL001", got.Code)
}

func TestDiscordPendingLink_DuplicateCode(t *testing.T) {
	s := newDiscordPendingLinkStore(t)
	ctx := context.Background()

	link := &store.DiscordPendingLink{
		Code:          "DUP001",
		DiscordUserID: "discord-user-dup1",
		Status:        "pending",
		ExpiresAt:     time.Now().Add(15 * time.Minute),
	}
	require.NoError(t, s.CreateDiscordPendingLink(ctx, link))

	dup := &store.DiscordPendingLink{
		Code:          "DUP001",
		DiscordUserID: "discord-user-dup2",
		Status:        "pending",
		ExpiresAt:     time.Now().Add(15 * time.Minute),
	}
	err := s.CreateDiscordPendingLink(ctx, dup)
	assert.Error(t, err)
}
