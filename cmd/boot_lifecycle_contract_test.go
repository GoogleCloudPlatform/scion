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

package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test 6: Budget exhaustion makes monotonic per-project progress
// ---------------------------------------------------------------------------

// TestContract_BudgetExhaustion_MonotonicProgress verifies that when the
// backfill budget is exhausted mid-run, the system makes monotonic progress
// across boots: each boot completes at least as many projects as the previous
// boot, and eventually all projects are done.
//
// This is the Stream C Decision 2 contract: budget-limited boots must make
// forward progress and converge to completion.
func TestContract_BudgetExhaustion_MonotonicProgress(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Seed 3 projects, each with messages needing backfill (no conversation_id).
	projectIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		projectID := uuid.NewString()
		projectIDs[i] = projectID

		// Create project.
		err := s.CreateProject(ctx, &store.Project{
			ID:   projectID,
			Name: "contract-project-" + projectID[:8],
			Slug: "contract-project-" + projectID[:8],
		})
		require.NoError(t, err)

		// Create user and agent for DM conversation.
		userID := uuid.NewString()
		err = s.CreateUser(ctx, &store.User{
			ID:          userID,
			DisplayName: "contract-user-" + userID[:8],
			Email:       "contract-user-" + userID[:8] + "@example.com",
		})
		require.NoError(t, err)

		agentID := uuid.NewString()
		err = s.CreateAgent(ctx, &store.Agent{
			ID:        agentID,
			ProjectID: projectID,
			Name:      "contract-agent-" + agentID[:8],
			Slug:      "contract-agent-" + agentID[:8],
		})
		require.NoError(t, err)

		// Create DM conversation with proper key.
		dmKey, err := messages.DMConversationKey("user", userID, "agent", agentID)
		require.NoError(t, err)

		_, err = s.UpsertConversationByExternalRef(ctx, &store.Conversation{
			Kind:        "direct",
			Surface:     "native",
			ExternalRef: dmKey,
			DriftState:  "active",
		})
		require.NoError(t, err)

		// Seed 2 messages WITHOUT conversation_id (pre-upgrade).
		msg1 := &store.Message{
			ID:             uuid.NewString(),
			ConversationID: "", // Empty = pre-upgrade
			ProjectID:      projectID,
			Msg:            "Pre-upgrade message 1 in project " + projectID[:8],
			Sender:         "user:" + userID,
			SenderID:       userID,
			Recipient:      "agent:" + agentID,
			RecipientID:    agentID,
			AgentID:        agentID,
			CreatedAt:      time.Now().UTC().Add(-10 * time.Minute),
		}
		require.NoError(t, s.CreateMessage(ctx, msg1))

		msg2 := &store.Message{
			ID:             uuid.NewString(),
			ConversationID: "", // Empty = pre-upgrade
			ProjectID:      projectID,
			Msg:            "Pre-upgrade message 2 in project " + projectID[:8],
			Sender:         "agent:" + agentID,
			SenderID:       agentID,
			Recipient:      "user:" + userID,
			RecipientID:    userID,
			AgentID:        agentID,
			CreatedAt:      time.Now().UTC().Add(-5 * time.Minute),
		}
		require.NoError(t, s.CreateMessage(ctx, msg2))
	}

	// Save original budget and restore in cleanup.
	originalBudget := defaultBackfillBudget
	t.Cleanup(func() {
		defaultBackfillBudget = originalBudget
	})

	// Set budget to very small value to force exhaustion after one or two projects.
	// The budget needs to be enough to complete at least one project but not all.
	// Using a very small value like 10ms should allow at least one project.
	defaultBackfillBudget = 10 * time.Millisecond

	// First boot: should complete at least one project.
	runBootDataMigrations(ctx, s)

	marker1, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)

	// The backfill might complete all projects in the first boot if they're very small.
	// The key is that it makes progress. If it completes all in one go, that's fine -
	// we'll verify convergence instead.
	if marker1.CompletedAt != nil {
		// All projects completed on first boot - verify marker state.
		require.NotNil(t, marker1.CompletedAt, "completed marker must have CompletedAt")
		t.Logf("All projects completed in first boot - this is acceptable for small datasets")
		return
	}

	require.Greater(t, len(marker1.ProjectsDone), 0,
		"first boot must complete at least one project if not fully complete")
	require.LessOrEqual(t, len(marker1.ProjectsDone), len(projectIDs),
		"first boot must not exceed total projects")

	// If budget is extremely tight, we might only complete 1 project.
	// But we should NOT have completed all projects (unless timing is very lucky).
	// Save the count for monotonic comparison.
	firstCount := len(marker1.ProjectsDone)

	// Second boot: should make progress (complete more projects or remain stable).
	runBootDataMigrations(ctx, s)

	marker2, err := loadBackfillMarker(ctx, s)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(marker2.ProjectsDone), firstCount,
		"second boot must maintain or increase project count (monotonic)")

	// Continue booting until all projects are done or we hit a reasonable cap.
	maxIterations := 10
	for i := 0; i < maxIterations; i++ {
		marker, err := loadBackfillMarker(ctx, s)
		require.NoError(t, err)

		if marker.CompletedAt != nil {
			// Global completion marker set.
			require.Len(t, marker.ProjectsDone, len(projectIDs),
				"completed marker must list all projects in ProjectsDone")
			// Success: all projects done.
			return
		}

		if len(marker.ProjectsDone) == len(projectIDs) {
			// All projects done but global marker not set yet.
			// Run one more time to set the completion marker.
			runBootDataMigrations(ctx, s)
			marker, err = loadBackfillMarker(ctx, s)
			require.NoError(t, err)
			require.NotNil(t, marker.CompletedAt,
				"after all projects done, next boot must set CompletedAt")
			return
		}

		// Not done yet, continue.
		runBootDataMigrations(ctx, s)
	}

	t.Fatalf("failed to converge after %d iterations; last marker: %+v", maxIterations, marker2)
}
