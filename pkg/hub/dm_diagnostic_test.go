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

// Phase 0 diagnostic tests: verify RunDMDiagnostic detects noncanonical
// direct conversation rows and extraneous participant rows.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestDMDiagnostic_CleanState verifies the diagnostic reports zero issues
// when all DM conversations have valid keys and correct participants.
func TestDMDiagnostic_CleanState(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "diag-project",
		Slug: "diag-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agentA := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "diag-a",
		Slug:       "diag-a",
		ProjectID:  project.ID,
		Visibility: store.VisibilityPrivate,
	}
	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "diag-b",
		Slug:       "diag-b",
		ProjectID:  project.ID,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Create a clean DM.
	extRef, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)

	now := time.Now().UTC()
	conv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv))
	addConvParticipant(t, s, conv.ID, "agent", agentA.ID)
	addConvParticipant(t, s, conv.ID, "agent", agentB.ID)

	report, err := RunDMDiagnostic(ctx, s)
	require.NoError(t, err)

	require.Equal(t, 1, report.TotalDMConversations)
	require.Equal(t, 1, report.CanonicalCount)
	require.Equal(t, 0, report.NoncanonicalCount)
	require.Empty(t, report.UnparseableKeys)
	require.Empty(t, report.ExtraneousParticipants)
	require.Empty(t, report.MissingParticipants)
}

// TestDMDiagnostic_UnparseableKey verifies the diagnostic detects DMs with
// keys that cannot be parsed.
func TestDMDiagnostic_UnparseableKey(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	now := time.Now().UTC()
	conv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    "not-a-dm-key",
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv))

	report, err := RunDMDiagnostic(ctx, s)
	require.NoError(t, err)

	require.Equal(t, 1, report.TotalDMConversations)
	require.Equal(t, 0, report.CanonicalCount)
	require.Equal(t, 1, report.NoncanonicalCount)
	require.Len(t, report.UnparseableKeys, 1)
	require.Contains(t, report.UnparseableKeys[0].Detail, "unparseable")
}

// TestDMDiagnostic_ExtraneousParticipant documents that extraneous participant
// rows cannot be created through the current store API due to the
// CheckDMParticipantKey guard. The diagnostic would detect such rows if they
// existed (e.g., from pre-guard legacy data).
//
// This test verifies:
//  1. The store guard correctly rejects adding non-key participants to DMs.
//  2. The diagnostic correctly reports a clean DM (no extraneous participants).
func TestDMDiagnostic_ExtraneousParticipant(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "diag-extra-project",
		Slug: "diag-extra-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agentA := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "diag-extra-a",
		Slug:       "diag-extra-a",
		ProjectID:  project.ID,
		Visibility: store.VisibilityPrivate,
	}
	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "diag-extra-b",
		Slug:       "diag-extra-b",
		ProjectID:  project.ID,
		Visibility: store.VisibilityPrivate,
	}
	intruder := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "diag-intruder",
		Slug:       "diag-intruder",
		ProjectID:  project.ID,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	require.NoError(t, s.CreateAgent(ctx, agentB))
	require.NoError(t, s.CreateAgent(ctx, intruder))

	extRef, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)

	now := time.Now().UTC()
	conv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv))
	addConvParticipant(t, s, conv.ID, "agent", agentA.ID)
	addConvParticipant(t, s, conv.ID, "agent", agentB.ID)

	// Verify the store guard rejects adding a non-key participant.
	t.Run("store_rejects_extraneous_participant", func(t *testing.T) {
		p := &store.ConversationParticipant{
			ID:             api.NewUUID(),
			ConversationID: conv.ID,
			PrincipalKind:  "agent",
			PrincipalID:    intruder.ID,
			Role:           "member",
			JoinedAt:       time.Now().UTC(),
		}
		err := s.AddParticipant(ctx, p)
		require.Error(t, err, "store should reject non-key participant")
	})

	// Verify the diagnostic reports this DM as canonical (clean).
	t.Run("diagnostic_reports_clean", func(t *testing.T) {
		report, diagErr := RunDMDiagnostic(ctx, s)
		require.NoError(t, diagErr)

		require.Equal(t, 1, report.TotalDMConversations)
		require.Equal(t, 1, report.CanonicalCount)
		require.Equal(t, 0, report.NoncanonicalCount)
		require.Empty(t, report.ExtraneousParticipants,
			"with store-level guard, no extraneous participants should be possible")
	})
}

// TestDMDiagnostic_MissingParticipant verifies the diagnostic detects
// key members without corresponding participant rows.
func TestDMDiagnostic_MissingParticipant(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "diag-missing-project",
		Slug: "diag-missing-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agentA := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "diag-miss-a",
		Slug:       "diag-miss-a",
		ProjectID:  project.ID,
		Visibility: store.VisibilityPrivate,
	}
	agentB := &store.Agent{
		ID:         api.NewUUID(),
		Name:       "diag-miss-b",
		Slug:       "diag-miss-b",
		ProjectID:  project.ID,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	require.NoError(t, s.CreateAgent(ctx, agentB))

	extRef, err := messages.DMConversationKey("agent", agentA.ID, "agent", agentB.ID)
	require.NoError(t, err)

	now := time.Now().UTC()
	conv := &store.Conversation{
		ID:             api.NewUUID(),
		Kind:           "direct",
		Surface:        "native",
		ExternalRef:    extRef,
		DriftState:     "active",
		LastActivityAt: now,
		CreatedAt:      now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv))

	// Only add one participant — the other is missing.
	addConvParticipant(t, s, conv.ID, "agent", agentA.ID)

	report, err := RunDMDiagnostic(ctx, s)
	require.NoError(t, err)

	require.Equal(t, 1, report.NoncanonicalCount)
	require.Len(t, report.MissingParticipants, 1)
	require.Contains(t, report.MissingParticipants[0].Detail, agentB.ID)
}

// TestDMDiagnostic_MixedConversations verifies the diagnostic correctly
// handles a mix of canonical and noncanonical DMs alongside group conversations.
func TestDMDiagnostic_MixedConversations(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "diag-mixed-project",
		Slug: "diag-mixed-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agents := make([]*store.Agent, 4)
	for i := range agents {
		agents[i] = &store.Agent{
			ID:         api.NewUUID(),
			Name:       fmt.Sprintf("diag-mixed-%d", i),
			Slug:       fmt.Sprintf("diag-mixed-%d", i),
			ProjectID:  project.ID,
			Visibility: store.VisibilityPrivate,
		}
		require.NoError(t, s.CreateAgent(ctx, agents[i]))
	}

	now := time.Now().UTC()

	// Canonical DM.
	extRef1, err := messages.DMConversationKey("agent", agents[0].ID, "agent", agents[1].ID)
	require.NoError(t, err)
	conv1 := &store.Conversation{
		ID: api.NewUUID(), Kind: "direct", Surface: "native",
		ExternalRef: extRef1, DriftState: "active",
		LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv1))
	addConvParticipant(t, s, conv1.ID, "agent", agents[0].ID)
	addConvParticipant(t, s, conv1.ID, "agent", agents[1].ID)

	// Noncanonical DM (malformed key).
	conv2 := &store.Conversation{
		ID: api.NewUUID(), Kind: "direct", Surface: "native",
		ExternalRef: "legacy-dm-key", DriftState: "active",
		LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv2))

	// Group conversation (should be ignored by diagnostic).
	conv3 := &store.Conversation{
		ID: api.NewUUID(), Kind: "group", Surface: "native",
		ProjectID: &project.ID, DisplayName: "Group Chat", DriftState: "active",
		LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(ctx, conv3))

	report, err := RunDMDiagnostic(ctx, s)
	require.NoError(t, err)

	require.Equal(t, 2, report.TotalDMConversations, "should only count direct conversations")
	require.Equal(t, 1, report.CanonicalCount)
	require.Equal(t, 1, report.NoncanonicalCount)
}
