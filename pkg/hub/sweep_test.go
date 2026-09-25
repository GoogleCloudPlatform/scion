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

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSweepTestServer builds a minimal Server backed by an in-memory SQLite
// store, sufficient for exercising the recurring sweep handlers directly
// (bypassing the scheduler).
func newSweepTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	client := enttest.NewClient(t)
	cs := entadapter.NewCompositeStore(client)
	srv := &Server{
		store:             cs,
		agentLifecycleLog: slog.Default(),
	}
	return srv, cs
}

func TestFailedMessageRetentionHandler_DefaultsWhenUnconfigured(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	// Older than the default 7-day window.
	oldFailed := &store.Message{
		ID: uuid.NewString(), ProjectID: projectID,
		Sender: "user:x", Recipient: "agent:a", Msg: "old",
		DispatchState: store.MessageDispatchFailed,
		CreatedAt:     time.Now().Add(-8 * 24 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, oldFailed))

	// Within the default 7-day window.
	recentFailed := &store.Message{
		ID: uuid.NewString(), ProjectID: projectID,
		Sender: "user:x", Recipient: "agent:b", Msg: "recent",
		DispatchState: store.MessageDispatchFailed,
	}
	require.NoError(t, cs.CreateMessage(ctx, recentFailed))

	// srv.config.FailedMessageRetentionDays is left at its zero value —
	// the handler must fall back to defaultFailedMessageRetentionDays (7).
	srv.failedMessageRetentionHandler()(ctx)

	_, err := cs.GetMessage(ctx, oldFailed.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "message past the default retention window is purged")
	_, err = cs.GetMessage(ctx, recentFailed.ID)
	require.NoError(t, err, "message within the default retention window survives")
}

func TestFailedMessageRetentionHandler_HonorsConfiguredWindow(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	srv.config.FailedMessageRetentionDays = 1
	ctx := context.Background()
	projectID := uuid.NewString()

	old := &store.Message{
		ID: uuid.NewString(), ProjectID: projectID,
		Sender: "user:x", Recipient: "agent:a", Msg: "old",
		DispatchState: store.MessageDispatchFailed,
		CreatedAt:     time.Now().Add(-2 * 24 * time.Hour),
	}
	require.NoError(t, cs.CreateMessage(ctx, old))

	srv.failedMessageRetentionHandler()(ctx)

	_, err := cs.GetMessage(ctx, old.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "configured 1-day window purges the 2-day-old message")
}

func TestBrokerMessageSweepHandler_FailsPendingMessagesWithMissingRecipient(t *testing.T) {
	srv, cs := newSweepTestServer(t)
	ctx := context.Background()

	proj := &store.Project{
		ID: uuid.NewString(), Name: "p", Slug: "p-" + uuid.NewString()[:8],
		OwnerID: uuid.NewString(),
	}
	require.NoError(t, cs.CreateProject(ctx, proj))

	// A pending message addressed to an agent ID that does not exist
	// (simulating a deleted/purged recipient) must be failed early by the
	// sweep, well before the 24h stuck-pending TTL.
	orphan := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:gone", RecipientID: uuid.NewString(), Msg: "hi",
	}
	require.NoError(t, cs.CreateMessage(ctx, orphan))
	assert.Equal(t, store.MessageDispatchPending, orphan.DispatchState)

	srv.brokerMessageSweepHandler()(ctx)

	got, err := cs.GetMessage(ctx, orphan.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, got.DispatchState)
	require.NotNil(t, got.DispatchFailureReason)
	assert.Equal(t, missingRecipientFailureReason, *got.DispatchFailureReason)
}
