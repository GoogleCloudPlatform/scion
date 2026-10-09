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
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestIsUniqueViolation_ClassifiesDriverErrors pins isUniqueViolation against
// real driver errors from the test backend (SQLite by default, Postgres under
// -tags integration with SCION_TEST_POSTGRES_URL): a duplicate primary key and
// a duplicate unique index are unique violations, a foreign-key violation is a
// constraint error but not a unique violation.
func TestIsUniqueViolation_ClassifiesDriverErrors(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)

	assert.False(t, isUniqueViolation(nil))
	assert.False(t, isUniqueViolation(errors.New("UNIQUE constraint failed: fake")),
		"a plain error that only looks like a unique violation must not match")

	newInstallation := func() *ent.GithubInstallationCreate {
		return client.GithubInstallation.Create().
			SetID(4242).
			SetAccountLogin("acme").
			SetAccountType("Organization").
			SetAppID(1).
			SetRepositories("[]").
			SetStatus(store.GitHubInstallationStatusActive)
	}
	require.NoError(t, newInstallation().Exec(ctx))
	err := newInstallation().Exec(ctx)
	require.Error(t, err)
	assert.True(t, ent.IsConstraintError(err), "duplicate primary key: want a constraint error, got %v", err)
	assert.True(t, isUniqueViolation(err), "duplicate primary key must be a unique violation: %v", err)

	projectID := uuid.New()
	keys := client.AgentIdentityKey
	require.NoError(t, keys.Create().SetProjectID(projectID).SetKey("k").SetAgentID(uuid.New()).Exec(ctx))
	err = keys.Create().SetProjectID(projectID).SetKey("k").SetAgentID(uuid.New()).Exec(ctx)
	require.Error(t, err)
	assert.True(t, isUniqueViolation(err), "duplicate unique index must be a unique violation: %v", err)

	err = client.GroupMembership.Create().SetGroupID(uuid.New()).Exec(ctx)
	require.Error(t, err)
	assert.True(t, ent.IsConstraintError(err), "dangling group_id: want a constraint error, got %v", err)
	assert.False(t, isUniqueViolation(err), "a foreign-key violation must not be a unique violation: %v", err)
}

// TestReplaceAgentIdentityKeys_ConflictWithOtherAgent pins that a key already
// held by another agent in the project still maps to ErrIdentityKeyConflict
// now that only unique violations (not every constraint error) do.
func TestReplaceAgentIdentityKeys_ConflictWithOtherAgent(t *testing.T) {
	ctx := context.Background()
	s := NewAgentIdentityKeyStore(enttest.NewClient(t))
	projectID := uuid.NewString()

	require.NoError(t, s.ReplaceAgentIdentityKeys(ctx, uuid.NewString(), projectID, []string{"taken"}))
	err := s.ReplaceAgentIdentityKeys(ctx, uuid.NewString(), projectID, []string{"taken"})
	assert.ErrorIs(t, err, store.ErrIdentityKeyConflict)

	// The same key in a different project is not a conflict.
	require.NoError(t, s.ReplaceAgentIdentityKeys(ctx, uuid.NewString(), uuid.NewString(), []string{"taken"}))
}
