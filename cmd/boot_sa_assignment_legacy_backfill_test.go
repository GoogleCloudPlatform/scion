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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSAAssignmentLegacyBackfill_WritesMarkerAndSkipsOnNextBoot: the first
// pass records the row for a legacy assign-mode agent, logs the skip of an
// agent with no edge, and writes the marker; the second pass sees the
// marker and leaves an agent added in between without a row.
func TestSAAssignmentLegacyBackfill_WritesMarkerAndSkipsOnNextBoot(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	proj := &store.Project{ID: uuid.NewString(), Name: "p", Slug: uuid.NewString()}
	require.NoError(t, s.CreateProject(ctx, proj))
	user := &store.User{ID: uuid.NewString(), Email: "u@test.com", DisplayName: "U", Role: "member", Status: store.UserStatusActive}
	require.NoError(t, s.CreateUser(ctx, user))
	saID := uuid.NewString()

	newAgent := func(slug string, withEdge bool) *store.Agent {
		a := &store.Agent{
			ID: uuid.NewString(), Slug: slug, Name: slug, ProjectID: proj.ID, Phase: "running",
			CreatedBy: user.ID, OwnerID: user.ID, Ancestry: []string{user.ID},
			AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: saID,
			}},
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		if withEdge {
			require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
				DelegatorType: store.DelegationPrincipalUser, DelegatorID: user.ID,
				DelegateType: store.DelegationPrincipalAgent, DelegateID: a.ID,
				ScopeType: store.RoleScopeProject, ScopeID: proj.ID, Role: "full", Active: true,
				AuthorityProvenance: store.AuthorityProvenance{
					ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: store.DelegationPrincipalUser,
					SourcePrincipalID: user.ID, SourceCredentialKind: store.SourceCredentialSession,
				},
				EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			}))
		}
		return a
	}
	legacy := newAgent("legacy", true)
	edgeless := newAgent("edgeless", false)

	buf, restore := captureSlog(t)
	defer restore()

	runSAAssignmentLegacyBackfill(ctx, s)

	out := buf.String()
	assert.Contains(t, out, "SA assignment legacy backfill: pass completed")
	assert.Contains(t, out, "agent left without an assignment")
	assert.Contains(t, out, edgeless.ID)
	assert.NotContains(t, out, "failed to write completion marker")
	done, err := IsMigrationComplete(ctx, s, MigrationSAAssignmentLegacyBackfill)
	require.NoError(t, err)
	assert.True(t, done, "a pass with per-agent skips still writes the marker")

	rows, err := s.GetActiveAgentServiceAccountAssignments(ctx, legacy.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SAAssignmentOriginLegacyBackfill, rows[0].Origin)
	assert.Equal(t, user.ID, rows[0].SourcePrincipalID)

	late := newAgent("late", true)
	runSAAssignmentLegacyBackfill(ctx, s)
	rows, err = s.GetActiveAgentServiceAccountAssignments(ctx, late.ID)
	require.NoError(t, err)
	assert.Empty(t, rows, "a completed backfill must not run again")
}
