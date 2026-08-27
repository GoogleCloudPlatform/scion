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

package messaging

import (
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockAgentLookup implements AgentLookup for testing.
type mockAgentLookup struct {
	agents map[string]*store.Agent // key: "projectID/slug"
}

func (m *mockAgentLookup) GetAgentBySlug(_ context.Context, projectID, slug string) (*store.Agent, error) {
	key := projectID + "/" + slug
	if agent, ok := m.agents[key]; ok {
		return agent, nil
	}
	return nil, store.ErrNotFound
}

func TestNormalizeAgentRef_UUIDPassthrough(t *testing.T) {
	ctx := context.Background()
	agentStore := &mockAgentLookup{}
	id := uuid.NewString()

	result, err := NormalizeAgentRef(ctx, agentStore, "some-project", id)
	require.NoError(t, err)
	assert.Equal(t, id, result)
}

func TestNormalizeAgentRef_SlugResolution(t *testing.T) {
	ctx := context.Background()
	projectID := uuid.NewString()
	agentID := uuid.NewString()
	agentStore := &mockAgentLookup{
		agents: map[string]*store.Agent{
			projectID + "/my-agent": {ID: agentID, Slug: "my-agent", ProjectID: projectID},
		},
	}

	result, err := NormalizeAgentRef(ctx, agentStore, projectID, "my-agent")
	require.NoError(t, err)
	assert.Equal(t, agentID, result)
}

func TestNormalizeAgentRef_SlugNotFound(t *testing.T) {
	ctx := context.Background()
	agentStore := &mockAgentLookup{}
	projectID := uuid.NewString()

	_, err := NormalizeAgentRef(ctx, agentStore, projectID, "nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrNotFound), "expected ErrNotFound, got: %v", err)
}

func TestNormalizeAgentRef_InvalidFormat(t *testing.T) {
	ctx := context.Background()
	agentStore := &mockAgentLookup{}
	projectID := uuid.NewString()

	tests := []struct {
		name string
		ref  string
	}{
		{"empty string", ""},
		{"contains spaces", "my agent"},
		{"starts with hyphen", "-my-agent"},
		{"ends with hyphen", "my-agent-"},
		{"uppercase", "My-Agent"},
		{"special characters", "my@agent"},
		{"contains slash", "space/thread"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NormalizeAgentRef(ctx, agentStore, projectID, tt.ref)
			require.Error(t, err)
			if tt.ref != "" {
				assert.True(t, errors.Is(err, store.ErrInvalidInput), "expected ErrInvalidInput for %q, got: %v", tt.ref, err)
			}
		})
	}
}

func TestNormalizeAgentRef_EmptyRef(t *testing.T) {
	ctx := context.Background()
	agentStore := &mockAgentLookup{}

	_, err := NormalizeAgentRef(ctx, agentStore, "proj", "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidInput))
}

func TestNormalizeAgentRef_SlugWithoutProject(t *testing.T) {
	ctx := context.Background()
	agentStore := &mockAgentLookup{}

	_, err := NormalizeAgentRef(ctx, agentStore, "", "my-agent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, store.ErrInvalidInput))
}

func TestNormalizeAgentRef_UUIDWithoutProject(t *testing.T) {
	// UUID passthrough should work even without a project ID.
	ctx := context.Background()
	agentStore := &mockAgentLookup{}
	id := uuid.NewString()

	result, err := NormalizeAgentRef(ctx, agentStore, "", id)
	require.NoError(t, err)
	assert.Equal(t, id, result)
}
