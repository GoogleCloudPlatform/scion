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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentStore_ShellAgentOwnerImmutable(t *testing.T) {
	ctx := context.Background()
	agentStore, projectID := newTestAgentStore(t)

	ownerA := api.NewUUID()
	ownerB := api.NewUUID()

	shellAgent := &store.Agent{
		ID:            api.NewUUID(),
		Slug:          "shell-owner-imm",
		Name:          "shell-owner-imm",
		ProjectID:     projectID,
		OwnerID:       ownerA,
		CreatedBy:     ownerA,
		Phase:         "running",
		HarnessConfig: config.ShellHarnessConfigName,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
	}
	require.NoError(t, agentStore.CreateAgent(ctx, shellAgent))

	t.Run("same owner update succeeds", func(t *testing.T) {
		got, err := agentStore.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		got.Name = "shell-owner-imm-renamed"
		require.NoError(t, agentStore.UpdateAgent(ctx, got))

		after, err := agentStore.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, "shell-owner-imm-renamed", after.Name)
		assert.Equal(t, ownerA, after.OwnerID)
	})

	t.Run("changing owner_id on shell agent is refused", func(t *testing.T) {
		got, err := agentStore.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		got.OwnerID = ownerB
		err = agentStore.UpdateAgent(ctx, got)
		require.ErrorIs(t, err, store.ErrShellAgentOwnerImmutable)
		require.ErrorIs(t, err, store.ErrInvalidInput)

		after, err := agentStore.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, ownerA, after.OwnerID)
	})

	t.Run("clearing owner_id on shell agent is refused", func(t *testing.T) {
		got, err := agentStore.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		got.OwnerID = ""
		err = agentStore.UpdateAgent(ctx, got)
		require.ErrorIs(t, err, store.ErrShellAgentOwnerImmutable)

		after, err := agentStore.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, ownerA, after.OwnerID)
	})

	t.Run("nil AppliedConfig still fails closed via stored harness_config column", func(t *testing.T) {
		got, err := agentStore.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		got.AppliedConfig = nil
		got.HarnessConfig = ""
		got.OwnerID = ownerB
		err = agentStore.UpdateAgent(ctx, got)
		require.ErrorIs(t, err, store.ErrShellAgentOwnerImmutable)

		after, err := agentStore.GetAgent(ctx, shellAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, ownerA, after.OwnerID)
	})

	t.Run("setting owner_id on ownerless shell agent is refused", func(t *testing.T) {
		ownerless := &store.Agent{
			ID:            api.NewUUID(),
			Slug:          "shell-ownerless",
			Name:          "shell-ownerless",
			ProjectID:     projectID,
			Phase:         "running",
			HarnessConfig: config.ShellHarnessConfigName,
			AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
		}
		require.NoError(t, agentStore.CreateAgent(ctx, ownerless))

		got, err := agentStore.GetAgent(ctx, ownerless.ID)
		require.NoError(t, err)
		got.OwnerID = ownerB
		err = agentStore.UpdateAgent(ctx, got)
		require.ErrorIs(t, err, store.ErrShellAgentOwnerImmutable)
	})

	t.Run("control: non-shell agent owner_id remains mutable", func(t *testing.T) {
		llmAgent := &store.Agent{
			ID:            api.NewUUID(),
			Slug:          "llm-owner-mut",
			Name:          "llm-owner-mut",
			ProjectID:     projectID,
			OwnerID:       ownerA,
			CreatedBy:     ownerA,
			Phase:         "running",
			HarnessConfig: "claude",
			AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
		}
		require.NoError(t, agentStore.CreateAgent(ctx, llmAgent))

		got, err := agentStore.GetAgent(ctx, llmAgent.ID)
		require.NoError(t, err)
		got.OwnerID = ownerB
		require.NoError(t, agentStore.UpdateAgent(ctx, got))

		after, err := agentStore.GetAgent(ctx, llmAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, ownerB, after.OwnerID)
	})
}
