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

// Phase 0 diagnostic: inventory existing noncanonical direct conversation rows
// and extraneous participant rows.
//
// This diagnostic provides a read-only report mechanism for:
//   1. Direct conversations with unparseable or malformed DM keys
//   2. Direct conversations with extraneous participant rows (participants
//      not named in the canonical key)
//   3. Direct conversations with missing participant rows (key participants
//      without listing rows)
//
// Rules:
//   - Only migrate where historical principal IDs prove the pair unambiguously.
//   - Never guess from display names or delete historical conversations.
//   - Unresolvable rows fail closed and have a documented operator recovery path.

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

// DMDiagnosticReport represents the diagnostic output for DM conversations.
type DMDiagnosticReport struct {
	TotalDMConversations int
	CanonicalCount       int
	NoncanonicalCount    int

	// Details of issues found.
	UnparseableKeys         []DMDiagnosticIssue
	ExtraneousParticipants  []DMDiagnosticIssue
	MissingParticipants     []DMDiagnosticIssue
	MigratableConversations []DMDiagnosticMigration
}

// DMDiagnosticIssue describes a single noncanonical DM row.
type DMDiagnosticIssue struct {
	ConversationID string
	ExternalRef    string
	Detail         string
}

// DMDiagnosticMigration describes a DM that can be deterministically migrated
// because historical principal IDs prove the pair unambiguously.
type DMDiagnosticMigration struct {
	ConversationID     string
	CurrentExternalRef string
	CanonicalKey       string
	Reason             string
}

// runDMDiagnostic inspects all direct conversations in the store and reports
// noncanonical rows, extraneous participants, and missing participants.
// This is a read-only operation — it never modifies data.
func runDMDiagnostic(ctx context.Context, s store.Store) (*DMDiagnosticReport, error) {
	report := &DMDiagnosticReport{}

	// List all conversations — we filter for kind=direct below.
	// In a production setting, this would use pagination; for the diagnostic
	// test, loading all conversations is acceptable.
	result, err := s.ListConversations(ctx, store.ConversationFilter{
		Kind: "direct",
	}, store.ListOptions{Limit: 10000})
	if err != nil {
		return nil, fmt.Errorf("listing conversations: %w", err)
	}

	for _, conv := range result.Items {
		report.TotalDMConversations++

		// Check 1: Can we parse the DM key?
		kindA, idA, kindB, idB, parseErr := messages.ParseDMKey(conv.ExternalRef)
		if parseErr != nil {
			report.NoncanonicalCount++
			report.UnparseableKeys = append(report.UnparseableKeys, DMDiagnosticIssue{
				ConversationID: conv.ID,
				ExternalRef:    conv.ExternalRef,
				Detail:         fmt.Sprintf("unparseable DM key: %v", parseErr),
			})
			continue
		}

		// Key is parseable — check participants.
		participants, partErr := s.ListParticipants(ctx, conv.ID)
		if partErr != nil {
			report.NoncanonicalCount++
			report.UnparseableKeys = append(report.UnparseableKeys, DMDiagnosticIssue{
				ConversationID: conv.ID,
				ExternalRef:    conv.ExternalRef,
				Detail:         fmt.Sprintf("error listing participants: %v", partErr),
			})
			continue
		}

		isCanonical := true

		// Check 2: Extraneous participants (not named in the key).
		for _, p := range participants {
			if (p.PrincipalKind == kindA && p.PrincipalID == idA) ||
				(p.PrincipalKind == kindB && p.PrincipalID == idB) {
				continue
			}
			isCanonical = false
			report.ExtraneousParticipants = append(report.ExtraneousParticipants, DMDiagnosticIssue{
				ConversationID: conv.ID,
				ExternalRef:    conv.ExternalRef,
				Detail: fmt.Sprintf("extraneous participant: kind=%s id=%s (not in DM key)",
					p.PrincipalKind, p.PrincipalID),
			})
		}

		// Check 3: Missing participants (key members without rows).
		hasA, hasB := false, false
		for _, p := range participants {
			if p.PrincipalKind == kindA && p.PrincipalID == idA {
				hasA = true
			}
			if p.PrincipalKind == kindB && p.PrincipalID == idB {
				hasB = true
			}
		}
		if !hasA {
			// Missing participant is a listing gap, not necessarily a data
			// integrity issue (the key is still the ACL). Report it.
			report.MissingParticipants = append(report.MissingParticipants, DMDiagnosticIssue{
				ConversationID: conv.ID,
				ExternalRef:    conv.ExternalRef,
				Detail:         fmt.Sprintf("missing participant row: kind=%s id=%s", kindA, idA),
			})
		}
		if !hasB {
			report.MissingParticipants = append(report.MissingParticipants, DMDiagnosticIssue{
				ConversationID: conv.ID,
				ExternalRef:    conv.ExternalRef,
				Detail:         fmt.Sprintf("missing participant row: kind=%s id=%s", kindB, idB),
			})
		}

		if isCanonical && hasA && hasB {
			report.CanonicalCount++
		} else {
			report.NoncanonicalCount++
		}
	}

	return report, nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

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

	report, err := runDMDiagnostic(ctx, s)
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

	report, err := runDMDiagnostic(ctx, s)
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
		report, diagErr := runDMDiagnostic(ctx, s)
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

	report, err := runDMDiagnostic(ctx, s)
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

	report, err := runDMDiagnostic(ctx, s)
	require.NoError(t, err)

	require.Equal(t, 2, report.TotalDMConversations, "should only count direct conversations")
	require.Equal(t, 1, report.CanonicalCount)
	require.Equal(t, 1, report.NoncanonicalCount)
}
