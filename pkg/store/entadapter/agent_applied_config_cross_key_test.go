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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The targeted applied-config writers each edit only their own key:
// recording the pending runtime target keeps a recorded native Cloud Run
// decision, and recording or removing that decision keeps the pending
// runtime target.
func TestAgentStore_TargetedAppliedConfigWritersKeepEachOthersKeys(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	t.Run("ClearAgentRuntimeTarget with recordPending keeps nativeCloudRun", func(t *testing.T) {
		a := makeAgent(projectID, "cross-key-clear")
		a.AppliedConfig = &store.AgentAppliedConfig{Image: "example/image:1", RuntimeTarget: "docker"}
		require.NoError(t, s.CreateAgent(ctx, a))
		require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))
		require.True(t, storedNativeCloudRun(t, s, a.ID))

		cleared, _, err := s.ClearAgentRuntimeTarget(ctx, a.ID, true)
		require.NoError(t, err)
		assert.True(t, cleared)

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		assert.Empty(t, got.AppliedConfig.RuntimeTarget)
		assert.Equal(t, "docker", got.AppliedConfig.RuntimeTargetPending)
		assert.True(t, got.AppliedConfig.NativeCloudRun, "the clear keeps the native Cloud Run decision")
		assert.Equal(t, "example/image:1", got.AppliedConfig.Image)
	})

	t.Run("ClearAgentRuntimeTarget recording only the pending target keeps nativeCloudRun", func(t *testing.T) {
		a := makeAgent(projectID, "cross-key-pending-only")
		a.AppliedConfig = &store.AgentAppliedConfig{Image: "example/image:1"}
		require.NoError(t, s.CreateAgent(ctx, a))
		require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))
		before, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, before.AppliedConfig)
		require.True(t, before.AppliedConfig.NativeCloudRun)

		cleared, _, err := s.ClearAgentRuntimeTarget(ctx, a.ID, true)
		require.NoError(t, err)
		assert.False(t, cleared, "recording only the pending target removes nothing")

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		assert.Equal(t, store.RuntimeTargetPendingDefault, got.AppliedConfig.RuntimeTargetPending)
		assert.True(t, got.AppliedConfig.NativeCloudRun, "the pending-only clear keeps the native Cloud Run decision")
		assert.Equal(t, before.StateVersion, got.StateVersion, "recording only the pending target does not bump state_version")
		assert.Equal(t, "example/image:1", got.AppliedConfig.Image)
	})

	t.Run("SetAgentNativeCloudRun keeps runtimeTargetPending", func(t *testing.T) {
		a := makeAgent(projectID, "cross-key-native")
		a.AppliedConfig = &store.AgentAppliedConfig{Image: "example/image:1", RuntimeTargetPending: "docker"}
		require.NoError(t, s.CreateAgent(ctx, a))

		require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, true))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		assert.True(t, got.AppliedConfig.NativeCloudRun)
		assert.Equal(t, "docker", got.AppliedConfig.RuntimeTargetPending, "recording the decision keeps the pending target")

		require.NoError(t, s.SetAgentNativeCloudRun(ctx, a.ID, false))
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		assert.False(t, got.AppliedConfig.NativeCloudRun)
		assert.Equal(t, "docker", got.AppliedConfig.RuntimeTargetPending, "removing the decision keeps the pending target")
	})
}
