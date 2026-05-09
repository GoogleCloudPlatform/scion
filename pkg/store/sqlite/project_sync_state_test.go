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

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectSyncStateCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID := createTestProject(t, s)

	now := time.Now().UTC().Truncate(time.Second)

	// Upsert (create)
	state := &store.ProjectSyncState{
		ProjectID:     projectID,
		BrokerID:      "",
		LastSyncTime:  &now,
		LastCommitSHA: "abc123",
		FileCount:     42,
		TotalBytes:    123456,
	}
	err := s.UpsertProjectSyncState(ctx, state)
	require.NoError(t, err)

	// Get
	got, err := s.GetProjectSyncState(ctx, projectID, "")
	require.NoError(t, err)
	assert.Equal(t, projectID, got.ProjectID)
	assert.Equal(t, "", got.BrokerID)
	assert.NotNil(t, got.LastSyncTime)
	assert.Equal(t, now, *got.LastSyncTime)
	assert.Equal(t, "abc123", got.LastCommitSHA)
	assert.Equal(t, 42, got.FileCount)
	assert.Equal(t, int64(123456), got.TotalBytes)

	// Upsert (update)
	later := now.Add(5 * time.Minute)
	state.LastSyncTime = &later
	state.FileCount = 50
	state.TotalBytes = 200000
	err = s.UpsertProjectSyncState(ctx, state)
	require.NoError(t, err)

	got, err = s.GetProjectSyncState(ctx, projectID, "")
	require.NoError(t, err)
	assert.Equal(t, later, *got.LastSyncTime)
	assert.Equal(t, 50, got.FileCount)
	assert.Equal(t, int64(200000), got.TotalBytes)

	// Add a broker-scoped state
	brokerState := &store.ProjectSyncState{
		ProjectID:  projectID,
		BrokerID:   "broker-1",
		FileCount:  10,
		TotalBytes: 5000,
	}
	err = s.UpsertProjectSyncState(ctx, brokerState)
	require.NoError(t, err)

	// List
	states, err := s.ListProjectSyncStates(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, states, 2)

	// Delete hub-native state
	err = s.DeleteProjectSyncState(ctx, projectID, "")
	require.NoError(t, err)

	// Verify only broker state remains
	states, err = s.ListProjectSyncStates(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, states, 1)
	assert.Equal(t, "broker-1", states[0].BrokerID)

	// Get not found
	_, err = s.GetProjectSyncState(ctx, projectID, "")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Delete not found
	err = s.DeleteProjectSyncState(ctx, projectID, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestProjectSyncStateValidation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Empty project ID
	err := s.UpsertProjectSyncState(ctx, &store.ProjectSyncState{})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestProjectSyncStateCascadeDelete(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID := createTestProject(t, s)

	now := time.Now().UTC().Truncate(time.Second)
	state := &store.ProjectSyncState{
		ProjectID:    projectID,
		LastSyncTime: &now,
		FileCount:    5,
		TotalBytes:   1000,
	}
	err := s.UpsertProjectSyncState(ctx, state)
	require.NoError(t, err)

	// Delete the project (project) - sync state should cascade
	err = s.DeleteProject(ctx, projectID)
	require.NoError(t, err)

	states, err := s.ListProjectSyncStates(ctx, projectID)
	require.NoError(t, err)
	assert.Empty(t, states)
}
