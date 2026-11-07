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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNotificationStore_ExcerptRoundTrip pins that the excerpt of a review
// summary notification (ptone/scion#6853) is stored and read back as it
// was written, in its own field, by both the single read and the list, and
// that a row which sets none reads empty.
func TestNotificationStore_ExcerptRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestNotificationStore(t)

	summary := newResourceNotification("user-1")
	summary.Kind = store.NotificationKindArtifactReviewSummary
	summary.Status = store.NotificationStatusArtifactReviewSubmitted
	summary.Message = "Review summary from Alice (v2 of your artifact):"
	summary.Excerpt = "Ship it. One question on the chart."
	require.NoError(t, s.CreateNotification(ctx, summary))
	plain := newResourceNotification("user-1")
	plain.ID = uuid.NewString()
	require.NoError(t, s.CreateNotification(ctx, plain))

	got, err := s.GetNotification(ctx, summary.ID)
	require.NoError(t, err)
	assert.Equal(t, summary.Excerpt, got.Excerpt)
	assert.Equal(t, summary.Message, got.Message, "the excerpt is not folded into the message")
	assert.Equal(t, store.NotificationKindArtifactReviewSummary, got.Kind)

	list, err := s.GetNotifications(ctx, store.SubscriberTypeUser, "user-1", false)
	require.NoError(t, err)
	byID := map[string]store.Notification{}
	for _, n := range list {
		byID[n.ID] = n
	}
	require.Len(t, byID, 2)
	assert.Equal(t, summary.Excerpt, byID[summary.ID].Excerpt)
	assert.Empty(t, byID[plain.ID].Excerpt, "a writer that sets no excerpt stores an empty one")
}

// TestNotificationStore_ReviewSummaryNotSwept pins that a review summary
// row is never handed to the dispatch sweep (the path that retries
// delivery to agents and plugins) and never expired by it: the sweep reads
// agent-subscriber rows only, and this kind is always a user row. The row
// even names a subscriber that is a live agent's slug in the same project.
// Controls: an eligible agent row IS returned by both sweep modes, and an
// old agent row IS expired, so a sweep or expiry that returned nothing
// could not pass.
func TestNotificationStore_ReviewSummaryNotSwept(t *testing.T) {
	ctx := context.Background()
	e := newRetryTestEnv(t)
	const broker = "broker-1"
	e.addAgent(t, "sub", broker, "running")
	eligible := e.addNotif(t, store.SubscriberTypeAgent, "sub", 2*time.Minute, false)
	stale := e.addNotif(t, store.SubscriberTypeAgent, "sub", 3*time.Hour, true)

	n := newResourceNotification("sub")
	n.ProjectID = e.projectID.String()
	n.Kind = store.NotificationKindArtifactReviewSummary
	n.Status = store.NotificationStatusArtifactReviewSubmitted
	n.Excerpt = "Looks good."
	n.CreatedAt = time.Now().Add(-48 * time.Hour)
	require.NoError(t, e.s.CreateNotification(ctx, n))

	for _, mode := range []string{"", broker} {
		got, err := e.s.GetUndispatchedAgentNotifications(ctx, mode)
		require.NoError(t, err)
		assert.Equal(t, []string{eligible}, notifIDs(got),
			"mode %q: the eligible agent row only, never the review summary row", mode)
	}

	// The stale agent row is claimed (dispatched) and old; release it so
	// expiry may decide it, as the control.
	reverted, err := e.s.UnmarkNotificationDispatched(ctx, stale, time.Now().Add(-time.Second))
	require.NoError(t, err)
	require.True(t, reverted)
	expired, err := e.s.ExpireStaleAgentNotifications(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, expired, "the old agent row is expired")
	assert.Equal(t, store.NotificationOutcomeFailed, e.get(t, stale).DispatchOutcome)

	got := e.get(t, n.ID)
	assert.False(t, got.Dispatched)
	assert.Empty(t, got.DispatchOutcome)
	assert.Empty(t, got.DispatchReason)
	assert.Equal(t, "Looks good.", got.Excerpt)
}
